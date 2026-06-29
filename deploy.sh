#!/usr/bin/env bash
#
# Deploy ubctrl to a Raspberry Pi and install it as a systemd service.
#
# Usage:
#   ./deploy.sh                       # deploy to default host "shari"
#   SSH_HOST=pi@shari.local ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST        SSH target            (default: shari)
#   SSH_USER        SSH user              (default: pi)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME    systemd service name  (default: ubctrl)
#   SERVICE_USER    system user to run as (default: ubctrl)
#   SERIAL_GROUP    group owning serial   (default: dialout)
#   INSTALL_DIR     remote install dir    (default: /opt/ubctrl)
#   BINARY          binary name           (default: ubctrl)
#
#   HTTP_ADDR       -http  value          (default: 0.0.0.0:8080)
#   SERIAL_PORT     -port  value          (default: empty -> mock)
#   BAUD            -baud  value          (default: 19200)
#   MQTT_BROKER     -mqtt-broker value    (default: empty -> disabled)
#   MQTT_CLIENT_ID  -mqtt-client-id       (default: ubctrl)
#   MQTT_PREFIX     -mqtt-prefix          (default: ubctrl)
#   MQTT_USER       -mqtt-user            (default: empty)
#   MQTT_PASSWORD   -mqtt-password        (default: empty)
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-shari}"
SSH_USER="${SSH_USER:-pi}"
SERVICE_NAME="${SERVICE_NAME:-ubctrl}"
SERVICE_USER="${SERVICE_USER:-ubctrl}"
SERIAL_GROUP="${SERIAL_GROUP:-dialout}"
INSTALL_DIR="${INSTALL_DIR:-/opt/ubctrl}"
BINARY="${BINARY:-ubctrl}"
PKG="./cmd/ubctrl"

HTTP_ADDR="${HTTP_ADDR:-0.0.0.0:8080}"
SERIAL_PORT="${SERIAL_PORT:-}"
BAUD="${BAUD:-19200}"
MQTT_BROKER="${MQTT_BROKER:-}"
MQTT_CLIENT_ID="${MQTT_CLIENT_ID:-ubctrl}"
MQTT_PREFIX="${MQTT_PREFIX:-ubctrl}"
MQTT_USER="${MQTT_USER:-}"
MQTT_PASSWORD="${MQTT_PASSWORD:-}"

# Allow "user@host" in SSH_HOST; otherwise prepend SSH_USER.
if [[ "$SSH_HOST" == *"@"* ]]; then
  SSH_TARGET="$SSH_HOST"
else
  SSH_TARGET="${SSH_USER}@${SSH_HOST}"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# --- build the command-line flags for the service --------------------------
ARGS="-http ${HTTP_ADDR} -baud ${BAUD}"
[[ -n "$SERIAL_PORT"    ]] && ARGS="$ARGS -port ${SERIAL_PORT}"
[[ -n "$MQTT_BROKER"    ]] && ARGS="$ARGS -mqtt-broker ${MQTT_BROKER}"
[[ -n "$MQTT_CLIENT_ID" ]] && ARGS="$ARGS -mqtt-client-id ${MQTT_CLIENT_ID}"
[[ -n "$MQTT_PREFIX"    ]] && ARGS="$ARGS -mqtt-prefix ${MQTT_PREFIX}"
[[ -n "$MQTT_USER"      ]] && ARGS="$ARGS -mqtt-user ${MQTT_USER}"
[[ -n "$MQTT_PASSWORD"  ]] && ARGS="$ARGS -mqtt-password ${MQTT_PASSWORD}"

# --- build for the Pi (Linux arm64) ----------------------------------------
echo ">> Building ${BINARY} for linux/arm64..."
OUT="dist/${BINARY}-linux-arm64"
mkdir -p dist
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$OUT" "$PKG"
echo "   built $OUT"

# --- generate the systemd unit ---------------------------------------------
UNIT_FILE="$(mktemp)"
trap 'rm -f "$UNIT_FILE"' EXIT
cat > "$UNIT_FILE" <<EOF
[Unit]
Description=UltraBeam antenna controller (ubctrl)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/${BINARY} ${ARGS}
Restart=on-failure
RestartSec=5
# Run as a dedicated unprivileged user...
User=${SERVICE_USER}
Group=${SERVICE_USER}
# ...that is a member of the serial group so it can open /dev/tty* devices.
SupplementaryGroups=${SERIAL_GROUP}
# Hardening.
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
# Grant access to serial/USB-serial character devices under the sandbox.
DeviceAllow=char-ttyUSB rw
DeviceAllow=char-ttyACM rw
DeviceAllow=char-tty rw

[Install]
WantedBy=multi-user.target
EOF

# --- copy artifacts to the Pi ----------------------------------------------
echo ">> Copying files to ${SSH_TARGET}..."
scp "$OUT" "${SSH_TARGET}:/tmp/${BINARY}.new"
scp "$UNIT_FILE" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.service"

# --- install remotely -------------------------------------------------------
echo ">> Installing on ${SSH_TARGET}..."
ssh "$SSH_TARGET" "INSTALL_DIR='${INSTALL_DIR}' BINARY='${BINARY}' SERVICE_NAME='${SERVICE_NAME}' SERVICE_USER='${SERVICE_USER}' SERIAL_GROUP='${SERIAL_GROUP}' bash -s" <<'REMOTE'
set -euo pipefail
# Create a dedicated system user/group (no login, no home) if missing.
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi
# Make sure the serial group exists and the service user is a member of it.
getent group "$SERIAL_GROUP" >/dev/null 2>&1 || sudo groupadd --system "$SERIAL_GROUP"
sudo usermod -aG "$SERIAL_GROUP" "$SERVICE_USER"
sudo mkdir -p "$INSTALL_DIR"
sudo systemctl stop "${SERVICE_NAME}.service" 2>/dev/null || true
sudo mv "/tmp/${BINARY}.new" "${INSTALL_DIR}/${BINARY}"
sudo chmod 755 "${INSTALL_DIR}/${BINARY}"
sudo mv "/tmp/${SERVICE_NAME}.service" "/etc/systemd/system/${SERVICE_NAME}.service"
sudo systemctl daemon-reload
sudo systemctl enable "${SERVICE_NAME}.service"
sudo systemctl restart "${SERVICE_NAME}.service"
echo "--- service status ---"
sudo systemctl --no-pager --full status "${SERVICE_NAME}.service" || true
REMOTE

echo ""
echo ">> Done. ubctrl deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Logs:    ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Web UI:  http://${SSH_HOST%%@*}:${HTTP_ADDR##*:}/"
