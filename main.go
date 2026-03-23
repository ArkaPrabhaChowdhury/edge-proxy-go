package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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

var mu sync.Mutex
var ipMap = make(map[string][]int64) // IP → request timestamps in current window
var current= 0

var statsMu       sync.Mutex
var totalRequests int64
var totalLimited  int64
var activeConns   int64
var accessLog     []LogEntry // last 100 entries

// ── Types ─────────────────────────────────────────────────────────────────────

type LogEntry struct {
	Time      string `json:"time"`
	IP        string `json:"ip"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
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
	// HTTP server: serves /stats as JSON and dashboard.html at /
	go func() {
		http.HandleFunc("/stats", statsHandler)
		http.HandleFunc("/", fileHandler)
		fmt.Println("Stats server on :8081  →  visit http://localhost:8081")
		if err := http.ListenAndServe(":8081", nil); err != nil {
			fmt.Println("Stats server error:", err)
		}
	}()

	// TCP proxy
	listener, err := net.Listen("tcp", ":8080")
	if err != nil {
		fmt.Println("Proxy listen error:", err)
		return
	}
	fmt.Println("Proxy listening on :8080")

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Accept error:", err)
			continue
		}
		go handleConnection(conn)
	}
}

// ── /stats handler ────────────────────────────────────────────────────────────
//
// Dashboard calls GET http://localhost:8081/stats every second.
// Returns a JSON snapshot of all current state.

func statsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*") // allow file:// origin
	w.Header().Set("Content-Type", "application/json")

	// Compute per-IP counts within the current sliding window
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
	resp := StatsResponse{
		TotalRequests: totalRequests,
		TotalLimited:  totalLimited,
		ActiveConns:   activeConns,
		IPCounts:      ipCounts,
		Log:           accessLog,
	}
	statsMu.Unlock()

	json.NewEncoder(w).Encode(resp)
}

// Serve dashboard.html when visiting http://localhost:8081
func fileHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "dashboard.html")
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

	// ── Rate limit check ──────────────────────────────────────────────────────
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
			Method:    "-",
			Path:      "-",
			Status:    429,
			LatencyMs: time.Since(start).Milliseconds(),
		})
		statsMu.Unlock()
		return
	}

	active = append(active, now)
	ipMap[host] = active
	mu.Unlock()

	// ── Forward to backend ────────────────────────────────────────────────────
	reader := bufio.NewReader(conn)

	requestLine, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("Read error:", err)
		return
	}

	parts := strings.Split(requestLine, " ")
	if len(parts) < 3 {
		fmt.Println("Invalid HTTP request line")
		return
	}
	method := strings.TrimSpace(parts[0])
	path := strings.TrimSpace(parts[1])

	mu.Lock()
	backends := []string{
		"backend1:9000",
		"backend2:9001",
		"backend3:9002",
	}


	port:= backends[current]
	current++
	current = current % len(backends)
	mu.Unlock()

	backendConn, err := net.Dial("tcp", port)
	fmt.Println("Routing to", port)
	if err != nil {
		fmt.Println("Backend dial error:", err)
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
	})
	statsMu.Unlock()
}

func appendLog(entry LogEntry) {
	accessLog = append(accessLog, entry)
	if len(accessLog) > 100 {
		accessLog = accessLog[len(accessLog)-100:]
	}
}