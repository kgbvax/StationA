#!/usr/bin/env bash
#
# Deploy antennaselect to a Raspberry Pi and install it as a systemd service.
#
# antennaselect is the HF antenna-selection reconciler (integration model §5, §7.1):
# pure logic over MQTT — no serial device, no HTTP server. It subscribes to radio /
# station / switch / operator state and emits intent to ant-switch (and the
# controller band-follow). See docs/antenna-select-mqtt-api.md.
#
# Usage:
#   ./deploy.sh                       # deploy to default host "shari"
#   SSH_HOST=pi@shari.local ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST          SSH target            (default: 192.168.1.139)
#   SSH_USER          SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME      systemd service name  (default: antenna-select)
#   SERVICE_USER      system user to run as (default: antenna-select)
#   INSTALL_DIR       remote install dir    (default: /opt/antenna-select)
#   BINARY            binary name           (default: antennaselect)
#
#   MQTT_BROKER       mqtt.broker     value (default: tcp://192.168.1.50:1883)
#   MQTT_SITE         mqtt.site       value (default: muehle)
#   MQTT_STATION      mqtt.station    value (default: hf)
#   MQTT_SLOT         mqtt.slot       value (default: antenna-select)
#   MQTT_USER         mqtt.user      value (default: hf)
#   MQTT_PASSWORD     mqtt.password  value (default: empty -> auto-pulled on-device
#                     from an existing hf service env, so a re-seed is self-sufficient)
#   LOCATION          location       value (default: bauwagen)  [published in /meta]
#   HOST_NAME         host           value (default: shari)     [published in /meta]
#   BAND_FOLLOW_RESOURCE  band_follow.resource (default: ultrabeam; empty disables)
#   BAND_FOLLOW_SLOT      band_follow.slot     (default: ant-ctrl)
#   PA_FOLLOW_ENABLED     pa_follow.enabled   (default: true; set false to disable)
#   PA_FOLLOW_SLOT        pa_follow.slot       (default: pa)
#   TUNER_FOLLOW_ENABLED  tuner_follow.enabled  (default: true; set false to disable)
#   TUNER_FOLLOW_SLOT     tuner_follow.slot     (default: tuner)
#   TUNER_FOLLOW_RESOURCE tuner_follow.resource (default: fan-dipole)
#   TUNER_FOLLOW_ATU_BANDS tuner_follow.atu_bands (default: 30m,60m,80m,160m; comma-separated)
#   IDLE_TIMEOUT_MINUTES  idle.timeout_minutes (default: 30)
#
# Configuration (including the MQTT password) lives in a single 0600 TOML file
# on the target, NOT in the systemd unit or process command line. The file is
# SEEDED ONCE on first deploy from the variables above plus the baked antenna
# arrangement (wiring_map / band_policy, which reflect the Mühle HF station as
# wired at deploy time); subsequent deploys leave the on-device file untouched so
# the Pi owns its own settings. To change a setting after the first deploy, edit
# the file on the device (or delete it and redeploy to re-seed).
#
# When MQTT_PASSWORD is not supplied, the seed is written with an empty password
# and the remote install step pulls the shared hf MQTT password from an existing
# station service env ON THE DEVICE and injects it into the seed before
# installing — so the password never leaves the Pi and a re-seed is self-sufficient.
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-antenna-select}"
SERVICE_USER="${SERVICE_USER:-antenna-select}"
INSTALL_DIR="${INSTALL_DIR:-/opt/antenna-select}"
CONFIG_DIR="${CONFIG_DIR:-/etc/antenna-select}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
BINARY="${BINARY:-antennaselect}"
PKG="./cmd/antennaselect"

MQTT_BROKER="${MQTT_BROKER:-tcp://192.168.1.50:1883}"
MQTT_SITE="${MQTT_SITE:-muehle}"
MQTT_STATION="${MQTT_STATION:-hf}"
MQTT_SLOT="${MQTT_SLOT:-antenna-select}"
MQTT_USER="${MQTT_USER:-hf}"
MQTT_PASSWORD="${MQTT_PASSWORD:-}"
LOCATION="${LOCATION:-bauwagen}"
HOST_NAME="${HOST_NAME:-shari}"
BAND_FOLLOW_RESOURCE="${BAND_FOLLOW_RESOURCE:-ultrabeam}"
BAND_FOLLOW_SLOT="${BAND_FOLLOW_SLOT:-ant-ctrl}"
PA_FOLLOW_ENABLED="${PA_FOLLOW_ENABLED:-true}"
PA_FOLLOW_SLOT="${PA_FOLLOW_SLOT:-pa}"
TUNER_FOLLOW_ENABLED="${TUNER_FOLLOW_ENABLED:-true}"
TUNER_FOLLOW_SLOT="${TUNER_FOLLOW_SLOT:-tuner}"
TUNER_FOLLOW_RESOURCE="${TUNER_FOLLOW_RESOURCE:-fan-dipole}"
TUNER_FOLLOW_ATU_BANDS="${TUNER_FOLLOW_ATU_BANDS:-30m,60m,80m,160m}"
IDLE_TIMEOUT_MINUTES="${IDLE_TIMEOUT_MINUTES:-30}"

# Render the comma-separated ATU bands list as a TOML array (["30m", "60m", "160m"]).
atu_bands_toml() {
  local out="[" first=1 band
  IFS=',' read -ra _bands <<<"$TUNER_FOLLOW_ATU_BANDS"
  for band in "${_bands[@]}"; do
    band="$(echo "$band" | xargs)"  # trim whitespace
    [[ -z "$band" ]] && continue
    if [[ $first -eq 1 ]]; then first=0; else out+=", "; fi
    out+="\"$(toml_escape "$band")\""
  done
  out+="]"
  printf '%s' "$out"
}

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
  echo "# antennaselect configuration. Contains the MQTT password -- keep this file 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo "# The antenna arrangement (wiring_map, band_policy) reflects the Mühle HF"
  echo "# station as wired at deploy time; edit here if the physical setup changes."
  echo "# The priority ladder is fixed in code (integration model §5); not configured."
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
  echo "[wiring_map]"
  echo "port1 = \"dummy-load\""
  echo "port4 = \"ultrabeam\"      # Ultrabeam -- controlled by the ant-ctrl slot"
  echo "port6 = \"fan-dipole\"     # 80/40 fan dipole on the HF mast (passive)"
  echo "off   = \"grounded\""
  echo ""
  echo "[band_policy]"
  echo "# Unmatched bands (incl. 160m -- no resonant antenna) route to fallback."
  echo "# 30/60/80/160m on the fan dipole are non-resonant -- the ATU is engaged in-line for"
  echo "# those bands by [tuner_follow] (model §7.1, §10 residual closed)."
  echo "fallback = \"fan-dipole\""
  echo ""
  echo "[band_policy.bands]"
  echo "ultrabeam  = [\"6m\", \"10m\", \"12m\", \"15m\", \"17m\", \"20m\"]"
  echo "fan-dipole = [\"30m\", \"40m\", \"60m\", \"80m\"]"
  echo ""
  echo "[band_follow]"
  echo "# Controller map (model §4): drive slot to track radio freq while resource is"
  echo "# the selected antenna. Empty resource disables."
  echo "resource = \"$(toml_escape "$BAND_FOLLOW_RESOURCE")\""
  echo "slot     = \"$(toml_escape "$BAND_FOLLOW_SLOT")\""
  echo ""
  echo "[pa_follow]"
  echo "# PA band-follow (model §7.1 soft binding pa.set_band <- radio.band): pre-position"
  echo "# the ACOM to the radio's band so the amp (auto-bands by RF sense) doesn't trip on"
  echo "# the 1st TX on a new band. NOT gated on antenna selection (PA is always in the RF"
  echo "# path). No TX guard: hot-switch protection is hardware. Emit is NOT retained."
  echo "enabled = ${PA_FOLLOW_ENABLED}"
  echo "slot    = \"$(toml_escape "$PA_FOLLOW_SLOT")\""
  echo ""
  echo "[tuner_follow]"
  echo "# Tuner in-line follow (model §7.1 soft binding tuner.set_inline <- band_policy):"
  echo '# engage the ATU in-line when `resource` is the selected antenna AND the band is in'
  echo "# atu_bands (the non-resonant bands); bypass otherwise. Gated on antenna selection"
  echo "# (the ATU only matters when its resource is in circuit). Emit is NOT retained."
  echo "enabled   = ${TUNER_FOLLOW_ENABLED}"
  echo "slot      = \"$(toml_escape "$TUNER_FOLLOW_SLOT")\""
  echo "resource  = \"$(toml_escape "$TUNER_FOLLOW_RESOURCE")\""
  echo "atu_bands = $(atu_bands_toml)"
  echo ""
  echo "[idle]"
  echo "# Walk-away safety (model §10): after this many minutes with no radio activity"
  echo "# (a VFO/frequency change or a transmit), the reconciler grounds the antenna"
  echo "# (target = off). The switch's off position shorts the open ports to ground."
  echo "timeout_minutes = ${IDLE_TIMEOUT_MINUTES}"
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
Description=HF antenna-selection reconciler (antennaselect)
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
# systemd manages /etc/antenna-select (created 0755, owned by the service user).
ConfigurationDirectory=${SERVICE_NAME}
# Hardening. No serial/USB devices and no HTTP server, so no DeviceAllow /
# SupplementaryGroups / port binding is needed.
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
echo ">> Done. antennaselect deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Logs:   ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Config: ssh ${SSH_TARGET} 'sudo -e ${CONFIG_FILE}'"