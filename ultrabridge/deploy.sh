#!/usr/bin/env bash
#
# Deploy ultrabridge to a Raspberry Pi and install it as a systemd service.
#
# Usage:
#   ./deploy.sh                       # deploy to default host "shari"
#   SSH_HOST=pi@shari.local ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST        SSH target            (default: 192.168.1.139)
#   SSH_USER        SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME    systemd service name  (default: ultrabridge)
#   SERVICE_USER    system user to run as (default: ultrabridge)
#   SERIAL_GROUP    group owning serial   (default: dialout)
#   SERIAL_USB_VENDOR USB vendor id for the udev serial-group rule (default: 0403=FTDI; empty=skip)
#   INSTALL_DIR     remote install dir    (default: /opt/ultrabridge)
#   BINARY          binary name           (default: ultrabridge)
#
#   HTTP_ADDR       http_addr  value        (default: 0.0.0.0:8080)
#   SERIAL_PORT     serial_port value        (default: empty -> mock)
#   BAUD            baud  value              (default: 19200)
#   LOCATION        location value           (default: bauwagen)  [published in /meta]
#   HOST_NAME       host value               (default: shari)     [published in /meta]
#   MQTT_BROKER     mqtt.broker value        (default: empty -> disabled)
#   MQTT_SITE       mqtt.site                (default: empty)
#   MQTT_STATION    mqtt.station             (default: empty)
#   MQTT_SLOT       mqtt.slot                (default: ant-ctrl, the canonical role)
#   MQTT_USER       mqtt.user                (default: empty)
#   MQTT_PASSWORD   mqtt.password            (default: empty)
#
# Configuration (including the MQTT password) lives in a single 0600 TOML file
# on the target, NOT in the systemd unit or process command line. The file is
# SEEDED ONCE on first deploy from the variables above; subsequent deploys leave
# the on-device file untouched so the Pi owns its own settings. To change a
# setting after the first deploy, edit the file on the device (or delete it and
# redeploy to re-seed).
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-ultrabridge}"
SERVICE_USER="${SERVICE_USER:-ultrabridge}"
SERIAL_GROUP="${SERIAL_GROUP:-dialout}"
# USB vendor id of the serial adapter. A udev rule forces matching tty devices
# into SERIAL_GROUP so the service user can always open them, regardless of the
# distro's default (e.g. Raspberry Pi OS uses dialout, some images use plugdev).
# Default 0403 = FTDI. Set empty to skip installing the udev rule.
SERIAL_USB_VENDOR="${SERIAL_USB_VENDOR:-0403}"
INSTALL_DIR="${INSTALL_DIR:-/opt/ultrabridge}"
CONFIG_DIR="${CONFIG_DIR:-/etc/ultrabridge}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
BINARY="${BINARY:-ultrabridge}"
PKG="./cmd/ultrabridge"

HTTP_ADDR="${HTTP_ADDR:-0.0.0.0:8080}"
SERIAL_PORT="${SERIAL_PORT:-}"
BAUD="${BAUD:-19200}"
LOCATION="${LOCATION:-bauwagen}"
HOST_NAME="${HOST_NAME:-shari}"
MQTT_BROKER="${MQTT_BROKER:-}"
MQTT_SITE="${MQTT_SITE:-}"
MQTT_STATION="${MQTT_STATION:-}"
MQTT_SLOT="${MQTT_SLOT:-ant-ctrl}"
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

# --- TOML escaping helper ---------------------------------------------------
# Escape backslashes and double quotes for a TOML basic string.
toml_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '%s' "$s"
}

# --- generate the seed config file (used only if none exists on target) -----
# Written with umask 077 so the local temp copy is never world-readable.
SEED_CONFIG="$(umask 077; mktemp)"
trap 'rm -f "$SEED_CONFIG" "${UNIT_FILE:-}"' EXIT
{
  echo "# ultrabridge configuration. Contains the MQTT password -- keep this file 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo "http_addr   = \"$(toml_escape "$HTTP_ADDR")\""
  echo "serial_port = \"$(toml_escape "$SERIAL_PORT")\""
  echo "baud        = ${BAUD}"
  echo "# Deployment identity, published in /meta (integration model §3)."
  echo "location    = \"$(toml_escape "$LOCATION")\""
  echo "host        = \"$(toml_escape "$HOST_NAME")\""
  echo ""
  echo "[mqtt]"
  echo "broker           = \"$(toml_escape "$MQTT_BROKER")\""
  echo '# client_id defaults to "<site>-<station>-<slot>" (model §8).'
  echo "site             = \"$(toml_escape "$MQTT_SITE")\""
  echo "station          = \"$(toml_escape "$MQTT_STATION")\""
  echo "slot             = \"$(toml_escape "$MQTT_SLOT")\""
  echo "discovery_prefix = \"homeassistant\""
  echo "# Legacy embedded HA discovery is OFF by default; the standalone hadiscovery"
  echo "# consumer renders discovery from this bridge's expose block in /meta (model §9)."
  echo "publish_ha_discovery = false"
  echo "user             = \"$(toml_escape "$MQTT_USER")\""
  echo "password         = \"$(toml_escape "$MQTT_PASSWORD")\""
} > "$SEED_CONFIG"

# --- build for the Pi (Linux arm64) ----------------------------------------
echo ">> Building ${BINARY} for linux/arm64..."
OUT="dist/${BINARY}-linux-arm64"
mkdir -p dist
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$OUT" "$PKG"
echo "   built $OUT"

# --- generate the systemd unit ---------------------------------------------
UNIT_FILE="$(mktemp)"
cat > "$UNIT_FILE" <<EOF
[Unit]
Description=UltraBeam antenna controller (ultrabridge)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# All settings (including the MQTT password) come from the config file, so no
# secrets appear on the command line / in the unit.
ExecStart=${INSTALL_DIR}/${BINARY} -config ${CONFIG_FILE}
Restart=on-failure
RestartSec=5
# Run as a dedicated unprivileged user...
User=${SERVICE_USER}
Group=${SERVICE_USER}
# ...that is a member of the serial group so it can open /dev/tty* devices.
SupplementaryGroups=${SERIAL_GROUP}
# systemd manages /etc/ultrabridge (created 0755, owned by the service user).
ConfigurationDirectory=${SERVICE_NAME}
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
# Transfer the seed config to a restrictive temp path; the remote installs it
# only if no config exists yet, then removes the temp copy.
scp "$SEED_CONFIG" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.config.seed"

# --- install remotely -------------------------------------------------------
echo ">> Installing on ${SSH_TARGET}..."
ssh "$SSH_TARGET" "INSTALL_DIR='${INSTALL_DIR}' BINARY='${BINARY}' SERVICE_NAME='${SERVICE_NAME}' SERVICE_USER='${SERVICE_USER}' SERIAL_GROUP='${SERIAL_GROUP}' SERIAL_USB_VENDOR='${SERIAL_USB_VENDOR}' CONFIG_DIR='${CONFIG_DIR}' CONFIG_FILE='${CONFIG_FILE}' bash -s" <<'REMOTE'
set -euo pipefail
SEED="/tmp/${SERVICE_NAME}.config.seed"
# Always remove the transferred seed (with its secret) when we're done.
trap 'rm -f "$SEED"' EXIT
# Create a dedicated system user/group (no login, no home) if missing.
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi
# Make sure the serial group exists and the service user is a member of it.
getent group "$SERIAL_GROUP" >/dev/null 2>&1 || sudo groupadd --system "$SERIAL_GROUP"
sudo usermod -aG "$SERIAL_GROUP" "$SERVICE_USER"
# Install a udev rule so the USB-serial adapter is owned by SERIAL_GROUP. Some
# distros assign FTDI tty devices to plugdev (not dialout) by default, which
# would deny the service user access; this pins the group to match.
if [ -n "$SERIAL_USB_VENDOR" ]; then
  printf 'SUBSYSTEM=="tty", SUBSYSTEMS=="usb", ATTRS{idVendor}=="%s", GROUP="%s", MODE="0660"\n' \
    "$SERIAL_USB_VENDOR" "$SERIAL_GROUP" | sudo tee /etc/udev/rules.d/99-ultrabridge-serial.rules >/dev/null
  sudo udevadm control --reload-rules
  sudo udevadm trigger --subsystem-match=tty
  echo "   installed udev rule: FTDI/vendor $SERIAL_USB_VENDOR tty -> group $SERIAL_GROUP."
fi
sudo mkdir -p "$INSTALL_DIR"
# Ensure the config directory exists (systemd also creates it via
# ConfigurationDirectory, but seed-once runs before the unit starts).
sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$CONFIG_DIR"
# Seed the config ONCE: install only if the device has no config yet.
if [ -e "$CONFIG_FILE" ]; then
  echo "   config exists at $CONFIG_FILE -- leaving it untouched (seed-once)."
else
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$SEED" "$CONFIG_FILE"
  echo "   seeded config at $CONFIG_FILE (0600, owner $SERVICE_USER)."
fi
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
echo ">> Done. ultrabridge deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Logs:    ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Web UI:  http://${SSH_HOST%%@*}:${HTTP_ADDR##*:}/"
