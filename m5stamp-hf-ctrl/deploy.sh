#!/usr/bin/env bash
# deploy.sh — M5 Stamp PLC #1 firmware build + flash helper.
#
# The firmware is NOT deployed to shari as a systemd service; it is flashed to
# the ESP32-S3 over USB or OTA. This script wraps PlatformIO and enforces the
# OTA password flow.
#
# First flash (or recovery): must be USB because the running firmware has no
# OTA listener yet.
#   ./deploy.sh usb /dev/ttyUSB0
#   ./deploy.sh usb                      # auto-detect USB serial port
#
# Subsequent updates over the network:
#   ./deploy.sh ota m5stamp-plc-1.local
#   ./deploy.sh ota 192.168.1.123
#
set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_USB="m5stamp-plc1"
ENV_OTA="m5stamp-plc1-ota"

usage() {
  cat <<'EOF'
Usage: ./deploy.sh [usb [port]|ota [host]]

  usb [port]   Build and upload via USB/serial. If port is omitted, the script
               tries to auto-detect /dev/ttyUSB* /dev/cu.usbserial-*.

  ota [host]   Build and upload over-the-air. If host is omitted, uses
               m5stamp-plc-1.local (the ArduinoOTA hostname set in main.cpp).

  --help       Show this help.
EOF
}

find_usb_port() {
  local port=""
  if [[ "$OSTYPE" == darwin* ]]; then
    port=$(ls /dev/cu.usbserial-* /dev/cu.SLAB_USBtoUART /dev/cu.wchusbserial* 2>/dev/null | head -n1 || true)
  else
    port=$(ls /dev/ttyUSB* /dev/ttyACM* 2>/dev/null | head -n1 || true)
  fi
  if [[ -z "$port" ]]; then
    echo "No USB serial port found. Plug in the M5 Stamp PLC and try again." >&2
    exit 1
  fi
  echo "$port"
}

case "${1:-}" in
  --help|-h|"")
    usage
    exit 0
    ;;
  usb)
    PORT="${2:-$(find_usb_port)}"
    echo "==> Building and flashing via USB on $PORT ..."
    cd "$PROJECT_DIR"
    pio run -e "$ENV_USB" -t upload --upload-port "$PORT"
    ;;
  ota)
    HOST="${2:-m5stamp-plc-1.local}"
    echo "==> Building and flashing via OTA to $HOST ..."
    cd "$PROJECT_DIR"
    pio run -e "$ENV_OTA" -t upload --upload-port "$HOST"
    ;;
  *)
    echo "Unknown command: $1" >&2
    usage >&2
    exit 1
    ;;
esac

echo "==> Done."
