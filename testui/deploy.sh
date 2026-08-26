#!/usr/bin/env bash
#
# Deploy testui to a Raspberry Pi and install it as a systemd service.
#
# testui is the schema-aware MQTT monitor + stimulator. It serves a static web
# UI over HTTP and forwards the <site>/# topic tree via Server-Sent Events.
# It is a logic slot: it owns no device, opens no serial port, and writes no /cmd
# of its own.
#
# Usage:
#   ./deploy.sh                       # deploy to default host "shari"
#   SSH_HOST=pi@shari.local ./deploy.sh
#   MQTT_PASSWORD=secret ./deploy.sh  # seed the MQTT password on first deploy
#
# Configurable via environment variables (with defaults):
#   SSH_HOST        SSH target            (default: 192.168.1.139)
#   SSH_USER        SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME    systemd service name  (default: testui)
#   SERVICE_USER    system user to run as (default: testui)
#   INSTALL_DIR     remote install dir    (default: /opt/testui)
#
#   HTTP_ADDR       http_addr value       (default: 0.0.0.0:8090)
#   MQTT_BROKER     mqtt.broker value     (default: tcp://127.0.0.1:1883)
#   MQTT_SITE       mqtt.site value       (default: muehle)
#   MQTT_USER       mqtt.user value       (default: hf)
#   MQTT_PASSWORD   mqtt.password value   (default: empty -> set on device)
#   MQTT_CLIENT_ID  mqtt.client_id value  (default: testui)
#
# Configuration lives in a single 0600 TOML file on the target. The MQTT password
# is kept in a separate 0600 EnvironmentFile so it never appears on the command
# line or in the unit. Both files are SEEDED ONCE on first deploy; subsequent
# deploys leave the on-device files untouched so the Pi owns its own settings.
# To change a setting after the first deploy, edit the file on the device (or
# delete it and redeploy to re-seed).
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-testui}"
SERVICE_USER="${SERVICE_USER:-testui}"
INSTALL_DIR="${INSTALL_DIR:-/opt/testui}"
CONFIG_DIR="${CONFIG_DIR:-/etc/testui}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
ENV_FILE="${ENV_FILE:-${CONFIG_DIR}/testui.env}"
BINARY="${BINARY:-testui}"
PKG="./cmd/testui"

HTTP_ADDR="${HTTP_ADDR:-0.0.0.0:8090}"
MQTT_BROKER="${MQTT_BROKER:-tcp://127.0.0.1:1883}"
MQTT_SITE="${MQTT_SITE:-muehle}"
MQTT_USER="${MQTT_USER:-hf}"
MQTT_PASSWORD="${MQTT_PASSWORD:-}"
MQTT_CLIENT_ID="${MQTT_CLIENT_ID:-testui}"

# Allow "user@host" in SSH_HOST; otherwise prepend SSH_USER.
if [[ "$SSH_HOST" == *"@"* ]]; then
  SSH_TARGET="$SSH_HOST"
else
  SSH_TARGET="${SSH_USER}@${SSH_HOST}"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# --- TOML/env escaping helper ------------------------------------------------
# Escape backslashes and double quotes for a TOML basic string or a systemd
# double-quoted EnvironmentFile value (same escaping rules).
toml_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '%s' "$s"
}

# --- generate the seed config + env files (used only if none exists on target) --
# Written with umask 077 so the local temp copies are never world-readable.
SEED_CONFIG="$(umask 077; mktemp)"
SEED_ENV="$(umask 077; mktemp)"
trap 'rm -f "$SEED_CONFIG" "$SEED_ENV" "${UNIT_FILE:-}"' EXIT
{
  echo "# testui configuration. Sensitive values are NOT here -- the MQTT password"
  echo "# lives in the EnvironmentFile (testui.env). Keep both 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo ""
  echo "# Serve the UI + HTTP API here. 0.0.0.0 binds the LAN so the workstation"
  echo "# browser reaches http://shari:8090 without a local server."
  echo "http_addr = \"$(toml_escape "$HTTP_ADDR")\""
  echo "site      = \"$(toml_escape "$MQTT_SITE")\"   # subscribe <site>/# ; browser may only publish under <site>/"
  echo ""
  echo "[mqtt]"
  echo "broker    = \"$(toml_escape "$MQTT_BROKER")\""
  echo "client_id = \"$(toml_escape "$MQTT_CLIENT_ID")\"   # distinct from any slot-derived bridge client ID"
  echo "user      = \"$(toml_escape "$MQTT_USER")\""
  echo "# password is loaded from TESTUI_MQTT_PASSWORD in testui.env, not here."
  echo "password  = \"\""
} > "$SEED_CONFIG"

{
  echo "# testui EnvironmentFile (read by the systemd unit). Keep 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change the password."
  if [[ -n "$MQTT_PASSWORD" ]]; then
    echo "TESTUI_MQTT_PASSWORD=\"$(toml_escape "$MQTT_PASSWORD")\""
  else
    echo "# TESTUI_MQTT_PASSWORD=\"...\"   # set on the device (copy from another hf service config)"
  fi
} > "$SEED_ENV"

# --- build for the Pi (Linux arm64) ----------------------------------------
echo ">> Building ${BINARY} for linux/arm64..."
OUT="dist/${BINARY}-linux-arm64"
mkdir -p dist
GOWORK=off GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$OUT" "$PKG"
echo "   built $OUT"

# --- generate the systemd unit ---------------------------------------------
UNIT_FILE="$(mktemp)"
cat > "$UNIT_FILE" <<EOF
[Unit]
Description=stationa test UI — MQTT relay + schema-aware browser console (testui)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# All non-secret settings come from the config file; the MQTT password comes from the
# EnvironmentFile. No secrets appear on the command line / in the unit.
ExecStart=${INSTALL_DIR}/${BINARY} -config ${CONFIG_FILE}
EnvironmentFile=${ENV_FILE}
Restart=on-failure
RestartSec=5
# Run as a dedicated unprivileged user (no login, no home).
User=${SERVICE_USER}
Group=${SERVICE_USER}
# systemd manages /etc/testui (created 0755, owned by the service user).
ConfigurationDirectory=${SERVICE_NAME}
StateDirectory=${SERVICE_NAME}

# Hardening. testui needs: inbound TCP for the HTTP listener, outbound TCP to the MQTT
# broker, and nothing else — no disk writes, no serial, no elevated capabilities.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
# AF_INET/AF_INET6 cover both the inbound HTTP listen and the outbound MQTT connect.
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
RestrictRealtime=true
RestrictSUIDSGID=true
RemoveIPC=true
CapabilityBoundingSet=
AmbientCapabilities=
ReadWritePaths=/var/lib/${SERVICE_NAME}
# Resource ceilings. shari is the single shared host; cap this small static binary so a
# runaway can never OOM the whole Pi. The tree is in-memory, so 128M is ample.
MemoryMax=128M
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
fi
# Seed the EnvironmentFile ONCE too (holds the MQTT password).
if [ -e "$ENV_FILE" ]; then
  echo "   env file exists at $ENV_FILE -- leaving it untouched (seed-once)."
else
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$SEED_ENV" "$ENV_FILE"
  echo "   seeded env file at $ENV_FILE (0600, owner $SERVICE_USER)."
  echo "   !! Set TESTUI_MQTT_PASSWORD in $ENV_FILE before relying on testui."
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
echo ">> Done. testui deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Logs:   ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Web UI: http://${SSH_HOST##*@}:${HTTP_ADDR##*:}/"
echo "   Config: ssh ${SSH_TARGET} 'sudo -e ${CONFIG_FILE}'"
echo "   Secret:  ssh ${SSH_TARGET} 'sudo -e ${ENV_FILE}'"
