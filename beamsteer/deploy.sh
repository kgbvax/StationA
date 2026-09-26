#!/usr/bin/env bash
#
# Deploy beamsteer to a Raspberry Pi and install it as a systemd service.
#
# beamsteer is the HF smart-rotation logic slot (muehle/hf/beam-steer): it
# emulates PstRotator's UDP interface for the contest logger and turns each
# rotate request into an Ultrabeam 180° flip or a rotation to the cheaper lobe.
# No serial device, no HTTP server; one UDP listener. See
# docs/beam-steer-mqtt-api.md.
#
# Usage:
#   ./deploy.sh                       # deploy to default host "shari"
#   SSH_HOST=pi@shari.local ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST          SSH target            (default: 192.168.1.139)
#   SSH_USER          SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME      systemd service name  (default: beamsteer)
#   SERVICE_USER      system user to run as (default: beamsteer)
#   INSTALL_DIR       remote install dir    (default: /opt/beamsteer)
#   BINARY            binary name           (default: beamsteer)
#
#   MQTT_BROKER       mqtt.broker     value (default: tcp://127.0.0.1:1883)
#   MQTT_SITE         mqtt.site       value (default: muehle)
#   MQTT_STATION      mqtt.station    value (default: hf)
#   MQTT_SLOT         mqtt.slot       value (default: beam-steer)
#   MQTT_USER         mqtt.user       value (default: hf)
#   MQTT_PASSWORD     mqtt.password   value (default: empty -> auto-pulled on-device
#                     from an existing hf service env, so a re-seed is self-sufficient)
#   LOCATION          location        value (default: bauwagen)  [published in /meta]
#   HOST_NAME         host            value (default: shari)     [published in /meta]
#   PST_PORT          pstrotator.port       (default: 12050; replies go to port+1)
#   PST_REPLY         pstrotator.reply      (default: manual; or xml)
#   LOBE_DEG          steer.lobe_deg        (default: 30)
#   BIDIR_LOBE_DEG    steer.bidir_lobe_deg  (default: 45)
#   MAX_AZ            steer.max_az          (default: 360; 450 only once the
#                     G-450 overlap readback is verified)
#
# Configuration (including the MQTT password) lives in a single 0600 TOML file
# on the target, NOT in the systemd unit or process command line. The file is
# SEEDED ONCE on first deploy; later deploys leave it untouched. To change a
# setting, edit the file on the device (or delete it and redeploy to re-seed).
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-beamsteer}"
SERVICE_USER="${SERVICE_USER:-beamsteer}"
INSTALL_DIR="${INSTALL_DIR:-/opt/beamsteer}"
CONFIG_DIR="${CONFIG_DIR:-/etc/beamsteer}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
BINARY="${BINARY:-beamsteer}"
PKG="./cmd/beamsteer"

MQTT_BROKER="${MQTT_BROKER:-tcp://127.0.0.1:1883}"
MQTT_SITE="${MQTT_SITE:-muehle}"
MQTT_STATION="${MQTT_STATION:-hf}"
MQTT_SLOT="${MQTT_SLOT:-beam-steer}"
MQTT_USER="${MQTT_USER:-hf}"
MQTT_PASSWORD="${MQTT_PASSWORD:-}"
LOCATION="${LOCATION:-bauwagen}"
HOST_NAME="${HOST_NAME:-shari}"
PST_PORT="${PST_PORT:-12050}"
PST_REPLY="${PST_REPLY:-manual}"
LOBE_DEG="${LOBE_DEG:-30}"
BIDIR_LOBE_DEG="${BIDIR_LOBE_DEG:-45}"
MAX_AZ="${MAX_AZ:-360}"

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
  echo "# beamsteer configuration. Contains the MQTT password -- keep this file 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo ""
  echo "# Deployment identity, published in /meta (integration model §3)."
  echo "location = \"$(toml_escape "$LOCATION")\""
  echo "host     = \"$(toml_escape "$HOST_NAME")\""
  echo ""
  echo "[mqtt]"
  echo "broker    = \"$(toml_escape "$MQTT_BROKER")\""
  echo '# client_id defaults to "<site>-<station>-<slot>" (model §8).'
  echo "site      = \"$(toml_escape "$MQTT_SITE")\""
  echo "station   = \"$(toml_escape "$MQTT_STATION")\""
  echo "slot      = \"$(toml_escape "$MQTT_SLOT")\""
  echo "user      = \"$(toml_escape "$MQTT_USER")\""
  echo "password  = \"$(toml_escape "$MQTT_PASSWORD")\""
  echo ""
  echo "[pstrotator]"
  echo "# The contest logger's PstRotator target. Query replies go to port+1."
  echo "bind  = \"0.0.0.0\""
  echo "port  = ${PST_PORT}"
  echo "reply = \"$(toml_escape "$PST_REPLY")\"   # manual = \"AZ:xxx.x<CR>\", xml = <PST><AZIMUTH>n</AZIMUTH></PST>"
  echo ""
  echo "[steer]"
  echo "lobe_deg       = ${LOBE_DEG}      # forward/reverse lobe half-width"
  echo "bidir_lobe_deg = ${BIDIR_LOBE_DEG}      # bidirectional lobe half-width (front and back)"
  echo "max_az         = ${MAX_AZ}     # 450 only once the G-450 overlap readback is verified"
  echo "rotator_slot   = \"rotator\""
  echo "ant_ctrl_slot  = \"ant-ctrl\""
  echo "radio_slot     = \"radio\""
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
Description=HF smart rotation (beamsteer)
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
# systemd manages /etc/beamsteer (created 0755, owned by the service user).
ConfigurationDirectory=${SERVICE_NAME}
# Hardening. No serial/USB devices; the PstRotator UDP port is unprivileged.
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
  # If the seed has an empty password (MQTT_PASSWORD not supplied at deploy time),
  # pull the shared hf MQTT password from an existing station service env on the
  # device and inject it into the freshly-installed config — so a re-seed is
  # self-sufficient and the password never leaves the Pi. We inject into the
  # INSTALLED config (in /etc, a non-sticky dir) rather than the /tmp seed
  # because fs.protected_regular blocks root from opening an other-user-owned
  # file in world-writable sticky /tmp for writing; the service-user-owned file
  # in /etc is writable by root (which is how the live config fix worked).
  if sudo grep -qE '^[[:space:]]*password[[:space:]]*=[[:space:]]*""' "$CONFIG_FILE"; then
    pw=""
    for f in \
      /etc/acom1200s-pa-bridge/acom1200s-pa-bridge.env \
      /etc/flexbridge/flexbridge.env \
      /etc/hadiscovery/hadiscovery.env \
      /etc/atr1k-tuner-bridge/atr1k-tuner-bridge.env ; do
      # These env files are 0600 owned by their service users; test readability via
      # sudo (not `[ -r ]`, which runs as the deploying user and skips every file).
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
cfg.write_text(t2)  # opens existing file in place — owner/mode (service user, 0600) preserved
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
echo ">> Done. beamsteer deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Logs:   ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Config: ssh ${SSH_TARGET} 'sudo -e ${CONFIG_FILE}'"