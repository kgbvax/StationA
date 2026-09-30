#!/usr/bin/env bash
#
# Deploy oscarwatch-sattrack-bridge to scmino as a hardened systemd service —
# the standard stationa pattern (see docs/conventions/deployment.md). scmino
# (192.168.1.178, Debian 12, arm64) is taking over shari's role; the bridge
# needs nothing host-specific, only outbound TCP.
#
# The bridge DIALS OscarWatch's "Satellite link" WebSocket on the shack PC
# (BWPC, ws://192.168.1.197:7373/) and publishes muehle/uhf/sat-track. In
# OscarWatch: Settings → Integrations → Satellite link → enable, "Allow
# connections from local network" on, Windows firewall allow (private).
#
# Config: /etc/oscarwatch-sattrack-bridge/config.toml (0600, seed-once)
# Secrets: /etc/oscarwatch-sattrack-bridge/oscarwatch-sattrack-bridge.env
#   (0600, seed-once): OSCARWATCH_SATTRACK_BRIDGE_MQTT_PASSWORD
# Both files are seeded on FIRST deploy from the variables below and never
# overwritten afterwards — the host owns its settings, updates are safe.
#
# Usage:
#   MQTT_PASSWORD=... ./deploy.sh              # first deploy (seeds the secret)
#   ./deploy.sh                                # updates
#   SSH_HOST=io@192.168.1.139 ./deploy.sh      # another host
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.178}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-oscarwatch-sattrack-bridge}"
SERVICE_USER="${SERVICE_USER:-oscarwatch-sattrack-bridge}"
INSTALL_DIR="${INSTALL_DIR:-/opt/oscarwatch-sattrack-bridge}"
CONFIG_DIR="${CONFIG_DIR:-/etc/oscarwatch-sattrack-bridge}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
ENV_FILE="${ENV_FILE:-${CONFIG_DIR}/oscarwatch-sattrack-bridge.env}"
BINARY="${BINARY:-oscarwatch-sattrack-bridge}"
PKG="./cmd/oscarwatch-sattrack-bridge"
GOARCH_TARGET="${GOARCH_TARGET:-arm64}"

HOST_NAME="${HOST_NAME:-scmino}"            # host identity published in /meta
LOCATION="${LOCATION:-bauwagen}"            # location label published in /meta
SOURCE_URL="${SOURCE_URL:-ws://192.168.1.197:7373/}"
PING_INTERVAL="${PING_INTERVAL:-30s}"
STATION_LOCATOR="${STATION_LOCATOR:-JO32WE}"
LOG_LEVEL="${LOG_LEVEL:-info}"
MQTT_BROKER="${MQTT_BROKER:-tcp://192.168.1.50:1883}"
MQTT_SITE="${MQTT_SITE:-muehle}"
MQTT_STATION="${MQTT_STATION:-uhf}"
MQTT_SLOT="${MQTT_SLOT:-sat-track}"
MQTT_USER="${MQTT_USER:-hf}"
MQTT_PASSWORD="${MQTT_PASSWORD:-}"

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
  echo "# oscarwatch-sattrack-bridge configuration. Secrets are NOT here — they"
  echo "# live in ${SERVICE_NAME}.env (read by the systemd unit). Keep both 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo ""
  echo "host     = \"$(toml_escape "$HOST_NAME")\""
  echo "location = \"$(toml_escape "$LOCATION")\""
  echo ""
  echo "[slot]"
  echo "station = \"$(toml_escape "$MQTT_STATION")\""
  echo "slot    = \"$(toml_escape "$MQTT_SLOT")\""
  echo ""
  echo "# OscarWatch Satellite-link WebSocket (Settings → Integrations → Satellite"
  echo "# link; \"Allow connections from local network\" must be on)."
  echo "[source]"
  echo "url           = \"$(toml_escape "$SOURCE_URL")\""
  echo "# Keepalive; the link counts as dead after 2.5× without a pong. 0 = off."
  echo "ping_interval = \"$(toml_escape "$PING_INTERVAL")\""
  echo ""
  echo "# Station position for the sub-satellite point. Must be the QTH"
  echo "# OscarWatch computes its look angles from. lat/lon (+ alt_m) beat the"
  echo "# locator when set."
  echo "[station]"
  echo "locator = \"$(toml_escape "$STATION_LOCATOR")\""
  echo "# lat   = 52.1875"
  echo "# lon   = 7.875"
  echo "# alt_m = 0"
  echo ""
  echo "[mqtt]"
  echo "broker    = \"$(toml_escape "$MQTT_BROKER")\""
  echo "# client_id defaults to \"<site>-<station>-<slot>\"."
  echo "site      = \"$(toml_escape "$MQTT_SITE")\""
  echo "user      = \"$(toml_escape "$MQTT_USER")\""
  echo "# password is loaded from OSCARWATCH_SATTRACK_BRIDGE_MQTT_PASSWORD in the env file."
  echo "password  = \"\""
  echo ""
  echo "[log]"
  echo "level = \"$(toml_escape "$LOG_LEVEL")\""
} > "$SEED_CONFIG"

{
  echo "# oscarwatch-sattrack-bridge EnvironmentFile (read by the systemd unit). Keep 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change the secret."
  if [[ -n "$MQTT_PASSWORD" ]]; then
    echo "OSCARWATCH_SATTRACK_BRIDGE_MQTT_PASSWORD=\"$(toml_escape "$MQTT_PASSWORD")\""
  else
    echo "# OSCARWATCH_SATTRACK_BRIDGE_MQTT_PASSWORD=\"...\"   # set on the host"
  fi
} > "$SEED_ENV"

# --- build (Linux arm64 by default: scmino and shari are both aarch64) -------
echo ">> Building ${BINARY} for linux/${GOARCH_TARGET}..."
OUT="dist/${BINARY}-linux-${GOARCH_TARGET}"
mkdir -p dist
GOOS=linux GOARCH="$GOARCH_TARGET" CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$OUT" "$PKG"
echo "   built $OUT"

# --- generate the systemd unit ---------------------------------------------
UNIT_FILE="$(mktemp)"
cat > "$UNIT_FILE" <<EOF
[Unit]
Description=OscarWatch satellite tracking bridge to MQTT (oscarwatch-sattrack-bridge)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# Non-secret settings come from the config file; the MQTT password comes from
# the EnvironmentFile. No secrets on the command line.
ExecStart=${INSTALL_DIR}/${BINARY} -config ${CONFIG_FILE}
EnvironmentFile=${ENV_FILE}
Restart=on-failure
RestartSec=5
User=${SERVICE_USER}
Group=${SERVICE_USER}
ConfigurationDirectory=${SERVICE_NAME}

# Hardening. Network-only: one outbound WebSocket (OscarWatch) plus the
# outbound MQTT connection. No disk writes at all.
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
# Resource ceilings — a leak must not OOM the host every station service runs on.
MemoryMax=256M
TasksMax=64
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

[Install]
WantedBy=multi-user.target
EOF

# --- copy artifacts to the host ---------------------------------------------
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
echo ">> Verifying the effective config on the host (-check, with the env file)..."
if sudo bash -c "set -a; . '$ENV_FILE'; set +a; exec '$INSTALL_DIR/$BINARY' -config '$CONFIG_FILE' -check"; then
  sudo systemctl restart "$SERVICE_NAME"
  echo "   service restarted."
else
  echo "!! config check FAILED — fix the values above (edit $CONFIG_FILE / $ENV_FILE on the host)"
  echo "   and re-run deploy.sh. The service was NOT restarted."
  exit 1
fi
REMOTE

echo ">> done. ${SERVICE_NAME} is enabled and running on ${SSH_TARGET}."
echo "   Logs:          ssh ${SSH_TARGET} journalctl -u ${SERVICE_NAME} -f"
echo "   OscarWatch:    Satellite link enabled + LAN access on ${SOURCE_URL}"
echo "   Watch the bus: mosquitto_sub -h ${MQTT_BROKER#tcp://} -u ${MQTT_USER} -P ... -t '${MQTT_SITE}/${MQTT_STATION}/${MQTT_SLOT}/#' -v"
