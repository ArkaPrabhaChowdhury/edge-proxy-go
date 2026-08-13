# EdgeProxy architecture and failure modes

EdgeProxy has two explicit serving modes:

- `http` uses Go's `http.Server`, so HTTP/1.1 keep-alive, multiple requests per
  connection, request parsing, body limits, status propagation, retries, and
  forwarding headers are handled as HTTP concerns.
- `tcp` accepts a connection and copies bytes in both directions to
  `tcp_backend`. It does not inspect HTTP and has no HTTP routes or retries.

The stats listener is separate from the data listener. Configuration is parsed
and validated before either listener starts.

## HTTP request flow

```text
client -> timeout/body limit -> request ID + X-Forwarded-* -> rate limiter
       -> longest route match -> healthy weighted least-connections backend
       -> safe retry on transport/502/503/504 failures -> response/status/logs
```

Only GET and HEAD are retried. A route timeout bounds the upstream attempt
sequence. The proxy forwards the backend's status code and response body; it
does not turn upstream errors into 200 responses.

## Redis and consistency

Redis stores sliding-window events, burst state, abuse counters, and operational insights. A Redis-backed decision is consistent across proxy replicas for a given Redis deployment. The implementation fails closed when configured Redis cannot be reached: requests receive `503 Rate limiter unavailable` rather than silently bypassing policy. Redis outage loses only ephemeral rate-limit state and insight history; it must not lose business data.

When Redis is omitted, the proxy uses an explicit process-local fixed-window
limiter. This is suitable for one instance or a development stack, but limits
must be backed by Redis when several proxy replicas need a shared policy.

## Backend selection and failure isolation

Health checks call `health_path` over HTTP and require a 2xx/3xx response. They
remove unhealthy backends from selection. Selection uses active requests,
observed latency, recent failures, and configured weight. Each backend has an
independent circuit breaker: repeated transport/5xx failures open the circuit,
the cooldown permits one half-open probe, and a successful probe closes it.

Failure behavior:

- no eligible backend: `503 Service Unavailable`
- upstream timeout: `504 Gateway Timeout`
- upstream connection/protocol failure: `502 Bad Gateway`
- rate limiter failure: `503 Service Unavailable` when Redis is configured
- request body over the configured limit: `413 Content Too Large`
- known oversized upstream response: `502 Bad Gateway`

During shutdown, the HTTP server stops accepting new work, disables keep-alives,
and drains active requests within the shutdown deadline.

## Safe loss and scaling

It is safe to lose rate-limit windows, abuse insight history, average-latency samples, and in-memory counters during a process restart. It is not safe to lose configuration, credentials, request payloads, or application data; those belong outside this proxy.

Across regions, use a regional proxy pool and a regional Redis deployment for low-latency decisions. Global policy requires a globally replicated or strongly coordinated limiter and accepts higher latency. Routing should prefer the nearest healthy region, with explicit failover and a conservative emergency policy during inter-region Redis loss.
