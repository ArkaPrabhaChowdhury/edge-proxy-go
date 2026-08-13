package main

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

type benchmarkWindow interface {
	Allow(string, time.Time) bool
}

type fixedWindowBenchmarkLimiter struct {
	mu       sync.Mutex
	window   time.Duration
	limit    int
	requests map[string]fixedWindowState
}

type fixedWindowState struct {
	start time.Time
	count int
}

func newFixedWindowBenchmarkLimiter(limit int, window time.Duration) *fixedWindowBenchmarkLimiter {
	return &fixedWindowBenchmarkLimiter{limit: limit, window: window, requests: make(map[string]fixedWindowState)}
}

func (l *fixedWindowBenchmarkLimiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	state := l.requests[key]
	if state.start.IsZero() || now.Sub(state.start) >= l.window {
		state = fixedWindowState{start: now, count: 0}
	}
	if state.count >= l.limit {
		l.requests[key] = state
		return false
	}
	state.count++
	l.requests[key] = state
	return true
}

type slidingWindowBenchmarkLimiter struct {
	mu       sync.Mutex
	window   time.Duration
	limit    int
	requests map[string][]time.Time
}

func newSlidingWindowBenchmarkLimiter(limit int, window time.Duration) *slidingWindowBenchmarkLimiter {
	return &slidingWindowBenchmarkLimiter{limit: limit, window: window, requests: make(map[string][]time.Time)}
}

func (l *slidingWindowBenchmarkLimiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	requests := l.requests[key]
	first := 0
	for first < len(requests) && !requests[first].After(cutoff) {
		first++
	}
	requests = requests[first:]
	if len(requests) >= l.limit {
		l.requests[key] = requests
		return false
	}
	l.requests[key] = append(requests, now)
	return true
}

func benchmarkLimiter(b *testing.B, limiter benchmarkWindow) {
	b.Helper()
	now := time.Unix(1_700_000_000, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		limiter.Allow("benchmark-client", now.Add(time.Duration(i)*time.Microsecond))
	}
}

func BenchmarkRateLimitFixedWindowLocal(b *testing.B) {
	benchmarkLimiter(b, newFixedWindowBenchmarkLimiter(1000, time.Second))
}

func BenchmarkRateLimitSlidingWindowLocal(b *testing.B) {
	benchmarkLimiter(b, newSlidingWindowBenchmarkLimiter(1000, time.Second))
}

func BenchmarkRateLimitRedisBacked(b *testing.B) {
	addr := os.Getenv("BENCH_REDIS_ADDR")
	if addr == "" {
		b.Skip("set BENCH_REDIS_ADDR to benchmark the Redis-backed limiter")
	}

	cfg := defaultConfig().RateLimit
	cfg.Redis.Addr = addr
	limiter := newRateLimiter(cfg)
	if err := limiter.ping(context.Background()); err != nil {
		b.Skipf("Redis unavailable at %s: %v", addr, err)
	}
	b.Cleanup(func() { _ = limiter.client.Close() })

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := limiter.Evaluate(context.Background(), "benchmark-client", "benchmark-client", cfg); err != nil {
			b.Fatalf("Redis-backed limit evaluation failed: %v", err)
		}
	}
}

func BenchmarkBackendSelectionOneBackend(b *testing.B) {
	benchmarkBackendSelection(b, 1)
}

func BenchmarkBackendSelectionThreeBackends(b *testing.B) {
	benchmarkBackendSelection(b, 3)
}

func benchmarkBackendSelection(b *testing.B, count int) {
	addresses := make([]string, count)
	for i := range addresses {
		addresses[i] = "backend-" + string(rune('1'+i))
	}
	pool := newBackendPool(addresses)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		address := pool.next()
		if address == "" {
			b.Fatal("expected an available backend")
		}
	}
}

func BenchmarkCircuitBreakerClosed(b *testing.B) {
	pool := newBackendPool([]string{"backend-1"})
	benchmarkCircuitBreakerState(b, pool, breakerStateClosed)
}

func BenchmarkCircuitBreakerOpen(b *testing.B) {
	pool := newBackendPool([]string{"backend-1"})
	for i := 0; i < breakerFailureThreshold; i++ {
		finish := pool.startRequest("backend-1")
		finish(500, time.Millisecond, context.Canceled)
	}
	benchmarkCircuitBreakerState(b, pool, breakerStateOpen)
}

func BenchmarkCircuitBreakerHalfOpen(b *testing.B) {
	pool := newBackendPool([]string{"backend-1"})
	for i := 0; i < breakerFailureThreshold; i++ {
		finish := pool.startRequest("backend-1")
		finish(500, time.Millisecond, context.Canceled)
	}
	pool.mu.Lock()
	pool.backends[0].breakerOpenedUntil = time.Now().Add(-time.Second)
	pool.mu.Unlock()
	benchmarkCircuitBreakerState(b, pool, breakerStateHalfOpen)
}

func benchmarkCircuitBreakerState(b *testing.B, pool *backendPool, expected string) {
	b.Helper()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		address := pool.next()
		if expected == breakerStateOpen && address != "" {
			b.Fatal("open breaker selected a backend")
		}
		if expected != breakerStateOpen && address == "" {
			b.Fatal("expected breaker probe to be available")
		}
		if address != "" {
			finish := pool.startRequest(address)
			finish(200, time.Microsecond, nil)
		}
	}
}
