# edge-proxy

A lightweight TCP reverse proxy with built-in load balancing, sliding-window rate limiting, backend health checks, and a real-time monitoring dashboard.

Single binary. No config required to get started. Drop it in front of any HTTP service.

![Go](https://img.shields.io/badge/Language-Go-00ADD8?style=for-the-badge&logo=go&logoColor=white)
![TCP](https://img.shields.io/badge/Protocol-TCP-blue?style=for-the-badge)
![License](https://img.shields.io/badge/License-MIT-green?style=for-the-badge)

---

## Features

- **Round-robin load balancing** across a configurable backend pool
- **Sliding-window rate limiting** per IP (configurable requests / window)
- **Automatic health checks** — unhealthy backends are removed from rotation and re-added when they recover
- **TLS termination** — point at your cert + key files, done
- **Real-time dashboard** — live request log, per-IP heatmap, backend health, sparkline
- **Prometheus `/metrics`** endpoint — drop into any existing Grafana stack
- **Dashboard basic auth** — one env var to protect the UI
- **Graceful shutdown** — drains in-flight connections on SIGTERM / SIGINT
- **Zero external runtime dependencies** — single static binary
- **Docker-first** — multi-stage image, under 20 MB

---

## Quickstart

### Option 1 — Docker Compose (recommended)

```bash
git clone https://github.com/arkaprabhachowdhury/edge-proxy-go
cd edge-proxy-go
docker compose up --build
```

- Proxy:     http://localhost:8080
- Dashboard: http://localhost:8081
- Metrics:   http://localhost:8081/metrics

### Option 2 — Run from source

```bash
# Terminal 1 — start three test backends
go run ./cmd/backend :9000
go run ./cmd/backend :9001
go run ./cmd/backend :9002

# Terminal 2 — start the proxy
go run .
```

### Option 3 — Single binary (no external backends needed)

```bash
INPROC_BACKENDS=1 go run .
```

This starts three in-process backends automatically — great for demos and trying it out.

---

## Configuration

### Config file (recommended)

```bash
cp config.example.yaml config.yaml
# edit config.yaml, then:
go run .
```

```yaml
proxy:
  port: "8080"
  tls:
    enabled: false
    cert_file: "cert.pem"
    key_file:  "key.pem"

stats:
  port: "8081"
  auth:
    enabled: false
    username: "admin"
    password: "changeme"

backends:
  - "localhost:9000"
  - "localhost:9001"
  - "localhost:9002"

rate_limit:
  requests: 5
  window_seconds: 10

health_check:
  enabled: true
  interval_seconds: 10
  timeout_seconds: 2
```

### Environment variables

All config values can be overridden by environment variables — useful for Docker / PaaS / Railway / Fly.io.

| Variable          | Description                                | Default              |
|-------------------|--------------------------------------------|----------------------|
| `PORT`            | Proxy listen port                          | `8080`               |
| `STATS_PORT`      | Dashboard / metrics port                   | `8081`               |
| `BACKENDS`        | Comma-separated backend addresses          | `localhost:9000,...` |
| `DASHBOARD_PASS`  | Enables basic auth with this password      | *(disabled)*         |
| `DASHBOARD_USER`  | Basic auth username                        | `admin`              |
| `TLS_CERT`        | Path to TLS certificate file               | *(disabled)*         |
| `TLS_KEY`         | Path to TLS private key file               | *(disabled)*         |
| `INPROC_BACKENDS` | Set to `1` to start built-in test backends | *(disabled)*         |

---

## TLS

```bash
# Generate a self-signed cert for local testing
openssl req -x509 -newkey rsa:4096 -keyout key.pem -out cert.pem \
  -days 365 -nodes -subj '/CN=localhost'

# Start proxy with TLS
TLS_CERT=cert.pem TLS_KEY=key.pem go run .
```

Or in `config.yaml`:

```yaml
proxy:
  tls:
    enabled: true
    cert_file: "/etc/ssl/certs/my.crt"
    key_file:  "/etc/ssl/private/my.key"
```

---

## Dashboard auth

```bash
DASHBOARD_PASS=mysecret go run .
# Visit http://localhost:8081 — browser prompts for admin / mysecret
```

---

## Prometheus metrics

Scrape `http://<host>:8081/metrics`:

```
edgeproxy_requests_total      — total requests proxied (counter)
edgeproxy_rate_limited_total  — requests rejected by rate limiter (counter)
edgeproxy_active_connections  — current open connections (gauge)
edgeproxy_healthy_backends    — backends passing health checks (gauge)
edgeproxy_total_backends      — total backends configured (gauge)
```

Prometheus `scrape_configs` entry:

```yaml
- job_name: edge-proxy
  static_configs:
    - targets: ['localhost:8081']
```

---

## HTTP API

| Endpoint       | Port | Description                       |
|----------------|------|-----------------------------------|
| `GET /`        | 8081 | Real-time dashboard               |
| `GET /stats`   | 8081 | Full stats snapshot (JSON)        |
| `GET /metrics` | 8081 | Prometheus metrics                |
| `*`            | 8080 | All traffic → load balanced proxy |

---

## Architecture

```
                     ┌──────────────────────────────┐
Client ──:8080──────▶│  TCP Proxy + Rate Limiter     │
                     │  • sliding-window per IP       │
                     │  • round-robin to backends     │
                     └────────────┬─────────────────┘
                                  │
               ┌──────────────────┼──────────────────┐
               ▼                  ▼                  ▼
          Backend :9000     Backend :9001     Backend :9002
               (health-checked every 10 s via TCP dial)

                     ┌──────────────────────────────┐
Browser ──:8081─────▶│  HTTP Stats Server            │
                     │  GET /         → dashboard    │
                     │  GET /stats    → JSON         │
                     │  GET /metrics  → Prometheus   │
                     └──────────────────────────────┘
```

---

## Building

```bash
# Proxy binary
go build -o proxy .

# Test backend server
go build -o backend ./cmd/backend/

# Docker image
docker build -t edge-proxy .
```

---

## Project layout

```
.
├── main.go                # proxy entry point, connection handler
├── config.go              # YAML config loading + env overrides
├── health.go              # backend pool with health checks
├── dashboard.html         # real-time monitoring UI
├── config.example.yaml    # annotated config reference
├── cmd/
│   └── backend/
│       └── main.go        # lightweight test backend server
├── Dockerfile
└── docker-compose.yml
```

---

## License

MIT
