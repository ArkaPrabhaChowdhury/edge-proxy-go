package main

import (
	"os"
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
	Requests      int `yaml:"requests"`
	WindowSeconds int `yaml:"window_seconds"`
}

type HealthCheckConfig struct {
	Enabled         bool `yaml:"enabled"`
	IntervalSeconds int  `yaml:"interval_seconds"`
	TimeoutSeconds  int  `yaml:"timeout_seconds"`
}

func defaultConfig() *Config {
	return &Config{
		Proxy: ProxyConfig{Port: ":8080"},
		Stats: StatsConfig{
			Port: ":8081",
			Auth: AuthConfig{Username: "admin", Password: "changeme"},
		},
		Backends: []string{"localhost:9000", "localhost:9001", "localhost:9002"},
		RateLimit: RateLimitConfig{Requests: 5, WindowSeconds: 10},
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
	if cfg.Proxy.TLS.CertFile != "" && cfg.Proxy.TLS.KeyFile != "" {
		cfg.Proxy.TLS.Enabled = true
	}
}

func normalizePort(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, ":") {
		return v
	}
	return ":" + v
}
