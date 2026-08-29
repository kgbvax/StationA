#!/usr/bin/env bash
#
# Deploy powerseq (binary `powerseq`, the station startup/shutdown sequencer) to
# a Raspberry Pi and install it as a hardened systemd service.
#
# powerseq is a logic slot (no device) implementing the integration-model
# `sequencer` role. It subscribes the /status of the power-distribution and HF
# slots (and hf/pa/state) and, on the operator one-button /cmd (start|stop, NOT
# retained), runs an ordered startup/shutdown over those slots' retained /cmd
# with delays and liveness confirmations (muehle/hf/power-seq). It is
# network-only (MQTT broker client) — no serial devices — so the unit runs under
# the strictest sandbox (PrivateDevices=true) with only AF_INET/AF_INET6.
#
# Usage:
#   ./deploy.sh                       # deploy to default host "shari"
#   SSH_HOST=pi@shari.local ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST        SSH target            (default: 192.168.1.139)
#   SSH_USER        SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME    systemd service name  (default: powerseq)
#   SERVICE_USER    system user to run as (default: powerseq)
#   INSTALL_DIR     remote install dir    (default: /opt/powerseq)
#   BINARY          binary name           (default: powerseq)
#
#   HOST_NAME       host value            (default: shari)     [published in /meta]
#   LOCATION        location value        (default: bauwagen)  [published in /meta]
#   LOG_LEVEL       log.level value       (default: info)
#   MQTT_BROKER     mqtt.broker value     (default: tcp://127.0.0.1:1883)
#   MQTT_SITE       mqtt.site             (default: muehle)
#   MQTT_STATION    mqtt.station           (default: hf)
#   MQTT_SLOT       mqtt.slot             (default: power-seq)
#   MQTT_USER       mqtt.user             (default: hf)
#   MQTT_PASSWORD   POWERSEQ_MQTT_PASSWORD (default: empty -> set on device)
#   NETWORK_DELAY_S  timing.network_delay_s  (default: 30)
#   STEP_TIMEOUT_S   timing.step_timeout_s   (default: 120)
#   SHUTDOWN_STAGGER_S timing.shutdown_stagger_s (default: 2)
#
# Configuration lives in a 0600 TOML file on the target
# (/etc/powerseq/config.toml); the MQTT password is NOT in the TOML — it is
# loaded from an EnvironmentFile (/etc/powerseq/powerseq.env, 0600) so it never
# appears in the unit file or process command line. Both files are SEEDED ONCE
# on first deploy from the variables above; subsequent deploys leave the
# on-device files untouched so the Pi owns its own settings. To change a setting
# after the first deploy, edit the file on the device (or delete it and
# redeploy to re-seed).
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-powerseq}"
SERVICE_USER="${SERVICE_USER:-powerseq}"
INSTALL_DIR="${INSTALL_DIR:-/opt/powerseq}"
CONFIG_DIR="${CONFIG_DIR:-/etc/powerseq}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
ENV_FILE="${ENV_FILE:-${CONFIG_DIR}/powerseq.env}"
BINARY="${BINARY:-powerseq}"
PKG="./cmd/powerseq"

HOST_NAME="${HOST_NAME:-shari}"
LOCATION="${LOCATION:-bauwagen}"
LOG_LEVEL="${LOG_LEVEL:-info}"
MQTT_BROKER="${MQTT_BROKER:-tcp://127.0.0.1:1883}"
MQTT_SITE="${MQTT_SITE:-muehle}"
MQTT_STATION="${MQTT_STATION:-hf}"
MQTT_SLOT="${MQTT_SLOT:-power-seq}"
MQTT_USER="${MQTT_USER:-hf}"
MQTT_PASSWORD="${MQTT_PASSWORD:-}"
NETWORK_DELAY_S="${NETWORK_DELAY_S:-30}"
STEP_TIMEOUT_S="${STEP_TIMEOUT_S:-120}"
SHUTDOWN_STAGGER_S="${SHUTDOWN_STAGGER_S:-2}"

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
  echo "# powerseq configuration. Sensitive values are NOT here — the MQTT"
  echo "# password lives in the EnvironmentFile (powerseq.env). Keep both 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo ""
  echo "host = \"$(toml_escape "$HOST_NAME")\""
  echo "location = \"$(toml_escape "$LOCATION")\""
  echo ""
  echo "[mqtt]"
  echo "broker           = \"$(toml_escape "$MQTT_BROKER")\""
  echo '# client_id defaults to "<site>-<station>-<slot>" (model §8) when empty.'
  echo "client_id        = \"\""
  echo "site             = \"$(toml_escape "$MQTT_SITE")\""
  echo "station          = \"$(toml_escape "$MQTT_STATION")\""
  echo "slot             = \"$(toml_escape "$MQTT_SLOT")\""
  echo "# Home Assistant discovery is rendered by the standalone hadiscovery consumer"
  echo "# from this slot's expose block in /meta (model §9) — no embedded discovery."
  echo "discovery_prefix = \"homeassistant\""
  echo "user             = \"$(toml_escape "$MQTT_USER")\""
  echo "# password is loaded from POWERSEQ_MQTT_PASSWORD in the EnvironmentFile, not here."
  echo "password         = \"\""
  echo ""
  echo "[timing]"
  echo "network_delay_s    = ${NETWORK_DELAY_S}"
  echo "step_timeout_s     = ${STEP_TIMEOUT_S}"
  echo "shutdown_stagger_s = ${SHUTDOWN_STAGGER_S}"
  echo "poll_interval_ms   = 200"
  echo ""
  echo "[log]"
  echo "level = \"$(toml_escape "$LOG_LEVEL")\""
} > "$SEED_CONFIG"

{
  echo "# powerseq EnvironmentFile (read by the systemd unit). Keep 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change the password."
  if [[ -n "$MQTT_PASSWORD" ]]; then
    echo "POWERSEQ_MQTT_PASSWORD=\"$(toml_escape "$MQTT_PASSWORD")\""
  else
    echo "# POWERSEQ_MQTT_PASSWORD=\"...\"   # set on the device (copy from another hf service config)"
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
Description=Station startup/shutdown sequencer (powerseq)
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
ConfigurationDirectory=${SERVICE_NAME}
StateDirectory=${SERVICE_NAME}

# Hardening. powerseq is network-only: it talks to the MQTT broker. No serial
# devices, no disk writes, no elevated capabilities. PrivateDevices is therefore
# safe (unlike the serial bridges), and RestrictAddressFamilies covers the
# outbound MQTT connection.
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
# service; a leaky service with no limit could OOM the whole Pi and take the
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
scp "$SEED_CONFIG" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.config.seed"
scp "$SEED_ENV" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.env.seed"

# --- install remotely -------------------------------------------------------
echo ">> Installing on ${SSH_TARGET}..."
ssh "$SSH_TARGET" "INSTALL_DIR='${INSTALL_DIR}' BINARY='${BINARY}' SERVICE_NAME='${SERVICE_NAME}' SERVICE_USER='${SERVICE_USER}' CONFIG_DIR='${CONFIG_DIR}' CONFIG_FILE='${CONFIG_FILE}' ENV_FILE='${ENV_FILE}' bash -s" <<'REMOTE'
set -euo pipefail
SEED_CFG="/tmp/${SERVICE_NAME}.config.seed"
SEED_ENV="/tmp/${SERVICE_NAME}.env.seed"
trap 'rm -f "$SEED_CFG" "$SEED_ENV"' EXIT
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi
sudo mkdir -p "$INSTALL_DIR"
sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$CONFIG_DIR"
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
  echo "   !! Set POWERSEQ_MQTT_PASSWORD in $ENV_FILE before relying on the service."
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
echo ">> Done. powerseq deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Logs:    ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Config:  ${CONFIG_FILE}"
echo "   Secret:  ${ENV_FILE}  (set POWERSEQ_MQTT_PASSWORD if not seeded)"
echo "   Topics:  muehle/hf/power-seq/{meta,state,status,cmd}"
echo "   Start:   mosquitto_pub -h 127.0.0.1 -t 'muehle/hf/power-seq/cmd' -m '{\"action\":\"start\"}'"