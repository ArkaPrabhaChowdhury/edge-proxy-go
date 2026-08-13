# Benchmark evidence

## Baseline captured locally

Machine: Lenovo 83JC, AMD Ryzen 7 7435HS, 24 GB RAM, Windows.

Toolchain: Go 1.25.0, `windows/386`.

Commit: `676225aadd3d8f94b694e610a8ebc5d07118dc71`.

Command:

```text
go test -run '^$' -bench BenchmarkProxyHTTPRequest -benchtime=3s -count=3 -benchmem ./...
```

Results:

| Sample | ns/op | B/op | allocs/op |
| ---: | ---: | ---: | ---: |
| 1 | 761,408 | 21,726 | 187 |
| 2 | 938,316 | 21,763 | 186 |
| 3 | 975,775 | 21,686 | 186 |

This benchmark exercises one in-process backend and a `net.Pipe`; it does not establish production throughput or p50/p95/p99 latency.

## k6 smoke benchmark captured locally

Machine: Lenovo 83JC, AMD Ryzen 7 7435HS, 24 GB RAM, Windows host with Docker Desktop 28.4.0.

Runtime: Docker Compose proxy, Redis, and three demo backends; host ports `18080` and `18081`; working tree based on commit `676225a` with the changes in this repository.

Command:

```text
docker run --rm --add-host=host.docker.internal:host-gateway -e BASE_URL=http://host.docker.internal:18080 -e RATE=20 -e DURATION=10s -e VUS=10 -e MAX_VUS=30 -v "${PWD}/benchmarks:/scripts:ro" grafana/k6 run /scripts/k6.js
```

Results: 201 requests in 10 seconds, 20.48 requests/second, 0 failed requests, p50 5.16 ms, p95 8.27 ms, p99 18 ms, maximum 26.32 ms. This is a smoke benchmark, not a capacity limit: the rate limiter was configured to allow the generated clients, and the run used demo backends on the same machine.

## Redis-backed limiter benchmark captured locally

Machine and toolchain were the same as the baseline. Redis ran in Docker on `127.0.0.1:16379`.

Command:

```text
go test -run '^$' -bench BenchmarkRateLimitRedisBacked -benchtime=200ms -count=3 -benchmem ./...
```

Results: 3,253,700 ns/op (3,803 B/op, 139 allocs/op), 1,752,542 ns/op (1,969 B/op, 55 allocs/op), and 2,042,873 ns/op (1,977 B/op, 55 allocs/op). These values include Redis round trips and are not directly comparable to the local in-memory algorithm benchmarks without matching workload and reset behavior.

## Required comparison matrix

The reproducible benchmark suite now includes local fixed-window, local sliding-window, optional Redis-backed, one-backend, three-backend, and circuit-breaker state benchmarks. k6 supplies sustained traffic and p50/p95/p99 latency. Each published run must include the exact environment and command; skipped Redis or k6 runs must remain marked as skipped.
