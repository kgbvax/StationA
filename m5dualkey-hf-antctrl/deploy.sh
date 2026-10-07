#!/usr/bin/env bash
# deploy.sh — M5Stack Chain DualKey (m5dualkey-hf-antctrl) build + flash helper.
#
# Flashed to the DualKey over USB or OTA; not a systemd service. Wireless OTA
# is in the image, so only the FIRST flash (or recovery) needs USB. With no
# arguments this script defaults to OTA.
#
#   ./deploy.sh                    # OTA to m5dualkey-antctrl-1.local (routine)
#   ./deploy.sh ota                # same, explicit
#   ./deploy.sh ota 192.168.1.123  # OTA to an explicit host
#
#   ./deploy.sh usb                # first flash / recovery, auto-detect port
#   ./deploy.sh usb /dev/cu.usbmodemXXXX
#
set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_USB="m5stack_chain_dualkey"
ENV_OTA="m5stack_chain_dualkey-ota"
OTA_DEFAULT_HOST="m5dualkey-antctrl-1.local"
SECRETS="include/secrets.h"

usage() {
  cat <<USAGE
Usage: ./deploy.sh [ota [host]|usb [port]]

  ota [host]   Build and upload over the air (default). Host defaults to
               $OTA_DEFAULT_HOST (OTA_HOSTNAME in include/config.h).

  usb [port]   Build and upload over USB — first flash or recovery. Without
               a port, uses the first /dev/cu.usbmodem* (the S3's native
               USB) on macOS, /dev/ttyACM* on Linux. If no port shows up,
               hold Key A (GPIO0) while plugging in to force download mode.
USAGE
}

find_usb_port() {
  local port=""
  if [[ "$OSTYPE" == darwin* ]]; then
    port=$(ls /dev/cu.usbmodem* 2>/dev/null | head -n1 || true)
  else
    port=$(ls /dev/ttyACM* 2>/dev/null | head -n1 || true)
  fi
  if [[ -z "$port" ]]; then
    echo "No USB port found. Plug in the DualKey (data cable); if it still does not" >&2
    echo "appear, hold Key A while plugging in to enter download mode." >&2
    exit 1
  fi
  echo "$port"
}

cd "$PROJECT_DIR"
case "${1:-ota}" in
  --help|-h)
    usage
    ;;
  usb)
    PORT="${2:-$(find_usb_port)}"
    echo "==> Building and flashing via USB on $PORT ..."
    pio run -e "$ENV_USB" -t upload --upload-port "$PORT"
    ;;
  ota)
    HOST="${2:-$OTA_DEFAULT_HOST}"
    if ! grep -q '#define SECRET_OTA_PASSWORD' "$SECRETS"; then
      echo "$SECRETS has no SECRET_OTA_PASSWORD — cannot flash OTA." >&2
      exit 1
    fi
    # Never echoed, never on a command line; platformio.ini reads it via
    # ${sysenv.DUALKEY_OTA_PASSWORD}.
    export DUALKEY_OTA_PASSWORD
    DUALKEY_OTA_PASSWORD=$(grep -m1 '#define SECRET_OTA_PASSWORD' "$SECRETS" | sed 's/.*"\(.*\)".*/\1/')
    echo "==> Building and flashing via OTA to $HOST ..."
    pio run -e "$ENV_OTA" -t upload --upload-port "$HOST"
    ;;
  *)
    echo "Unknown command: $1" >&2
    usage >&2
    exit 1
    ;;
esac
echo "==> Done."
