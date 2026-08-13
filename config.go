package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Mode        string            `yaml:"mode"`
	Proxy       ProxyConfig       `yaml:"proxy"`
	Stats       StatsConfig       `yaml:"stats"`
	Backends    []string          `yaml:"backends"`
	Routes      []RouteConfig     `yaml:"routes"`
	TCPBackend  string            `yaml:"tcp_backend"`
	RateLimit   RateLimitConfig   `yaml:"rate_limit"`
	Policies    []PolicyConfig    `yaml:"policies"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
	Tracing     TracingConfig     `yaml:"tracing"`
}

type RouteConfig struct {
	Path       string          `yaml:"path"`
	Backends   []BackendConfig `yaml:"backends"`
	HealthPath string          `yaml:"health_path"`
	TimeoutMs  int             `yaml:"timeout_ms"`
	Retries    int             `yaml:"retries"`
}

type BackendConfig struct {
	Address string `yaml:"address"`
	Weight  int    `yaml:"weight"`
}

type ProxyConfig struct {
	Port                 string    `yaml:"port"`
	TLS                  TLSConfig `yaml:"tls"`
	ReadTimeoutMs        int       `yaml:"read_timeout_ms"`
	ReadHeaderTimeoutMs  int       `yaml:"read_header_timeout_ms"`
	WriteTimeoutMs       int       `yaml:"write_timeout_ms"`
	IdleTimeoutMs        int       `yaml:"idle_timeout_ms"`
	ConnectTimeoutMs     int       `yaml:"connect_timeout_ms"`
	ResponseHeaderMs     int       `yaml:"response_header_timeout_ms"`
	MaxHeaderBytes       int       `yaml:"max_header_bytes"`
	MaxBodyBytes         int64     `yaml:"max_body_bytes"`
	MaxResponseBodyBytes int64     `yaml:"max_response_body_bytes"`
}

type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type StatsConfig struct {
	Port string     `yaml:"port"`
	Auth AuthConfig `yaml:"auth"`
}

type AuthConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type RateLimitConfig struct {
	Name                     string      `yaml:"name"`
	Requests                 int         `yaml:"requests"`
	WindowSeconds            int         `yaml:"window_seconds"`
	IdentifierHeader         string      `yaml:"identifier_header"`
	Key                      string      `yaml:"key"`
	Enabled                  bool        `yaml:"enabled"`
	BaseRequestsPerSecond    float64     `yaml:"base_requests_per_second"`
	BurstCapacity            float64     `yaml:"burst_capacity"`
	SlidingWindowSeconds     int         `yaml:"sliding_window_seconds"`
	SoftThrottleMilliseconds int         `yaml:"soft_throttle_milliseconds"`
	BlockSeconds             int         `yaml:"block_seconds"`
	RepeatedAbuseThreshold   int         `yaml:"repeated_abuse_threshold"`
	EventRetentionSeconds    int         `yaml:"event_retention_seconds"`
	TopN                     int         `yaml:"top_n"`
	Redis                    RedisConfig `yaml:"redis"`
}

type PolicyConfig struct {
	Name                     string   `yaml:"name"`
	RoutePrefix              string   `yaml:"route_prefix"`
	Methods                  []string `yaml:"methods"`
	APIKeys                  []string `yaml:"api_keys"`
	Requests                 int      `yaml:"requests"`
	WindowSeconds            int      `yaml:"window_seconds"`
	BaseRequestsPerSecond    float64  `yaml:"base_requests_per_second"`
	BurstCapacity            float64  `yaml:"burst_capacity"`
	SlidingWindowSeconds     int      `yaml:"sliding_window_seconds"`
	SoftThrottleMilliseconds int      `yaml:"soft_throttle_milliseconds"`
	BlockSeconds             int      `yaml:"block_seconds"`
	RepeatedAbuseThreshold   int      `yaml:"repeated_abuse_threshold"`
	EventRetentionSeconds    int      `yaml:"event_retention_seconds"`
	TopN                     int      `yaml:"top_n"`
}

type HealthCheckConfig struct {
	Enabled         bool   `yaml:"enabled"`
	IntervalSeconds int    `yaml:"interval_seconds"`
	TimeoutSeconds  int    `yaml:"timeout_seconds"`
	Path            string `yaml:"path"`
}

type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type TracingConfig struct {
	Enabled     bool    `yaml:"enabled"`
	ServiceName string  `yaml:"service_name"`
	Endpoint    string  `yaml:"endpoint"`
	Insecure    bool    `yaml:"insecure"`
	SampleRatio float64 `yaml:"sample_ratio"`
}

func defaultConfig() *Config {
	return &Config{
		Mode: "http",
		Proxy: ProxyConfig{
			Port: ":8080", ReadTimeoutMs: 15000, ReadHeaderTimeoutMs: 5000,
			WriteTimeoutMs: 30000, IdleTimeoutMs: 60000, ConnectTimeoutMs: 3000,
			ResponseHeaderMs: 15000, MaxHeaderBytes: 1 << 20,
			MaxBodyBytes: 10 << 20, MaxResponseBodyBytes: 50 << 20,
		},
		Stats: StatsConfig{
			Port: ":8081",
			Auth: AuthConfig{Username: "admin", Password: "changeme"},
		},
		Backends: []string{"localhost:9000", "localhost:9001", "localhost:9002"},
		RateLimit: RateLimitConfig{
			Enabled:                  true,
			Name:                     "default",
			Key:                      "ip",
			Requests:                 5,
			WindowSeconds:            10,
			IdentifierHeader:         "X-API-Key",
			BaseRequestsPerSecond:    5,
			BurstCapacity:            10,
			SlidingWindowSeconds:     10,
			SoftThrottleMilliseconds: 250,
			BlockSeconds:             30,
			RepeatedAbuseThreshold:   3,
			EventRetentionSeconds:    3600,
			TopN:                     10,
			Redis: RedisConfig{
				Addr: "",
			},
		},
		HealthCheck: HealthCheckConfig{
			Enabled: true, IntervalSeconds: 10, TimeoutSeconds: 2, Path: "/health",
		},
		Tracing: TracingConfig{
			Enabled:     false,
			ServiceName: "edge-proxy",
			Endpoint:    "",
			Insecure:    true,
			SampleRatio: 1,
		},
	}
}

func (cfg *Config) Validate() error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if strings.TrimSpace(cfg.Proxy.Port) == "" {
		return fmt.Errorf("proxy.port is required")
	}
	if strings.TrimSpace(cfg.Stats.Port) == "" {
		return fmt.Errorf("stats.port is required")
	}
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	if cfg.Mode == "" {
		cfg.Mode = "http"
	}
	if cfg.Mode != "http" && cfg.Mode != "tcp" {
		return fmt.Errorf("mode must be http or tcp")
	}
	if cfg.Mode == "tcp" && strings.TrimSpace(cfg.TCPBackend) == "" {
		return fmt.Errorf("tcp_backend is required in tcp mode")
	}
	validateAddress := func(address string) error {
		if strings.TrimSpace(address) == "" || !strings.Contains(address, ":") {
			return fmt.Errorf("backend address %q must include host and port", address)
		}
		return nil
	}
	for _, address := range cfg.Backends {
		if err := validateAddress(address); err != nil {
			return err
		}
	}
	for i, route := range cfg.Routes {
		if strings.TrimSpace(route.Path) == "" || !strings.HasPrefix(route.Path, "/") {
			return fmt.Errorf("routes[%d].path must start with /", i)
		}
		if route.TimeoutMs < 0 || route.Retries < 0 {
			return fmt.Errorf("routes[%d] timeout_ms and retries cannot be negative", i)
		}
		if route.HealthPath != "" && !strings.HasPrefix(route.HealthPath, "/") {
			return fmt.Errorf("routes[%d].health_path must start with /", i)
		}
		if len(route.Backends) == 0 {
			return fmt.Errorf("routes[%d].backends is required", i)
		}
		for j, backend := range route.Backends {
			if err := validateAddress(backend.Address); err != nil {
				return fmt.Errorf("routes[%d].backends[%d]: %w", i, j, err)
			}
			if backend.Weight <= 0 {
				return fmt.Errorf("routes[%d].backends[%d].weight must be greater than 0", i, j)
			}
		}
	}
	if cfg.Mode == "http" && len(cfg.Backends) == 0 && len(cfg.Routes) == 0 && strings.TrimSpace(os.Getenv("INPROC_BACKENDS")) != "1" {
		return fmt.Errorf("at least one backend is required unless INPROC_BACKENDS=1")
	}
	if cfg.Proxy.TLS.Enabled {
		if strings.TrimSpace(cfg.Proxy.TLS.CertFile) == "" || strings.TrimSpace(cfg.Proxy.TLS.KeyFile) == "" {
			return fmt.Errorf("proxy.tls.cert_file and proxy.tls.key_file are required when TLS is enabled")
		}
	}
	if !cfg.Proxy.TLS.Enabled && (strings.TrimSpace(cfg.Proxy.TLS.CertFile) != "" || strings.TrimSpace(cfg.Proxy.TLS.KeyFile) != "") {
		if strings.TrimSpace(cfg.Proxy.TLS.CertFile) == "" || strings.TrimSpace(cfg.Proxy.TLS.KeyFile) == "" {
			return fmt.Errorf("both TLS_CERT and TLS_KEY must be set together")
		}
	}
	if cfg.Stats.Auth.Enabled {
		if strings.TrimSpace(cfg.Stats.Auth.Username) == "" {
			return fmt.Errorf("stats.auth.username is required when dashboard auth is enabled")
		}
		if strings.TrimSpace(cfg.Stats.Auth.Password) == "" {
			return fmt.Errorf("stats.auth.password is required when dashboard auth is enabled")
		}
	}
	if cfg.RateLimit.WindowSeconds < 0 || cfg.RateLimit.Requests < 0 {
		return fmt.Errorf("rate_limit.requests and window_seconds cannot be negative")
	}
	if cfg.RateLimit.BaseRequestsPerSecond <= 0 {
		return fmt.Errorf("rate_limit.base_requests_per_second must be greater than 0")
	}
	if cfg.RateLimit.BurstCapacity <= 0 {
		return fmt.Errorf("rate_limit.burst_capacity must be greater than 0")
	}
	if cfg.RateLimit.SlidingWindowSeconds <= 0 {
		return fmt.Errorf("rate_limit.sliding_window_seconds must be greater than 0")
	}
	if cfg.RateLimit.BlockSeconds <= 0 {
		return fmt.Errorf("rate_limit.block_seconds must be greater than 0")
	}
	if cfg.HealthCheck.Enabled {
		if cfg.HealthCheck.IntervalSeconds <= 0 {
			return fmt.Errorf("health_check.interval_seconds must be greater than 0")
		}
		if cfg.HealthCheck.TimeoutSeconds <= 0 {
			return fmt.Errorf("health_check.timeout_seconds must be greater than 0")
		}
		if cfg.HealthCheck.Path == "" || !strings.HasPrefix(cfg.HealthCheck.Path, "/") {
			return fmt.Errorf("health_check.path must start with /")
		}
	}
	if cfg.Proxy.MaxHeaderBytes <= 0 || cfg.Proxy.MaxBodyBytes <= 0 || cfg.Proxy.MaxResponseBodyBytes <= 0 {
		return fmt.Errorf("proxy max header/body sizes must be greater than 0")
	}
	if cfg.Tracing.Enabled {
		if strings.TrimSpace(cfg.Tracing.ServiceName) == "" {
			return fmt.Errorf("tracing.service_name is required when tracing is enabled")
		}
		if strings.TrimSpace(cfg.Tracing.Endpoint) == "" {
			return fmt.Errorf("tracing.endpoint is required when tracing is enabled")
		}
		if cfg.Tracing.SampleRatio < 0 || cfg.Tracing.SampleRatio > 1 {
			return fmt.Errorf("tracing.sample_ratio must be between 0 and 1")
		}
	}
	policyNames := make(map[string]bool, len(cfg.Policies))
	for _, policy := range cfg.Policies {
		if strings.TrimSpace(policy.Name) == "" {
			return fmt.Errorf("policy.name is required")
		}
		if policyNames[policy.Name] {
			return fmt.Errorf("duplicate policy.name %q", policy.Name)
		}
		policyNames[policy.Name] = true
	}
	return nil
}

// loadConfig reads config.yaml if present, then applies env var overrides.
// Missing config file is not an error — defaults are used instead.
func loadConfig(path string) (*Config, error) {
	cfg := defaultConfig()

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			applyEnvOverrides(cfg)
			if err := cfg.Validate(); err != nil {
				return nil, err
			}
			return cfg, nil
		}
		return nil, err
	}
	defer f.Close()

	if err := yaml.NewDecoder(f).Decode(cfg); err != nil {
		return nil, err
	}

	applyEnvOverrides(cfg)
	applyRateLimitDefaults(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnvOverrides lets environment variables override config file values,
// enabling Docker / PaaS deployments without a mounted config file.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("MODE"); v != "" {
		cfg.Mode = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv("PORT"); v != "" {
		cfg.Proxy.Port = normalizePort(v)
	}
	if v := os.Getenv("STATS_PORT"); v != "" {
		cfg.Stats.Port = normalizePort(v)
	}
	if v := os.Getenv("BACKENDS"); v != "" {
		var out []string
		for _, b := range strings.Split(v, ",") {
			if b = strings.TrimSpace(b); b != "" {
				out = append(out, b)
			}
		}
		if len(out) > 0 {
			cfg.Backends = out
		}
	}
	if v := os.Getenv("TCP_BACKEND"); v != "" {
		cfg.TCPBackend = strings.TrimSpace(v)
	}
	if v := os.Getenv("DASHBOARD_USER"); v != "" {
		cfg.Stats.Auth.Username = v
	}
	if v := os.Getenv("DASHBOARD_PASS"); v != "" {
		cfg.Stats.Auth.Enabled = true
		cfg.Stats.Auth.Password = v
	}
	if v := os.Getenv("TLS_CERT"); v != "" {
		cfg.Proxy.TLS.CertFile = v
	}
	if v := os.Getenv("TLS_KEY"); v != "" {
		cfg.Proxy.TLS.KeyFile = v
	}
	if v := os.Getenv("RATE_LIMIT_IDENTIFIER_HEADER"); v != "" {
		cfg.RateLimit.IdentifierHeader = v
	}
	if v := os.Getenv("RATE_LIMIT_RPS"); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.RateLimit.BaseRequestsPerSecond = parsed
		}
	}
	if v := os.Getenv("RATE_LIMIT_BURST"); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.RateLimit.BurstCapacity = parsed
		}
	}
	if v := os.Getenv("RATE_LIMIT_WINDOW_SECONDS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			cfg.RateLimit.SlidingWindowSeconds = parsed
		}
	}
	if v := os.Getenv("RATE_LIMIT_SOFT_THROTTLE_MS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			cfg.RateLimit.SoftThrottleMilliseconds = parsed
		}
	}
	if v := os.Getenv("RATE_LIMIT_BLOCK_SECONDS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			cfg.RateLimit.BlockSeconds = parsed
		}
	}
	if v := os.Getenv("RATE_LIMIT_ABUSE_THRESHOLD"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			cfg.RateLimit.RepeatedAbuseThreshold = parsed
		}
	}
	if v := os.Getenv("RATE_LIMIT_EVENT_RETENTION_SECONDS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			cfg.RateLimit.EventRetentionSeconds = parsed
		}
	}
	if v := os.Getenv("RATE_LIMIT_TOP_N"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			cfg.RateLimit.TopN = parsed
		}
	}
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		cfg.RateLimit.Redis.Addr = v
	}
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		cfg.RateLimit.Redis.Password = v
	}
	if v := os.Getenv("REDIS_DB"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			cfg.RateLimit.Redis.DB = parsed
		}
	}
	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		cfg.Tracing.ServiceName = v
	}
	if v := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		cfg.Tracing.Endpoint = v
		cfg.Tracing.Enabled = true
	}
	if v := os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"); v != "" {
		cfg.Tracing.Insecure = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("OTEL_SAMPLE_RATIO"); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Tracing.SampleRatio = parsed
			cfg.Tracing.Enabled = true
		}
	}
	if cfg.Proxy.TLS.CertFile != "" && cfg.Proxy.TLS.KeyFile != "" {
		cfg.Proxy.TLS.Enabled = true
	}
	applyRateLimitDefaults(cfg)
}

func applyRateLimitDefaults(cfg *Config) {
	cfg.RateLimit.Name = "default"
	applyRateLimitDefaultsToConfig(&cfg.RateLimit)
	for i := range cfg.Policies {
		merged := cfg.RateLimit
		merged.Name = cfg.Policies[i].Name
		applyPolicyOverrides(&merged, cfg.Policies[i])
		applyRateLimitDefaultsToConfig(&merged)
		cfg.Policies[i] = policyFromRateLimitConfig(merged, cfg.Policies[i])
	}
}

func applyRateLimitDefaultsToConfig(rateLimit *RateLimitConfig) {
	if rateLimit.Name == "" {
		rateLimit.Name = "default"
	}
	if rateLimit.IdentifierHeader == "" {
		rateLimit.IdentifierHeader = "X-API-Key"
	}
	if rateLimit.BaseRequestsPerSecond <= 0 && rateLimit.Requests > 0 && rateLimit.WindowSeconds > 0 {
		rateLimit.BaseRequestsPerSecond = float64(rateLimit.Requests) / float64(rateLimit.WindowSeconds)
	}
	if rateLimit.BaseRequestsPerSecond <= 0 {
		rateLimit.BaseRequestsPerSecond = 1
	}
	if rateLimit.SlidingWindowSeconds <= 0 {
		if rateLimit.WindowSeconds > 0 {
			rateLimit.SlidingWindowSeconds = rateLimit.WindowSeconds
		} else {
			rateLimit.SlidingWindowSeconds = 10
		}
	}
	if rateLimit.BurstCapacity <= 0 {
		rateLimit.BurstCapacity = rateLimit.BaseRequestsPerSecond * 2
	}
	if rateLimit.BurstCapacity < 1 {
		rateLimit.BurstCapacity = 1
	}
	if rateLimit.SoftThrottleMilliseconds <= 0 {
		rateLimit.SoftThrottleMilliseconds = 250
	}
	if rateLimit.BlockSeconds <= 0 {
		rateLimit.BlockSeconds = 30
	}
	if rateLimit.RepeatedAbuseThreshold <= 0 {
		rateLimit.RepeatedAbuseThreshold = 3
	}
	if rateLimit.EventRetentionSeconds <= 0 {
		rateLimit.EventRetentionSeconds = 3600
	}
	if rateLimit.TopN <= 0 {
		rateLimit.TopN = 10
	}
}

func applyPolicyOverrides(dst *RateLimitConfig, policy PolicyConfig) {
	if policy.Requests > 0 {
		dst.Requests = policy.Requests
	}
	if policy.WindowSeconds > 0 {
		dst.WindowSeconds = policy.WindowSeconds
	}
	if policy.BaseRequestsPerSecond > 0 {
		dst.BaseRequestsPerSecond = policy.BaseRequestsPerSecond
	}
	if policy.BurstCapacity > 0 {
		dst.BurstCapacity = policy.BurstCapacity
	}
	if policy.SlidingWindowSeconds > 0 {
		dst.SlidingWindowSeconds = policy.SlidingWindowSeconds
	}
	if policy.SoftThrottleMilliseconds > 0 {
		dst.SoftThrottleMilliseconds = policy.SoftThrottleMilliseconds
	}
	if policy.BlockSeconds > 0 {
		dst.BlockSeconds = policy.BlockSeconds
	}
	if policy.RepeatedAbuseThreshold > 0 {
		dst.RepeatedAbuseThreshold = policy.RepeatedAbuseThreshold
	}
	if policy.EventRetentionSeconds > 0 {
		dst.EventRetentionSeconds = policy.EventRetentionSeconds
	}
	if policy.TopN > 0 {
		dst.TopN = policy.TopN
	}
}

func policyFromRateLimitConfig(rateLimit RateLimitConfig, policy PolicyConfig) PolicyConfig {
	policy.Requests = rateLimit.Requests
	policy.WindowSeconds = rateLimit.WindowSeconds
	policy.BaseRequestsPerSecond = rateLimit.BaseRequestsPerSecond
	policy.BurstCapacity = rateLimit.BurstCapacity
	policy.SlidingWindowSeconds = rateLimit.SlidingWindowSeconds
	policy.SoftThrottleMilliseconds = rateLimit.SoftThrottleMilliseconds
	policy.BlockSeconds = rateLimit.BlockSeconds
	policy.RepeatedAbuseThreshold = rateLimit.RepeatedAbuseThreshold
	policy.EventRetentionSeconds = rateLimit.EventRetentionSeconds
	policy.TopN = rateLimit.TopN
	return policy
}

func normalizePort(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, ":") {
		return v
	}
	return ":" + v
}
