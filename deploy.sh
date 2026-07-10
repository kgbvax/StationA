#!/usr/bin/env bash
#
# Deploy acombridge (the ACOM 1200S PA bridge, binary `acombridge`) to a
# Raspberry Pi and install it as a hardened systemd service.
#
# acombridge is a serial bridge: it reads the ACOM 600S/1200S proprietary
# telemetry protocol over a USB-serial adapter (Prolific, vendor 067b) at
# 9600 8N1 and publishes canonical PA state to MQTT on the station three-plane
# topics (muehle/hf/pa/{meta,state,status,cmd}). It also subscribes to /cmd
# (set_mode, set_band). Because it opens /dev/tty* devices, the unit needs
# serial group membership, DeviceAllow rules, and a udev rule pinning the
# adapter's tty device to the dialout group — but can otherwise run under a
# strict sandbox (no PrivateDevices, since it needs the serial char devices).
#
# Usage:
#   ./deploy.sh                       # deploy to default host "shari"
#   SSH_HOST=pi@shari.local ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST        SSH target            (default: 192.168.1.139)
#   SSH_USER        SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME    systemd service name  (default: acombridge)
#   SERVICE_USER    system user to run as (default: acombridge)
#   SERIAL_GROUP    group owning serial   (default: dialout)
#   SERIAL_USB_VENDOR USB vendor id for the udev serial-group rule (default: 067b=Prolific; empty=skip)
#   INSTALL_DIR     remote install dir    (default: /opt/acombridge)
#   BINARY          binary name           (default: acombridge)
#
#   SERIAL_PORT     serial.port value        (default: /dev/serial/by-id/usb-Prolific_..._if00-port0)
#   AVG_TIME_MS     serial.avg_time_ms value (default: 300)
#   LOCATION        location value           (default: bauwagen)  [published in /meta]
#   HOST_NAME       host value               (default: shari)     [published in /meta]
#   DEVICE_MODEL    device.model value       (default: ACOM 1200S)
#   DEVICE_SERIAL   device.serial value      (default: empty -> bridge derives "acom-1200s")
#   DEVICE_LINK     device.link value        (default: serial)
#   LOG_LEVEL       log.level value          (default: info)
#   MQTT_BROKER     mqtt.broker value        (default: tcp://192.168.1.50:1883)
#   MQTT_SITE       mqtt.site                (default: muehle)
#   MQTT_STATION    mqtt.station             (default: hf)
#   MQTT_SLOT       mqtt.slot                (default: pa)
#   MQTT_USER       mqtt.user                (default: hf)
#   MQTT_PASSWORD   ACOMBRIDGE_MQTT_PASSWORD (default: empty -> set on device)
#   DISCOVERY_PREFIX mqtt.discovery_prefix   (default: homeassistant)
#
# Configuration lives in a 0600 TOML file on the target
# (/etc/acombridge/config.toml); the MQTT password is NOT in the TOML — it is
# loaded from an EnvironmentFile (/etc/acombridge/acombridge.env, 0600) so it
# never appears in the unit file or process command line. Both files are
# SEEDED ONCE on first deploy from the variables above; subsequent deploys
# leave the on-device files untouched so the Pi owns its own settings. To
# change a setting after the first deploy, edit the file on the device (or
# delete it and redeploy to re-seed).
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-acombridge}"
SERVICE_USER="${SERVICE_USER:-acombridge}"
SERIAL_GROUP="${SERIAL_GROUP:-dialout}"
# USB vendor id of the serial adapter. A udev rule forces matching tty devices
# into SERIAL_GROUP so the service user can always open them, regardless of the
# distro's default. Default 067b = Prolific (the ACOM's adapter). Set empty to
# skip installing the udev rule.
SERIAL_USB_VENDOR="${SERIAL_USB_VENDOR:-067b}"
INSTALL_DIR="${INSTALL_DIR:-/opt/acombridge}"
CONFIG_DIR="${CONFIG_DIR:-/etc/acombridge}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
ENV_FILE="${ENV_FILE:-${CONFIG_DIR}/acombridge.env}"
BINARY="${BINARY:-acombridge}"
PKG="./cmd/acombridge"

SERIAL_PORT="${SERIAL_PORT:-/dev/serial/by-id/usb-Prolific_Technology_Inc._USB-Serial_Controller_D-if00-port0}"
AVG_TIME_MS="${AVG_TIME_MS:-300}"
LOCATION="${LOCATION:-bauwagen}"
HOST_NAME="${HOST_NAME:-shari}"
DEVICE_MODEL="${DEVICE_MODEL:-ACOM 1200S}"
DEVICE_SERIAL="${DEVICE_SERIAL:-}"
DEVICE_LINK="${DEVICE_LINK:-serial}"
LOG_LEVEL="${LOG_LEVEL:-info}"
MQTT_BROKER="${MQTT_BROKER:-tcp://192.168.1.50:1883}"
MQTT_SITE="${MQTT_SITE:-muehle}"
MQTT_STATION="${MQTT_STATION:-hf}"
MQTT_SLOT="${MQTT_SLOT:-pa}"
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
  echo "# acombridge configuration. Sensitive values are NOT here — the MQTT"
  echo "# password lives in the EnvironmentFile (acombridge.env). Keep both 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo ""
  echo "host = \"$(toml_escape "$HOST_NAME")\""
  echo ""
  echo "[serial]"
  echo "# Prolific USB-serial adapter (vendor 067b). The udev rule pins this tty"
  echo "# to the dialout group so the service user can always open it."
  echo "port        = \"$(toml_escape "$SERIAL_PORT")\""
  echo "avg_time_ms = ${AVG_TIME_MS}"
  echo ""
  echo "[device]"
  echo "# The ACOM serial protocol reports no serial number; device.serial is a"
  echo "# stable configured id (left empty -> bridge derives \"acom-1200s\")."
  echo "model  = \"$(toml_escape "$DEVICE_MODEL")\""
  echo "serial = \"$(toml_escape "$DEVICE_SERIAL")\""
  echo "link   = \"$(toml_escape "$DEVICE_LINK")\""
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
  echo "# password is loaded from ACOMBRIDGE_MQTT_PASSWORD in acombridge.env, not here."
  echo "password         = \"\""
  echo ""
  echo "[log]"
  echo "level = \"$(toml_escape "$LOG_LEVEL")\""
} > "$SEED_CONFIG"

{
  echo "# acombridge EnvironmentFile (read by the systemd unit). Keep 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change the password."
  if [[ -n "$MQTT_PASSWORD" ]]; then
    echo "ACOMBRIDGE_MQTT_PASSWORD=\"$(toml_escape "$MQTT_PASSWORD")\""
  else
    echo "# ACOMBRIDGE_MQTT_PASSWORD=\"...\"   # set on the device (copy from another hf service config)"
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
Description=ACOM 1200S PA to MQTT bridge (acombridge)
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
# Run as a dedicated unprivileged user...
User=${SERVICE_USER}
Group=${SERVICE_USER}
# ...that is a member of the serial group so it can open /dev/tty* devices.
SupplementaryGroups=${SERIAL_GROUP}
# systemd owns /etc/acombridge (created 0755, owned by the service user).
ConfigurationDirectory=${SERVICE_NAME}
StateDirectory=${SERVICE_NAME}

# Hardening. acombridge needs the serial char devices (no PrivateDevices) and
# outbound TCP to the MQTT broker — nothing else: no disk writes, no elevated
# capabilities, no inbound sockets.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
# NOTE: no PrivateDevices — the bridge must open /dev/ttyUSB* (the ACOM adapter).
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
# Grant access to serial / USB-serial character devices under the sandbox.
DeviceAllow=char-ttyUSB rw
DeviceAllow=char-ttyACM rw
DeviceAllow=char-tty rw
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
ssh "$SSH_TARGET" "INSTALL_DIR='${INSTALL_DIR}' BINARY='${BINARY}' SERVICE_NAME='${SERVICE_NAME}' SERVICE_USER='${SERVICE_USER}' SERIAL_GROUP='${SERIAL_GROUP}' SERIAL_USB_VENDOR='${SERIAL_USB_VENDOR}' CONFIG_DIR='${CONFIG_DIR}' CONFIG_FILE='${CONFIG_FILE}' ENV_FILE='${ENV_FILE}' bash -s" <<'REMOTE'
set -euo pipefail
SEED_CFG="/tmp/${SERVICE_NAME}.config.seed"
SEED_ENV="/tmp/${SERVICE_NAME}.env.seed"
# Always remove the transferred seeds (the env one carries the secret) when done.
trap 'rm -f "$SEED_CFG" "$SEED_ENV"' EXIT
# Create a dedicated system user/group (no login, no home) if missing.
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi
# Make sure the serial group exists and the service user is a member of it.
getent group "$SERIAL_GROUP" >/dev/null 2>&1 || sudo groupadd --system "$SERIAL_GROUP"
sudo usermod -aG "$SERIAL_GROUP" "$SERVICE_USER"
# Install a udev rule so the USB-serial adapter is owned by SERIAL_GROUP. The
# ACOM uses a Prolific adapter (vendor 067b); some distros assign its tty device
# to a group the service user isn't in, which would deny access — this pins it.
if [ -n "$SERIAL_USB_VENDOR" ]; then
  printf 'SUBSYSTEM=="tty", SUBSYSTEMS=="usb", ATTRS{idVendor}=="%s", GROUP="%s", MODE="0660"\n' \
    "$SERIAL_USB_VENDOR" "$SERIAL_GROUP" | sudo tee /etc/udev/rules.d/99-acombridge-serial.rules >/dev/null
  sudo udevadm control --reload-rules
  sudo udevadm trigger --subsystem-match=tty
  echo "   installed udev rule: Prolific/vendor $SERIAL_USB_VENDOR tty -> group $SERIAL_GROUP."
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
  echo "   !! Set ACOMBRIDGE_MQTT_PASSWORD in $ENV_FILE before relying on the bridge."
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
echo ">> Done. acombridge deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Logs:    ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Config:  ${CONFIG_FILE}"
echo "   Secret:  ${ENV_FILE}  (set ACOMBRIDGE_MQTT_PASSWORD if not seeded)"
echo "   Topics:  muehle/hf/pa/{meta,state,status,cmd}"