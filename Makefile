SHELL := /bin/bash
BINARY := mcp-forj
CMD := ./cmd/mcp-forj
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build test test-race test-e2e cover vet fmt fmt-check tidy lint check run clean

all: fmt vet test build

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/$(BINARY) $(CMD)

test:
	go test ./...

test-race:
	go test -race ./...

# End-to-end tests behind the "e2e" build tag. The gitproxy ones need the git
# binary; they skip when it is missing or when run with -short.
test-e2e:
	go test -tags e2e ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

vet:
	go vet ./...

fmt:
	gofmt -s -w .

# Fails when gofmt would change any file; reads nothing, writes nothing.
fmt-check:
	@out=$$(gofmt -s -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

tidy:
	go mod tidy

lint:
	golangci-lint run

# check is what a pull request must pass (writes nothing).
check: fmt-check vet test

# Run the server against a config (defaults to configs/config.yaml).
run:
	go run $(CMD) -config $${CONFIG:-configs/config.yaml}

clean:
	rm -rf bin coverage.out
