.PHONY: help build run test test-bench lint clean deps

# Default target: show help
help:
	@echo "Available targets:"
	@echo "  make build       - Build the worker binary"
	@echo "  make run         - Run the worker locally"
	@echo "  make test        - Run tests"
	@echo "  make test-bench  - Run tests with benchmarks"
	@echo "  make lint        - Run linter"
	@echo "  make clean       - Clean build artifacts"
	@echo "  make deps        - Install dependencies"

# Build the worker
build:
	go build -o bin/coralie-livekit-worker ./cmd/coralie-livekit-worker

# Run the worker locally (builds first to avoid go run linker issues on macOS)
run: build
	@echo "Starting worker (ensure .env is configured)..."
	@./bin/coralie-livekit-worker || (echo "Worker exited. Check configuration in .env file." && exit 1)

# Run tests
test:
	go test ./...

# Run tests with benchmarks
test-bench:
	go test -run Test... -bench . ./...

# Run linter
lint:
	golangci-lint run

# Clean build artifacts
clean:
	rm -rf bin/

# Install dependencies
deps:
	go mod download
	go mod tidy
