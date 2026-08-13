package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	runtimeMu         sync.RWMutex
	cfg               *Config
	runtimeConfigPath string
	pool              *backendPool
	rl                *rateLimiter

	version = "dev"
	commit  = "none"
	date    = "unknown"

	requestSeq    uint64
	statsMu       sync.Mutex
	totalRequests int64
	totalLimited  int64
	activeConns   int64
	accessLog     []LogEntry
	statusCounts  = map[int]int64{}
)

type LogEntry struct {
	RequestID  string `json:"request_id"`
	PolicyName string `json:"policy_name"`
	Time       string `json:"time"`
	IP         string `json:"ip"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	LatencyMs  int64  `json:"latency_ms"`
	Backend    string `json:"backend"`
	Limited    bool   `json:"limited"`
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
	os.Exit(runCLI(os.Args[1:]))
}

func runCLI(args []string) int {
	command := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}

	switch command {
	case "run":
		configPath, err := parseConfigFlag("run", args)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if err := runProxy(configPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "validate":
		configPath, err := parseConfigFlag("validate", args)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return validateConfigCommand(configPath)
	case "version":
		fmt.Printf("edge-proxy version=%s commit=%s date=%s\n", version, commit, date)
		return 0
	case "help", "-h", "--help":
		printUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", command)
		printUsage()
		return 2
	}
}

func parseConfigFlag(name string, args []string) (string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", defaultConfigPath(), "Path to config file")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() != 0 {
		return "", fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return *configPath, nil
}

func defaultConfigPath() string {
	if value := strings.TrimSpace(os.Getenv("EDGE_PROXY_CONFIG")); value != "" {
		return value
	}
	return "config.yaml"
}

func validateConfigCommand(configPath string) int {
	loadedCfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config invalid: %v\n", err)
		return 1
	}
	fmt.Printf(
		"config valid: mode=%s proxy=%s stats=%s backends=%d routes=%d tls=%t dashboard_auth=%t redis=%s\n",
		loadedCfg.Mode,
		loadedCfg.Proxy.Port,
		loadedCfg.Stats.Port,
		len(loadedCfg.Backends),
		len(loadedCfg.Routes),
		loadedCfg.Proxy.TLS.Enabled,
		loadedCfg.Stats.Auth.Enabled,
		loadedCfg.RateLimit.Redis.Addr,
	)
	return 0
}

func printUsage() {
	fmt.Println(`edge-proxy

Usage:
  edge-proxy run [-config path]
  edge-proxy validate [-config path]
  edge-proxy version

Environment:
  EDGE_PROXY_CONFIG   Default config path (defaults to config.yaml)`)
}

func runProxy(configPath string) error {
	loadedCfg, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("config error: %w", err)
	}
	tracingShutdown, err := setupTracing(context.Background(), loadedCfg.Tracing)
	if err != nil {
		return fmt.Errorf("tracing error: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracingShutdown(ctx)
	}()

	initialLimiter := newRateLimiter(loadedCfg.RateLimit)
	if loadedCfg.RateLimit.Enabled {
		if err := initialLimiter.ping(context.Background()); err != nil {
			return fmt.Errorf("redis error: %w", err)
		}
	}
	runtimeMu.Lock()
	cfg = loadedCfg
	runtimeConfigPath = configPath
	pool = newBackendPoolFromConfig(loadedCfg)
	rl = initialLimiter
	runtimeMu.Unlock()

	if strings.TrimSpace(os.Getenv("INPROC_BACKENDS")) == "1" {
		startLocalBackend(":9000")
		startLocalBackend(":9001")
		startLocalBackend(":9002")
	}

	startHealthChecker(pool, currentHealthCheckConfig)

	statsServer := startStatsServer()

	listener, err := startListener()
	if err != nil {
		_ = statsServer.Shutdown(context.Background())
		_ = pool.close()
		return fmt.Errorf("proxy listen error: %w", err)
	}

	scheme := "http"
	if currentConfig().Proxy.TLS.Enabled {
		scheme = "https"
	}
	currentCfg := currentConfig()
	fmt.Printf("Proxy     -> %s://localhost%s\n", scheme, currentCfg.Proxy.Port)
	fmt.Printf("Dashboard -> http://localhost%s\n", currentCfg.Stats.Port)
	fmt.Printf("Metrics   -> http://localhost%s/metrics\n", currentCfg.Stats.Port)

	var httpServer *http.Server
	var tcpDone chan struct{}
	if loadedCfg.Mode == "http" {
		httpServer = newProxyServer()
		go func() {
			if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Println("proxy server error:", err)
			}
		}()
	} else {
		tcpDone = make(chan struct{})
		go serveTCPProxy(listener, loadedCfg.TCPBackend, tcpDone)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	fmt.Println("\nshutting down gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if httpServer != nil {
		httpServer.SetKeepAlivesEnabled(false)
		_ = httpServer.Shutdown(ctx)
	} else {
		_ = listener.Close()
		if tcpDone != nil {
			select {
			case <-tcpDone:
			case <-ctx.Done():
			}
		}
	}
	_ = statsServer.Shutdown(ctx)
	_ = pool.close()

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
	return nil
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

func newProxyServer() *http.Server {
	c := currentConfig()
	return &http.Server{
		Addr:              c.Proxy.Port,
		Handler:           http.HandlerFunc(proxyHandler),
		ReadTimeout:       durationMs(c.Proxy.ReadTimeoutMs),
		ReadHeaderTimeout: durationMs(c.Proxy.ReadHeaderTimeoutMs),
		WriteTimeout:      durationMs(c.Proxy.WriteTimeoutMs),
		IdleTimeout:       durationMs(c.Proxy.IdleTimeoutMs),
		MaxHeaderBytes:    c.Proxy.MaxHeaderBytes,
		ConnState:         trackConnectionState,
	}
}

func trackConnectionState(_ net.Conn, state http.ConnState) {
	statsMu.Lock()
	defer statsMu.Unlock()
	switch state {
	case http.StateNew:
		activeConns++
	case http.StateClosed, http.StateHijacked:
		if activeConns > 0 {
			activeConns--
		}
	}
}

func durationMs(value int) time.Duration {
	if value <= 0 {
		return 0
	}
	return time.Duration(value) * time.Millisecond
}

func serveTCPProxy(listener net.Listener, backend string, done chan<- struct{}) {
	defer close(done)
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		go handleTCPProxyConnection(client, backend)
	}
}

func handleTCPProxyConnection(client net.Conn, backend string) {
	defer client.Close()
	statsMu.Lock()
	activeConns++
	statsMu.Unlock()
	defer func() {
		statsMu.Lock()
		if activeConns > 0 {
			activeConns--
		}
		statsMu.Unlock()
	}()
	c := currentConfig()
	upstream, err := net.DialTimeout("tcp", backend, durationMs(c.Proxy.ConnectTimeoutMs))
	if err != nil {
		return
	}
	defer upstream.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, client)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream)
		if tcp, ok := client.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	wg.Wait()
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
	if requestID == "" {
		requestID = nextRequestID()
	}
	w.Header().Set("X-Request-Id", requestID)
	c := currentConfig()
	clientIP := clientIPFromRequest(r)
	key := c.RateLimit.Key
	if key == "" {
		key = c.RateLimit.IdentifierHeader
	}
	user, identifier := identifyRequester(clientIP, r.Header, key)
	entry := LogEntry{RequestID: requestID, Time: started.Format(time.RFC3339), IP: clientIP, Method: r.Method, Path: r.URL.RequestURI(), Backend: "-"}
	defer func() { entry.LatencyMs = time.Since(started).Milliseconds(); recordRequest(entry, entry.Limited) }()

	if c.Proxy.MaxBodyBytes > 0 {
		if r.ContentLength > c.Proxy.MaxBodyBytes {
			entry.Status = http.StatusRequestEntityTooLarge
			http.Error(w, "request body too large", entry.Status)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, c.Proxy.MaxBodyBytes+1))
		if err != nil {
			entry.Status = http.StatusBadRequest
			http.Error(w, "could not read request body", entry.Status)
			return
		}
		if int64(len(body)) > c.Proxy.MaxBodyBytes {
			entry.Status = http.StatusRequestEntityTooLarge
			http.Error(w, "request body too large", entry.Status)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}

	if c.RateLimit.Enabled {
		effective := resolveRateLimitConfig(r, identifier, c)
		decision, err := currentRateLimiter().Evaluate(r.Context(), user, identifier, effective)
		if err != nil {
			entry.Status = http.StatusServiceUnavailable
			http.Error(w, "rate limiter unavailable", entry.Status)
			return
		}
		entry.PolicyName = decision.PolicyName
		if decision.Action == ActionThrottle && decision.ThrottleDelay > 0 {
			timer := time.NewTimer(time.Duration(decision.ThrottleDelay) * time.Millisecond)
			select {
			case <-timer.C:
			case <-r.Context().Done():
				timer.Stop()
				entry.Status = http.StatusRequestTimeout
				http.Error(w, "request timed out", entry.Status)
				return
			}
		}
		if decision.Action == ActionBlock {
			entry.Status, entry.Limited = http.StatusTooManyRequests, true
			w.Header().Set("Retry-After", strconv.Itoa(decision.RetryAfter))
			w.Header().Set("X-RateLimit-Reason", decision.Reason)
			http.Error(w, "rate limit exceeded", entry.Status)
			return
		}
	}

	route := matchRoute(r.URL.Path, c.Routes)
	allowed := routeBackendSet(route)
	retries := 0
	timeout := durationMs(c.Proxy.ResponseHeaderMs)
	if route != nil {
		retries = route.Retries
		if route.TimeoutMs > 0 {
			timeout = durationMs(route.TimeoutMs)
		}
	}
	if retries > 0 && r.Method != http.MethodGet && r.Method != http.MethodHead {
		retries = 0
	}
	ctx := r.Context()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	r = r.WithContext(ctx)
	var response *http.Response
	var backend string
	var err error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 && r.GetBody != nil {
			body, bodyErr := r.GetBody()
			if bodyErr != nil {
				err = bodyErr
				break
			}
			r.Body = body
		}
		backend = pool.nextFor(allowed)
		if backend == "" {
			break
		}
		response, err = roundTripToBackend(r, backend, requestID)
		if err == nil && (response.StatusCode < 502 || response.StatusCode > 504 || attempt == retries) {
			break
		}
		if response != nil {
			response.Body.Close()
		}
	}
	if err != nil || response == nil {
		entry.Status = http.StatusBadGateway
		if backend == "" {
			entry.Status = http.StatusServiceUnavailable
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			entry.Status = http.StatusGatewayTimeout
		}
		http.Error(w, http.StatusText(entry.Status), entry.Status)
		return
	}
	entry.Backend, entry.Status = backend, response.StatusCode
	copyResponse(w, response, c.Proxy.MaxResponseBodyBytes)
}

func clientIPFromRequest(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func matchRoute(path string, routes []RouteConfig) *RouteConfig {
	var matched *RouteConfig
	for i := range routes {
		if strings.HasPrefix(path, routes[i].Path) && (matched == nil || len(routes[i].Path) > len(matched.Path)) {
			matched = &routes[i]
		}
	}
	return matched
}

func routeBackendSet(route *RouteConfig) map[string]bool {
	if route == nil {
		return nil
	}
	set := make(map[string]bool, len(route.Backends))
	for _, backend := range route.Backends {
		set[backend.Address] = true
	}
	return set
}

func roundTripToBackend(req *http.Request, backend, requestID string) (*http.Response, error) {
	start := time.Now()
	finish := pool.startRequest(backend)
	outReq := req.Clone(req.Context())
	outReq.RequestURI = ""
	outReq.URL.Scheme = "http"
	outReq.URL.Host = backend
	if outReq.Host == "" {
		outReq.Host = backend
	}
	outReq.Header = req.Header.Clone()
	removeHopHeaders(outReq.Header)
	outReq.Header.Set("X-Request-Id", requestID)
	proto := "http"
	if currentConfig().Proxy.TLS.Enabled {
		proto = "https"
	}
	outReq.Header.Set("X-Forwarded-Proto", proto)
	if ip := clientIPFromRequest(req); ip != "" {
		appendForwardedFor(outReq.Header, ip)
	}
	injectTraceHeaders(req.Context(), outReq.Header)
	resp, err := pool.transportFor(backend).RoundTrip(outReq)
	if err != nil {
		finish(0, time.Since(start), err)
		return nil, err
	}
	resp.Body = &trackedResponseBody{ReadCloser: resp.Body, finish: func() { finish(resp.StatusCode, time.Since(start), nil) }}
	return resp, nil
}

type trackedResponseBody struct {
	io.ReadCloser
	finishOnce sync.Once
	finish     func()
}

func (b *trackedResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.finishOnce.Do(b.finish)
	}
	return n, err
}

func (b *trackedResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.finishOnce.Do(b.finish)
	return err
}

func removeHopHeaders(header http.Header) {
	for _, key := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(key)
	}
}

func copyResponse(w http.ResponseWriter, resp *http.Response, maxBody int64) {
	defer resp.Body.Close()
	if maxBody > 0 && resp.ContentLength > maxBody {
		http.Error(w, "upstream response body too large", http.StatusBadGateway)
		return
	}
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if maxBody > 0 {
		_, _ = io.Copy(w, io.LimitReader(resp.Body, maxBody))
	} else {
		_, _ = io.Copy(w, resp.Body)
	}
}

func startStatsServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", statsHandler)
	mux.HandleFunc("/metrics", metricsHandler)
	mux.HandleFunc("/top-abusers", topAbusersHandler)
	mux.HandleFunc("/traffic-spikes", trafficSpikesHandler)
	mux.HandleFunc("/rate-limit-events", rateLimitEventsHandler)
	mux.HandleFunc("/-/backends", backendAdminHandler)
	mux.HandleFunc("/-/backends/", backendAdminHandler)
	mux.HandleFunc("/-/reload", reloadHandler)
	mux.HandleFunc("/", dashboardHandler)

	var handler http.Handler = mux
	currentCfg := currentConfig()
	if currentCfg.Stats.Auth.Enabled {
		handler = basicAuth(mux, currentCfg.Stats.Auth.Username, currentCfg.Stats.Auth.Password)
	}

	srv := &http.Server{Addr: currentCfg.Stats.Port, Handler: handler}
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
	currentCfg := currentConfig()
	currentLimiter := currentRateLimiter()
	ipCounts := map[string]int{}
	if abusers, err := currentLimiter.TopAbusers(context.Background(), int64(currentCfg.RateLimit.TopN)); err == nil {
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
			Requests:              currentCfg.RateLimit.Requests,
			WindowSeconds:         currentCfg.RateLimit.SlidingWindowSeconds,
			IdentifierHeader:      currentCfg.RateLimit.IdentifierHeader,
			BaseRequestsPerSecond: currentCfg.RateLimit.BaseRequestsPerSecond,
			BurstCapacity:         currentCfg.RateLimit.BurstCapacity,
			BlockSeconds:          currentCfg.RateLimit.BlockSeconds,
			RedisAddr:             currentCfg.RateLimit.Redis.Addr,
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
	available := 0
	for _, s := range statuses {
		if s.Healthy {
			healthy++
		}
		if s.Available {
			available++
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP edgeproxy_requests_total Total requests proxied\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_requests_total counter\n")
	fmt.Fprintf(w, "edgeproxy_requests_total %d\n\n", total)
	fmt.Fprintf(w, "# HELP edgeproxy_rate_limited_total Requests blocked by the limiter\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_rate_limited_total counter\n")
	fmt.Fprintf(w, "edgeproxy_rate_limited_total %d\n\n", limited)
	statsMu.Lock()
	for status, count := range statusCounts {
		fmt.Fprintf(w, "edgeproxy_responses_total{status=\"%d\"} %d\n", status, count)
	}
	statsMu.Unlock()
	fmt.Fprintf(w, "# HELP edgeproxy_active_connections Current open proxy connections\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_active_connections gauge\n")
	fmt.Fprintf(w, "edgeproxy_active_connections %d\n\n", active)
	fmt.Fprintf(w, "# HELP edgeproxy_healthy_backends Backends passing health checks\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_healthy_backends gauge\n")
	fmt.Fprintf(w, "edgeproxy_healthy_backends %d\n\n", healthy)
	fmt.Fprintf(w, "# HELP edgeproxy_total_backends Total backends configured\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_total_backends gauge\n")
	fmt.Fprintf(w, "edgeproxy_total_backends %d\n", len(statuses))
	fmt.Fprintf(w, "\n# HELP edgeproxy_available_backends Backends currently eligible for traffic\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_available_backends gauge\n")
	fmt.Fprintf(w, "edgeproxy_available_backends %d\n", available)
	fmt.Fprintf(w, "\n# HELP edgeproxy_backend_requests_total Requests sent to each backend\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_backend_requests_total counter\n")
	for _, s := range statuses {
		fmt.Fprintf(w, "edgeproxy_backend_requests_total{backend=%q} %d\n", s.Address, s.Requests)
	}
	fmt.Fprintf(w, "\n# HELP edgeproxy_backend_errors_total Errors observed for each backend\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_backend_errors_total counter\n")
	for _, s := range statuses {
		fmt.Fprintf(w, "edgeproxy_backend_errors_total{backend=%q} %d\n", s.Address, s.Errors)
	}
	fmt.Fprintf(w, "\n# HELP edgeproxy_backend_active_requests Active in-flight requests per backend\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_backend_active_requests gauge\n")
	for _, s := range statuses {
		fmt.Fprintf(w, "edgeproxy_backend_active_requests{backend=%q} %d\n", s.Address, s.ActiveRequests)
	}
	fmt.Fprintf(w, "\n# HELP edgeproxy_backend_average_latency_ms Average backend latency in milliseconds\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_backend_average_latency_ms gauge\n")
	for _, s := range statuses {
		fmt.Fprintf(w, "edgeproxy_backend_average_latency_ms{backend=%q} %.2f\n", s.Address, s.AvgLatencyMs)
	}
	fmt.Fprintf(w, "\n# HELP edgeproxy_backend_breaker_state Circuit breaker state per backend (0=closed,1=open,2=half-open)\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_backend_breaker_state gauge\n")
	for _, s := range statuses {
		fmt.Fprintf(w, "edgeproxy_backend_breaker_state{backend=%q} %d\n", s.Address, breakerStateValue(s.BreakerState))
	}
	fmt.Fprintf(w, "\n# HELP edgeproxy_backend_consecutive_errors Consecutive backend errors seen by the proxy\n")
	fmt.Fprintf(w, "# TYPE edgeproxy_backend_consecutive_errors gauge\n")
	for _, s := range statuses {
		fmt.Fprintf(w, "edgeproxy_backend_consecutive_errors{backend=%q} %d\n", s.Address, s.ConsecutiveErr)
	}
}

func topAbusersHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	currentCfg := currentConfig()
	payload, err := currentRateLimiter().TopAbusers(r.Context(), int64(currentCfg.RateLimit.TopN))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func trafficSpikesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	currentCfg := currentConfig()
	payload, err := currentRateLimiter().TrafficSpikes(r.Context(), int64(currentCfg.RateLimit.TopN))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func rateLimitEventsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	currentCfg := currentConfig()
	payload, err := currentRateLimiter().RateLimitEvents(r.Context(), int64(currentCfg.RateLimit.TopN))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func backendAdminHandler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/-/backends":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pool.status())
		return
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/-/backends/"):
		address := strings.TrimPrefix(r.URL.Path, "/-/backends/")
		address, err := url.PathUnescape(address)
		if err != nil || strings.TrimSpace(address) == "" {
			http.Error(w, "invalid backend address", http.StatusBadRequest)
			return
		}
		var input struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		if err := pool.setBackendState(address, strings.ToLower(strings.TrimSpace(input.Action))); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"backend": address,
			"action":  input.Action,
		})
		return
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func reloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := reloadRuntimeConfig(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"message": "runtime config reloaded",
	})
}

func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	path, err := resolveDashboardPath()
	if err != nil {
		http.Error(w, "dashboard not found", http.StatusInternalServerError)
		return
	}
	http.ServeFile(w, r, path)
}

func handleConnection(conn net.Conn) {
	// Compatibility entry point for callers from the original demo. The
	// production listener uses http.Server directly; this keeps the same
	// persistent-connection semantics when a single connection is supplied.
	server := &http.Server{Handler: http.HandlerFunc(proxyHandler), MaxHeaderBytes: currentConfig().Proxy.MaxHeaderBytes}
	_ = server.Serve(&singleConnListener{conn: conn})
	return

	/* Legacy one-request implementation retained below only as historical
	   context; it is unreachable and is not part of the serving path. */
	/*
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
		requestID := nextRequestID()
		reader := bufio.NewReader(conn)
		req, err := http.ReadRequest(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				writePlainResponse(conn, http.StatusBadRequest, "Bad Request", map[string]string{"X-Request-Id": requestID})
			}
			return
		}
		defer req.Body.Close()
		req.RemoteAddr = conn.RemoteAddr().String()
		traceCtx, span := startIngressSpan(req, requestID)
		defer span.End()
		req = req.WithContext(traceCtx)

		method := req.Method
		path := req.URL.Path

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
				RequestID: requestID,
				Time:      time.Now().Format("15:04:05"),
				IP:        host,
				Method:    method,
				Path:      path,
				Status:    200,
				LatencyMs: time.Since(start).Milliseconds(),
				Backend:   "dashboard",
				Limited:   false,
			}, false)
			return
		}

		currentCfg := currentConfig()
		user, identifier := identifyRequester(host, req.Header, currentCfg.RateLimit.IdentifierHeader)
		effectiveLimit := resolveRateLimitConfig(req, identifier, currentCfg)
		decision, err := currentRateLimiter().Evaluate(context.Background(), user, identifier, effectiveLimit)
		if err != nil {
			writePlainResponse(conn, http.StatusServiceUnavailable, "Rate limiter unavailable", map[string]string{"X-Request-Id": requestID})
			return
		}
		if decision.Action == ActionThrottle && decision.ThrottleDelay > 0 {
			time.Sleep(time.Duration(decision.ThrottleDelay) * time.Millisecond)
		}
		if decision.Action == ActionBlock {
			writePlainResponse(conn, http.StatusTooManyRequests, "Rate limit exceeded", map[string]string{
				"Retry-After":        strconv.Itoa(decision.RetryAfter),
				"X-RateLimit-Reason": decision.Reason,
				"X-Request-Id":       requestID,
				"X-RateLimit-Policy": decision.PolicyName,
			})
			recordRequest(LogEntry{
				RequestID:  requestID,
				PolicyName: decision.PolicyName,
				Time:       time.Now().Format("15:04:05"),
				IP:         user,
				Method:     method,
				Path:       path,
				Status:     429,
				LatencyMs:  time.Since(start).Milliseconds(),
				Backend:    "-",
				Limited:    true,
			}, true)
			return
		}

		backend := pool.next()
		if backend == "" {
			writePlainResponse(conn, http.StatusServiceUnavailable, "No healthy backends available", map[string]string{"X-Request-Id": requestID})
			return
		}

		statusCode, err := proxyHTTPRequest(conn, req, backend, requestID)
		if err != nil {
			writePlainResponse(conn, http.StatusBadGateway, "Bad Gateway", map[string]string{"X-Request-Id": requestID})
			return
		}

		recordRequest(LogEntry{
			RequestID:  requestID,
			PolicyName: decision.PolicyName,
			Time:       time.Now().Format("15:04:05"),
			IP:         user,
			Method:     method,
			Path:       path,
			Status:     statusCode,
			LatencyMs:  time.Since(start).Milliseconds(),
			Backend:    backend,
			Limited:    false,
		}, false)
	*/
}

type singleConnListener struct {
	conn net.Conn
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.conn == nil {
		return nil, net.ErrClosed
	}
	conn := l.conn
	l.conn = nil
	return conn, nil
}

func (l *singleConnListener) Close() error {
	if l.conn != nil {
		return l.conn.Close()
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return dummyAddr("single-connection") }

type dummyAddr string

func (a dummyAddr) Network() string { return string(a) }
func (a dummyAddr) String() string  { return string(a) }

func proxyHTTPRequest(clientConn net.Conn, req *http.Request, backend, requestID string) (int, error) {
	finish := pool.startRequest(backend)
	start := time.Now()
	upstreamCtx, upstreamSpan := startUpstreamSpan(req.Context(), backend)
	defer upstreamSpan.End()
	outReq := req.Clone(req.Context())
	outReq = outReq.WithContext(upstreamCtx)
	outReq.RequestURI = ""
	outReq.URL.Scheme = "http"
	outReq.URL.Host = backend
	outReq.Host = req.Host
	if strings.TrimSpace(outReq.Host) == "" {
		outReq.Host = backend
	}
	outReq.Header = req.Header.Clone()
	removeHopHeaders(outReq.Header)
	outReq.Header.Set("X-Request-Id", requestID)
	injectTraceHeaders(req.Context(), outReq.Header)
	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil && strings.TrimSpace(clientIP) != "" {
		appendForwardedFor(outReq.Header, clientIP)
	}

	transport := pool.transportFor(backend)
	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		finish(0, time.Since(start), err)
		return 0, err
	}
	defer resp.Body.Close()

	resp.Header.Set("X-Request-Id", requestID)
	if err := resp.Write(clientConn); err != nil {
		finish(resp.StatusCode, time.Since(start), err)
		return 0, err
	}

	finish(resp.StatusCode, time.Since(start), nil)
	return resp.StatusCode, nil
}

func recordRequest(entry LogEntry, limited bool) {
	statsMu.Lock()
	defer statsMu.Unlock()
	totalRequests++
	if limited {
		totalLimited++
	}
	if entry.Status > 0 {
		statusCounts[entry.Status]++
	}
	appendLog(entry)
	if payload, err := json.Marshal(entry); err == nil {
		fmt.Println(string(payload))
	}
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

func serveDashboard(conn net.Conn) {
	path, err := resolveDashboardPath()
	if err != nil {
		body := "Dashboard not found"
		fmt.Fprintf(conn,
			"HTTP/1.1 500 Internal Server Error\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
			len(body), body,
		)
		return
	}
	content, err := os.ReadFile(path)
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

func resolveDashboardPath() (string, error) {
	candidates := make([]string, 0, 3)
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, "dashboard.html"))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "dashboard.html"))
	}
	candidates = append(candidates, "dashboard.html")

	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", os.ErrNotExist
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

func identifyRequester(host string, headers http.Header, identifierHeader string) (string, string) {
	if strings.EqualFold(strings.TrimSpace(identifierHeader), "ip") || strings.TrimSpace(identifierHeader) == "" {
		return "ip:" + host, host
	}
	key := strings.ToLower(identifierHeader)
	if value := strings.TrimSpace(headers.Get(key)); value != "" {
		return "api_key:" + value, value
	}
	return "ip:" + host, host
}

func writePlainResponse(conn net.Conn, statusCode int, body string, headers map[string]string) {
	statusText := http.StatusText(statusCode)
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n", statusCode, statusText)
	fmt.Fprintf(conn, "Content-Type: text/plain\r\n")
	for key, value := range headers {
		fmt.Fprintf(conn, "%s: %s\r\n", key, value)
	}
	fmt.Fprintf(conn, "Content-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
}

func appendForwardedFor(headers http.Header, clientIP string) {
	existing := strings.TrimSpace(headers.Get("X-Forwarded-For"))
	if existing == "" {
		headers.Set("X-Forwarded-For", clientIP)
		return
	}
	headers.Set("X-Forwarded-For", existing+", "+clientIP)
}

func breakerStateValue(state string) int {
	switch state {
	case breakerStateOpen:
		return 1
	case breakerStateHalfOpen:
		return 2
	default:
		return 0
	}
}

func resolveRateLimitConfig(req *http.Request, identifier string, currentCfg *Config) RateLimitConfig {
	base := currentCfg.RateLimit
	base.Name = "default"
	for _, policy := range currentCfg.Policies {
		if policy.RoutePrefix != "" && !strings.HasPrefix(req.URL.Path, policy.RoutePrefix) {
			continue
		}
		if len(policy.Methods) > 0 && !containsFold(policy.Methods, req.Method) {
			continue
		}
		if len(policy.APIKeys) > 0 && !containsExact(policy.APIKeys, identifier) {
			continue
		}
		resolved := base
		resolved.Name = policy.Name
		applyPolicyOverrides(&resolved, policy)
		applyRateLimitDefaultsToConfig(&resolved)
		return resolved
	}
	return base
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), target) {
			return true
		}
	}
	return false
}

func containsExact(values []string, target string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == target {
			return true
		}
	}
	return false
}

func currentConfig() *Config {
	runtimeMu.RLock()
	defer runtimeMu.RUnlock()
	return cfg
}

func currentRateLimiter() *rateLimiter {
	runtimeMu.RLock()
	defer runtimeMu.RUnlock()
	return rl
}

func currentHealthCheckConfig() HealthCheckConfig {
	currentCfg := currentConfig()
	if currentCfg == nil {
		return HealthCheckConfig{}
	}
	return currentCfg.HealthCheck
}

func reloadRuntimeConfig() error {
	runtimeMu.RLock()
	configPath := runtimeConfigPath
	runtimeMu.RUnlock()
	loadedCfg, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("reload failed: %w", err)
	}
	newLimiter := newRateLimiter(loadedCfg.RateLimit)
	if loadedCfg.RateLimit.Enabled {
		if err := newLimiter.ping(context.Background()); err != nil {
			return fmt.Errorf("reload failed: redis unreachable: %w", err)
		}
	}
	pool.reconfigure(backendAddresses(loadedCfg))
	runtimeMu.Lock()
	oldLimiter := rl
	cfg = loadedCfg
	rl = newLimiter
	runtimeMu.Unlock()
	if oldLimiter != nil && oldLimiter.client != nil {
		_ = oldLimiter.client.Close()
	}
	fmt.Printf("config reloaded: mode=%s backends=%d routes=%d redis=%s rate_key=%s\n", loadedCfg.Mode, len(backendAddresses(loadedCfg)), len(loadedCfg.Routes), loadedCfg.RateLimit.Redis.Addr, loadedCfg.RateLimit.Key)
	return nil
}

func backendAddresses(c *Config) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(c.Backends))
	for _, address := range c.Backends {
		if !seen[address] {
			seen[address] = true
			out = append(out, address)
		}
	}
	for _, route := range c.Routes {
		for _, backend := range route.Backends {
			if !seen[backend.Address] {
				seen[backend.Address] = true
				out = append(out, backend.Address)
			}
		}
	}
	return out
}

func nextRequestID() string {
	return fmt.Sprintf("req-%d", atomic.AddUint64(&requestSeq, 1))
}

func topAbusersPayload() []byte {
	currentCfg := currentConfig()
	data, err := currentRateLimiter().TopAbusers(context.Background(), int64(currentCfg.RateLimit.TopN))
	if err != nil {
		return []byte(`[]`)
	}
	payload, _ := json.Marshal(data)
	return payload
}

func trafficSpikesPayload() []byte {
	currentCfg := currentConfig()
	data, err := currentRateLimiter().TrafficSpikes(context.Background(), int64(currentCfg.RateLimit.TopN))
	if err != nil {
		return []byte(`[]`)
	}
	payload, _ := json.Marshal(data)
	return payload
}

func rateLimitEventsPayload() []byte {
	currentCfg := currentConfig()
	data, err := currentRateLimiter().RateLimitEvents(context.Background(), int64(currentCfg.RateLimit.TopN))
	if err != nil {
		return []byte(`[]`)
	}
	payload, _ := json.Marshal(data)
	return payload
}
