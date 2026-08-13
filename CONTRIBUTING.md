# Contributing

## Local setup

1. Install Go 1.22+ and Redis.
2. Copy `config.example.yaml` to `config.yaml` if you want a local config file.
3. Run `go run ./cmd/backend :9000`, `:9001`, and `:9002` in separate terminals, or set `INPROC_BACKENDS=1`.
4. Start the proxy with `go run . run`.

## Common commands

- `go run . validate`
- `go test ./...`
- `make build`
- `docker compose up --build`
- Follow the abuse-control demo in `README.md` to verify rate limiting manually.

## Pull requests

- Keep changes focused.
- Run `gofmt -w *.go cmd/backend/*.go`.
- Run `go test ./...`.
- Update `README.md` when behavior or setup changes.
