FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/edge-proxy .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/edge-backend ./cmd/backend

FROM alpine:3.19

RUN apk add --no-cache ca-certificates wget

WORKDIR /srv/edge-proxy

COPY --from=builder /out/edge-proxy /usr/local/bin/edge-proxy
COPY --from=builder /out/edge-backend /usr/local/bin/edge-backend
COPY dashboard.html ./dashboard.html
COPY config.example.yaml ./config.example.yaml

USER 65532:65532

EXPOSE 8080 8081

ENTRYPOINT ["/usr/local/bin/edge-proxy"]
CMD ["run"]
