.PHONY: all build test test-integration clean install fmt lint

# Binary name
BINARY=hookd
VERSION=0.1.0

# Build variables
BUILD_DIR=bin
MAIN_PATH=cmd/hookd/main.go

# Go variables
GOFLAGS=-ldflags="-s -w -X main.version=$(VERSION)"

all: test build

build:
	@echo "Building $(BINARY)..."
	@mkdir -p $(BUILD_DIR)
	go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY) $(MAIN_PATH)
	@echo "Build complete: $(BUILD_DIR)/$(BINARY)"

build-linux:
	@echo "Building for Linux AMD64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY)-linux-amd64 $(MAIN_PATH)
	@echo "Build complete: $(BUILD_DIR)/$(BINARY)-linux-amd64"

build-linux-arm64:
	@echo "Building for Linux ARM64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY)-linux-arm64 $(MAIN_PATH)
	@echo "Build complete: $(BUILD_DIR)/$(BINARY)-linux-arm64"

build-darwin:
	@echo "Building for macOS..."
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=amd64 go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY)-darwin-amd64 $(MAIN_PATH)
	@echo "Build complete: $(BUILD_DIR)/$(BINARY)-darwin-amd64"

build-all: build-linux build-linux-arm64 build-darwin
	@echo "All builds complete"

test:
	@echo "Running unit tests..."
	go test -v -cover ./internal/...

test-integration:
	@echo "Running integration tests..."
	go test -v -tags=integration ./test/...

test-all: test test-integration

coverage:
	@echo "Generating coverage report..."
	go test -coverprofile=coverage.out ./internal/...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

fmt:
	@echo "Formatting code..."
	go fmt ./...

lint:
	@echo "Running linter..."
	golangci-lint run

clean:
	@echo "Cleaning..."
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html
	@echo "Clean complete"

install: build
	@echo "Installing $(BINARY)..."
	sudo cp $(BUILD_DIR)/$(BINARY) /usr/local/bin/
	@echo "Installed to /usr/local/bin/$(BINARY)"

run: build
	@echo "Running $(BINARY)..."
	./$(BUILD_DIR)/$(BINARY) --config config.example.yaml

deps:
	@echo "Downloading dependencies..."
	go mod download
	go mod tidy

help:
	@echo "Available targets:"
	@echo "  make build              - Build binary for current platform"
	@echo "  make build-linux        - Build for Linux AMD64"
	@echo "  make build-linux-arm64  - Build for Linux ARM64"
	@echo "  make build-darwin       - Build for macOS"
	@echo "  make build-all          - Build for all platforms"
	@echo "  make test               - Run unit tests"
	@echo "  make test-integration   - Run integration tests"
	@echo "  make test-all           - Run all tests"
	@echo "  make coverage           - Generate coverage report"
	@echo "  make fmt                - Format code"
	@echo "  make lint               - Run linter"
	@echo "  make clean              - Remove build artifacts"
	@echo "  make install            - Install binary to /usr/local/bin"
	@echo "  make run                - Build and run with example config"
	@echo "  make deps               - Download dependencies"
