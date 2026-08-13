package main

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// BackendStatus is the JSON-serialisable view of a backend entry.
type BackendStatus struct {
	Address        string  `json:"address"`
	Weight         int     `json:"weight"`
	Healthy        bool    `json:"healthy"`
	Available      bool    `json:"available"`
	Disabled       bool    `json:"disabled"`
	Draining       bool    `json:"draining"`
	BreakerState   string  `json:"breaker_state"`
	ConsecutiveErr int64   `json:"consecutive_errors"`
	Requests       int64   `json:"requests"`
	Errors         int64   `json:"errors"`
	ActiveRequests int64   `json:"active_requests"`
	AvgLatencyMs   float64 `json:"avg_latency_ms"`
	LastStatus     int     `json:"last_status"`
}

type backendEntry struct {
	address            string
	weight             int
	healthPath         string
	healthy            bool
	transport          *http.Transport
	stats              *backendStats
	breakerState       string
	breakerOpenedUntil time.Time
	consecutiveErr     int
	halfOpenInFlight   bool
	disabled           bool
	draining           bool
}

type backendStats struct {
	requests       atomic.Int64
	errors         atomic.Int64
	activeRequests atomic.Int64
	totalLatencyNs atomic.Int64
	lastStatus     atomic.Int64
}

type backendPool struct {
	mu       sync.RWMutex
	backends []backendEntry
	current  int
	stop     chan struct{}
	stopOnce sync.Once
}

const (
	breakerStateClosed   = "closed"
	breakerStateOpen     = "open"
	breakerStateHalfOpen = "half-open"

	breakerFailureThreshold = 3
	breakerCooldown         = 15 * time.Second
)

func newBackendPool(addresses []string) *backendPool {
	entries := make([]backendEntry, len(addresses))
	for i, addr := range addresses {
		entries[i] = backendEntry{
			address:      addr,
			weight:       1,
			healthy:      true,
			transport:    newBackendTransport(),
			stats:        &backendStats{},
			breakerState: breakerStateClosed,
		}
	}
	return &backendPool{backends: entries, stop: make(chan struct{})}
}

func newBackendPoolFromConfig(cfg *Config) *backendPool {
	weights := make(map[string]int)
	healthPaths := make(map[string]string)
	addresses := append([]string(nil), cfg.Backends...)
	for _, address := range cfg.Backends {
		weights[address] = 1
	}
	for _, route := range cfg.Routes {
		for _, backend := range route.Backends {
			if _, ok := weights[backend.Address]; !ok {
				addresses = append(addresses, backend.Address)
			}
			if backend.Weight > weights[backend.Address] {
				weights[backend.Address] = backend.Weight
			}
			if route.HealthPath != "" {
				healthPaths[backend.Address] = route.HealthPath
			}
		}
	}
	p := newBackendPool(addresses)
	p.mu.Lock()
	for i := range p.backends {
		p.backends[i].weight = maxInt(1, weights[p.backends[i].address])
		p.backends[i].transport.CloseIdleConnections()
		p.backends[i].transport = newBackendTransportWithTimeout(durationMs(cfg.Proxy.ConnectTimeoutMs), durationMs(cfg.Proxy.ResponseHeaderMs))
		if healthPaths[p.backends[i].address] != "" {
			p.backends[i].healthPath = healthPaths[p.backends[i].address]
		}
	}
	p.mu.Unlock()
	return p
}

// next returns the healthiest available backend based on active load and latency.
// Returns "" when all backends are down or breakers are open.
func (p *backendPool) next() string {
	return p.nextFor(nil)
}

func (p *backendPool) nextFor(allowed map[string]bool) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	type candidate struct {
		idx   int
		score float64
	}
	candidates := make([]candidate, 0, len(p.backends))
	for i := range p.backends {
		entry := &p.backends[i]
		if allowed != nil && !allowed[entry.address] {
			continue
		}
		if !entry.healthy {
			continue
		}
		if entry.disabled || entry.draining {
			continue
		}
		if entry.breakerState == breakerStateOpen {
			if now.Before(entry.breakerOpenedUntil) {
				continue
			}
			entry.breakerState = breakerStateHalfOpen
			entry.halfOpenInFlight = false
		}
		if entry.breakerState == breakerStateHalfOpen && entry.halfOpenInFlight {
			continue
		}
		score := backendScore(entry)
		if entry.breakerState == breakerStateHalfOpen {
			score -= 250
		}
		candidates = append(candidates, candidate{idx: i, score: score})
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].idx < candidates[j].idx
		}
		return candidates[i].score < candidates[j].score
	})
	minScore := candidates[0].score
	weighted := make([]candidate, 0, len(candidates))
	totalWeight := 0
	for _, item := range candidates {
		if item.score > minScore+0.001 {
			break
		}
		weighted = append(weighted, item)
		totalWeight += maxInt(1, p.backends[item.idx].weight)
	}
	position := p.current % maxInt(totalWeight, 1)
	chosenIndex := weighted[0].idx
	for _, item := range weighted {
		weight := maxInt(1, p.backends[item.idx].weight)
		if position < weight {
			chosenIndex = item.idx
			break
		}
		position -= weight
	}
	chosen := &p.backends[chosenIndex]
	p.current = (p.current + 1) % maxInt(totalWeight, 1)
	if chosen.breakerState == breakerStateHalfOpen {
		chosen.halfOpenInFlight = true
	}
	return chosen.address
}

func backendScore(entry *backendEntry) float64 {
	requests := entry.stats.requests.Load()
	totalLatencyNs := entry.stats.totalLatencyNs.Load()
	avgLatencyMs := 0.0
	if requests > 0 {
		avgLatencyMs = float64(totalLatencyNs) / float64(requests) / float64(time.Millisecond)
	}
	activePenalty := float64(entry.stats.activeRequests.Load()) / float64(maxInt(1, entry.weight)) * 1000
	errorPenalty := float64(entry.consecutiveErr) * 200
	return activePenalty + avgLatencyMs + errorPenalty
}

func (p *backendPool) availableCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	count := 0
	for i := range p.backends {
		entry := &p.backends[i]
		if !entry.healthy {
			continue
		}
		if entry.disabled || entry.draining {
			continue
		}
		if entry.breakerState == breakerStateOpen && now.Before(entry.breakerOpenedUntil) {
			continue
		}
		if entry.breakerState == breakerStateOpen && !now.Before(entry.breakerOpenedUntil) {
			entry.breakerState = breakerStateHalfOpen
			entry.halfOpenInFlight = false
		}
		if entry.breakerState == breakerStateHalfOpen && entry.halfOpenInFlight {
			continue
		}
		count++
	}
	return count
}

func (p *backendPool) healthyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for i := range p.backends {
		if p.backends[i].healthy {
			n++
		}
	}
	return n
}

func (p *backendPool) status() []BackendStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]BackendStatus, len(p.backends))
	now := time.Now()
	for i := range p.backends {
		b := &p.backends[i]
		requests := b.stats.requests.Load()
		totalLatencyNs := b.stats.totalLatencyNs.Load()
		avgLatencyMs := 0.0
		if requests > 0 {
			avgLatencyMs = float64(totalLatencyNs) / float64(requests) / float64(time.Millisecond)
		}
		out[i] = BackendStatus{
			Address:        b.address,
			Weight:         b.weight,
			Healthy:        b.healthy,
			Available:      p.backendAvailableLocked(b, now),
			Disabled:       b.disabled,
			Draining:       b.draining,
			BreakerState:   p.currentBreakerStateLocked(b, now),
			ConsecutiveErr: int64(b.consecutiveErr),
			Requests:       requests,
			Errors:         b.stats.errors.Load(),
			ActiveRequests: b.stats.activeRequests.Load(),
			AvgLatencyMs:   avgLatencyMs,
			LastStatus:     int(b.stats.lastStatus.Load()),
		}
	}
	return out
}

func (p *backendPool) currentBreakerStateLocked(entry *backendEntry, now time.Time) string {
	if entry.breakerState == breakerStateOpen && !now.Before(entry.breakerOpenedUntil) {
		return breakerStateHalfOpen
	}
	return entry.breakerState
}

func (p *backendPool) backendAvailableLocked(entry *backendEntry, now time.Time) bool {
	if !entry.healthy {
		return false
	}
	if entry.disabled || entry.draining {
		return false
	}
	if entry.breakerState == breakerStateOpen && now.Before(entry.breakerOpenedUntil) {
		return false
	}
	if entry.breakerState == breakerStateHalfOpen && entry.halfOpenInFlight {
		return false
	}
	return true
}

func (p *backendPool) transportFor(address string) *http.Transport {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.backends {
		if p.backends[i].address == address {
			return p.backends[i].transport
		}
	}
	return newBackendTransport()
}

func (p *backendPool) startRequest(address string) func(int, time.Duration, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.backends {
		if p.backends[i].address != address {
			continue
		}
		stats := p.backends[i].stats
		stats.activeRequests.Add(1)
		return func(statusCode int, latency time.Duration, err error) {
			stats.activeRequests.Add(-1)
			stats.requests.Add(1)
			stats.totalLatencyNs.Add(latency.Nanoseconds())
			stats.lastStatus.Store(int64(statusCode))
			failed := err != nil || statusCode >= http.StatusInternalServerError
			if failed {
				stats.errors.Add(1)
			}
			p.finishRequest(address, statusCode, failed)
		}
	}
	return func(int, time.Duration, error) {}
}

func (p *backendPool) finishRequest(address string, statusCode int, failed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.backends {
		entry := &p.backends[i]
		if entry.address != address {
			continue
		}
		if entry.breakerState == breakerStateHalfOpen {
			entry.halfOpenInFlight = false
		}
		if !failed {
			entry.consecutiveErr = 0
			entry.breakerState = breakerStateClosed
			entry.breakerOpenedUntil = time.Time{}
			return
		}
		entry.consecutiveErr++
		if entry.consecutiveErr >= breakerFailureThreshold {
			entry.breakerState = breakerStateOpen
			entry.breakerOpenedUntil = time.Now().Add(breakerCooldown)
		}
		return
	}
}

func (p *backendPool) close() error {
	p.stopOnce.Do(func() { close(p.stop) })
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.backends {
		if p.backends[i].transport == nil {
			continue
		}
		p.backends[i].transport.CloseIdleConnections()
	}
	return nil
}

func (p *backendPool) reconfigure(addresses []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	existing := make(map[string]*backendEntry, len(p.backends))
	for i := range p.backends {
		existing[p.backends[i].address] = &p.backends[i]
	}

	nextBackends := make([]backendEntry, 0, len(addresses))
	keep := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		keep[address] = true
		if backend, ok := existing[address]; ok {
			nextBackends = append(nextBackends, cloneBackendEntry(backend))
			continue
		}
		nextBackends = append(nextBackends, backendEntry{
			address:      address,
			healthy:      true,
			transport:    newBackendTransport(),
			stats:        &backendStats{},
			breakerState: breakerStateClosed,
		})
	}

	for i := range p.backends {
		backend := &p.backends[i]
		if keep[backend.address] || backend.transport == nil {
			continue
		}
		backend.transport.CloseIdleConnections()
	}

	p.backends = nextBackends
	if len(p.backends) == 0 {
		p.current = 0
		return
	}
	p.current = p.current % len(p.backends)
}

func cloneBackendEntry(source *backendEntry) backendEntry {
	clone := backendEntry{
		address:            source.address,
		weight:             source.weight,
		healthPath:         source.healthPath,
		healthy:            source.healthy,
		transport:          source.transport,
		breakerState:       source.breakerState,
		breakerOpenedUntil: source.breakerOpenedUntil,
		consecutiveErr:     source.consecutiveErr,
		halfOpenInFlight:   source.halfOpenInFlight,
		disabled:           source.disabled,
		draining:           source.draining,
	}
	clone.stats = &backendStats{}
	clone.stats.requests.Store(source.stats.requests.Load())
	clone.stats.errors.Store(source.stats.errors.Load())
	clone.stats.activeRequests.Store(source.stats.activeRequests.Load())
	clone.stats.totalLatencyNs.Store(source.stats.totalLatencyNs.Load())
	clone.stats.lastStatus.Store(source.stats.lastStatus.Load())
	return clone
}

func (p *backendPool) setBackendState(address, action string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.backends {
		entry := &p.backends[i]
		if entry.address != address {
			continue
		}
		switch action {
		case "disable":
			entry.disabled = true
			entry.draining = false
		case "enable":
			entry.disabled = false
			entry.draining = false
		case "drain":
			entry.disabled = false
			entry.draining = true
		default:
			return fmt.Errorf("unknown backend action %q", action)
		}
		return nil
	}
	return fmt.Errorf("backend %q not found", address)
}

func newBackendTransport() *http.Transport {
	return newBackendTransportWithTimeout(5*time.Second, 15*time.Second)
}

func newBackendTransportWithTimeout(connectTimeout, responseHeaderTimeout time.Duration) *http.Transport {
	if connectTimeout <= 0 {
		connectTimeout = 5 * time.Second
	}
	if responseHeaderTimeout <= 0 {
		responseHeaderTimeout = 15 * time.Second
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    false,
	}
}

// startHealthChecker periodically calls an HTTP health endpoint and updates
// the healthy flag. A successful TCP connection alone is not health.
func startHealthChecker(pool *backendPool, cfgProvider func() HealthCheckConfig) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		nextRun := time.Time{}
		for {
			select {
			case <-pool.stop:
				return
			case <-ticker.C:
			}
			cfg := cfgProvider()
			if !cfg.Enabled {
				continue
			}
			if time.Now().Before(nextRun) {
				continue
			}
			interval := time.Duration(cfg.IntervalSeconds) * time.Second
			timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
			nextRun = time.Now().Add(interval)
			pool.mu.RLock()
			checks := make([]struct{ address, path string }, len(pool.backends))
			for i := range pool.backends {
				path := pool.backends[i].healthPath
				if path == "" {
					path = cfg.Path
				}
				checks[i] = struct{ address, path string }{pool.backends[i].address, path}
			}
			pool.mu.RUnlock()
			client := &http.Client{Timeout: timeout}
			for _, check := range checks {
				ok, err := probeHTTPBackend(client, check.address, check.path)
				pool.mu.Lock()
				for i := range pool.backends {
					if pool.backends[i].address != check.address {
						continue
					}
					was := pool.backends[i].healthy
					pool.backends[i].healthy = ok
					if !ok {
						pool.backends[i].halfOpenInFlight = false
					}
					if was != ok {
						if ok {
							fmt.Printf("[health] %s UP (%s)\n", check.address, check.path)
						} else {
							fmt.Printf("[health] %s DOWN (%s): %v\n", check.address, check.path, err)
						}
					}
				}
				pool.mu.Unlock()
			}
		}
	}()
}

func probeHTTPBackend(client *http.Client, address, path string) (bool, error) {
	resp, err := client.Get("http://" + address + path)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return false, err
	}
	return resp.StatusCode >= 200 && resp.StatusCode < 400, nil
}
