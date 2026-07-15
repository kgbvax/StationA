#!/usr/bin/env bash
#
# Deploy wrc-rotator-bridge (the HF rotator bridge, binary `wrc-rotator-bridge`) to a
# Raspberry Pi and install it as a hardened systemd service.
#
# wrc-rotator-bridge bridges the HF antenna rotator (Yaesu G-450DC steered via an
# AF6SA WRC controller) to MQTT on the station three-plane topics
# (muehle/hf/rotator/{meta,state,status,cmd}). It dials the WRC over a
# WebSocket (default ws://192.168.1.108/wsrotor), subscribes to /cmd
# (set_az, stop, fwd, rev), and optionally runs a GS-232B TCP server on port
# 7373 for legacy rotator-control software. It is network-only — no serial
# devices — so the unit runs under the strictest sandbox (PrivateDevices=true)
# with only AF_INET/AF_INET6 for the outbound WS+MQTT and the inbound GS-232
# listen.
#
# Usage:
#   ./deploy.sh                       # deploy to default host "shari"
#   SSH_HOST=pi@shari.local ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST        SSH target            (default: 192.168.1.139)
#   SSH_USER        SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME    systemd service name  (default: wrc-rotator-bridge)
#   SERVICE_USER    system user to run as (default: wrc-rotator-bridge)
#   INSTALL_DIR     remote install dir    (default: /opt/wrc-rotator-bridge)
#   BINARY          binary name           (default: wrc-rotator-bridge)
#
#   ROTOR_URL       rotor.url value        (default: ws://192.168.1.108/wsrotor)
#   GS232_ENABLED   gs232.enabled          (default: true)
#   GS232_BIND      gs232.bind             (default: 0.0.0.0)
#   GS232_PORT      gs232.port             (default: 7373)
#   LOCATION        location value         (default: bauwagen)  [published in /meta]
#   HOST_NAME       host value             (default: shari)     [published in /meta]
#   DEVICE_MODEL    device.model value     (default: Yaesu G-450DC)
#   DEVICE_LINK     device.link value      (default: ethernet)
#   LOG_LEVEL       log.level value        (default: info)
#   MQTT_BROKER     mqtt.broker value      (default: tcp://192.168.1.50:1883)
#   MQTT_SITE       mqtt.site              (default: muehle)
#   MQTT_STATION    mqtt.station           (default: hf)
#   MQTT_SLOT       mqtt.slot              (default: rotator)
#   MQTT_USER       mqtt.user              (default: hf)
#   MQTT_PASSWORD   WRC_ROTATOR_BRIDGE_MQTT_PASSWORD (default: empty -> set on device)
#   DISCOVERY_PREFIX mqtt.discovery_prefix (default: homeassistant)
#
# Configuration lives in a 0600 TOML file on the target
# (/etc/wrc-rotator-bridge/config.toml); the MQTT password is NOT in the TOML — it is
# loaded from an EnvironmentFile (/etc/wrc-rotator-bridge/wrc-rotator-bridge.env, 0600)
# so it never appears in the unit file or process command line. Both files are
# SEEDED ONCE on first deploy from the variables above; subsequent deploys
# leave the on-device files untouched so the Pi owns its own settings. To
# change a setting after the first deploy, edit the file on the device (or
# delete it and redeploy to re-seed).
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-wrc-rotator-bridge}"
SERVICE_USER="${SERVICE_USER:-wrc-rotator-bridge}"
INSTALL_DIR="${INSTALL_DIR:-/opt/wrc-rotator-bridge}"
CONFIG_DIR="${CONFIG_DIR:-/etc/wrc-rotator-bridge}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
ENV_FILE="${ENV_FILE:-${CONFIG_DIR}/wrc-rotator-bridge.env}"
BINARY="${BINARY:-wrc-rotator-bridge}"
PKG="./cmd/wrc-rotator-bridge"

ROTOR_URL="${ROTOR_URL:-ws://192.168.1.108/wsrotor}"
GS232_ENABLED="${GS232_ENABLED:-true}"
GS232_BIND="${GS232_BIND:-0.0.0.0}"
GS232_PORT="${GS232_PORT:-7373}"
LOCATION="${LOCATION:-bauwagen}"
HOST_NAME="${HOST_NAME:-shari}"
DEVICE_MODEL="${DEVICE_MODEL:-Yaesu G-450DC}"
DEVICE_LINK="${DEVICE_LINK:-ethernet}"
LOG_LEVEL="${LOG_LEVEL:-info}"
MQTT_BROKER="${MQTT_BROKER:-tcp://192.168.1.50:1883}"
MQTT_SITE="${MQTT_SITE:-muehle}"
MQTT_STATION="${MQTT_STATION:-hf}"
MQTT_SLOT="${MQTT_SLOT:-rotator}"
MQTT_USER="${MQTT_USER:-hf}"
MQTT_PASSWORD="${MQTT_PASSWORD:-}"
DISCOVERY_PREFIX="${DISCOVERY_PREFIX:-homeassistant}"

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

# --- generate the seed config file (used only if none exists on target) -----
# Written with umask 077 so the local temp copy is never world-readable.
# The password is deliberately NOT here — it lives in the EnvironmentFile.
SEED_CONFIG="$(umask 077; mktemp)"
# Seed the EnvironmentFile too (password only). Separate temp so the secret
# never rides alongside the non-secret TOML if someone inspects the transfer.
SEED_ENV="$(umask 077; mktemp)"
trap 'rm -f "$SEED_CONFIG" "$SEED_ENV" "${UNIT_FILE:-}"' EXIT
{
  echo "# wrc-rotator-bridge configuration. Sensitive values are NOT here — the MQTT"
  echo "# password lives in the EnvironmentFile (wrc-rotator-bridge.env). Keep both 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo ""
  echo "host = \"$(toml_escape "$HOST_NAME")\""
  echo ""
  echo "[rotor]"
  echo "# AF6SA WRC controller WebSocket endpoint."
  echo "url = \"$(toml_escape "$ROTOR_URL")\""
  echo ""
  echo "[gs232]"
  echo "# Optional GS-232B inbound TCP server for legacy rotator-control software"
  echo "# (PSTRotator, N1MM, rotctld). Orthogonal to the MQTT contract; drives the"
  echo "# same device the bridge does. Disable if not needed."
  echo "enabled = ${GS232_ENABLED}"
  echo "bind = \"$(toml_escape "$GS232_BIND")\""
  echo "port = ${GS232_PORT}"
  echo ""
  echo "[device]"
  echo "# Identity published in /meta. link reflects the transport to the WRC."
  echo "model = \"$(toml_escape "$DEVICE_MODEL")\""
  echo "link  = \"$(toml_escape "$DEVICE_LINK")\""
  echo ""
  echo "[mqtt]"
  echo "broker           = \"$(toml_escape "$MQTT_BROKER")\""
  echo '# client_id defaults to "<site>-<station>-<slot>" (model §8).'
  echo "site             = \"$(toml_escape "$MQTT_SITE")\""
  echo "station          = \"$(toml_escape "$MQTT_STATION")\""
  echo "slot             = \"$(toml_escape "$MQTT_SLOT")\""
  echo "location         = \"$(toml_escape "$LOCATION")\""
  echo "discovery_prefix = \"$(toml_escape "$DISCOVERY_PREFIX")\""
  echo "# Legacy embedded HA discovery is OFF by default; the standalone hadiscovery"
  echo "# consumer renders discovery from this bridge's expose block in /meta (model §9)."
  echo "publish_ha_discovery = false"
  echo "user             = \"$(toml_escape "$MQTT_USER")\""
  echo "# password is loaded from WRC_ROTATOR_BRIDGE_MQTT_PASSWORD in wrc-rotator-bridge.env, not here."
  echo "password         = \"\""
  echo ""
  echo "[log]"
  echo "level = \"$(toml_escape "$LOG_LEVEL")\""
} > "$SEED_CONFIG"

{
  echo "# wrc-rotator-bridge EnvironmentFile (read by the systemd unit). Keep 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change the password."
  if [[ -n "$MQTT_PASSWORD" ]]; then
    echo "WRC_ROTATOR_BRIDGE_MQTT_PASSWORD=\"$(toml_escape "$MQTT_PASSWORD")\""
  else
    echo "# WRC_ROTATOR_BRIDGE_MQTT_PASSWORD=\"...\"   # set on the device (copy from another hf service config)"
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
Description=HF rotator to MQTT bridge (wrc-rotator-bridge)
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
# systemd owns /etc/wrc-rotator-bridge (created 0755, owned by the service user).
ConfigurationDirectory=${SERVICE_NAME}
StateDirectory=${SERVICE_NAME}

# Hardening. wrc-rotator-bridge is network-only: it dials the WRC over a WebSocket,
# talks to the MQTT broker, and listens on the GS-232 port. No serial devices,
# no disk writes, no elevated capabilities. PrivateDevices is therefore safe
# (unlike the serial bridges), and RestrictAddressFamilies covers both the
# outbound connections and the inbound GS-232 listen socket.
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
fi
# Seed the EnvironmentFile ONCE too (holds the MQTT password).
if [ -e "$ENV_FILE" ]; then
  echo "   env file exists at $ENV_FILE -- leaving it untouched (seed-once)."
else
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$SEED_ENV" "$ENV_FILE"
  echo "   seeded env file at $ENV_FILE (0600, owner $SERVICE_USER)."
  echo "   !! Set WRC_ROTATOR_BRIDGE_MQTT_PASSWORD in $ENV_FILE before relying on the bridge."
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
echo ">> Done. wrc-rotator-bridge deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Logs:    ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Config:  ${CONFIG_FILE}"
echo "   Secret:  ${ENV_FILE}  (set WRC_ROTATOR_BRIDGE_MQTT_PASSWORD if not seeded)"
echo "   Topics:  muehle/hf/rotator/{meta,state,status,cmd}"
echo "   GS-232:  ${GS232_BIND}:${GS232_PORT} (when enabled)"