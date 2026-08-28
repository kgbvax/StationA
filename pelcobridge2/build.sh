#!/bin/sh
# Cross-compile pelcobridge2. Default target is the Windows shack PC;
# TARGETS=all adds linux and darwin.
set -eu
cd "$(dirname "$0")"

TARGETS="${TARGETS:-windows}"
OUT=dist
mkdir -p "$OUT"

LDFLAGS="-s -w"

build() {
    goos="$1"; goarch="$2"; ext="${3:-}"
    # The port enumerator (go.bug.st/serial/enumerator) needs cgo on darwin
    # (IOKit); windows and linux are pure syscall and build static.
    cgo=0
    [ "$goos" = darwin ] && cgo=1
    name="pelcobridge2-${goos}-${goarch}${ext}"
    echo "building $name"
    CGO_ENABLED=$cgo GOOS="$goos" GOARCH="$goarch" \
        go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/$name" ./cmd/pelcobridge2
    CGO_ENABLED=$cgo GOOS="$goos" GOARCH="$goarch" \
        go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/pelcobridge2-mock-${goos}-${goarch}${ext}" ./cmd/pelcobridge2-mock
}

case "$TARGETS" in
all)
    build windows amd64 .exe
    build linux amd64
    build linux arm64
    build darwin amd64
    build darwin arm64
    ;;
windows)
    build windows amd64 .exe
    build linux amd64
    build darwin arm64
    ;;
*)
    echo "usage: TARGETS=all|windows $0" >&2
    exit 2
    ;;
esac

ls -la "$OUT"