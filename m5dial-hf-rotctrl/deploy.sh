#!/usr/bin/env bash
# deploy.sh — M5Stack Dial HF-rotator control head build + flash helper.
#
# The firmware is NOT deployed to shari as a systemd service; it is flashed to
# the M5Stack Dial over USB or OTA. This script wraps PlatformIO.
#
# Wireless OTA is built into the first image, so only the FIRST flash (or
# recovery) needs USB. With no arguments this script defaults to OTA.
#
#   ./deploy.sh                    # OTA to m5dial-rotctrl-1.local (routine)
#   ./deploy.sh ota                # same, explicit
#   ./deploy.sh ota 192.168.1.123  # OTA to an explicit host
#
#   ./deploy.sh usb                # first flash / recovery, auto-detect port
#   ./deploy.sh usb /dev/cu.usbserial-XXXX
#
#   ./deploy.sh sim                # desk-simulation build (no WiFi/MQTT/OTA)
#
set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_USB="m5dial-rotctrl1"
ENV_OTA="m5dial-rotctrl1-ota"
ENV_SIM="m5dial-rotctrl1-sim"
OTA_DEFAULT_HOST="m5dial-rotctrl-1.local"

usage() {
  cat <<EOF
Usage: ./deploy.sh [ota [host]|usb [port]|sim [port]]

  ota [host]   Build and upload over-the-air (the default when no argument
               is given). If host is omitted, uses $OTA_DEFAULT_HOST
               (the ArduinoOTA hostname set in src/main.cpp).

  usb [port]   Build and upload via USB/serial — first flash or recovery
               only. If port is omitted, the script auto-detects the port
               (/dev/cu.usbmodem* on macOS — the Dial's native S3 USB —
               plus the common /dev/cu.usbserial* /dev/cu.wchusbserial*
               adapters; /dev/ttyUSB* /dev/ttyACM* on Linux).

  sim [port]   Build and upload the desk-simulation image (SIM_MODE=1: the
               antenna is a local model — no WiFi, MQTT or OTA, so USB is
               the only way in AND the only way back to the live image).

  --help       Show this help.
EOF
}

find_usb_port() {
  local port=""
  if [[ "$OSTYPE" == darwin* ]]; then
    port=$(ls /dev/cu.usbserial-* /dev/cu.SLAB_USBtoUART /dev/cu.wchusbserial* /dev/cu.usbmodem* 2>/dev/null | head -n1 || true)
  else
    port=$(ls /dev/ttyUSB* /dev/ttyACM* 2>/dev/null | head -n1 || true)
  fi
  if [[ -z "$port" ]]; then
    echo "No USB serial port found. Plug in the M5Stack Dial and try again." >&2
    exit 1
  fi
  echo "$port"
}

case "${1:-ota}" in
  --help|-h)
    usage
    exit 0
    ;;
  usb)
    PORT="${2:-$(find_usb_port)}"
    echo "==> Building and flashing via USB on $PORT ..."
    cd "$PROJECT_DIR"
    pio run -e "$ENV_USB" -t upload --upload-port "$PORT"
    ;;
  sim)
    PORT="${2:-$(find_usb_port)}"
    echo "==> Building and flashing the SIM image via USB on $PORT ..."
    echo "    (no WiFi/MQTT/OTA in this image; ./deploy.sh usb returns to live)"
    cd "$PROJECT_DIR"
    pio run -e "$ENV_SIM" -t upload --upload-port "$PORT"
    ;;
  ota)
    HOST="${2:-$OTA_DEFAULT_HOST}"
    echo "==> Building and flashing via OTA to $HOST ..."
    cd "$PROJECT_DIR"
    # The firmware demands an OTA password; espota must present it. Pull it
    # from src/secrets.h into the environment (never echoed, never on the
    # command line of this script) — platformio.ini picks it up via
    # ${sysenv.DIAL_OTA_PASSWORD}.
    if ! grep -q '#define OTA_PASSWORD' src/secrets.h; then
      echo "src/secrets.h has no OTA_PASSWORD — cannot flash OTA." >&2
      exit 1
    fi
    export DIAL_OTA_PASSWORD
    DIAL_OTA_PASSWORD=$(grep -m1 '#define OTA_PASSWORD' src/secrets.h \
      | sed 's/.*"\(.*\)".*/\1/')
    pio run -e "$ENV_OTA" -t upload --upload-port "$HOST"
    ;;
  *)
    echo "Unknown command: $1" >&2
    usage >&2
    exit 1
    ;;
esac

echo "==> Done."