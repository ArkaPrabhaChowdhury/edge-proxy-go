package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyHandlerKeepsHTTPConnectionAliveAndForwardsHeaders(t *testing.T) {
	var requests atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("X-Forwarded-Proto") != "http" {
			t.Errorf("missing forwarded proto: %q", r.Header.Get("X-Forwarded-Proto"))
		}
		if r.Header.Get("X-Forwarded-For") == "" {
			t.Error("missing forwarded for")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	}))
	defer backend.Close()

	oldCfg, oldPool, oldLimiter := cfg, pool, rl
	t.Cleanup(func() { cfg, pool, rl = oldCfg, oldPool, oldLimiter })
	address := strings.TrimPrefix(backend.URL, "http://")
	testCfg := defaultConfig()
	testCfg.Backends = []string{address}
	testCfg.RateLimit.Enabled = false
	testCfg.HealthCheck.Enabled = false
	cfg, pool, rl = testCfg, newBackendPool([]string{address}), newRateLimiter(testCfg.RateLimit)

	proxy := httptest.NewServer(http.HandlerFunc(proxyHandler))
	defer proxy.Close()
	client := proxy.Client()
	for i := 0; i < 2; i++ {
		resp, err := client.Get(proxy.URL + "/api/items")
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201, got %d", resp.StatusCode)
		}
		resp.Body.Close()
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("expected two backend requests, got %d", got)
	}
}

func TestLocalRateLimiterUsesConfiguredWindow(t *testing.T) {
	config := defaultConfig().RateLimit
	config.Redis.Addr = ""
	config.Requests = 2
	config.WindowSeconds = 60
	config.BaseRequestsPerSecond = 1
	config.SlidingWindowSeconds = 60
	limiter := newRateLimiter(config)
	for i := 0; i < 2; i++ {
		decision, err := limiter.Evaluate(context.Background(), "ip:127.0.0.1", "127.0.0.1", config)
		if err != nil || decision.Action != ActionAllow {
			t.Fatalf("request %d should be allowed: %+v %v", i, decision, err)
		}
	}
	decision, err := limiter.Evaluate(context.Background(), "ip:127.0.0.1", "127.0.0.1", config)
	if err != nil || decision.Action != ActionBlock {
		t.Fatalf("third request should be blocked: %+v %v", decision, err)
	}
}

func TestWeightedLeastConnectionsDistribution(t *testing.T) {
	p := newBackendPool([]string{"api-1:8000", "api-2:8000"})
	p.backends[0].weight = 2
	p.backends[1].weight = 1
	counts := map[string]int{}
	for i := 0; i < 30; i++ {
		counts[p.next()]++
	}
	if counts["api-1:8000"] != 20 || counts["api-2:8000"] != 10 {
		t.Fatalf("expected 2:1 weighted distribution, got %+v", counts)
	}
}

func TestHTTPHealthProbeRequiresHealthyStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	client := &http.Client{Timeout: time.Second}
	ok, err := probeHTTPBackend(client, address, "/health")
	if err != nil || ok {
		t.Fatalf("expected 503 health probe to fail: ok=%t err=%v", ok, err)
	}
}

func FuzzHTTPParserDoesNotPanic(f *testing.F) {
	f.Add("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	f.Add("POST / HTTP/1.1\r\nContent-Length: 10\r\n\r\nshort")
	f.Fuzz(func(t *testing.T, input string) {
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(input)))
		if err == nil && req.Body != nil {
			req.Body.Close()
		}
	})
}
