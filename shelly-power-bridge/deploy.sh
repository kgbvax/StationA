#!/usr/bin/env bash
#
# Deploy shelly-power-bridge (binary `shelly-power-bridge`) to a Raspberry Pi and
# install it as a hardened systemd service.
#
# shelly-power-bridge fronts one or more Shelly Gen2+ smart plugs that speak
# MQTT natively, translating them into site-level station integration-model
# `power` slots (muehle/power/<slot>/{meta,state,status,cmd}). It is a compound
# bridge: one process fronts N Shelies, each [[slot]] running its own paho
# client with its own LWT. It subscribes to the Shelly native
# "<id>/status/switch:0" + "<id>/online" topics and the canonical /cmd, and
# commands the Shelly over "<id>/rpc" (Gen2+ RPC-over-MQTT). It is network-only
# (no serial devices), so the unit runs under the strictest sandbox
# (PrivateDevices=true) with only AF_INET/AF_INET6 for the outbound MQTT.
#
# Usage:
#   ./deploy.sh                       # deploy to default host "shari"
#   SSH_HOST=pi@shari.local ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST        SSH target            (default: 192.168.1.139)
#   SSH_USER        SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME    systemd service name  (default: shelly-power-bridge)
#   SERVICE_USER    system user to run as (default: shelly-power-bridge)
#   INSTALL_DIR     remote install dir    (default: /opt/shelly-power-bridge)
#   BINARY          binary name           (default: shelly-power-bridge)
#
#   HOST_NAME       host value            (default: shari)     [published in /meta]
#   LOG_LEVEL       log.level value       (default: info)
#   MQTT_BROKER     mqtt.broker value     (default: tcp://192.168.1.50:1883)
#   MQTT_SITE       mqtt.site             (default: muehle)
#   MQTT_USER       mqtt.user             (default: hf)
#   MQTT_PASSWORD   SHELLY_POWER_BRIDGE_MQTT_PASSWORD (default: empty -> set on device)
#
#   MASTER_SHELLY_ID   [[slot]] shelly_id for the master mains plug
#   PSU_SHELLY_ID     [[slot]] shelly_id for the 13.8 V PSU plug
#
# Per-slot [[slot]] blocks are seeded from the example config. The Shelly ids
# (MASTER_SHELLY_ID / PSU_SHELLY_ID) MUST be set to the real Gen2 device ids of
# the two plugs; deploy will refuse to seed a placeholder id. Other slot fields
# (station/slot/device_model/device_serial/feeds) keep their example values —
# edit the on-device config.toml after first deploy to correct them.
#
# Configuration lives in a 0600 TOML file on the target
# (/etc/shelly-power-bridge/config.toml); the MQTT password is NOT in the TOML —
# it is loaded from an EnvironmentFile
# (/etc/shelly-power-bridge/shelly-power-bridge.env, 0600) so it never appears in
# the unit file or process command line. Both files are SEEDED ONCE on first
# deploy from the variables above; subsequent deploys leave the on-device files
# untouched so the Pi owns its own settings. To change a setting after the first
# deploy, edit the file on the device (or delete it and redeploy to re-seed).
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-shelly-power-bridge}"
SERVICE_USER="${SERVICE_USER:-shelly-power-bridge}"
INSTALL_DIR="${INSTALL_DIR:-/opt/shelly-power-bridge}"
CONFIG_DIR="${CONFIG_DIR:-/etc/shelly-power-bridge}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
ENV_FILE="${ENV_FILE:-${CONFIG_DIR}/shelly-power-bridge.env}"
BINARY="${BINARY:-shelly-power-bridge}"
PKG="./cmd/shelly-power-bridge"

HOST_NAME="${HOST_NAME:-shari}"
LOG_LEVEL="${LOG_LEVEL:-info}"
MQTT_BROKER="${MQTT_BROKER:-tcp://192.168.1.50:1883}"
MQTT_SITE="${MQTT_SITE:-muehle}"
MQTT_USER="${MQTT_USER:-hf}"
MQTT_PASSWORD="${MQTT_PASSWORD:-}"

# Shelly Gen2+ device ids (MQTT prefixes). MUST be set to the real plug ids
# before first deploy; a placeholder is refused.
MASTER_SHELLY_ID="${MASTER_SHELLY_ID:-shellyplus1pm-aabbccddeeff}"
PSU_SHELLY_ID="${PSU_SHELLY_ID:-shellyplus1pm-112233445566}"

# Allow "user@host" in SSH_HOST; otherwise prepend SSH_USER.
if [[ "$SSH_HOST" == *"@"* ]]; then
  SSH_TARGET="$SSH_HOST"
else
  SSH_TARGET="${SSH_USER}@${SSH_HOST}"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# --- TOML/env escaping helper ------------------------------------------------
# Escape backslash and double quotes for a TOML basic string or a systemd
# double-quoted EnvironmentFile value (same escaping rules).
toml_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '%s' "$s"
}

# --- generate the seed config file (used only if none exists on target) -----
SEED_CONFIG="$(umask 077; mktemp)"
# Seed the EnvironmentFile too (password only). Separate temp so the secret
# never rides alongside the non-secret TOML if someone inspects the transfer.
SEED_ENV="$(umask 077; mktemp)"
trap 'rm -f "$SEED_CONFIG" "$SEED_ENV" "${UNIT_FILE:-}"' EXIT
{
  echo "# shelly-power-bridge configuration. Sensitive values are NOT here — the MQTT"
  echo "# password lives in the EnvironmentFile (shelly-power-bridge.env). Keep both 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo ""
  echo "host = \"$(toml_escape "$HOST_NAME")\""
  echo ""
  echo "[mqtt]"
  echo "broker           = \"$(toml_escape "$MQTT_BROKER")\""
  echo '# client_id defaults to "<site>-<station>-<slot>" PER slot when empty.'
  echo "site             = \"$(toml_escape "$MQTT_SITE")\""
  echo "# Home Assistant discovery is rendered by the standalone hadiscovery consumer"
  echo "# from each slot's expose block in /meta (model §9) — no embedded discovery."
  echo "discovery_prefix = \"homeassistant\""
  echo "user             = \"$(toml_escape "$MQTT_USER")\""
  echo "# password is loaded from SHELLY_POWER_BRIDGE_MQTT_PASSWORD in the EnvironmentFile, not here."
  echo "password         = \"\""
  echo ""
  echo "[log]"
  echo "level = \"$(toml_escape "$LOG_LEVEL")\""
  echo ""
  echo "# --- station master mains ---"
  echo "[[slot]]"
  echo "station       = \"power\""
  echo "slot          = \"master\""
  echo "location      = \"bauwagen\""
  echo "device_model  = \"Shelly Plus 1PM\""
  echo "device_serial = \"$(toml_escape "$MASTER_SHELLY_ID")\""
  echo "shelly_id     = \"$(toml_escape "$MASTER_SHELLY_ID")\""
  echo "fail_safe     = \"off\""
  echo ""
  echo "# --- 13.8 V PSU (site-level DC rail; feeds both HF and UHF) ---"
  echo "[[slot]]"
  echo "station       = \"power\""
  echo "slot          = \"psu-13v8\""
  echo "location      = \"bauwagen\""
  echo "device_model  = \"Shelly Plus 1PM\""
  echo "device_serial = \"$(toml_escape "$PSU_SHELLY_ID")\""
  echo "shelly_id     = \"$(toml_escape "$PSU_SHELLY_ID")\""
  echo "fail_safe     = \"off\""
  echo "feeds = ["
  echo "  \"hf/radio\", \"uhf/radio\", \"hf/tuner\", \"hf/ant-ctrl\","
  echo "  \"hf/ant-switch\", \"hf/rotator\", \"hf/switch\", \"hf/pa-arm\","
  echo "]"
} > "$SEED_CONFIG"

{
  echo "# shelly-power-bridge EnvironmentFile (read by the systemd unit). Keep 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change the password."
  if [[ -n "$MQTT_PASSWORD" ]]; then
    echo "SHELLY_POWER_BRIDGE_MQTT_PASSWORD=\"$(toml_escape "$MQTT_PASSWORD")\""
  else
    echo "# SHELLY_POWER_BRIDGE_MQTT_PASSWORD=\"...\"   # set on the device (copy from another hf service config)"
  fi
} > "$SEED_ENV"

# --- build for the Pi (Linux arm64) -----------------------------------------
echo ">> Building ${BINARY} for linux/arm64..."
OUT="dist/${BINARY}-linux-arm64"
mkdir -p dist
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$OUT" "$PKG"
echo "   built $OUT"

# --- generate the systemd unit ---------------------------------------------
UNIT_FILE="$(mktemp)"
cat > "$UNIT_FILE" <<EOF
[Unit]
Description=Shelly Gen2+ smart-plug to MQTT bridge (shelly-power-bridge)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# All non-secret settings come from the config file; the MQTT password comes
# from the EnvironmentFile. No secrets appear on the command line / in the unit.
ExecStart=${INSTALL_DIR}/${BINARY} -config ${CONFIG_FILE}
EnvironmentFile=${ENV_FILE}
Restart=on-failure
RestartSec=5
# Run as a dedicated unprivileged user.
User=${SERVICE_USER}
Group=${SERVICE_USER}
# systemd owns /etc/shelly-power-bridge (created 0755, owned by the service user).
ConfigurationDirectory=${SERVICE_NAME}
StateDirectory=${SERVICE_NAME}

# Hardening. shelly-power-bridge is network-only: it talks to the MQTT broker
# (which the Shelies are also clients of). No serial devices, no disk writes,
# no elevated capabilities. PrivateDevices is therefore safe (unlike the serial
# bridges), and RestrictAddressFamilies covers the outbound MQTT connection.
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
ReadWritePaths=/var/lib/${SERVICE_NAME}
# Resource ceilings. shari is the single shared host running every station
# service; a leaky bridge with no limit could OOM the whole Pi and take the
# entire station down.
MemoryMax=256M
TasksMax=64
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

[Install]
WantedBy=multi-user.target
EOF

# --- copy artifacts to the Pi ----------------------------------------------
echo ">> Copying files to ${SSH_TARGET}..."
scp "$OUT" "${SSH_TARGET}:/tmp/${BINARY}.new"
scp "$UNIT_FILE" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.service"
# Transfer the seed files to restrictive temp paths; the remote installs them
# only if neither exists yet, then removes the temp copies.
scp "$SEED_CONFIG" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.config.seed"
scp "$SEED_ENV" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.env.seed"

# --- install remotely -------------------------------------------------------
echo ">> Installing on ${SSH_TARGET}..."
ssh "$SSH_TARGET" "INSTALL_DIR='${INSTALL_DIR}' BINARY='${BINARY}' SERVICE_NAME='${SERVICE_NAME}' SERVICE_USER='${SERVICE_USER}' CONFIG_DIR='${CONFIG_DIR}' CONFIG_FILE='${CONFIG_FILE}' ENV_FILE='${ENV_FILE}' bash -s" <<'REMOTE'
set -euo pipefail
SEED_CFG="/tmp/${SERVICE_NAME}.config.seed"
SEED_ENV="/tmp/${SERVICE_NAME}.env.seed"
# Always remove the transferred seeds (the env one carries the secret) when done.
trap 'rm -f "$SEED_CFG" "$SEED_ENV"' EXIT
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
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$SEED_CFG" "$CONFIG_FILE"
  echo "   seeded config at $CONFIG_FILE (0600, owner $SERVICE_USER)."
  echo "   !! Verify the [[slot]] shelly_id values match your physical Sh plugs before relying on the bridge."
fi
# Seed the EnvironmentFile ONCE too (holds the MQTT password).
if [ -e "$ENV_FILE" ]; then
  echo "   env file exists at $ENV_FILE -- leaving it untouched (seed-once)."
else
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$SEED_ENV" "$ENV_FILE"
  echo "   seeded env file at $ENV_FILE (0600, owner $SERVICE_USER)."
  echo "   !! Set SHELLY_POWER_BRIDGE_MQTT_PASSWORD in $ENV_FILE before relying on the bridge."
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
echo ">> Done. shelly-power-bridge deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Logs:    ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Config:  ${CONFIG_FILE}"
echo "   Secret:  ${ENV_FILE}  (set SHELLY_POWER_BRIDGE_MQTT_PASSWORD if not seeded)"
echo "   Topics:  muehle/power/{master,psu-13v8}/{meta,state,status,cmd}"
echo "   !! Verify each [[slot]] shelly_id in ${CONFIG_FILE} matches the real Shelly device id."