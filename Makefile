.PHONY: all build clean test run-server run-dashboard run-builder run-client build-image-factory build-desktop-runner build-migrate build-signer

# Variables
BINARY_SERVER=bin/portage-server
BINARY_DASHBOARD=bin/portage-dashboard
BINARY_BUILDER=bin/portage-builder
BINARY_CLIENT=bin/portage-client
BINARY_IMAGE_FACTORY=bin/portage-image-factory
BINARY_DESKTOP_RUNNER=bin/portage-desktop-runner
BINARY_MIGRATE=bin/portage-migrate
BINARY_SIGNER=bin/portage-signer
GO=go
GOFLAGS=-v
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS=-ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)"

all: build

# Build all binaries
build: build-server build-dashboard build-builder build-client build-image-factory build-desktop-runner build-migrate build-signer

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

# Clean build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf bin/
	@$(GO) clean

# Run tests
test:
	@echo "Running tests..."
	$(GO) test -v ./...

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
	golangci-lint run ./...

# Install binaries
install: build
	@echo "Installing binaries..."
	@mkdir -p /usr/local/bin
	@cp $(BINARY_SERVER) /usr/local/bin/
	@cp $(BINARY_DASHBOARD) /usr/local/bin/
	@cp $(BINARY_BUILDER) /usr/local/bin/
	@cp $(BINARY_CLIENT) /usr/local/bin/
	@cp $(BINARY_MIGRATE) /usr/local/bin/
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
