#!/usr/bin/env bash
#
# Deploy antmode to a Raspberry Pi and install it as a systemd service.
#
# antmode is the station antenna-mode logic slot (muehle/hf/ant-mode): it owns
# normal | storm | maintenance and publishes the retained lock that
# wrc-rotator-bridge and ultrabridge enforce. It polls BBK NINA (DWD warnings)
# and the DWD app backend (day wind) over HTTPS and persists its state in
# /var/lib/antmode/state.json (systemd StateDirectory). No serial device, no
# listener. See docs/ant-mode-mqtt-api.md.
#
# Usage:
#   ./deploy.sh                       # deploy to default host "scmino"
#   NINA_ARS=<12 digits> DWD_STATION=<id> ./deploy.sh   # first deploy elsewhere (seed)
#
# Configurable via environment variables (with defaults):
#   SSH_HOST          SSH target            (default: 192.168.1.178, scmino)
#   SSH_USER          SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME      systemd service name  (default: antmode)
#   SERVICE_USER      system user to run as (default: antmode)
#   INSTALL_DIR       remote install dir    (default: /opt/antmode)
#   BINARY            binary name           (default: antmode)
#
#   MQTT_BROKER       mqtt.broker     value (default: tcp://192.168.1.50:1883 — the live
#                     station broker)
#   MQTT_SITE         mqtt.site       value (default: muehle)
#   MQTT_STATION      mqtt.station    value (default: hf)
#   MQTT_SLOT         mqtt.slot       value (default: ant-mode)
#   MQTT_USER         mqtt.user       value (default: hf)
#   MQTT_PASSWORD     mqtt.password   value (default: empty -> auto-pulled on-device
#                     from an existing hf service env, so a re-seed is self-sufficient)
#   LOCATION          location        value (default: bauwagen)  [published in /meta]
#   HOST_NAME         host            value (default: scmino)    [published in /meta]
#   Site values (defaults = the Mühle, locator JO32WE; "" disables a poll):
#   NINA_ARS          nina.ars              (default: 055660040040 — Stadt Lengerich NRW,
#                     AGS 05566040, Kreis Steinfurt; DWD warncell 805566040; NINA
#                     queries the district 055660000000)
#   DWD_STATION       wind.dwd_station      (default: P0441 — MOSMIX "LENGERICH", 0.7 km
#                     from JO32WE)
#   FALLBACK_AZ       storm.fallback_az     (default: 150 — boom broadside to the
#                     prevailing SW–W storms; used when the wind is stale)
#   MAINT_AZ          maintenance.park_az   (default: 0 — mast lowering)
#   MAX_AZ            storm.max_az          (default: 360; 450 only once the
#                     G-450 overlap readback is verified)
#   AUTO_ENTER        storm.auto_enter      (default: false — observe only)
#   POWER_BORROW      power.borrow          (default: false)
#
# Configuration (including the MQTT password) lives in a single 0600 TOML file
# on the target, NOT in the systemd unit or process command line. The file is
# SEEDED ONCE on first deploy; later deploys leave it untouched. To change a
# setting, edit the file on the device (or delete it and redeploy to re-seed).
# The state file (/var/lib/antmode/state.json) is never touched by a deploy:
# it is the lock authority.
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.178}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-antmode}"
SERVICE_USER="${SERVICE_USER:-antmode}"
INSTALL_DIR="${INSTALL_DIR:-/opt/antmode}"
CONFIG_DIR="${CONFIG_DIR:-/etc/antmode}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
BINARY="${BINARY:-antmode}"
PKG="./cmd/antmode"

MQTT_BROKER="${MQTT_BROKER:-tcp://192.168.1.50:1883}"
MQTT_SITE="${MQTT_SITE:-muehle}"
MQTT_STATION="${MQTT_STATION:-hf}"
MQTT_SLOT="${MQTT_SLOT:-ant-mode}"
MQTT_USER="${MQTT_USER:-hf}"
MQTT_PASSWORD="${MQTT_PASSWORD:-}"
LOCATION="${LOCATION:-bauwagen}"
HOST_NAME="${HOST_NAME:-scmino}"
NINA_ARS="${NINA_ARS-055660040040}"
DWD_STATION="${DWD_STATION-P0441}"
FALLBACK_AZ="${FALLBACK_AZ:-150}"
MAINT_AZ="${MAINT_AZ:-0}"
MAX_AZ="${MAX_AZ:-360}"
AUTO_ENTER="${AUTO_ENTER:-false}"
POWER_BORROW="${POWER_BORROW:-false}"

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
# Written with umask 077 so the local temp copy is never world-readable. Keys
# not written here (timeouts, ii_codes, slots) take the binary's defaults;
# config.example.toml documents every key.
SEED_CONFIG="$(umask 077; mktemp)"
trap 'rm -f "$SEED_CONFIG" "${UNIT_FILE:-}"' EXIT
{
  echo "# antmode configuration. Contains the MQTT password -- keep this file 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo "# Every key and its default: config.example.toml in the repo."
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
  echo "[nina]"
  echo "# 12-digit Amtlicher Regionalschluessel (the district level is queried). Empty = no warning poll."
  echo "ars = \"$(toml_escape "$NINA_ARS")\""
  echo ""
  echo "[wind]"
  echo "# DWD station id (Stationskennung) for the day wind. Empty = storm parks at fallback_az."
  echo "dwd_station = \"$(toml_escape "$DWD_STATION")\""
  echo ""
  echo "[storm]"
  echo "fallback_az = ${FALLBACK_AZ}      # park azimuth when the wind is stale"
  echo "max_az      = ${MAX_AZ}    # 450 only once the G-450 overlap readback is verified"
  echo "auto_enter  = ${AUTO_ENTER}  # false = observe only (storm_suggested); plan Phase 6"
  echo ""
  echo "[maintenance]"
  echo "park_az = ${MAINT_AZ}          # azimuth for lowering the mast"
  echo ""
  echo "[power]"
  echo "borrow = ${POWER_BORROW}        # switch power/antennas on to park when it is off"
  echo ""
  echo "[state]"
  echo "path = \"/var/lib/${SERVICE_NAME}/state.json\""
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
Description=HF antenna mode (antmode)
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
# systemd manages /etc/antmode (config, 0755) and /var/lib/antmode (the
# persisted mode/lock/borrow state — the lock authority; it survives restarts
# and redeploys), both owned by the service user.
ConfigurationDirectory=${SERVICE_NAME}
StateDirectory=${SERVICE_NAME}
StateDirectoryMode=0700
# Hardening. No devices, no listener; outbound HTTPS (NINA, DWD) and MQTT only.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictSUIDSGID=true
LockPersonality=true

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
echo ">> Done. antmode deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Logs:   ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Config: ssh ${SSH_TARGET} 'sudo -e ${CONFIG_FILE}'"