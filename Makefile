# Makefile for OpenViking (Go)

GO ?= go
GOWORK ?= off
CMD_DIR := cmd
BIN_DIR := bin
SERVER := $(BIN_DIR)/openviking-server
OV := $(BIN_DIR)/ov
VIKINGBOT := $(BIN_DIR)/vikingbot
MIGRATE := $(BIN_DIR)/openviking-migrate
DOCTOR := $(BIN_DIR)/openviking-doctor

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG := github.com/volcengine/openviking/internal/version
LDFLAGS := -ldflags="-s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).BuildTime=$(BUILD_TIME)"

GOFLAGS := -trimpath
# go-tree-sitter (used by internal/parse/parsers/code) requires cgo for its
# C bindings; CGO_ENABLED=0 excludes the cgo files and breaks the build.
CGO_ENABLED ?= 1

# Web-studio (frontend) paths. WEB_STUDIO_DIR defaults to the on-disk
# web-studio/dist produced by `pnpm build`; the server reads it via
# OPENVIKING_WEB_STUDIO_DIR env (mirrors the Python gate in app.py:659).
WEB_STUDIO_DIR ?= $(CURDIR)/web-studio/dist
SERVER_HOST ?= 127.0.0.1
SERVER_PORT ?= 1933
SERVER_URL := http://$(SERVER_HOST):$(SERVER_PORT)

# Config file for `make run`. Defaults to the local-memory smoke config;
# callers can override with OV_CONFIG=/path/to/ov.conf.
OV_CONFIG ?= $(CURDIR)/examples/ov.conf.local-memory

.PHONY: all build test test-short test-integration test-e2e bench lint cover tidy fmt vet docker dev run run-server open-browser clean web-studio web-studio-install help

all: build

build: web-studio
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) GOWORK=$(GOWORK) $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(SERVER) ./$(CMD_DIR)/openviking-server
	CGO_ENABLED=$(CGO_ENABLED) GOWORK=$(GOWORK) $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(OV) ./$(CMD_DIR)/ov
	CGO_ENABLED=$(CGO_ENABLED) GOWORK=$(GOWORK) $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(VIKINGBOT) ./$(CMD_DIR)/vikingbot
	CGO_ENABLED=$(CGO_ENABLED) GOWORK=$(GOWORK) $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(MIGRATE) ./$(CMD_DIR)/openviking-migrate
	CGO_ENABLED=$(CGO_ENABLED) GOWORK=$(GOWORK) $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(DOCTOR) ./$(CMD_DIR)/openviking-doctor

web-studio-install:
	@if [ -d web-studio ] && [ -f web-studio/package.json ]; then \
		cd web-studio && pnpm install --ignore-workspace; \
	else \
		echo "  [SKIP] web-studio not found"; \
	fi

web-studio:
	@if [ -d web-studio ] && [ -f web-studio/package.json ]; then \
		cd web-studio && (pnpm install --frozen-lockfile --ignore-workspace 2>/dev/null || pnpm install --ignore-workspace) && pnpm build; \
	else \
		echo "  [SKIP] web-studio not found, embedding empty asset"; \
	fi

test:
	GOWORK=$(GOWORK) $(GO) test -race -cover -coverprofile=coverage.out ./...

test-short:
	GOWORK=$(GOWORK) $(GO) test -short -race ./...

test-integration:
	GOWORK=$(GOWORK) $(GO) test -tags=integration -race ./...

test-e2e:
	GOWORK=$(GOWORK) $(GO) test -tags=e2e -race ./...

bench:
	GOWORK=$(GOWORK) $(GO) test -bench=. -benchmem -run=^$$ ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || echo "golangci-lint not installed, skipping"

cover:
	GOWORK=$(GOWORK) $(GO) tool cover -html=coverage.out -o coverage.html

tidy:
	GOWORK=$(GOWORK) $(GO) mod tidy

docker:
	docker build -t openviking:dev .

dev:
	GOWORK=$(GOWORK) $(GO) run ./$(CMD_DIR)/openviking-server

# run-server starts the OpenViking server with the local-memory smoke
# config + web-studio static assets. Use `make run` for the full
# frontend+backend experience; this target is for callers who already
# built the frontend and just want the server.
run-server: web-studio
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) GOWORK=$(GOWORK) $(GO) build $(GOFLAGS) -o $(SERVER) ./$(CMD_DIR)/openviking-server
	OPENVIKING_WEB_STUDIO_DIR=$(WEB_STUDIO_DIR) $(SERVER) --config $(OV_CONFIG)

# open-browser opens the default browser to the web-access entry point.
# Detects xdg-open (Linux), open (macOS), and start (Windows).
open-browser:
	@if command -v xdg-open >/dev/null 2>&1; then \
		xdg-open $(SERVER_URL)/web-access; \
	elif command -v open >/dev/null 2>&1; then \
		open $(SERVER_URL)/web-access; \
	elif command -v start >/dev/null 2>&1; then \
		start $(SERVER_URL)/web-access; \
	else \
		echo "  [WARN] no browser opener found; open $(SERVER_URL)/web-access manually"; \
	fi

# run starts the backend (with web-studio static assets wired via
# OPENVIKING_WEB_STUDIO_DIR) and opens the browser to /web-access. The
# server runs in the foreground; Ctrl-C stops it. Backend build is
# skipped when the binary is up to date. The frontend is built on first
# run and reused afterwards — drop into web-studio/ and run `pnpm dev`
# separately for hot-reload during frontend work.
run: run-server
	@echo "OpenViking server started at $(SERVER_URL)"
	@echo "Opening browser to $(SERVER_URL)/web-access ..."
	@$(MAKE) --no-print-directory open-browser
	@echo "Server is running in the foreground. Press Ctrl-C to stop."

clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html

fmt:
	GOWORK=$(GOWORK) $(GO) fmt ./...

vet:
	GOWORK=$(GOWORK) $(GO) vet ./...

help:
	@echo "OpenViking Go monorepo targets:"
	@echo "  build           - Build all 5 binaries into bin/"
	@echo "  run             - Build web-studio + server, start server, open browser to /web-access"
	@echo "  run-server      - Build web-studio + server, start server (no browser)"
	@echo "  open-browser    - Open browser to http://127.0.0.1:1933/web-access"
	@echo "  web-studio      - Build web-studio frontend (pnpm install + build)"
	@echo "  web-studio-install - pnpm install only (no build)"
	@echo "  test            - Run all tests with race + coverage"
	@echo "  test-short      - Run only short tests"
	@echo "  test-integration- Run integration tests (requires docker)"
	@echo "  test-e2e        - Run E2E tests"
	@echo "  bench           - Run benchmarks"
	@echo "  lint            - Run golangci-lint"
	@echo "  cover           - Generate HTML coverage report"
	@echo "  tidy            - go mod tidy"
	@echo "  docker          - Build docker image"
	@echo "  dev             - Run server in dev mode (no frontend build)"
	@echo "  clean           - Remove build artifacts"
	@echo "  fmt             - go fmt"
	@echo "  vet             - go vet"
