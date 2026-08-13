APP_NAME := edge-proxy
BACKEND_NAME := edge-backend
DIST_DIR := dist
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || python -c "from datetime import datetime, timezone; print(datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'))")
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build build-backend run validate test fmt clean docker-build compose-up compose-down

build:
	mkdir -p $(DIST_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(APP_NAME) .

build-backend:
	mkdir -p $(DIST_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BACKEND_NAME) ./cmd/backend

run:
	go run . run

validate:
	go run . validate

test:
	go test ./...

fmt:
	gofmt -w *.go cmd/backend/*.go

clean:
	rm -rf $(DIST_DIR)

docker-build:
	docker build -t $(APP_NAME):dev .

compose-up:
	docker compose up --build

compose-down:
	docker compose down
