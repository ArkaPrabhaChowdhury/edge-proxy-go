package main

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// BackendStatus is the JSON-serialisable view of a backend entry.
type BackendStatus struct {
	Address string `json:"address"`
	Healthy bool   `json:"healthy"`
}

type backendEntry struct {
	address string
	healthy bool
}

type backendPool struct {
	mu       sync.RWMutex
	backends []backendEntry
	current  int
}

func newBackendPool(addresses []string) *backendPool {
	entries := make([]backendEntry, len(addresses))
	for i, addr := range addresses {
		entries[i] = backendEntry{address: addr, healthy: true}
	}
	return &backendPool{backends: entries}
}

// next returns the next healthy backend via round-robin.
// Returns "" when all backends are down.
func (p *backendPool) next() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.backends)
	for i := 0; i < n; i++ {
		idx := (p.current + i) % n
		if p.backends[idx].healthy {
			p.current = (idx + 1) % n
			return p.backends[idx].address
		}
	}
	return ""
}

func (p *backendPool) healthyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, b := range p.backends {
		if b.healthy {
			n++
		}
	}
	return n
}

func (p *backendPool) status() []BackendStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]BackendStatus, len(p.backends))
	for i, b := range p.backends {
		out[i] = BackendStatus{Address: b.address, Healthy: b.healthy}
	}
	return out
}

// startHealthChecker periodically TCP-dials each backend and updates its
// healthy flag, printing a line only when state changes.
func startHealthChecker(pool *backendPool, cfg HealthCheckConfig) {
	interval := time.Duration(cfg.IntervalSeconds) * time.Second
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			pool.mu.Lock()
			for i := range pool.backends {
				addr := pool.backends[i].address
				was := pool.backends[i].healthy
				conn, err := net.DialTimeout("tcp", addr, timeout)
				if err == nil {
					conn.Close()
					pool.backends[i].healthy = true
					if !was {
						fmt.Printf("[health] %s  UP\n", addr)
					}
				} else {
					pool.backends[i].healthy = false
					if was {
						fmt.Printf("[health] %s  DOWN: %v\n", addr, err)
					}
				}
			}
			pool.mu.Unlock()
		}
	}()
}
