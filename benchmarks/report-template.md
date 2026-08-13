# EdgeProxy benchmark report

Record one row per scenario and concurrency level. Keep direct-backend and
proxy runs on the same machine, with the same payload and duration.

| Scenario | Connections/VUs | RPS | p50 | p95 | p99 | Error rate | CPU | Memory | Backend connections | Recovery time |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| direct, one backend | 100 | | | | | | | | | n/a |
| proxy, one backend | 100 | | | | | | | | | n/a |
| proxy, three backends | 1,000 | | | | | | | | | n/a |
| proxy, rate limit on | 10,000 | | | | | | | | | n/a |
| proxy, rate limit off | 10,000 | | | | | | | | | n/a |
| proxy, failing backend | 1,000 | | | | | | | | | seconds |

Capture CPU/memory with `docker stats --no-stream` (or container metrics),
connection counts from `edgeproxy_active_connections` and backend metrics, and
failure recovery time from the first failed health check to the first successful
request after the backend is restored. Do not present an unfilled template as
measured performance.
