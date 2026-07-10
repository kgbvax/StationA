#!/usr/bin/env bash
#
# Deploy hadiscovery to a Raspberry Pi and install it as a systemd service.
#
# hadiscovery is the central Home Assistant discovery consumer for the station bus
# (integration model §3.1 / §9). It is a passive consumer: it subscribes to slot /meta
# announcements, reads each slot's consumer-neutral `expose` block, and renders HA MQTT
# discovery. It owns no device, opens no serial port, serves no HTTP, and writes no /cmd.
# See docs/discovery-mqtt-api.md.
#
# Usage:
#   ./deploy.sh                       # deploy to default host "shari"
#   SSH_HOST=pi@shari.local ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST          SSH target            (default: 192.168.1.139)
#   SSH_USER          SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME      systemd service name  (default: hadiscovery)
#   SERVICE_USER      system user to run as (default: hadiscovery)
#   INSTALL_DIR       remote install dir    (default: /opt/hadiscovery)
#   BINARY            binary name           (default: hadiscovery)
#
#   MQTT_BROKER        mqtt.broker         value (default: tcp://192.168.1.50:1883)
#   MQTT_SITE          mqtt.site           value (default: muehle)
#   MQTT_STATION       mqtt.station        value (default: hf)
#   MQTT_SLOT          mqtt.slot           value (default: discovery)
#   MQTT_USER          mqtt.user           value (default: hf)
#   MQTT_PASSWORD      mqtt.password       value (default: empty -> set on device)
#   DISCOVERY_PREFIX   mqtt.discovery_prefix value (default: homeassistant)
#   LOCATION           location            value (default: bauwagen)  [published in /meta]
#   HOST_NAME          host                value (default: shari)     [published in /meta]
#   HA_AREA             area               value (default: Bauwagen)  [HA suggested_area]
#
# Configuration (including the MQTT password) lives in a single 0600 TOML file on the
# target, NOT in the systemd unit or process command line. The file is SEEDED ONCE on
# first deploy from the variables above; subsequent deploys leave the on-device file
# untouched so the Pi owns its own settings. To change a setting after the first deploy,
# edit the file on the device (or delete it and redeploy to re-seed).
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-hadiscovery}"
SERVICE_USER="${SERVICE_USER:-hadiscovery}"
INSTALL_DIR="${INSTALL_DIR:-/opt/hadiscovery}"
CONFIG_DIR="${CONFIG_DIR:-/etc/hadiscovery}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
BINARY="${BINARY:-hadiscovery}"
PKG="./cmd/hadiscovery"

MQTT_BROKER="${MQTT_BROKER:-tcp://192.168.1.50:1883}"
MQTT_SITE="${MQTT_SITE:-muehle}"
MQTT_STATION="${MQTT_STATION:-hf}"
MQTT_SLOT="${MQTT_SLOT:-discovery}"
MQTT_USER="${MQTT_USER:-hf}"
MQTT_PASSWORD="${MQTT_PASSWORD:-}"
DISCOVERY_PREFIX="${DISCOVERY_PREFIX:-homeassistant}"
LOCATION="${LOCATION:-bauwagen}"
HOST_NAME="${HOST_NAME:-shari}"
# HA area every discovered device is suggested into when its own expose.device.area is unset.
# Maps to HA's device-level `suggested_area`. Default "Bauwagen"; set to "" to suppress.
HA_AREA="${HA_AREA:-Bauwagen}"

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
  echo "# hadiscovery configuration. Contains the MQTT password -- keep this file 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo ""
  echo "# Deployment identity, published in /meta (integration model §3)."
  echo "# hadiscovery is a logic slot: role \"discovery\", link \"none\", no device."
  echo "location = \"$(toml_escape "$LOCATION")\""
  echo "host     = \"$(toml_escape "$HOST_NAME")\""
  echo "# HA area suggested for every discovered device whose expose.device.area is unset."
  echo "# Set to \"\" to emit no suggested_area. Default \"Bauwagen\"."
  echo "area     = \"$(toml_escape "$HA_AREA")\""
  echo ""
  echo "[mqtt]"
  echo "broker           = \"$(toml_escape "$MQTT_BROKER")\""
  echo '# client_id defaults to "<site>-<station>-<slot>" (model §8).'
  echo "site             = \"$(toml_escape "$MQTT_SITE")\""
  echo "station          = \"$(toml_escape "$MQTT_STATION")\""
  echo "slot             = \"$(toml_escape "$MQTT_SLOT")\""
  echo "user             = \"$(toml_escape "$MQTT_USER")\""
  echo "password         = \"$(toml_escape "$MQTT_PASSWORD")\""
  echo "# HA discovery tree root; HA's default is \"homeassistant\"."
  echo "discovery_prefix = \"$(toml_escape "$DISCOVERY_PREFIX")\""
  echo "# meta_filter defaults to \"<site>/+/+/meta\" (scoped to site) -- one consumer"
  echo "# discovers every slot under the site. Override only to narrow scope."
  echo "# meta_filter     = \"muehle/+/+/meta\""
} > "$SEED_CONFIG"

# --- build for the Pi (Linux arm64) ----------------------------------------
echo ">> Building ${BINARY} for linux/arm64..."
OUT="dist/${BINARY}-linux-arm64"
mkdir -p dist
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X hadiscovery/internal/ha.Version=${VERSION:-dev}" -o "$OUT" "$PKG"
echo "   built $OUT"

# --- generate the systemd unit ---------------------------------------------
UNIT_FILE="$(mktemp)"
cat > "$UNIT_FILE" <<EOF
[Unit]
Description=Home Assistant discovery consumer for the station bus (hadiscovery)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# All settings (including the MQTT password) come from the config file, so no
# secrets appear on the command line / in the unit.
ExecStart=${INSTALL_DIR}/${BINARY} -config ${CONFIG_FILE}
Restart=on-failure
RestartSec=5
# Run as a dedicated unprivileged user (no login, no home).
User=${SERVICE_USER}
Group=${SERVICE_USER}
# systemd manages /etc/hadiscovery (created 0755, owned by the service user).
ConfigurationDirectory=${SERVICE_NAME}
# Hardening. Passive MQTT consumer: no serial/USB devices, no HTTP server, no
# network listeners — only an outbound broker connection. No DeviceAllow /
# SupplementaryGroups / port binding needed.
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true

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
ssh "$SSH_TARGET" "INSTALL_DIR='${INSTALL_DIR}' BINARY='${BINARY}' SERVICE_NAME='${SERVICE_NAME}' SERVICE_USER='${SERVICE_USER}' CONFIG_DIR='${CONFIG_DIR}' CONFIG_FILE='${CONFIG_FILE}' bash -s" <<'REMOTE'
set -euo pipefail
SEED="/tmp/${SERVICE_NAME}.config.seed"
# Always remove the transferred seed (with its secret) when we're done.
trap 'rm -f "$SEED"' EXIT
# Create a dedicated system user/group (no login, no home) if missing.
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
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
  echo "   NOTE: set the MQTT password on the device:  sudo -e $CONFIG_FILE"
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
echo ">> Done. hadiscovery deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Logs:   ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Config: ssh ${SSH_TARGET} 'sudo -e ${CONFIG_FILE}'"