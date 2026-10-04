SHELL := /bin/bash
BINARY := mcp-forj
CMD := ./cmd/mcp-forj
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build test test-race cover vet fmt tidy lint run clean

all: fmt vet test build

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/$(BINARY) $(CMD)

test:
	go test ./...

test-race:
	go test -race ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

vet:
	go vet ./...

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

lint:
	golangci-lint run

# Run the server against a config (defaults to configs/config.yaml).
run:
	go run $(CMD) -config $${CONFIG:-configs/config.yaml}

clean:
	rm -rf bin coverage.out
