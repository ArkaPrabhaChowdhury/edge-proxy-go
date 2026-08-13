# EdgeProxy - Lightweight HTTP Reverse Proxy and Rate Limiter

EdgeProxy is an HTTP-aware reverse proxy and rate limiter written in Go. It
supports persistent HTTP/1.1 connections, weighted least-connections routing,
safe retries, circuit breaking, HTTP health checks, Prometheus metrics, JSON
access logs, TLS termination, and graceful draining.

It is not a generic TCP proxy in HTTP mode. For byte-for-byte forwarding, set
`mode: tcp` and `tcp_backend`; TCP mode does not parse or modify application data.

It is for developers who want a small self-hosted gateway they can read, modify, and extend without learning a large proxy stack like Nginx, Envoy, or Kong.

## Why someone would clone this

Clone this project when you need:

- a simple edge layer in front of internal APIs or side-project backends
- Redis-backed rate limiting you can customize in Go
- a local abuse-control sandbox for testing throttling and blocking behavior
- a proxy you can fully read end-to-end in one codebase
- a sandbox for load-balancing and circuit-breaker behavior you can benchmark yourself

For large internet-facing deployments, validate the benchmark matrix against
your workload and consider a battle-tested edge when you need service discovery,
HTTP/2 end-to-end routing, or enterprise policy management.

## Best use cases

This repo is strongest for:

- small SaaS projects that want basic abuse protection in front of an API
- internal tools that need per-IP or per-API-key request control
- staging environments where you want to simulate traffic spikes safely
- teams experimenting with custom gateway logic before moving to a heavier stack
- developers learning how rate limiting, health checks, and backend routing work together

## What it gives you

- adaptive backend selection using active load and observed latency
- process-local or Redis-backed rate limiting
- per-route and per-API-key policy overrides
- abuse tracking with throttle and block events
- active HTTP health checks
- per-backend circuit breaker with half-open recovery
- admin backend control with disable, enable, and drain actions
- OTLP tracing support for distributed request tracing
- hot runtime reload for backends and rate-limit settings
- TLS termination
- Prometheus metrics
- request IDs propagated to upstreams and responses
- structured JSON access logs
- live dashboard and JSON stats endpoints
- a small Go codebase that is easy to fork and change

## Fresh setup

### Easiest path: Docker

```bash
git clone https://github.com/ArkaPrabhaChowdhury/edge-proxy-go
cd edge-proxy-go
docker compose up --build
```

That starts:

- `edge-proxy` on `http://localhost:8080`
- dashboard on `http://localhost:8081`
- metrics on `http://localhost:8081/metrics`
- Redis is optional; it is only needed for shared rate-limit state across replicas.
- three demo backends

### Source setup

Install:

- Go `1.22+`
- Redis

Then run:

```bash
git clone https://github.com/ArkaPrabhaChowdhury/edge-proxy-go
cd edge-proxy-go
go run ./cmd/backend :9000
go run ./cmd/backend :9001
go run ./cmd/backend :9002
```

In another terminal:

```bash
go run . validate
go run . run
```

### Source demo with built-in backends

If Redis is already running and you do not want to manually start backends:

```bash
INPROC_BACKENDS=1 go run . run
```

### Real API demo: FastAPI behind EdgeProxy

The repository includes three FastAPI instances and a route-aware EdgeProxy
configuration:

```powershell
docker compose -f docker-compose.fastapi.yml up --build
curl http://localhost:8080/api/items
curl http://localhost:8081/metrics
```

Each FastAPI instance exposes `/health` and returns its instance name, making
load distribution and unhealthy-instance recovery visible without the built-in
Go demo backend.

## CLI

```bash
edge-proxy run [-config path]
edge-proxy validate [-config path]
edge-proxy version
```

`EDGE_PROXY_CONFIG` sets the default config path.

## Real local test: abuse-control gateway

The most believable demo is to treat this like a small API gateway sitting in front of a backend service.

### 1. Start the stack

Use Docker or source setup above.

### 2. Send normal traffic

```bash
curl http://localhost:8080/hello
curl http://localhost:8080/home
```

You should get successful responses from rotating backends.

### 3. Simulate abuse

PowerShell:

```powershell
1..20 | ForEach-Object { curl.exe -s -o NUL -w "%{http_code}`n" http://localhost:8080/hello }
```

Bash:

```bash
for i in $(seq 1 20); do curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/hello; done
```

This should start triggering throttling or temporary blocks, depending on the rate-limit state.

### 4. Inspect the dashboard

Open:

- `http://localhost:8081`
- `http://localhost:8081/stats`
- `http://localhost:8081/metrics`

Watch for:

- total requests increasing
- limited requests increasing
- top abusers appearing
- rate-limit events being recorded
- per-backend request/error/latency metrics changing as traffic shifts

### 5. Reload config without restart

Edit `config.yaml`, then trigger:

```bash
curl -X POST http://localhost:8081/-/reload
```

This reloads:

- backend list
- rate-limit settings
- health-check settings

These still require a full restart:

- proxy port
- stats port
- TLS listener settings
- dashboard auth middleware

### 6. Manage backends live

List backend state:

```bash
curl http://localhost:8081/-/backends
```

Drain a backend out of rotation:

```bash
curl -X POST http://localhost:8081/-/backends/localhost%3A9000 \
  -H "Content-Type: application/json" \
  -d "{\"action\":\"drain\"}"
```

Disable a backend completely:

```bash
curl -X POST http://localhost:8081/-/backends/localhost%3A9000 \
  -H "Content-Type: application/json" \
  -d "{\"action\":\"disable\"}"
```

Re-enable it:

```bash
curl -X POST http://localhost:8081/-/backends/localhost%3A9000 \
  -H "Content-Type: application/json" \
  -d "{\"action\":\"enable\"}"
```

## Backend resilience and routing

The proxy now does three backend-control things:

- prefers the backend with lower current load and lower observed latency
- opens a circuit breaker after repeated backend failures
- retries a backend only after breaker cooldown through a half-open probe

This makes the project a much stronger systems design example than plain round-robin.

## Observability

Each proxied request now has:

- an `X-Request-Id` header on the response
- the same request ID forwarded upstream
- a structured JSON access log line on stdout
- optional OTLP trace export when tracing is enabled

That makes it much easier to trace a single request through the gateway and backend.

To enable tracing:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
OTEL_SERVICE_NAME=edge-proxy \
OTEL_SAMPLE_RATIO=1 \
go run . run
```

The proxy injects trace context into upstream requests and creates gateway spans for ingress and upstream forwarding.

For a local Prometheus, Grafana, and OpenTelemetry Collector stack:

```bash
docker compose --profile observability up -d
```

See [`docs/architecture.md`](docs/architecture.md), [`docs/benchmarks.md`](docs/benchmarks.md), and [`docs/deployment.md`](docs/deployment.md) for the design, benchmark protocol, and Oracle/Kubernetes rollout story.

## A realistic “why use this” scenario

Imagine you run a small SaaS product with:

- a public API used by customer scripts
- a few noisy clients that occasionally spike traffic
- one internal Go service behind the API

You clone this repo when you want a gateway in front of that service that:

- rate limits by IP or API key header
- gives you visible throttle and block events
- lets you adjust the algorithm in code instead of wrestling with a large proxy config
- is easy for your team to run locally and understand

In that setup, you would replace the demo backends with your real service addresses and set `RATE_LIMIT_IDENTIFIER_HEADER` to something like `X-API-Key`.

## Policy-based limiting

The default rate-limit config applies to all traffic. You can then add narrower policies for:

- a route prefix such as `/api` or `/admin`
- specific HTTP methods
- specific API keys

Example:

```yaml
policies:
  - name: "gold-api"
    route_prefix: "/api"
    methods: ["GET", "POST"]
    api_keys: ["gold-key"]
    base_requests_per_second: 50
    burst_capacity: 100

  - name: "admin-strict"
    route_prefix: "/admin"
    base_requests_per_second: 2
    burst_capacity: 4
    block_seconds: 60
```

This is useful when a normal public API and a sensitive admin route should not share the same abuse controls.

## Configuration

Copy the example file and adjust only what you need:

```bash
cp config.example.yaml config.yaml
go run . validate
go run . run
```

Common environment variables:

- `PORT`
- `STATS_PORT`
- `BACKENDS`
- `REDIS_ADDR`
- `DASHBOARD_USER`
- `DASHBOARD_PASS`
- `TLS_CERT`
- `TLS_KEY`
- `RATE_LIMIT_IDENTIFIER_HEADER`
- `INPROC_BACKENDS`

See [`config.example.yaml`](config.example.yaml) for the full schema.

## Kubernetes and Helm

The repo now includes:

- raw Kubernetes examples in [`k8s/configmap.yaml`](k8s/configmap.yaml), [`k8s/deployment.yaml`](k8s/deployment.yaml), and [`k8s/service.yaml`](k8s/service.yaml)
- a starter Helm chart in [`helm/edge-proxy`](helm/edge-proxy)

Production notes:

- run at least 2 replicas
- keep Redis highly available because policy enforcement depends on it
- protect the stats/admin port with auth or network policy
- point tracing to an OTEL collector instead of exposing it directly to a vendor endpoint
- use drain before disabling a backend during live maintenance
- create the `edge-proxy-auth` Secret before applying the manifests; stats/admin endpoints must not be public

## Development

Useful commands:

```bash
go test ./...
go test -bench BenchmarkProxyHTTPRequest -benchmem
make build
make build-backend
make docker-build
```

CI runs:

- `gofmt -l .`
- `go test ./...`
- `go run . validate`

## Open source workflow

- `LICENSE` is included.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) explains local setup and PR rules.
- [`SECURITY.md`](SECURITY.md) explains how to report vulnerabilities.
- `.github/workflows/ci.yml` runs formatting and tests.
- `.github/workflows/release.yml` publishes tagged releases.

## Project layout

```text
.
|-- cmd/backend/main.go
|-- main.go
|-- config.go
|-- rate_limiter.go
|-- health.go
|-- dashboard.html
|-- Dockerfile
|-- docker-compose.yml
|-- .goreleaser.yml
`-- .github/workflows
```

## License

MIT. See [`LICENSE`](LICENSE).
