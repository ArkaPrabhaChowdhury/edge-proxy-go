package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ── Constants ─────────────────────────────────────────────────────────────────

const (
	limit  = 5
	window = 10 // seconds
)

// ── Shared state ──────────────────────────────────────────────────────────────

var (
	mu      sync.Mutex
	ipMap   = make(map[string][]int64) // IP → request timestamps in current window
	current = 0
	backends []string // resolved once at startup

	statsMu       sync.Mutex
	totalRequests int64
	totalLimited  int64
	activeConns   int64
	accessLog     []LogEntry // last 100 entries
)

// ── Port helpers ──────────────────────────────────────────────────────────────

func normalizePort(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return v
	}
	if strings.HasPrefix(v, ":") {
		return v
	}
	return ":" + v
}

func getPort() string {
	// Railway (and most PaaS) injects PORT; honour it for the proxy.
	if v := strings.TrimSpace(os.Getenv("PORT")); v != "" {
		return normalizePort(v)
	}
	return ":8080"
}

func getStatsPort() string {
	if v := strings.TrimSpace(os.Getenv("STATS_PORT")); v != "" {
		return normalizePort(v)
	}
	return ":8081"
}

// resolveBackends is called once at startup.
func resolveBackends() []string {
	if env := strings.TrimSpace(os.Getenv("BACKENDS")); env != "" {
		var out []string
		for _, p := range strings.Split(env, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{
		"localhost:9000",
		"localhost:9001",
		"localhost:9002",
	}
}

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

type StatsResponse struct {
	TotalRequests int64          `json:"total_requests"`
	TotalLimited  int64          `json:"total_limited"`
	ActiveConns   int64          `json:"active_conns"`
	IPCounts      map[string]int `json:"ip_counts"`
	Log           []LogEntry     `json:"log"`
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	backends = resolveBackends()

	// Optional: run local backends in-process (single-service deployments)
	if strings.TrimSpace(os.Getenv("INPROC_BACKENDS")) == "1" {
		startLocalBackend(":9000")
		startLocalBackend(":9001")
		startLocalBackend(":9002")
	}

	proxyPort := getPort()
	statsPort := getStatsPort()

	// HTTP server: serves /stats as JSON and the dashboard at /
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/stats", statsHandler)
		mux.HandleFunc("/", dashboardHTTPHandler)
		fmt.Printf("Stats/dashboard HTTP server on %s\n", statsPort)
		if err := http.ListenAndServe(statsPort, mux); err != nil {
			fmt.Println("Stats server error:", err)
		}
	}()

	// TCP proxy
	listener, err := net.Listen("tcp", proxyPort)
	if err != nil {
		fmt.Println("Proxy listen error:", err)
		return
	}
	fmt.Printf("Proxy listening on %s\n", proxyPort)

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Accept error:", err)
			continue
		}
		go handleConnection(conn)
	}
}

// ── Stats helpers ─────────────────────────────────────────────────────────────

// buildStats computes the current stats snapshot (caller must NOT hold locks).
func buildStats() StatsResponse {
	mu.Lock()
	ipCounts := make(map[string]int)
	cutoff := time.Now().Unix() - window
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
		Log:           accessLog,
	}
}

// ── HTTP handlers (stats port) ────────────────────────────────────────────────

func statsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(buildStats())
}

func dashboardHTTPHandler(w http.ResponseWriter, r *http.Request) {
	// Resolve dashboard.html relative to the binary's location so it works
	// regardless of the current working directory.
	exe, err := os.Executable()
	if err != nil {
		http.Error(w, "cannot resolve binary path", http.StatusInternalServerError)
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
		fmt.Println("IP parse error:", err)
		return
	}

	start := time.Now()

	// ── Parse request line FIRST ──────────────────────────────────────────────
	// We must know the path before deciding whether to rate-limit, so that
	// internal routes (/stats, /favicon.ico, /) never consume a rate-limit
	// slot and never appear in the access log.
	reader := bufio.NewReader(conn)

	requestLine, err := reader.ReadString('\n')
	if err != nil {
		return // client closed before sending anything
	}

	parts := strings.Split(requestLine, " ")
	if len(parts) < 3 {
		return
	}
	method := strings.TrimSpace(parts[0])
	path := strings.TrimSpace(parts[1])

	// ── Internal routes — no rate-limit, no log ───────────────────────────────
	if method == "GET" && path == "/stats" {
		readRequestHeaders(reader)
		serveStatsTCP(conn)
		return
	}
	if method == "GET" && path == "/favicon.ico" {
		readRequestHeaders(reader)
		fmt.Fprintf(conn, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
		return
	}

	// ── Serve dashboard ───────────────────────────────────────────────────────
	if method == "GET" && (path == "/" || path == "/index.html") {
		readRequestHeaders(reader)
		serveDashboard(conn)
		statsMu.Lock()
		totalRequests++
		appendLog(LogEntry{
			Time:      time.Now().Format("15:04:05"),
			IP:        host,
			Method:    method,
			Path:      path,
			Status:    200,
			LatencyMs: time.Since(start).Milliseconds(),
			Backend:   "dashboard",
		})
		statsMu.Unlock()
		return
	}

	// ── Rate limit check (only for real backend traffic) ─────────────────────
	mu.Lock()
	now := time.Now().Unix()
	cutoff := now - window

	var active []int64
	for _, t := range ipMap[host] {
		if t > cutoff {
			active = append(active, t)
		}
	}

	if len(active) >= limit {
		mu.Unlock()

		body := "Rate limit exceeded"
		fmt.Fprintf(conn,
			"HTTP/1.1 429 Too Many Requests\r\n"+
				"Content-Type: text/plain\r\n"+
				"Access-Control-Allow-Origin: *\r\n"+
				"Content-Length: %d\r\n"+
				"\r\n%s",
			len(body), body,
		)

		statsMu.Lock()
		totalRequests++
		totalLimited++
		appendLog(LogEntry{
			Time:      time.Now().Format("15:04:05"),
			IP:        host,
			Method:    method,
			Path:      path,
			Status:    429,
			LatencyMs: time.Since(start).Milliseconds(),
			Backend:   "-",
		})
		statsMu.Unlock()
		return
	}

	active = append(active, now)
	ipMap[host] = active
	mu.Unlock()

	// ── Forward to backend (round-robin) ─────────────────────────────────────
	mu.Lock()
	if len(backends) == 0 {
		mu.Unlock()
		return
	}
	port := backends[current]
	current = (current + 1) % len(backends)
	mu.Unlock()

	backendConn, err := net.Dial("tcp", port)
	fmt.Println("Routing to", port)
	if err != nil {
		fmt.Println("Backend dial error:", err)
		body := "Bad Gateway"
		fmt.Fprintf(conn,
			"HTTP/1.1 502 Bad Gateway\r\n"+
				"Content-Type: text/plain\r\n"+
				"Access-Control-Allow-Origin: *\r\n"+
				"Content-Length: %d\r\n"+
				"\r\n%s",
			len(body), body,
		)
		return
	}
	defer backendConn.Close()

	backendConn.Write([]byte(requestLine))
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println("Header read error:", err)
			return
		}
		backendConn.Write([]byte(line))
		if line == "\r\n" {
			break
		}
	}

	io.Copy(conn, backendConn)

	// ── Record stats ──────────────────────────────────────────────────────────
	statsMu.Lock()
	totalRequests++
	appendLog(LogEntry{
		Time:      time.Now().Format("15:04:05"),
		IP:        host,
		Method:    method,
		Path:      path,
		Status:    200,
		LatencyMs: time.Since(start).Milliseconds(),
		Backend:   port,
	})
	statsMu.Unlock()
}

func appendLog(entry LogEntry) {
	accessLog = append(accessLog, entry)
	if len(accessLog) > 100 {
		accessLog = accessLog[len(accessLog)-100:]
	}
}

// ── Local in-process backends ─────────────────────────────────────────────────

func startLocalBackend(port string) {
	listener, err := net.Listen("tcp", port)
	if err != nil {
		fmt.Println("Backend listen error:", err)
		return
	}
	fmt.Printf("Backend listening on %s\n", listener.Addr())

	go func() {
		defer listener.Close()
		for {
			conn, err := listener.Accept()
			if err != nil {
				fmt.Println("Backend accept error:", err)
				continue
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
		fmt.Println("Backend read error:", err)
		return
	}

	reqArr := strings.Split(request, " ")
	if len(reqArr) < 3 {
		fmt.Println("Invalid HTTP request")
		return
	}

	path := reqArr[1]

	body := "Hello from " + port
	if path == "/hello" {
		body = "Hey! How are you?"
	} else if path == "/home" {
		body = "This is the home page"
	}

	response := fmt.Sprintf(
		"HTTP/1.1 200 OK\r\n"+
			"Content-Type: text/plain\r\n"+
			"Access-Control-Allow-Origin: *\r\n"+
			"Content-Length: %d\r\n"+
			"\r\n"+
			"%s",
		len(body),
		body,
	)

	conn.Write([]byte(response))
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
			"HTTP/1.1 500 Internal Server Error\r\n"+
				"Content-Type: text/plain\r\n"+
				"Access-Control-Allow-Origin: *\r\n"+
				"Content-Length: %d\r\n"+
				"\r\n%s",
			len(body), body,
		)
		return
	}

	fmt.Fprintf(conn,
		"HTTP/1.1 200 OK\r\n"+
			"Content-Type: text/html; charset=utf-8\r\n"+
			"Access-Control-Allow-Origin: *\r\n"+
			"Content-Length: %d\r\n"+
			"\r\n",
		len(content),
	)
	conn.Write(content)
}

// serveStatsTCP writes the stats JSON payload directly over a raw TCP conn.
func serveStatsTCP(conn net.Conn) {
	resp := buildStats()
	payload, _ := json.Marshal(resp)
	fmt.Fprintf(conn,
		"HTTP/1.1 200 OK\r\n"+
			"Content-Type: application/json\r\n"+
			"Access-Control-Allow-Origin: *\r\n"+
			"Content-Length: %d\r\n"+
			"\r\n",
		len(payload),
	)
	conn.Write(payload)
}
