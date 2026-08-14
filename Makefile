.PHONY: all build clean test test-release test-recovery run-server run-dashboard run-builder run-client build-image-factory build-desktop-runner build-migrate build-signer build-capacity-actuator build-persistent-executor-template test-persistent-executor-gate build-distcc-gate distcc-gate lint lint-security lint-complexity web-deps web-build web-lint web-test web-clean web

# Variables
BINARY_SERVER=bin/portage-server
BINARY_DASHBOARD=bin/portage-dashboard
BINARY_BUILDER=bin/portage-builder
BINARY_CLIENT=bin/portage-client
BINARY_IMAGE_FACTORY=bin/portage-image-factory
BINARY_DESKTOP_RUNNER=bin/portage-desktop-runner
BINARY_MIGRATE=bin/portage-migrate
BINARY_SIGNER=bin/portage-signer
BINARY_CAPACITY_ACTUATOR=bin/portage-capacity-actuator
BINARY_DISTCC_GATE=bin/portage-distcc-gate
GO=go
GOFLAGS=-v
PYTHON ?= python3
NPM ?= npm
WEB_DIR=web
# Where `npm run build` writes. web/vite.config.ts points its outDir here, so
# the bytes go:embed compiles in are the bytes Vite just emitted; there is no
# copy step in between that could carry a stale tree.
WEB_DIST=internal/dashboard/webassets/bundle/dist
WEB_NODE_MODULES_STAMP=$(WEB_DIR)/node_modules/.package-lock.json
# Must be a release built with Go 1.26 or newer. golangci-lint refuses to load
# any config whose module targets a language version above its own toolchain,
# and go.mod is go 1.26.6, so v2.7.2 (built with go1.25) exits 3 before it lints
# a single file.
GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT = $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS=-ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)"

all: build

# Build all binaries. The console bundle comes first: the dashboard binary
# embeds it, and a Go build that runs before it produces a binary that compiles,
# starts, serves every old route, and answers 503 on /ui — which is the one
# failure an operator would only find in a browser.
build: web-build build-server build-dashboard build-builder build-client build-image-factory build-desktop-runner build-migrate build-signer build-capacity-actuator build-distcc-gate

# ---- operator console (web/) ----
# The frontend is a real project with its own toolchain: Vite + React +
# TypeScript, built with npm, compiled into the Go binary with go:embed. The
# lockfile is committed and every install goes through `npm ci`, so a build
# resolves the same tree it resolved last time and an offline mirror can serve it.

# npm ci writes this file, so it is the honest stamp for "node_modules holds the
# tree the lockfile names". A file target rather than a phony one because
# web-build depends on it: on a fresh clone the file is absent and the install
# runs, and on every later `make build` it is newer than the lockfile and the
# install is skipped.
$(WEB_NODE_MODULES_STAMP): $(WEB_DIR)/package-lock.json $(WEB_DIR)/package.json
	@echo "Installing console dependencies..."
	cd $(WEB_DIR) && $(NPM) ci

web-deps: $(WEB_NODE_MODULES_STAMP)

# Depends on the install because `build` depends on this: without it a fresh
# clone runs `npm run build` with no node_modules and dies on `tsc: not found`
# before a single Go binary is produced.
web-build: web-deps
	@echo "Building Portage Engine Console..."
	cd $(WEB_DIR) && $(NPM) run build

web-lint:
	@echo "Linting console (eslint, stylelint, prettier)..."
	cd $(WEB_DIR) && $(NPM) run lint

web-test:
	@echo "Testing console..."
	cd $(WEB_DIR) && $(NPM) run test

web-clean:
	@rm -rf $(WEB_DIST)

# Everything the console has to pass, in the order CI runs it.
web: web-deps web-build web-lint web-test

# Build server
build-server:
	@echo "Building Portage Engine Server..."
	@mkdir -p bin
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BINARY_SERVER) cmd/server/main.go

# Build dashboard
build-dashboard:
	@echo "Building Portage Engine Dashboard..."
	@mkdir -p bin
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BINARY_DASHBOARD) cmd/dashboard/main.go

# Build builder
build-builder:
	@echo "Building Portage Engine Builder..."
	@mkdir -p bin
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BINARY_BUILDER) cmd/builder/main.go

# Build client
build-client:
	@echo "Building Portage Engine Client..."
	@mkdir -p bin
	$(GO) build $(GOFLAGS) -o $(BINARY_CLIENT) cmd/client/main.go

# Build the offline image-factory control binary
build-image-factory:
	@echo "Building Portage Engine Image Factory..."
	@mkdir -p bin
	$(GO) build $(GOFLAGS) -o $(BINARY_IMAGE_FACTORY) cmd/image-factory/main.go

# Build the deterministic desktop verification runner
build-desktop-runner:
	@echo "Building Portage Engine Desktop Runner..."
	@mkdir -p bin
	$(GO) build $(GOFLAGS) -o $(BINARY_DESKTOP_RUNNER) cmd/desktop-runner/main.go

# Build the one-shot PostgreSQL schema migration binary
build-migrate:
	@echo "Building Portage Engine Migration CLI..."
	@mkdir -p bin
	$(GO) build $(GOFLAGS) -ldflags "-X main.version=$(VERSION)" -o $(BINARY_MIGRATE) cmd/migrate/main.go

build-signer:
	@echo "Building isolated Portage signer..."
	@mkdir -p bin
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BINARY_SIGNER) cmd/signer/main.go

build-capacity-actuator:
	@echo "Building fenced capacity actuator..."
	@mkdir -p bin
	$(GO) build $(GOFLAGS) -o $(BINARY_CAPACITY_ACTUATOR) cmd/capacity-actuator/main.go

build-persistent-executor-template:
	image-factory/persistent-executor/run.sh

test-persistent-executor-gate:
	scripts/persistent-executor-gate.sh repo

build-distcc-gate:
	@echo "Building Distributed Build Alpha comparison gate..."
	@mkdir -p bin
	$(GO) build $(GOFLAGS) -o $(BINARY_DISTCC_GATE) cmd/distcc-gate/main.go

# Usage: make distcc-gate LOCAL_EVIDENCE=... DISTCC_EVIDENCE=... GATE_RECEIPT=...
distcc-gate:
	$(GO) run ./cmd/distcc-gate -local "$(LOCAL_EVIDENCE)" -distcc "$(DISTCC_EVIDENCE)" -output "$(GATE_RECEIPT)"

# Clean build artifacts
clean: web-clean
	@echo "Cleaning..."
	@rm -rf bin/
	@$(GO) clean

# Run tests
test:
	@echo "Running tests..."
	$(GO) test -v ./...
	$(MAKE) test-release
	$(MAKE) test-recovery

test-release:
	@echo "Running release manifest and workflow contract tests..."
	$(PYTHON) -m unittest -v tests.release_pipeline_test

test-recovery:
	@echo "Running recovery drill shell tests..."
	bash tests/recovery-drill-test.sh

# Run server
run-server:
	@echo "Starting Portage Engine Server..."
	$(GO) run cmd/server/main.go -config configs/server.conf

# Run dashboard
run-dashboard:
	@echo "Starting Portage Engine Dashboard..."
	$(GO) run cmd/dashboard/main.go -config configs/dashboard.conf

# Run builder
run-builder:
	@echo "Starting Portage Engine Builder..."
	$(GO) run cmd/builder/main.go -config configs/builder.conf

# Run client example
run-client:
	@echo "Running client example..."
	$(GO) run cmd/client/main.go build -package dev-lang/python -version 3.11

# Download dependencies
deps:
	@echo "Downloading dependencies..."
	$(GO) mod download
	$(GO) mod tidy

# Format code
fmt:
	@echo "Formatting code..."
	$(GO) fmt ./...

# Lint code
lint:
	@echo "Linting code..."
	$(GOLANGCI_LINT) run --timeout=10m ./...

lint-security:
	@echo "Running gosec security checks..."
	$(GOLANGCI_LINT) run --enable-only=gosec --timeout=10m ./...

lint-complexity:
	@echo "Checking function complexity..."
	$(GOLANGCI_LINT) run --enable-only=cyclop,gocyclo --timeout=10m ./...

# Install binaries
install: build
	@echo "Installing binaries..."
	@mkdir -p /usr/local/bin
	@cp $(BINARY_SERVER) /usr/local/bin/
	@cp $(BINARY_DASHBOARD) /usr/local/bin/
	@cp $(BINARY_BUILDER) /usr/local/bin/
	@cp $(BINARY_CLIENT) /usr/local/bin/
	@cp $(BINARY_MIGRATE) /usr/local/bin/
	@cp $(BINARY_CAPACITY_ACTUATOR) /usr/local/bin/
	@echo "Installation complete"

# Build for multiple architectures
build-all: build-linux-amd64 build-linux-arm64 build-darwin-amd64 build-darwin-arm64

build-linux-amd64:
	GOOS=linux GOARCH=amd64 $(GO) build -o bin/portage-server-linux-amd64 cmd/server/main.go
	GOOS=linux GOARCH=amd64 $(GO) build -o bin/portage-dashboard-linux-amd64 cmd/dashboard/main.go
	GOOS=linux GOARCH=amd64 $(GO) build -o bin/portage-builder-linux-amd64 cmd/builder/main.go
	GOOS=linux GOARCH=amd64 $(GO) build -o bin/portage-client-linux-amd64 cmd/client/main.go

build-linux-arm64:
	GOOS=linux GOARCH=arm64 $(GO) build -o bin/portage-server-linux-arm64 cmd/server/main.go
	GOOS=linux GOARCH=arm64 $(GO) build -o bin/portage-dashboard-linux-arm64 cmd/dashboard/main.go
	GOOS=linux GOARCH=arm64 $(GO) build -o bin/portage-builder-linux-arm64 cmd/builder/main.go
	GOOS=linux GOARCH=arm64 $(GO) build -o bin/portage-client-linux-arm64 cmd/client/main.go

build-darwin-amd64:
	GOOS=darwin GOARCH=amd64 $(GO) build -o bin/portage-server-darwin-amd64 cmd/server/main.go
	GOOS=darwin GOARCH=amd64 $(GO) build -o bin/portage-dashboard-darwin-amd64 cmd/dashboard/main.go

build-darwin-arm64:
	GOOS=darwin GOARCH=arm64 $(GO) build -o bin/portage-server-darwin-arm64 cmd/server/main.go
	GOOS=darwin GOARCH=arm64 $(GO) build -o bin/portage-dashboard-darwin-arm64 cmd/dashboard/main.go
