# Deployment story

## Local development

```powershell
docker compose up -d --build
docker compose --profile observability up -d
```

The proxy, three demo backends, Redis, Prometheus, Grafana, and the OpenTelemetry Collector are then available locally. Grafana is at `http://127.0.0.1:3000`; Prometheus is at `http://127.0.0.1:9090`.

## Oracle rolling deployment

GitHub Actions deploys `main` over SSH to the Oracle VM. The workflow clones or fast-forwards a dedicated deployment directory, runs `docker compose up -d --build --remove-orphans`, and checks the local metrics endpoint plus the public HTTPS endpoint.

Required repository secrets:

- `OCI_HOST`
- `OCI_USER`
- `OCI_SSH_KEY`
- `OCI_DEPLOY_DIR` (absolute path on the VM, for example `/opt/edge-proxy`)
- `OCI_PORT` (optional; defaults to 22)

The deployment directory must be dedicated to this service. Compose recreates containers one service at a time; for Kubernetes use the supplied Deployment with at least two replicas and `kubectl rollout status deployment/edge-proxy` before considering the rollout complete.

The workflow does not claim success until both the remote Compose health check and the public `/metrics` check pass. A missing secret fails before SSH starts with an actionable error.

## Kubernetes / Helm

```powershell
helm upgrade --install edge-proxy ./helm/edge-proxy --wait --timeout 5m
kubectl rollout status deployment/edge-proxy --timeout=5m
```

Use a Kubernetes Secret for stats credentials and Redis credentials in a real cluster. The example manifests are intentionally starter manifests and should not be exposed directly to the public internet.
