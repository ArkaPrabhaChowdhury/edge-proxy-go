package main

import (
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Proxy       ProxyConfig       `yaml:"proxy"`
	Stats       StatsConfig       `yaml:"stats"`
	Backends    []string          `yaml:"backends"`
	RateLimit   RateLimitConfig   `yaml:"rate_limit"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
}

type ProxyConfig struct {
	Port string    `yaml:"port"`
	TLS  TLSConfig `yaml:"tls"`
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
	Requests                 int         `yaml:"requests"`
	WindowSeconds            int         `yaml:"window_seconds"`
	IdentifierHeader         string      `yaml:"identifier_header"`
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

type HealthCheckConfig struct {
	Enabled         bool `yaml:"enabled"`
	IntervalSeconds int  `yaml:"interval_seconds"`
	TimeoutSeconds  int  `yaml:"timeout_seconds"`
}

type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

func defaultConfig() *Config {
	return &Config{
		Proxy: ProxyConfig{Port: ":8080"},
		Stats: StatsConfig{
			Port: ":8081",
			Auth: AuthConfig{Username: "admin", Password: "changeme"},
		},
		Backends: []string{"localhost:9000", "localhost:9001", "localhost:9002"},
		RateLimit: RateLimitConfig{
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
				Addr: "localhost:6379",
			},
		},
		HealthCheck: HealthCheckConfig{
			Enabled: true, IntervalSeconds: 10, TimeoutSeconds: 2,
		},
	}
}

// loadConfig reads config.yaml if present, then applies env var overrides.
// Missing config file is not an error — defaults are used instead.
func loadConfig(path string) (*Config, error) {
	cfg := defaultConfig()

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			applyEnvOverrides(cfg)
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
	return cfg, nil
}

// applyEnvOverrides lets environment variables override config file values,
// enabling Docker / PaaS deployments without a mounted config file.
func applyEnvOverrides(cfg *Config) {
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
	if cfg.Proxy.TLS.CertFile != "" && cfg.Proxy.TLS.KeyFile != "" {
		cfg.Proxy.TLS.Enabled = true
	}
	applyRateLimitDefaults(cfg)
}

func applyRateLimitDefaults(cfg *Config) {
	if cfg.RateLimit.IdentifierHeader == "" {
		cfg.RateLimit.IdentifierHeader = "X-API-Key"
	}
	if cfg.RateLimit.BaseRequestsPerSecond <= 0 && cfg.RateLimit.Requests > 0 && cfg.RateLimit.WindowSeconds > 0 {
		cfg.RateLimit.BaseRequestsPerSecond = float64(cfg.RateLimit.Requests) / float64(cfg.RateLimit.WindowSeconds)
	}
	if cfg.RateLimit.BaseRequestsPerSecond <= 0 {
		cfg.RateLimit.BaseRequestsPerSecond = 1
	}
	if cfg.RateLimit.SlidingWindowSeconds <= 0 {
		if cfg.RateLimit.WindowSeconds > 0 {
			cfg.RateLimit.SlidingWindowSeconds = cfg.RateLimit.WindowSeconds
		} else {
			cfg.RateLimit.SlidingWindowSeconds = 10
		}
	}
	if cfg.RateLimit.BurstCapacity <= 0 {
		cfg.RateLimit.BurstCapacity = cfg.RateLimit.BaseRequestsPerSecond * 2
	}
	if cfg.RateLimit.BurstCapacity < 1 {
		cfg.RateLimit.BurstCapacity = 1
	}
	if cfg.RateLimit.SoftThrottleMilliseconds <= 0 {
		cfg.RateLimit.SoftThrottleMilliseconds = 250
	}
	if cfg.RateLimit.BlockSeconds <= 0 {
		cfg.RateLimit.BlockSeconds = 30
	}
	if cfg.RateLimit.RepeatedAbuseThreshold <= 0 {
		cfg.RateLimit.RepeatedAbuseThreshold = 3
	}
	if cfg.RateLimit.EventRetentionSeconds <= 0 {
		cfg.RateLimit.EventRetentionSeconds = 3600
	}
	if cfg.RateLimit.TopN <= 0 {
		cfg.RateLimit.TopN = 10
	}
}

func normalizePort(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, ":") {
		return v
	}
	return ":" + v
}
