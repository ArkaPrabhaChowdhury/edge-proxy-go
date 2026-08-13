package main

import "testing"

func TestConfigValidateRejectsMissingBackends(t *testing.T) {
	cfg := defaultConfig()
	cfg.Backends = nil

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected missing backends to fail validation")
	}
}

func TestConfigValidateRejectsIncompleteTLS(t *testing.T) {
	cfg := defaultConfig()
	cfg.Proxy.TLS.Enabled = true
	cfg.Proxy.TLS.CertFile = "cert.pem"
	cfg.Proxy.TLS.KeyFile = ""

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected incomplete TLS config to fail validation")
	}
}

func TestConfigValidateAcceptsDefaults(t *testing.T) {
	cfg := defaultConfig()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected default config to validate: %v", err)
	}
}

func TestConfigValidateRejectsInvalidTracing(t *testing.T) {
	cfg := defaultConfig()
	cfg.Tracing.Enabled = true
	cfg.Tracing.Endpoint = ""

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected tracing without endpoint to fail validation")
	}
}
