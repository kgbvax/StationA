#!/usr/bin/env bash
#
# Deploy hf_console as a web app on shari.
#
# This builds the Flutter web app and a tiny Go HTTP/WebSocket bridge, then
# installs them on the Raspberry Pi as a hardened systemd service.
#
# The browser cannot open raw TCP sockets, so the web build connects to the
# Go bridge via WebSocket (/mqtt); the bridge forwards bytes to the station
# MQTT broker at 192.168.1.50:1883.
#
# Usage:
#   ./deploy.sh                       # deploy to default host 192.168.1.139
#   SSH_HOST=pi@shari.local ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST        SSH target            (default: 192.168.1.139)
#   SSH_USER        SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME    systemd service name  (default: hf-console-web)
#   SERVICE_USER    system user to run as (default: hfconsoleweb)
#   INSTALL_DIR     remote install dir    (default: /opt/hf-console-web)
#   HTTP_PORT       HTTP/WebSocket port   (default: 8091)
#   MQTT_BROKER     MQTT broker TCP addr  (default: 192.168.1.50:1883)
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-hf-console-web}"
SERVICE_USER="${SERVICE_USER:-hfconsoleweb}"
INSTALL_DIR="${INSTALL_DIR:-/opt/hf-console-web}"
HTTP_PORT="${HTTP_PORT:-8091}"
MQTT_BROKER="${MQTT_BROKER:-192.168.1.50:1883}"

# Allow "user@host" in SSH_HOST; otherwise prepend SSH_USER.
if [[ "$SSH_HOST" == *"@"* ]]; then
  SSH_TARGET="$SSH_HOST"
else
  SSH_TARGET="${SSH_USER}@${SSH_HOST}"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# --- build Flutter web ------------------------------------------------------
echo ">> Running prebuild gate..."
tool/prebuild.sh

echo ">> Building Flutter web..."
flutter build web --release --base-href /

# --- build Go bridge --------------------------------------------------------
echo ">> Building WebSocket bridge for linux/arm64..."
WEBBRIDGE_OUT="dist/hf-console-web-linux-arm64"
mkdir -p dist
(cd webbridge && GOWORK=off GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "../$WEBBRIDGE_OUT" .)
echo "   built $WEBBRIDGE_OUT"

# --- generate systemd unit --------------------------------------------------
UNIT_FILE="$(mktemp)"
cat > "$UNIT_FILE" <<EOF
[Unit]
Description=Mühle HF station console web channel (hf_console)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/hf-console-web -listen 0.0.0.0:${HTTP_PORT} -mqtt-broker ${MQTT_BROKER} -web-root ${INSTALL_DIR}/build/web
Restart=on-failure
RestartSec=5

# Run as a dedicated unprivileged user (no login, no home).
User=${SERVICE_USER}
Group=${SERVICE_USER}

# Hardening. The service only needs inbound TCP for HTTP/WebSocket and outbound
# TCP to the MQTT broker. It needs no disk writes, no serial, no capabilities.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
RestrictRealtime=true
RestrictSUIDSGID=true
RemoveIPC=true
CapabilityBoundingSet=
AmbientCapabilities=
# Resource ceilings for a static web + proxy service.
MemoryMax=64M
TasksMax=32
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

[Install]
WantedBy=multi-user.target
EOF

# --- copy artifacts to shari ------------------------------------------------
echo ">> Copying files to ${SSH_TARGET}..."
scp "$WEBBRIDGE_OUT" "${SSH_TARGET}:/tmp/hf-console-web.new"
scp "$UNIT_FILE" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.service"
# Prefer rsync for the static tree; fall back to scp -r if rsync is missing.
if command -v rsync >/dev/null 2>&1; then
  rsync -avz --delete build/web/ "${SSH_TARGET}:/tmp/hf-console-web-static/"
else
  ssh "$SSH_TARGET" "rm -rf /tmp/hf-console-web-static && mkdir -p /tmp/hf-console-web-static"
  scp -r build/web/* "${SSH_TARGET}:/tmp/hf-console-web-static/"
fi

# --- install remotely -------------------------------------------------------
echo ">> Installing on ${SSH_TARGET}..."
ssh "$SSH_TARGET" "INSTALL_DIR='${INSTALL_DIR}' SERVICE_NAME='${SERVICE_NAME}' SERVICE_USER='${SERVICE_USER}' HTTP_PORT='${HTTP_PORT}' bash -s" <<'REMOTE'
set -euo pipefail
# Create a dedicated system user/group if missing.
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi

sudo mkdir -p "$INSTALL_DIR"

sudo systemctl stop "${SERVICE_NAME}.service" 2>/dev/null || true

sudo mv "/tmp/hf-console-web.new" "${INSTALL_DIR}/hf-console-web"
sudo chmod 755 "${INSTALL_DIR}/hf-console-web"

sudo rm -rf "${INSTALL_DIR}/build"
sudo mkdir -p "${INSTALL_DIR}/build"
sudo mv "/tmp/hf-console-web-static" "${INSTALL_DIR}/build/web"

sudo mv "/tmp/${SERVICE_NAME}.service" "/etc/systemd/system/${SERVICE_NAME}.service"
sudo systemctl daemon-reload
sudo systemctl enable "${SERVICE_NAME}.service"
sudo systemctl restart "${SERVICE_NAME}.service"

echo "--- service status ---"
sudo systemctl --no-pager --full status "${SERVICE_NAME}.service" || true
REMOTE

WEB_URL="http://${SSH_HOST##*@}:${HTTP_PORT}/"
echo ""
echo ">> Done. hf_console web deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Web UI: ${WEB_URL}"
echo "   Logs:   ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
