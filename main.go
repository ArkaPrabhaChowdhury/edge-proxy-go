package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ── Shared state ──────────────────────────────────────────────────────────────

var (
	cfg  *Config
	pool *backendPool

	mu    sync.Mutex
	ipMap = make(map[string][]int64)

	statsMu       sync.Mutex
	totalRequests int64
	totalLimited  int64
	activeConns   int64
	accessLog     []LogEntry
)

// ── Types ─────────────────────────────────────────────────────────────────────

type LogEntry struct {
	Time      string `json:"time"`
	IP        string `json:"ip"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	Backend   string `json:"backend"`
}

type RateLimitInfo struct {
	Requests      int `json:"requests"`
	WindowSeconds int `json:"window_seconds"`
}

type StatsResponse struct {
	TotalRequests int64           `json:"total_requests"`
	TotalLimited  int64           `json:"total_limited"`
	ActiveConns   int64           `json:"active_conns"`
	IPCounts      map[string]int  `json:"ip_counts"`
	Backends      []BackendStatus `json:"backends"`
	RateLimit     RateLimitInfo   `json:"rate_limit"`
	Log           []LogEntry      `json:"log"`
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	var err error
	cfg, err = loadConfig("config.yaml")
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}

	pool = newBackendPool(cfg.Backends)

	// Optional: start local backends inside the same process (demo / single-binary mode).
	if strings.TrimSpace(os.Getenv("INPROC_BACKENDS")) == "1" {
		startLocalBackend(":9000")
		startLocalBackend(":9001")
		startLocalBackend(":9002")
	}

	if cfg.HealthCheck.Enabled {
		startHealthChecker(pool, cfg.HealthCheck)
	}

	statsServer := startStatsServer()

	listener, err := startListener()
	if err != nil {
		fmt.Fprintln(os.Stderr, "proxy listen error:", err)
		os.Exit(1)
	}

	scheme := "http"
	if cfg.Proxy.TLS.Enabled {
		scheme = "https"
	}
	fmt.Printf("Proxy     → %s://localhost%s\n", scheme, cfg.Proxy.Port)
	fmt.Printf("Dashboard → http://localhost%s\n", cfg.Stats.Port)
	fmt.Printf("Metrics   → http://localhost%s/metrics\n", cfg.Stats.Port)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-stop:
				default:
					fmt.Println("accept error:", err)
				}
				return
			}
			go handleConnection(conn)
		}
	}()

	<-stop
	fmt.Println("\nshutting down gracefully…")

	listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	statsServer.Shutdown(ctx)

	// Wait up to 30 s for in-flight connections to drain.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		statsMu.Lock()
		active := activeConns
		statsMu.Unlock()
		if active == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("done.")
}

// startListener opens a TCP (or TLS) listener on the configured proxy port.
func startListener() (net.Listener, error) {
	if cfg.Proxy.TLS.Enabled {
		cert, err := tls.LoadX509KeyPair(cfg.Proxy.TLS.CertFile, cfg.Proxy.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("TLS cert: %w", err)
		}
		return tls.Listen("tcp", cfg.Proxy.Port, &tls.Config{
			Certificates: []tls.Certificate{cert},
		})
	}
	return net.Listen("tcp", cfg.Proxy.Port)
}

// startStatsServer launches the HTTP server for the dashboard, /stats, and /metrics.
func startStatsServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", statsHandler)
	mux.HandleFunc("/metrics", metricsHandler)
	mux.HandleFunc("/", dashboardHandler)

	var handler http.Handler = mux
	if cfg.Stats.Auth.Enabled {
		handler = basicAuth(mux, cfg.Stats.Auth.Username, cfg.Stats.Auth.Password)
	}

	srv := &http.Server{Addr: cfg.Stats.Port, Handler: handler}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Println("stats server error:", err)
		}
	}()
	return srv
}

func basicAuth(next http.Handler, username, password string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != username || p != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="Edge Proxy Dashboard"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Stats helpers ─────────────────────────────────────────────────────────────

func buildStats() StatsResponse {
	mu.Lock()
	ipCounts := make(map[string]int)
	cutoff := time.Now().Unix() - int64(cfg.RateLimit.WindowSeconds)
	for ip, timestamps := range ipMap {
		count := 0
		for _, t := range timestamps {
			if t > cutoff {
				count++
			}
		}
		if count > 0 {
			ipCounts[ip] = count
		}
	}
	mu.Unlock()

	statsMu.Lock()
	defer statsMu.Unlock()
	return StatsResponse{
		TotalRequests: totalRequests,
		TotalLimited:  totalLimited,
		ActiveConns:   activeConns,
		IPCounts:      ipCounts,
		Backends:      pool.status(),
		RateLimit: RateLimitInfo{
			Requests:      cfg.RateLimit.Requests,
			WindowSeconds: cfg.RateLimit.WindowSeconds,
		},
		Log: accessLog,
	}
}

// ── HTTP handlers (stats port) ────────────────────────────────────────────────

func statsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(buildStats())
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	statsMu.Lock()
	total := totalRequests
	limited := totalLimited
	active := activeConns
	statsMu.Unlock()

	statuses := pool.status()
	healthy := 0
	for _, s := range statuses {
		if s.Healthy {
			healthy++
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP edgeproxy_requests_total Total requests proxied\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_requests_total counter\n")
	fmt.Fprintf(w, "edgeproxy_requests_total %d\n\n", total)
	fmt.Fprintf(w, "# HELP edgeproxy_rate_limited_total Requests rejected by rate limiter\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_rate_limited_total counter\n")
	fmt.Fprintf(w, "edgeproxy_rate_limited_total %d\n\n", limited)
	fmt.Fprintf(w, "# HELP edgeproxy_active_connections Current open proxy connections\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_active_connections gauge\n")
	fmt.Fprintf(w, "edgeproxy_active_connections %d\n\n", active)
	fmt.Fprintf(w, "# HELP edgeproxy_healthy_backends Backends passing health checks\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_healthy_backends gauge\n")
	fmt.Fprintf(w, "edgeproxy_healthy_backends %d\n\n", healthy)
	fmt.Fprintf(w, "# HELP edgeproxy_total_backends Total backends configured\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_total_backends gauge\n")
	fmt.Fprintf(w, "edgeproxy_total_backends %d\n", len(statuses))
}

func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	exe, err := os.Executable()
	if err != nil {
		http.Error(w, "cannot resolve executable path", http.StatusInternalServerError)
		return
	}
	dir := exe[:strings.LastIndex(exe, string(os.PathSeparator))]
	http.ServeFile(w, r, dir+string(os.PathSeparator)+"dashboard.html")
}

// ── Proxy connection handler ──────────────────────────────────────────────────

func handleConnection(conn net.Conn) {
	defer conn.Close()

	statsMu.Lock()
	activeConns++
	statsMu.Unlock()
	defer func() {
		statsMu.Lock()
		activeConns--
		statsMu.Unlock()
	}()

	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return
	}

	start := time.Now()
	reader := bufio.NewReader(conn)

	requestLine, err := reader.ReadString('\n')
	if err != nil {
		return
	}

	parts := strings.Split(requestLine, " ")
	if len(parts) < 3 {
		return
	}
	method := strings.TrimSpace(parts[0])
	path := strings.TrimSpace(parts[1])

	// Internal routes — bypass rate limiting and backend forwarding.
	switch {
	case method == "GET" && path == "/stats":
		readRequestHeaders(reader)
		serveStatsTCP(conn)
		return
	case method == "GET" && path == "/metrics":
		readRequestHeaders(reader)
		serveMetricsTCP(conn)
		return
	case method == "GET" && path == "/favicon.ico":
		readRequestHeaders(reader)
		fmt.Fprintf(conn, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
		return
	case method == "GET" && (path == "/" || path == "/index.html"):
		readRequestHeaders(reader)
		serveDashboard(conn)
		statsMu.Lock()
		totalRequests++
		appendLog(LogEntry{
			Time: time.Now().Format("15:04:05"), IP: host,
			Method: method, Path: path, Status: 200,
			LatencyMs: time.Since(start).Milliseconds(), Backend: "dashboard",
		})
		statsMu.Unlock()
		return
	}

	// ── Rate limit ────────────────────────────────────────────────────────────
	mu.Lock()
	now := time.Now().Unix()
	cutoff := now - int64(cfg.RateLimit.WindowSeconds)

	var active []int64
	for _, t := range ipMap[host] {
		if t > cutoff {
			active = append(active, t)
		}
	}

	if len(active) >= cfg.RateLimit.Requests {
		mu.Unlock()
		body := "Rate limit exceeded"
		fmt.Fprintf(conn,
			"HTTP/1.1 429 Too Many Requests\r\n"+
				"Content-Type: text/plain\r\n"+
				"Retry-After: %d\r\n"+
				"Content-Length: %d\r\n\r\n%s",
			cfg.RateLimit.WindowSeconds, len(body), body,
		)
		statsMu.Lock()
		totalRequests++
		totalLimited++
		appendLog(LogEntry{
			Time: time.Now().Format("15:04:05"), IP: host,
			Method: method, Path: path, Status: 429,
			LatencyMs: time.Since(start).Milliseconds(), Backend: "-",
		})
		statsMu.Unlock()
		return
	}

	active = append(active, now)
	ipMap[host] = active
	mu.Unlock()

	// ── Forward to backend ────────────────────────────────────────────────────
	backend := pool.next()
	if backend == "" {
		body := "No healthy backends available"
		fmt.Fprintf(conn,
			"HTTP/1.1 503 Service Unavailable\r\n"+
				"Content-Type: text/plain\r\n"+
				"Content-Length: %d\r\n\r\n%s",
			len(body), body,
		)
		return
	}

	backendConn, err := net.Dial("tcp", backend)
	if err != nil {
		body := "Bad Gateway"
		fmt.Fprintf(conn,
			"HTTP/1.1 502 Bad Gateway\r\n"+
				"Content-Type: text/plain\r\n"+
				"Content-Length: %d\r\n\r\n%s",
			len(body), body,
		)
		return
	}
	defer backendConn.Close()

	// Replay the request line + headers to the backend.
	backendConn.Write([]byte(requestLine))
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		backendConn.Write([]byte(line))
		if line == "\r\n" {
			break
		}
	}

	io.Copy(conn, backendConn)

	statsMu.Lock()
	totalRequests++
	appendLog(LogEntry{
		Time: time.Now().Format("15:04:05"), IP: host,
		Method: method, Path: path, Status: 200,
		LatencyMs: time.Since(start).Milliseconds(), Backend: backend,
	})
	statsMu.Unlock()
}

func appendLog(entry LogEntry) {
	accessLog = append(accessLog, entry)
	if len(accessLog) > 100 {
		accessLog = accessLog[len(accessLog)-100:]
	}
}

// ── Local in-process backends (demo / single-binary mode) ────────────────────

func startLocalBackend(port string) {
	listener, err := net.Listen("tcp", port)
	if err != nil {
		fmt.Println("backend listen error:", err)
		return
	}
	fmt.Printf("local backend on %s\n", listener.Addr())
	go func() {
		defer listener.Close()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleBackendConnection(conn, port)
		}
	}()
}

func handleBackendConnection(conn net.Conn, port string) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	request, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	parts := strings.Split(request, " ")
	if len(parts) < 3 {
		return
	}
	path := parts[1]
	body := "Hello from " + port
	if path == "/hello" {
		body = "Hey! How are you?"
	} else if path == "/home" {
		body = "This is the home page"
	}
	fmt.Fprintf(conn,
		"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
		len(body), body,
	)
}

// ── TCP-level response helpers ────────────────────────────────────────────────

func readRequestHeaders(reader *bufio.Reader) {
	for {
		line, err := reader.ReadString('\n')
		if err != nil || line == "\r\n" {
			return
		}
	}
}

func serveDashboard(conn net.Conn) {
	content, err := os.ReadFile("dashboard.html")
	if err != nil {
		body := "Dashboard not found"
		fmt.Fprintf(conn,
			"HTTP/1.1 500 Internal Server Error\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
			len(body), body,
		)
		return
	}
	fmt.Fprintf(conn,
		"HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: %d\r\n\r\n",
		len(content),
	)
	conn.Write(content)
}

func serveStatsTCP(conn net.Conn) {
	payload, _ := json.Marshal(buildStats())
	fmt.Fprintf(conn,
		"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n",
		len(payload),
	)
	conn.Write(payload)
}

func serveMetricsTCP(conn net.Conn) {
	statsMu.Lock()
	total := totalRequests
	limited := totalLimited
	active := activeConns
	statsMu.Unlock()

	statuses := pool.status()
	healthy := 0
	for _, s := range statuses {
		if s.Healthy {
			healthy++
		}
	}

	body := fmt.Sprintf(
		"edgeproxy_requests_total %d\n"+
			"edgeproxy_rate_limited_total %d\n"+
			"edgeproxy_active_connections %d\n"+
			"edgeproxy_healthy_backends %d\n"+
			"edgeproxy_total_backends %d\n",
		total, limited, active, healthy, len(statuses),
	)
	fmt.Fprintf(conn,
		"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
		len(body), body,
	)
}
