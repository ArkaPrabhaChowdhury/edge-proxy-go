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

var (
	cfg  *Config
	pool *backendPool
	rl   *rateLimiter

	statsMu       sync.Mutex
	totalRequests int64
	totalLimited  int64
	activeConns   int64
	accessLog     []LogEntry
)

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
	Requests              int     `json:"requests"`
	WindowSeconds         int     `json:"window_seconds"`
	IdentifierHeader      string  `json:"identifier_header"`
	BaseRequestsPerSecond float64 `json:"base_requests_per_second"`
	BurstCapacity         float64 `json:"burst_capacity"`
	BlockSeconds          int     `json:"block_seconds"`
	RedisAddr             string  `json:"redis_addr"`
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

func main() {
	var err error
	cfg, err = loadConfig("config.yaml")
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}

	pool = newBackendPool(cfg.Backends)
	rl = newRateLimiter(cfg.RateLimit)
	if err := rl.ping(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "redis error:", err)
		os.Exit(1)
	}

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
	fmt.Printf("Proxy     -> %s://localhost%s\n", scheme, cfg.Proxy.Port)
	fmt.Printf("Dashboard -> http://localhost%s\n", cfg.Stats.Port)
	fmt.Printf("Metrics   -> http://localhost%s/metrics\n", cfg.Stats.Port)

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
	fmt.Println("\nshutting down gracefully...")

	_ = listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = statsServer.Shutdown(ctx)

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

func startStatsServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", statsHandler)
	mux.HandleFunc("/metrics", metricsHandler)
	mux.HandleFunc("/top-abusers", topAbusersHandler)
	mux.HandleFunc("/traffic-spikes", trafficSpikesHandler)
	mux.HandleFunc("/rate-limit-events", rateLimitEventsHandler)
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

func buildStats() StatsResponse {
	ipCounts := map[string]int{}
	if abusers, err := rl.TopAbusers(context.Background(), int64(cfg.RateLimit.TopN)); err == nil {
		for _, abuser := range abusers {
			label := abuser.Identifier
			if label == "" {
				label = abuser.User
			}
			ipCounts[label] = abuser.AbuseCount
		}
	}

	statsMu.Lock()
	defer statsMu.Unlock()
	return StatsResponse{
		TotalRequests: totalRequests,
		TotalLimited:  totalLimited,
		ActiveConns:   activeConns,
		IPCounts:      ipCounts,
		Backends:      pool.status(),
		RateLimit: RateLimitInfo{
			Requests:              cfg.RateLimit.Requests,
			WindowSeconds:         cfg.RateLimit.SlidingWindowSeconds,
			IdentifierHeader:      cfg.RateLimit.IdentifierHeader,
			BaseRequestsPerSecond: cfg.RateLimit.BaseRequestsPerSecond,
			BurstCapacity:         cfg.RateLimit.BurstCapacity,
			BlockSeconds:          cfg.RateLimit.BlockSeconds,
			RedisAddr:             cfg.RateLimit.Redis.Addr,
		},
		Log: accessLog,
	}
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(buildStats())
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
	fmt.Fprintf(w, "# HELP edgeproxy_rate_limited_total Requests blocked by the limiter\n")
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

func topAbusersHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	payload, err := rl.TopAbusers(r.Context(), int64(cfg.RateLimit.TopN))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func trafficSpikesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	payload, err := rl.TrafficSpikes(r.Context(), int64(cfg.RateLimit.TopN))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func rateLimitEventsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	payload, err := rl.RateLimitEvents(r.Context(), int64(cfg.RateLimit.TopN))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
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
	headers, rawHeaders := readRequestHeaders(reader)

	switch {
	case method == "GET" && path == "/stats":
		serveStatsTCP(conn)
		return
	case method == "GET" && path == "/metrics":
		serveMetricsTCP(conn)
		return
	case method == "GET" && path == "/top-abusers":
		serveJSONTCP(conn, topAbusersPayload())
		return
	case method == "GET" && path == "/traffic-spikes":
		serveJSONTCP(conn, trafficSpikesPayload())
		return
	case method == "GET" && path == "/rate-limit-events":
		serveJSONTCP(conn, rateLimitEventsPayload())
		return
	case method == "GET" && path == "/favicon.ico":
		fmt.Fprintf(conn, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
		return
	case method == "GET" && (path == "/" || path == "/index.html"):
		serveDashboard(conn)
		recordRequest(LogEntry{
			Time:      time.Now().Format("15:04:05"),
			IP:        host,
			Method:    method,
			Path:      path,
			Status:    200,
			LatencyMs: time.Since(start).Milliseconds(),
			Backend:   "dashboard",
		}, false)
		return
	}

	user, identifier := identifyRequester(host, headers)
	decision, err := rl.Evaluate(context.Background(), user, identifier)
	if err != nil {
		body := "Rate limiter unavailable"
		fmt.Fprintf(conn,
			"HTTP/1.1 503 Service Unavailable\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
			len(body), body,
		)
		return
	}
	if decision.Action == ActionThrottle && decision.ThrottleDelay > 0 {
		time.Sleep(time.Duration(decision.ThrottleDelay) * time.Millisecond)
	}
	if decision.Action == ActionBlock {
		body := "Rate limit exceeded"
		fmt.Fprintf(conn,
			"HTTP/1.1 429 Too Many Requests\r\nContent-Type: text/plain\r\nRetry-After: %d\r\nX-RateLimit-Reason: %s\r\nContent-Length: %d\r\n\r\n%s",
			decision.RetryAfter, decision.Reason, len(body), body,
		)
		recordRequest(LogEntry{
			Time:      time.Now().Format("15:04:05"),
			IP:        user,
			Method:    method,
			Path:      path,
			Status:    429,
			LatencyMs: time.Since(start).Milliseconds(),
			Backend:   "-",
		}, true)
		return
	}

	backend := pool.next()
	if backend == "" {
		body := "No healthy backends available"
		fmt.Fprintf(conn,
			"HTTP/1.1 503 Service Unavailable\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
			len(body), body,
		)
		return
	}

	backendConn, err := net.Dial("tcp", backend)
	if err != nil {
		body := "Bad Gateway"
		fmt.Fprintf(conn,
			"HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
			len(body), body,
		)
		return
	}
	defer backendConn.Close()

	_, _ = backendConn.Write([]byte(requestLine))
	for _, line := range rawHeaders {
		_, _ = backendConn.Write([]byte(line))
	}

	_, _ = io.Copy(conn, backendConn)

	recordRequest(LogEntry{
		Time:      time.Now().Format("15:04:05"),
		IP:        user,
		Method:    method,
		Path:      path,
		Status:    200,
		LatencyMs: time.Since(start).Milliseconds(),
		Backend:   backend,
	}, false)
}

func recordRequest(entry LogEntry, limited bool) {
	statsMu.Lock()
	defer statsMu.Unlock()
	totalRequests++
	if limited {
		totalLimited++
	}
	appendLog(entry)
}

func appendLog(entry LogEntry) {
	accessLog = append(accessLog, entry)
	if len(accessLog) > 100 {
		accessLog = accessLog[len(accessLog)-100:]
	}
}

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

func readRequestHeaders(reader *bufio.Reader) (map[string]string, []string) {
	headers := make(map[string]string)
	var lines []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return headers, lines
		}
		lines = append(lines, line)
		if line == "\r\n" {
			return headers, lines
		}
		parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
		if len(parts) == 2 {
			headers[strings.ToLower(strings.TrimSpace(parts[0]))] = strings.TrimSpace(parts[1])
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
	_, _ = conn.Write(content)
}

func serveStatsTCP(conn net.Conn) {
	payload, _ := json.Marshal(buildStats())
	serveJSONTCP(conn, payload)
}

func serveJSONTCP(conn net.Conn, payload []byte) {
	fmt.Fprintf(conn,
		"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n",
		len(payload),
	)
	_, _ = conn.Write(payload)
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

func identifyRequester(host string, headers map[string]string) (string, string) {
	key := strings.ToLower(cfg.RateLimit.IdentifierHeader)
	if value := strings.TrimSpace(headers[key]); value != "" {
		return "api_key:" + value, value
	}
	return "ip:" + host, host
}

func topAbusersPayload() []byte {
	data, err := rl.TopAbusers(context.Background(), int64(cfg.RateLimit.TopN))
	if err != nil {
		return []byte(`[]`)
	}
	payload, _ := json.Marshal(data)
	return payload
}

func trafficSpikesPayload() []byte {
	data, err := rl.TrafficSpikes(context.Background(), int64(cfg.RateLimit.TopN))
	if err != nil {
		return []byte(`[]`)
	}
	payload, _ := json.Marshal(data)
	return payload
}

func rateLimitEventsPayload() []byte {
	data, err := rl.RateLimitEvents(context.Background(), int64(cfg.RateLimit.TopN))
	if err != nil {
		return []byte(`[]`)
	}
	payload, _ := json.Marshal(data)
	return payload
}
