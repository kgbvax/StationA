BINARY := testui
PKG := ./cmd/testui
VERSION := $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
LDFLAGS := -s -w
BIN_DIR := bin

.PHONY: all build run vet test test-race fmt clean

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(PKG)

run: build
	./$(BIN_DIR)/$(BINARY) -config config.toml

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