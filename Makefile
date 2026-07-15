# flexbridge Makefile. Cross-compiles for Raspberry Pi (arm64).
BINARY := flexbridge
PKG := ./cmd/flexbridge
VERSION := $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
LDFLAGS := -s -w
BIN_DIR := bin

.PHONY: all build run vet test test-race fmt clean pi install-tools

all: build

# Local dev build.
build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(PKG)

# Build and run with a config file.
run: build
	./$(BIN_DIR)/$(BINARY) -config config.example.toml

# Cross-compile for Raspberry Pi 3B+/4/5 (64-bit, arm64).
pi:
	mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
	  go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY)-linux-arm64 $(PKG)
	@echo "Built $(BIN_DIR)/$(BINARY)-linux-arm64"

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

fmt:
	gofmt -s -w .

clean:
	rm -rf $(BIN_DIR)

install-tools:
	@echo "No extra tools required (paho/toml pulled via go mod)."
