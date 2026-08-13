package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestProxyHTTPRequestForwardsRequestBody(t *testing.T) {
	pool = newBackendPool(nil)
	backend, receivedBody, shutdown := startTestBackend(t, http.StatusCreated, "backend-ok")
	defer shutdown()

	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()
	defer proxyConn.Close()

	req := httptestRequest(t, "POST", "http://example.com/upload", "payload-body")

	errCh := make(chan error, 1)
	go func() {
		_, err := proxyHTTPRequest(proxyConn, req, backend, "req-test-1")
		errCh <- err
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}

	if got := <-receivedBody; got != "payload-body" {
		t.Fatalf("expected backend to receive payload-body, got %q", got)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, resp.StatusCode)
	}
	if string(body) != "backend-ok" {
		t.Fatalf("expected backend body, got %q", string(body))
	}
	if got := resp.Header.Get("X-Request-Id"); got != "req-test-1" {
		t.Fatalf("expected request id header req-test-1, got %q", got)
	}
}

func TestProxyHTTPRequestPreservesUpstreamStatus(t *testing.T) {
	pool = newBackendPool(nil)
	backend, _, shutdown := startTestBackend(t, http.StatusBadGateway, "upstream-failed")
	defer shutdown()

	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()
	defer proxyConn.Close()

	req := httptestRequest(t, "GET", "http://example.com/hello", "")

	statusCh := make(chan int, 1)
	errCh := make(chan error, 1)
	go func() {
		status, err := proxyHTTPRequest(proxyConn, req, backend, "req-test-2")
		statusCh <- status
		errCh <- err
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain response body: %v", err)
	}

	if got := <-statusCh; got != http.StatusBadGateway {
		t.Fatalf("expected proxy status %d, got %d", http.StatusBadGateway, got)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected client status %d, got %d", http.StatusBadGateway, resp.StatusCode)
	}
}

func TestProxyHTTPRequestRecordsBackendMetrics(t *testing.T) {
	backend, _, shutdown := startTestBackend(t, http.StatusAccepted, "ok")
	defer shutdown()

	pool = newBackendPool([]string{backend})

	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()
	defer proxyConn.Close()

	req := httptestRequest(t, "GET", "http://example.com/metrics", "")

	errCh := make(chan error, 1)
	go func() {
		_, err := proxyHTTPRequest(proxyConn, req, backend, "req-test-3")
		errCh <- err
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain response body: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}

	statuses := pool.status()
	if len(statuses) != 1 {
		t.Fatalf("expected one backend status, got %d", len(statuses))
	}
	if statuses[0].Requests != 1 {
		t.Fatalf("expected request count 1, got %d", statuses[0].Requests)
	}
	if statuses[0].Errors != 0 {
		t.Fatalf("expected error count 0, got %d", statuses[0].Errors)
	}
	if statuses[0].LastStatus != http.StatusAccepted {
		t.Fatalf("expected last status %d, got %d", http.StatusAccepted, statuses[0].LastStatus)
	}
	if statuses[0].AvgLatencyMs < 0 {
		t.Fatalf("expected non-negative latency, got %f", statuses[0].AvgLatencyMs)
	}
}

func TestAppendForwardedFor(t *testing.T) {
	headers := make(http.Header)
	appendForwardedFor(headers, "10.0.0.1")
	appendForwardedFor(headers, "10.0.0.2")

	if got := headers.Get("X-Forwarded-For"); got != "10.0.0.1, 10.0.0.2" {
		t.Fatalf("unexpected forwarded header %q", got)
	}
}

func TestBackendPoolReconfigurePreservesExistingBackendStats(t *testing.T) {
	pool := newBackendPool([]string{"backend-a", "backend-b"})
	pool.backends[0].stats.requests.Store(7)
	pool.backends[0].stats.errors.Store(2)

	pool.reconfigure([]string{"backend-a", "backend-c"})

	statuses := pool.status()
	if len(statuses) != 2 {
		t.Fatalf("expected 2 backends after reconfigure, got %d", len(statuses))
	}
	if statuses[0].Address != "backend-a" || statuses[0].Requests != 7 || statuses[0].Errors != 2 {
		t.Fatalf("expected backend-a stats preserved, got %+v", statuses[0])
	}
	if statuses[1].Address != "backend-c" {
		t.Fatalf("expected backend-c added, got %+v", statuses[1])
	}
}

func TestBackendPoolPrefersLessLoadedBackend(t *testing.T) {
	pool := newBackendPool([]string{"backend-a", "backend-b"})
	pool.backends[0].stats.activeRequests.Store(3)
	pool.backends[0].stats.requests.Store(10)
	pool.backends[0].stats.totalLatencyNs.Store(int64(20 * time.Millisecond * 10))

	pool.backends[1].stats.activeRequests.Store(0)
	pool.backends[1].stats.requests.Store(10)
	pool.backends[1].stats.totalLatencyNs.Store(int64(5 * time.Millisecond * 10))

	if got := pool.next(); got != "backend-b" {
		t.Fatalf("expected backend-b to be selected, got %q", got)
	}
}

func TestBackendPoolOpensCircuitBreakerAfterFailures(t *testing.T) {
	pool := newBackendPool([]string{"backend-a"})

	for i := 0; i < breakerFailureThreshold; i++ {
		pool.finishRequest("backend-a", http.StatusBadGateway, true)
	}

	statuses := pool.status()
	if len(statuses) != 1 {
		t.Fatalf("expected one backend status, got %d", len(statuses))
	}
	if statuses[0].BreakerState != breakerStateOpen {
		t.Fatalf("expected breaker open, got %q", statuses[0].BreakerState)
	}
	if statuses[0].Available {
		t.Fatal("expected open breaker backend to be unavailable")
	}
}

func TestBackendPoolHalfOpenRecoversOnSuccess(t *testing.T) {
	pool := newBackendPool([]string{"backend-a"})
	pool.backends[0].breakerState = breakerStateOpen
	pool.backends[0].breakerOpenedUntil = time.Now().Add(-time.Second)

	if got := pool.next(); got != "backend-a" {
		t.Fatalf("expected half-open backend to be selected, got %q", got)
	}
	if pool.backends[0].breakerState != breakerStateHalfOpen {
		t.Fatalf("expected half-open state, got %q", pool.backends[0].breakerState)
	}

	pool.finishRequest("backend-a", http.StatusOK, false)

	statuses := pool.status()
	if statuses[0].BreakerState != breakerStateClosed {
		t.Fatalf("expected closed breaker after recovery, got %q", statuses[0].BreakerState)
	}
	if statuses[0].ConsecutiveErr != 0 {
		t.Fatalf("expected consecutive errors reset, got %d", statuses[0].ConsecutiveErr)
	}
}

func TestBackendPoolSetBackendState(t *testing.T) {
	pool := newBackendPool([]string{"backend-a"})

	if err := pool.setBackendState("backend-a", "drain"); err != nil {
		t.Fatalf("drain failed: %v", err)
	}
	status := pool.status()[0]
	if !status.Draining || status.Available {
		t.Fatalf("expected draining backend to be unavailable, got %+v", status)
	}

	if err := pool.setBackendState("backend-a", "disable"); err != nil {
		t.Fatalf("disable failed: %v", err)
	}
	status = pool.status()[0]
	if !status.Disabled {
		t.Fatalf("expected disabled backend, got %+v", status)
	}

	if err := pool.setBackendState("backend-a", "enable"); err != nil {
		t.Fatalf("enable failed: %v", err)
	}
	status = pool.status()[0]
	if status.Disabled || status.Draining || !status.Available {
		t.Fatalf("expected enabled backend to be available, got %+v", status)
	}
}

func TestResolveRateLimitConfigUsesMatchingPolicy(t *testing.T) {
	cfg := defaultConfig()
	cfg.Policies = []PolicyConfig{
		{
			Name:                  "gold-api",
			RoutePrefix:           "/api",
			Methods:               []string{"GET"},
			APIKeys:               []string{"gold-key"},
			BaseRequestsPerSecond: 50,
			BurstCapacity:         100,
			BlockSeconds:          5,
		},
	}
	applyRateLimitDefaults(cfg)
	req := httptestRequest(t, "GET", "http://example.com/api/items", "")

	resolved := resolveRateLimitConfig(req, "gold-key", cfg)
	if resolved.Name != "gold-api" {
		t.Fatalf("expected policy gold-api, got %q", resolved.Name)
	}
	if resolved.BaseRequestsPerSecond != 50 {
		t.Fatalf("expected policy rps 50, got %f", resolved.BaseRequestsPerSecond)
	}
	if resolved.BurstCapacity != 100 {
		t.Fatalf("expected burst 100, got %f", resolved.BurstCapacity)
	}
}

func BenchmarkProxyHTTPRequest(b *testing.B) {
	backend, _, shutdown := startTestBackendForBenchmark(b, http.StatusOK, "ok")
	defer shutdown()

	pool = newBackendPool([]string{backend})
	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()
	defer proxyConn.Close()

	respReader := bufio.NewReader(clientConn)
	b.ResetTimer()
	for range b.N {
		req := benchmarkRequest(b, "GET", "http://example.com/bench", "")
		errCh := make(chan error, 1)
		go func() {
			_, err := proxyHTTPRequest(proxyConn, req, backend, "req-bench")
			errCh <- err
		}()
		resp, err := http.ReadResponse(respReader, req)
		if err != nil {
			b.Fatalf("read response: %v", err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			b.Fatalf("drain response body: %v", err)
		}
		resp.Body.Close()
		if err := <-errCh; err != nil {
			b.Fatalf("proxy request failed: %v", err)
		}
	}
}

func startTestBackend(t *testing.T, statusCode int, responseBody string) (string, <-chan string, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	receivedBody := make(chan string, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		defer req.Body.Close()

		body, err := io.ReadAll(req.Body)
		if err != nil {
			return
		}
		receivedBody <- string(body)

		fmt.Fprintf(
			conn,
			"HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			statusCode,
			http.StatusText(statusCode),
			len(responseBody),
			responseBody,
		)
	}()

	shutdown := func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("backend did not shut down")
		}
	}

	return listener.Addr().String(), receivedBody, shutdown
}

func startTestBackendForBenchmark(b *testing.B, statusCode int, responseBody string) (string, <-chan string, func()) {
	b.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}

	receivedBody := make(chan string, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				defer req.Body.Close()
				body, err := io.ReadAll(req.Body)
				if err == nil {
					select {
					case receivedBody <- string(body):
					default:
					}
				}
				fmt.Fprintf(
					conn,
					"HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
					statusCode,
					http.StatusText(statusCode),
					len(responseBody),
					responseBody,
				)
			}(conn)
		}
	}()

	shutdown := func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			b.Fatal("backend did not shut down")
		}
	}
	return listener.Addr().String(), receivedBody, shutdown
}

func httptestRequest(t *testing.T, method, rawURL, body string) *http.Request {
	t.Helper()

	req, err := http.NewRequest(method, rawURL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	return req
}

func benchmarkRequest(b *testing.B, method, rawURL, body string) *http.Request {
	b.Helper()

	req, err := http.NewRequest(method, rawURL, strings.NewReader(body))
	if err != nil {
		b.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	req.RemoteAddr = "127.0.0.1:12345"
	return req
}
