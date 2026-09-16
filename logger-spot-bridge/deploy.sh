#!/usr/bin/env bash
#
# Deploy logger-spot-bridge to shari (the Raspberry Pi) as a hardened systemd
# service — the standard stationa pattern (see docs/conventions/deployment.md).
#
# The bridge fronts the shack logging software (DXLog, Log4OM) as the canonical
# muehle/hf/spots slot. It LISTENS on UDP next to the loggers:
#   12060  N1MM-family lookupinfo (DXLog Options|Broadcast — subnet broadcast)
#   2249   Log4OM outbound CALLSIGN (point Log4OM at shari:2249)
# UDP broadcast is LAN-wide, so the bridge does NOT need to run on the logging
# PC; on shari it gets auto-start, restart supervision and the standard unit
# hardening like every other bridge. (It lived on the Windows shack PC as a
# schtasks logon task until 2026-09-16 — a deviation that cost an outage; the
# task is disabled there, files kept as fallback.)
#
# Config: /etc/logger-spot-bridge/config.toml (0600, seed-once)
# Secrets: /etc/logger-spot-bridge/logger-spot-bridge.env (0600, seed-once)
#   LOGGER_SPOT_BRIDGE_MQTT_PASSWORD
#   LOGGER_SPOT_BRIDGE_QRZ_PASSWORD   (only needed with [qrz] enabled)
# Both files are seeded on FIRST deploy from the variables below and never
# overwritten afterwards — the Pi owns its settings, updates are safe.
#
# Usage:
#   ./deploy.sh                                    # defaults, shari
#   SSH_HOST=pi@shari.local ./deploy.sh
#   MQTT_PASSWORD=... QRZ_PASSWORD=... ./deploy.sh # first-deploy secrets
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-logger-spot-bridge}"
SERVICE_USER="${SERVICE_USER:-logger-spot-bridge}"
INSTALL_DIR="${INSTALL_DIR:-/opt/logger-spot-bridge}"
CONFIG_DIR="${CONFIG_DIR:-/etc/logger-spot-bridge}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
ENV_FILE="${ENV_FILE:-${CONFIG_DIR}/logger-spot-bridge.env}"
BINARY="${BINARY:-logger-spot-bridge}"
PKG="./cmd/logger-spot-bridge"

HOST_NAME="${HOST_NAME:-shack-pc}"   # host identity published in /meta
STATION_LOCATOR="${STATION_LOCATOR:-JO32WE}"
N1MM_ENABLED="${N1MM_ENABLED:-true}"
N1MM_PORT="${N1MM_PORT:-12060}"
LOG4OM_ENABLED="${LOG4OM_ENABLED:-true}"
LOG4OM_PORT="${LOG4OM_PORT:-2249}"
STALE_AFTER="${STALE_AFTER:-10m}"
QRZ_ENABLED="${QRZ_ENABLED:-true}"
QRZ_USERNAME="${QRZ_USERNAME:-dl9et}"
STALE_LOG_LEVEL="${LOG_LEVEL:-info}"
MQTT_BROKER="${MQTT_BROKER:-tcp://192.168.1.50:1883}"
MQTT_SITE="${MQTT_SITE:-muehle}"
MQTT_STATION="${MQTT_STATION:-hf}"
MQTT_SLOT="${MQTT_SLOT:-spots}"
MQTT_USER="${MQTT_USER:-hf}"
MQTT_PASSWORD="${MQTT_PASSWORD:-}"
QRZ_PASSWORD="${QRZ_PASSWORD:-}"

# Allow "user@host" in SSH_HOST; otherwise prepend SSH_USER.
if [[ "$SSH_HOST" == *"@"* ]]; then
  SSH_TARGET="$SSH_HOST"
else
  SSH_TARGET="${SSH_USER}@${SSH_HOST}"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# --- TOML/env escaping helper ------------------------------------------------
toml_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '%s' "$s"
}

# --- generate the seed config (used only if none exists on target) -----------
SEED_CONFIG="$(umask 077; mktemp)"
SEED_ENV="$(umask 077; mktemp)"
trap 'rm -f "$SEED_CONFIG" "$SEED_ENV" "${UNIT_FILE:-}"' EXIT
{
  echo "# logger-spot-bridge configuration. Secrets are NOT here — they live in"
  echo "# logger-spot-bridge.env (read by the systemd unit). Keep both 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo ""
  echo "host            = \"$(toml_escape "$HOST_NAME")\""
  echo ""
  echo "# The shack QTH — turns DXLog's station-relative azimuth/distance into"
  echo "# map coordinates. Must match the QTH the loggers are configured with."
  echo "station_locator = \"$(toml_escape "$STATION_LOCATOR")\""
  echo ""
  echo "[slot]"
  echo "station = \"$(toml_escape "$MQTT_STATION")\""
  echo "slot    = \"$(toml_escape "$MQTT_SLOT")\""
  echo ""
  echo "# One [[listener]] per logger broadcast stream. Broadcasts travel the"
  echo "# whole LAN — the loggers do not need to know about this host beyond"
  echo "# Log4OM's outbound target (which must be shari:${LOG4OM_PORT})."
  if [[ "$N1MM_ENABLED" == "true" ]]; then
    echo "[[listener]]"
    echo "name = \"dxlog\""
    echo "kind = \"n1mm\""
    echo "port = ${N1MM_PORT}"
    echo ""
  fi
  if [[ "$LOG4OM_ENABLED" == "true" ]]; then
    echo "[[listener]]"
    echo "name = \"log4om\""
    echo "kind = \"log4om\""
    echo "port = ${LOG4OM_PORT}"
    echo ""
  fi
  echo "# UDP-silence window after which device_online drops."
  echo "stale_after = \"$(toml_escape "$STALE_AFTER")\""
  echo ""
  echo "[qrz]"
  echo "# QRZ.com callsign→position gap-fill for bare-call loggers (Log4OM)."
  echo "# The password is NOT here: LOGGER_SPOT_BRIDGE_QRZ_PASSWORD in the env file."
  echo "enabled  = ${QRZ_ENABLED}"
  echo "username = \"$(toml_escape "$QRZ_USERNAME")\""
  echo "# Cache under /var/lib (StateDirectory) — /etc is read-only under the"
  echo "# hardened unit (ProtectSystem=strict)."
  echo "cache_path = \"/var/lib/${SERVICE_NAME}/qrz-cache.json\""
  echo ""
  echo "[mqtt]"
  echo "broker    = \"$(toml_escape "$MQTT_BROKER")\""
  echo "# client_id defaults to \"<site>-<station>-<slot>\"."
  echo "site      = \"$(toml_escape "$MQTT_SITE")\""
  echo "user      = \"$(toml_escape "$MQTT_USER")\""
  echo "# password is loaded from LOGGER_SPOT_BRIDGE_MQTT_PASSWORD in the env file."
  echo "password  = \"\""
  echo ""
  echo "[log]"
  echo "level = \"$(toml_escape "$STALE_LOG_LEVEL")\""
} > "$SEED_CONFIG"

{
  echo "# logger-spot-bridge EnvironmentFile (read by the systemd unit). Keep 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change the secrets."
  if [[ -n "$MQTT_PASSWORD" ]]; then
    echo "LOGGER_SPOT_BRIDGE_MQTT_PASSWORD=\"$(toml_escape "$MQTT_PASSWORD")\""
  else
    echo "# LOGGER_SPOT_BRIDGE_MQTT_PASSWORD=\"...\"   # set on the device"
  fi
  if [[ -n "$QRZ_PASSWORD" ]]; then
    echo "LOGGER_SPOT_BRIDGE_QRZ_PASSWORD=\"$(toml_escape "$QRZ_PASSWORD")\""
  else
    echo "# LOGGER_SPOT_BRIDGE_QRZ_PASSWORD=\"...\"    # only needed with [qrz] enabled"
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
Description=Shack logger spot bridge to MQTT (logger-spot-bridge)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# Non-secret settings come from the config file; the MQTT (and optional QRZ)
# password comes from the EnvironmentFile. No secrets on the command line.
ExecStart=${INSTALL_DIR}/${BINARY} -config ${CONFIG_FILE}
EnvironmentFile=${ENV_FILE}
Restart=on-failure
RestartSec=5
User=${SERVICE_USER}
Group=${SERVICE_USER}
ConfigurationDirectory=${SERVICE_NAME}
StateDirectory=${SERVICE_NAME}

# Hardening. Network-only: two inbound UDP listeners plus the outbound MQTT
# connection. Disk writes are limited to the QRZ cache in /var/lib.
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
# Resource ceilings — shari runs every station service; a leak must not OOM
# the whole Pi.
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
scp -q "$OUT" "${SSH_TARGET}:/tmp/${BINARY}.new"
scp -q "$UNIT_FILE" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.service"
scp -q "$SEED_CONFIG" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.config.seed"
scp -q "$SEED_ENV" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.env.seed"

# --- install remotely -------------------------------------------------------
echo ">> Installing on ${SSH_TARGET}..."
ssh "$SSH_TARGET" "INSTALL_DIR='${INSTALL_DIR}' BINARY='${BINARY}' SERVICE_NAME='${SERVICE_NAME}' SERVICE_USER='${SERVICE_USER}' CONFIG_DIR='${CONFIG_DIR}' CONFIG_FILE='${CONFIG_FILE}' ENV_FILE='${ENV_FILE}' bash -s" <<'REMOTE'
set -euo pipefail
SEED_CFG="/tmp/${SERVICE_NAME}.config.seed"
SEED_ENV="/tmp/${SERVICE_NAME}.env.seed"
trap 'rm -f "$SEED_CFG" "$SEED_ENV" "/tmp/${SERVICE_NAME}.service" "/tmp/${BINARY}.new"' EXIT
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi
sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$INSTALL_DIR" "$CONFIG_DIR"
if [ -e "$CONFIG_FILE" ]; then
  echo "   config exists at $CONFIG_FILE -- leaving it untouched (seed-once)."
else
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$SEED_CFG" "$CONFIG_FILE"
  echo "   seeded config at $CONFIG_FILE (0600, owner $SERVICE_USER)."
fi
if [ -e "$ENV_FILE" ]; then
  echo "   env file exists at $ENV_FILE -- leaving it untouched (seed-once)."
else
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$SEED_ENV" "$ENV_FILE"
  echo "   seeded env file at $ENV_FILE (0600, owner $SERVICE_USER)."
fi
sudo install -o root -g root -m 0755 "/tmp/${BINARY}.new" "$INSTALL_DIR/$BINARY"
sudo chown "$SERVICE_USER:$SERVICE_USER" "$CONFIG_FILE" "$ENV_FILE" 2>/dev/null || true
sudo install -o root -g root -m 0644 "/tmp/${SERVICE_NAME}.service" "/etc/systemd/system/${SERVICE_NAME}.service"
sudo systemctl daemon-reload
sudo systemctl enable "$SERVICE_NAME" >/dev/null
echo ">> Verifying the effective config on the Pi (-check, with the env file)..."
if sudo bash -c "set -a; . '$ENV_FILE'; set +a; exec '$INSTALL_DIR/$BINARY' -config '$CONFIG_FILE' -check"; then
  sudo systemctl restart "$SERVICE_NAME"
  echo "   service restarted."
else
  echo "!! config check FAILED — fix the values above (edit $CONFIG_FILE / $ENV_FILE on the Pi)"
  echo "   and re-run deploy.sh. The service was NOT restarted."
  exit 1
fi
REMOTE

echo ">> done. ${SERVICE_NAME} is enabled and running on ${SSH_TARGET}."
echo "   Logs:            ssh ${SSH_TARGET} journalctl -u ${SERVICE_NAME} -f"
echo "   Log4OM:          point the outbound CALLSIGN service at ${SSH_HOST#*@}:${LOG4OM_PORT}"
echo "   Watch the bus:   mosquitto_sub -h ${MQTT_BROKER#tcp://} -u ${MQTT_USER} -P ... -t 'muehle/${MQTT_STATION}/${MQTT_SLOT}/#' -v"
