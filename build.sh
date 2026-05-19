#!/usr/bin/env bash
set -euo pipefail

BINARY="ubctrl"
PKG="./cmd/ubctrl"
OUT="dist"

mkdir -p "$OUT"

echo "Building for Windows (amd64)..."
GOOS=windows GOARCH=amd64 go build -o "$OUT/${BINARY}-windows-amd64.exe" "$PKG"

echo "Building for Linux (arm64)..."
GOOS=linux GOARCH=arm64 go build -o "$OUT/${BINARY}-linux-arm64" "$PKG"

echo "Done. Binaries in $OUT/"
