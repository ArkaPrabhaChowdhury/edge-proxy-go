package main

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRateLimiterRedisOutageReturnsBoundedError(t *testing.T) {
	cfg := defaultConfig().RateLimit
	cfg.Redis.Addr = "127.0.0.1:1"
	limiter := newRateLimiter(cfg)
	defer limiter.client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := limiter.ping(ctx); err == nil {
		t.Fatal("expected Redis outage to return an error")
	}
}

func TestBackendPoolPartialFailureKeepsHealthyBackendAvailable(t *testing.T) {
	pool := newBackendPool([]string{"failed", "healthy"})
	pool.mu.Lock()
	pool.backends[0].healthy = false
	pool.mu.Unlock()

	if got := pool.next(); got != "healthy" {
		t.Fatalf("expected healthy backend, got %q", got)
	}
}

func TestBackendPoolSlowBackendIsAvoided(t *testing.T) {
	pool := newBackendPool([]string{"slow", "fast"})
	pool.backends[0].stats.requests.Store(10)
	pool.backends[0].stats.totalLatencyNs.Store(int64(10 * time.Second))
	pool.backends[1].stats.requests.Store(10)
	pool.backends[1].stats.totalLatencyNs.Store(int64(10 * time.Millisecond))

	if got := pool.next(); got != "fast" {
		t.Fatalf("expected fast backend, got %q", got)
	}
}

func TestRequestIDsAreUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := nextRequestID()
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate request ID %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestTLSMisconfigurationIsRejected(t *testing.T) {
	cfg := defaultConfig()
	cfg.Proxy.TLS.Enabled = true
	cfg.Proxy.TLS.CertFile = "missing-cert.pem"
	cfg.Proxy.TLS.KeyFile = "missing-key.pem"
	if err := cfg.Validate(); err != nil {
		return
	}
	if _, err := tls.LoadX509KeyPair(cfg.Proxy.TLS.CertFile, cfg.Proxy.TLS.KeyFile); err == nil {
		t.Fatal("expected missing TLS files to be rejected")
	}
}

func TestBackendPoolReconfigureDuringTraffic(t *testing.T) {
	pool := newBackendPool([]string{"backend-1", "backend-2", "backend-3"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			pool.reconfigure([]string{"backend-1", "backend-2"})
			pool.reconfigure([]string{"backend-1", "backend-2", "backend-3"})
		}
	}()
	for i := 0; i < 1000; i++ {
		if address := pool.next(); address == "" {
			t.Fatal("expected a backend during reconfiguration")
		}
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("backend reconfiguration did not complete")
	}
}

func TestRuntimeConfigReloadDuringTraffic(t *testing.T) {
	redisAddr := os.Getenv("RELIABILITY_REDIS_ADDR")
	if redisAddr == "" {
		t.Skip("set RELIABILITY_REDIS_ADDR to run the Redis-backed reload test")
	}

	oldCfg, oldPath, oldPool, oldLimiter := cfg, runtimeConfigPath, pool, rl
	t.Cleanup(func() {
		cfg, runtimeConfigPath, pool, rl = oldCfg, oldPath, oldPool, oldLimiter
	})

	loaded := defaultConfig()
	loaded.RateLimit.Redis.Addr = redisAddr
	loaded.Backends = []string{"backend-1", "backend-2"}
	data, err := yaml.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + string(os.PathSeparator) + "config.yaml"
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg = loaded
	runtimeConfigPath = path
	pool = newBackendPool(loaded.Backends)
	rl = newRateLimiter(loaded.RateLimit)
	testLimiter := rl
	t.Cleanup(func() { _ = testLimiter.client.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			_ = currentConfig()
			_ = pool.next()
		}
	}()
	if err := reloadRuntimeConfig(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("traffic did not complete during config reload")
	}
}

func TestFailoverTransportCanReachHealthyBackend(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	pool := newBackendPool([]string{"127.0.0.1:1", listener.Addr().String()})
	pool.mu.Lock()
	pool.backends[0].healthy = false
	pool.mu.Unlock()
	if got := pool.next(); got != listener.Addr().String() {
		t.Fatalf("expected failover backend %q, got %q", listener.Addr().String(), got)
	}
}
