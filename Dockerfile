# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o proxy .
RUN go build -o backend ./cmd/backend/

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.19

# ca-certificates needed for TLS outbound connections
RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /app/proxy        ./proxy
COPY --from=builder /app/backend      ./backend
COPY dashboard.html                   ./dashboard.html
COPY config.example.yaml              ./config.example.yaml

EXPOSE 8080 8081

CMD ["./proxy"]
