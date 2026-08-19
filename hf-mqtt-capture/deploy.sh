#!/usr/bin/env bash
#
# Deploy hf-mqtt-capture to shari and install it as a passive diagnostic systemd service.
#
# Usage:
#   ./deploy.sh                       # deploy to default host 192.168.1.139
#   SSH_HOST=pi@shari.local ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST          SSH target            (default: 192.168.1.139)
#   SSH_USER          SSH user              (default: io) [used only if SSH_HOST has no user@]
#   SERVICE_NAME      systemd service name  (default: hf-mqtt-capture)
#   SERVICE_USER      system user to run as (default: hf-mqtt-capture)
#   INSTALL_DIR       remote install dir    (default: /opt/hf-mqtt-capture)
#
#   MQTT_BROKER       broker     value (default: tcp://192.168.1.50:1883)
#   MQTT_USER         user       value (default: hf)
#   MQTT_PASSWORD     password   value (default: empty -> auto-pulled on-device
#                     from an existing hf service env, so a re-seed is self-sufficient)
#   SITE              site       value (default: muehle)
#   STATION           station    value (default: hf)
#   LOG_DIR           log_dir    value (default: /var/log/hf-mqtt-capture)
#   RETENTION_HOURS   retention  value (default: 72)
#
# The MQTT password lives in a 0600 TOML file on the target, NOT in the systemd unit
# or process command line. The config is seeded ONCE on first deploy; subsequent deploys
# leave it untouched so the Pi owns its own settings.
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-hf-mqtt-capture}"
SERVICE_USER="${SERVICE_USER:-hf-mqtt-capture}"
INSTALL_DIR="${INSTALL_DIR:-/opt/hf-mqtt-capture}"
CONFIG_DIR="${CONFIG_DIR:-/etc/hf-mqtt-capture}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
BINARY="${BINARY:-hf-mqtt-capture}"
PKG="./cmd/hf-mqtt-capture"

MQTT_BROKER="${MQTT_BROKER:-tcp://192.168.1.50:1883}"
MQTT_USER="${MQTT_USER:-hf}"
MQTT_PASSWORD="${MQTT_PASSWORD:-}"
SITE="${SITE:-muehle}"
STATION="${STATION:-hf}"
LOG_DIR="${LOG_DIR:-/var/log/hf-mqtt-capture}"
RETENTION_HOURS="${RETENTION_HOURS:-72}"

# Allow "user@host" in SSH_HOST; otherwise prepend SSH_USER.
if [[ "$SSH_HOST" == *"@"* ]]; then
  SSH_TARGET="$SSH_HOST"
else
  SSH_TARGET="${SSH_USER}@${SSH_HOST}"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# --- TOML escaping helper ---------------------------------------------------
toml_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '%s' "$s"
}

# --- generate the seed config file -----------------------------------------
SEED_CONFIG="$(umask 077; mktemp)"
trap 'rm -f "$SEED_CONFIG" "${UNIT_FILE:-}"' EXIT
{
  echo "# hf-mqtt-capture configuration. Contains the MQTT password -- keep this file 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo ""
  echo "broker   = \"$(toml_escape "$MQTT_BROKER")\""
  echo "user     = \"$(toml_escape "$MQTT_USER")\""
  echo "password = \"$(toml_escape "$MQTT_PASSWORD")\""
  echo "site     = \"$(toml_escape "$SITE")\""
  echo "station  = \"$(toml_escape "$STATION")\""
  echo "log_dir  = \"$(toml_escape "$LOG_DIR")\""
  echo "retention_hours = ${RETENTION_HOURS}"
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
Description=HF MQTT traffic capture recorder (hf-mqtt-capture)
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
# systemd manages /etc/hf-mqtt-capture (created 0755, owned by the service user).
ConfigurationDirectory=${SERVICE_NAME}
# Hardening. This service only connects to MQTT and writes logs.
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
# Allow writing the log directory.
ReadWritePaths=${LOG_DIR}

[Install]
WantedBy=multi-user.target
EOF

# --- copy artifacts to the Pi ----------------------------------------------
echo ">> Copying files to ${SSH_TARGET}..."
scp "$OUT" "${SSH_TARGET}:/tmp/${BINARY}.new"
scp "$UNIT_FILE" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.service"
scp "$SEED_CONFIG" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.config.seed"

# --- install remotely -------------------------------------------------------
echo ">> Installing on ${SSH_TARGET}..."
ssh "$SSH_TARGET" "INSTALL_DIR='${INSTALL_DIR}' BINARY='${BINARY}' SERVICE_NAME='${SERVICE_NAME}' SERVICE_USER='${SERVICE_USER}' CONFIG_DIR='${CONFIG_DIR}' CONFIG_FILE='${CONFIG_FILE}' LOG_DIR='${LOG_DIR}' bash -s" <<'REMOTE'
set -euo pipefail
SEED="/tmp/${SERVICE_NAME}.config.seed"
trap 'rm -f "$SEED"' EXIT

# Create a dedicated system user/group if missing.
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi

sudo mkdir -p "$INSTALL_DIR"
sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$CONFIG_DIR"
sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$LOG_DIR"

# Seed the config ONCE: install only if the device has no config yet.
if [ -e "$CONFIG_FILE" ]; then
  echo "   config exists at $CONFIG_FILE -- leaving it untouched (seed-once)."
else
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$SEED" "$CONFIG_FILE"
  echo "   seeded config at $CONFIG_FILE (0600, owner $SERVICE_USER)."
  # If the seed has an empty password, pull the shared hf MQTT password from an
  # existing station service env on the device and inject it into the config.
  if sudo grep -qE '^[[:space:]]*password[[:space:]]*=[[:space:]]*""' "$CONFIG_FILE"; then
    pw=""
    for f in \
      /etc/acom1200s-pa-bridge/acom1200s-pa-bridge.env \
      /etc/flexbridge/flexbridge.env \
      /etc/hadiscovery/hadiscovery.env \
      /etc/atr1k-tuner-bridge/atr1k-tuner-bridge.env ; do
      sudo test -r "$f" || continue
      v=$(sudo grep -hE '^[A-Z0-9_]*MQTT_PASSWORD=' "$f" 2>/dev/null | head -1 | sed -E 's/^[^=]*=//; s/^"(.*)"$/\1/')
      [ -n "$v" ] && pw="$v" && break
    done
    if [ -n "$pw" ]; then
      sudo CFG_PATH="$CONFIG_FILE" HF_PW="$pw" python3 - <<'PY'
import os, re, pathlib
pw = os.environ["HF_PW"]
cfg = pathlib.Path(os.environ["CFG_PATH"])
t = cfg.read_text()
esc = pw.replace("\\", "\\\\").replace("\"", "\\\"")
t2, n = re.subn(r"^[ \t]*password[ \t]*=.*$", f'password = "{esc}"', t, count=1, flags=re.MULTILINE)
assert n == 1, "password line not found in config"
cfg.write_text(t2)
PY
      echo "   injected hf MQTT password (pulled on-device from an existing service env) into the config."
    else
      echo "   !! No hf service env found to copy the password from. Set it on the device: sudo -e $CONFIG_FILE"
    fi
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
echo ">> Done. hf-mqtt-capture deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Logs:   ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Captures: ssh ${SSH_TARGET} 'tail -F ${LOG_DIR}/\$(date +%Y-%m-%d)/\$(date +%H).log'"
echo "   Config: ssh ${SSH_TARGET} 'sudo -e ${CONFIG_FILE}'"
