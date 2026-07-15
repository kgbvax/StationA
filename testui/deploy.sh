#!/usr/bin/env bash
#
# Deploy testui (the stationa MQTT relay + schema-aware test UI, binary `testui`) to the
# Raspberry Pi (shari) as a hardened systemd service, so the browser can hit the UI on the
# LAN without running a local server on the workstation.
#
# testui is a network service, not a serial bridge: it connects outbound to the MQTT broker
# (credentials from a 0600 config / TESTUI_MQTT_PASSWORD env) and serves the embedded static
# UI + JSON/SSE API inbound over HTTP. It opens no /dev/tty* devices, so the unit can run
# under a stricter sandbox than the serial bridges (PrivateDevices=true). The HTTP listener
# binds 0.0.0.0:8090 by default so the workstation browser reaches http://shari:8090.
#
# /api/publish and /api/clear are unauthenticated HTTP endpoints that write to the station
# bus. Binding to the LAN therefore exposes the relay to every host on 192.168.1.0/24 —
# this is intentional for the trusted home LAN; do NOT deploy this default onto an
# untrusted network without adding auth / a reverse proxy in front.
#
# Usage:
#   ./deploy.sh                       # deploy to default host "shari"
#   SSH_HOST=pi@shari.local ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST        SSH target            (default: 192.168.1.139)
#   SSH_USER        SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME    systemd service name  (default: testui)
#   SERVICE_USER    system user to run as (default: testui)
#   INSTALL_DIR     remote install dir    (default: /opt/testui)
#   HTTP_ADDR       http_addr value       (default: 0.0.0.0:8090)
#   SITE            site value            (default: muehle)
#   MQTT_BROKER     mqtt.broker value     (default: tcp://192.168.1.50:1883)
#   MQTT_CLIENT_ID  mqtt.client_id value  (default: testui)
#   MQTT_USER       mqtt.user value       (default: hf)
#   MQTT_PASSWORD   TESTUI_MQTT_PASSWORD  (default: empty -> pulled on-device from an existing hf service env)
#
# Configuration lives in a 0600 TOML file on the target (/etc/testui/config.toml); the MQTT
# password is NOT in the TOML — it is loaded from an EnvironmentFile (/etc/testui/testui.env,
# 0600) so it never appears in the unit file or process command line. Both files are SEEDED
# ONCE on first deploy; subsequent deploys leave the on-device files untouched so the Pi
# owns its own settings. To change a setting after the first deploy, edit the file on the
# device (or delete it and redeploy to re-seed).
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
SITE="${SITE:-muehle}"
MQTT_BROKER="${MQTT_BROKER:-tcp://192.168.1.50:1883}"
MQTT_CLIENT_ID="${MQTT_CLIENT_ID:-testui}"
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

# --- generate the seed config file (used only if none exists on target) -----
SEED_CONFIG="$(umask 077; mktemp)"
SEED_ENV="$(umask 077; mktemp)"
trap 'rm -f "$SEED_CONFIG" "$SEED_ENV" "${UNIT_FILE:-}"' EXIT
{
  echo "# testui configuration. Sensitive values are NOT here — the MQTT password lives"
  echo "# in the EnvironmentFile (testui.env). Keep both 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo ""
  echo "# Serve the UI + HTTP API here. 0.0.0.0 binds the LAN so the workstation browser"
  echo "# reaches http://shari:8090 without a local server."
  echo "http_addr = \"$(toml_escape "$HTTP_ADDR")\""
  echo "site      = \"$(toml_escape "$SITE")\"   # subscribe <site>/# ; browser may only publish under <site>/"
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
    echo "# TESTUI_MQTT_PASSWORD=\"...\"   # auto-pulled on-device from an existing hf service env if available"
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
scp "$SEED_CONFIG" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.config.seed"
scp "$SEED_ENV" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.env.seed"

# --- install remotely -------------------------------------------------------
echo ">> Installing on ${SSH_TARGET}..."
ssh "$SSH_TARGET" "INSTALL_DIR='${INSTALL_DIR}' BINARY='${BINARY}' SERVICE_NAME='${SERVICE_NAME}' SERVICE_USER='${SERVICE_USER}' CONFIG_DIR='${CONFIG_DIR}' CONFIG_FILE='${CONFIG_FILE}' ENV_FILE='${ENV_FILE}' bash -s" <<'REMOTE'
set -euo pipefail
SEED_CFG="/tmp/${SERVICE_NAME}.config.seed"
SEED_ENV="/tmp/${SERVICE_NAME}.env.seed"
trap 'rm -f "$SEED_CFG" "$SEED_ENV"' EXIT
# Create a dedicated system user/group (no login, no home) if missing.
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi
sudo mkdir -p "$INSTALL_DIR"
sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$CONFIG_DIR"
# Seed the config ONCE: install only if the device has no config yet.
if [ -e "$CONFIG_FILE" ]; then
  echo "   config exists at $CONFIG_FILE -- leaving it untouched (seed-once)."
else
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$SEED_CFG" "$CONFIG_FILE"
  echo "   seeded config at $CONFIG_FILE (0600, owner $SERVICE_USER)."
fi
# Seed the EnvironmentFile ONCE. If the deploy did not supply a password, try to pull the
# shared `hf` MQTT password from an existing station service env on the device so the
# secret never leaves the Pi. Rewrite the var name to TESTUI_MQTT_PASSWORD.
if [ -e "$ENV_FILE" ]; then
  echo "   env file exists at $ENV_FILE -- leaving it untouched (seed-once)."
else
  pw=""
  for f in \
    /etc/acom1200s-pa-bridge/acom1200s-pa-bridge.env \
    /etc/flexbridge/flexbridge.env \
    /etc/antenna-select/antenna-select.env \
    /etc/wrc-rotator-bridge/wrc-rotator-bridge.env \
    /etc/hadiscovery/hadiscovery.env \
    /etc/atr1k-tuner-bridge/atr1k-tuner-bridge.env ; do
    # These env files are 0600 owned by their service users, so test readability via sudo
    # (not `[ -r ]`, which runs as the deploying user and would skip every file).
    sudo test -r "$f" || continue
    v=$(sudo grep -hE '^[A-Z0-9_]*MQTT_PASSWORD=' "$f" 2>/dev/null | head -1 | sed -E 's/^[^=]*=//; s/^"(.*)"$/\1/')
    [ -n "$v" ] && pw="$v" && break
  done
  if [ -n "$pw" ]; then
    ( umask 077; printf 'TESTUI_MQTT_PASSWORD="%s"\n' "$pw" ) > "$SEED_ENV"
    sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$SEED_ENV" "$ENV_FILE"
    echo "   seeded env file at $ENV_FILE (0600) — password pulled on-device from an existing hf service env."
  else
    sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$SEED_ENV" "$ENV_FILE"
    echo "   seeded env file at $ENV_FILE (0600)."
    echo "   !! Set TESTUI_MQTT_PASSWORD in $ENV_FILE before relying on testui (no hf service env found to copy from)."
  fi
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
echo "   UI:      http://${SSH_HOST}:8090   (bound to ${HTTP_ADDR})"
echo "   Logs:    ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Config:  ${CONFIG_FILE}"
echo "   Secret:  ${ENV_FILE}  (set TESTUI_MQTT_PASSWORD if not seeded)"
echo "   NOTE: /api/publish + /api/clear are unauthenticated; the HTTP listener is LAN-wide."