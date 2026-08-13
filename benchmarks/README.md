# Benchmark protocol

Run the local stack first:

```powershell
docker compose up -d --build
docker compose --profile observability up -d
```

For the Redis-backed Go benchmark, expose Redis on a free host port and point the test at it:

```powershell
$env:REDIS_HOST_PORT = '16379'
docker compose up -d redis
$env:BENCH_REDIS_ADDR = '127.0.0.1:16379'
go test -run '^$' -bench BenchmarkRateLimitRedisBacked -benchmem -count=5 ./...
```

Run the Go microbenchmarks:

```powershell
go test -run '^$' -bench 'BenchmarkRateLimit|BenchmarkBackendSelection|BenchmarkCircuitBreaker' -benchmem -count=5 ./...
```

Run the traffic benchmark with k6:

```powershell
docker run --rm --network host -e BASE_URL=http://127.0.0.1:8080 -e RATE=100 -e DURATION=60s -v "${PWD}/benchmarks:/scripts:ro" grafana/k6 run /scripts/k6.js
```

For the interviewer-facing comparison matrix, start each deliberately isolated
stack (direct backend, one backend, three backends, rate limiting on/off, and a
health-check failure) and run `./benchmarks/run-matrix.ps1`. Repeat each stack
at 100, 1,000, and 10,000 VUs. Store k6 summaries and fill
[`report-template.md`](report-template.md) with throughput, p50/p95/p99,
errors, proxy/backend connections, CPU, memory, and failure recovery time.

Record every run with: machine model and CPU, RAM, OS, Go/k6/Docker versions, git SHA, exact command, backend count, rate-limit mode, Redis mode, concurrency/VUs, request count, duration, errors, throughput, and p50/p95/p99 latency. Do not compare runs made with different backend counts or limiter modes.

The repository's initial Go baseline is recorded in [`docs/benchmarks.md`](../docs/benchmarks.md). It is intentionally labeled as a single-backend microbenchmark; it is not a replacement for the scenario matrix above.
