# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY *.go ./
RUN go build -o proxy   main.go
RUN go build -o backend backend.go

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.19

WORKDIR /app

COPY --from=builder /app/proxy   ./proxy
COPY --from=builder /app/backend ./backend
COPY dashboard.html              ./dashboard.html

EXPOSE 8080 8081

CMD ["./proxy"]