#!/usr/bin/env bash
#
# Deploy spid-ercm-rotator-bridge (the SPID az + ERC-M/GS-500 el rotator bridge,
# binary `spid-ercm-rotator-bridge`) to a Raspberry Pi and install it as a
# hardened systemd service.
#
# spid-ercm-rotator-bridge is a compound bridge: one process fronts TWO rotator
# slots (muehle/uhf/az-rotator, SPID Rot1Prog over serial at 1200 baud;
# muehle/uhf/el-rotator, GS-500 elevation via the ERC-M controller speaking the
# GS-232B dialect at 9600 baud), plus a rotctld TCP server (:4534) and a
# PstRotator UDP listener (:12041) fed from the same dispatch core. The unit
# therefore needs a NEW hardening combination (plan KTD3): serial access forbids
# PrivateDevices, so it takes ultrabridge's SupplementaryGroups=dialout +
# DeviceAllow + udev rules, atr1k's remaining hardening with MemoryMax/TasksMax,
# and wrc's RestrictAddressFamilies for the inbound TCP/UDP listeners.
#
# Usage:
#   ./deploy.sh                       # deploy to default host "shari"
#   SSH_HOST=pi@shari.local ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST        SSH target            (default: 192.168.1.139)
#   SSH_USER        SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME    systemd service name  (default: spid-ercm-rotator-bridge)
#   SERVICE_USER    system user to run as (default: spid-ercm-rotator-bridge)
#   INSTALL_DIR     remote install dir    (default: /opt/spid-ercm-rotator-bridge)
#   BINARY          binary name           (default: spid-ercm-rotator-bridge)
#
#   HOST_NAME       host value            (default: shari)     [published in /meta]
#   LOCATION        mqtt.location value   (default: bauwagen)
#   LOG_LEVEL       log.level value       (default: info)
#   MQTT_BROKER     mqtt.broker value     (default: tcp://127.0.0.1:1883)
#   MQTT_SITE       mqtt.site             (default: muehle)
#   MQTT_STATION    mqtt.station          (default: uhf)
#   MQTT_USER       mqtt.user             (default: hf)
#   MQTT_PASSWORD   SPID_ERCM_ROTATOR_BRIDGE_MQTT_PASSWORD (default: empty -> set on device)
#
#   AZ_PORT         az slot serial port   (default: empty -> MOCK mode, KTD7)
#   AZ_BAUD         az slot baud          (default: 1200 — Rot1Prog fixed, KTD5)
#   AZ_DEVICE_MODEL az device.model       (default: "SPID Rotor (Rot1Prog)")
#   EL_PORT         el slot serial port   (default: empty -> MOCK mode, KTD7)
#   EL_BAUD         el slot baud          (default: 9600 — ERC-M GS-232B, KTD6)
#   EL_DEVICE_MODEL el device.model       (default: "ERC-M / GS-500")
#
#   ROTCTLD_PORT    rotctld TCP port      (default: 4534)
#   PSTROTATOR_PORT pstrotator UDP port   (default: 12041)
#   POLL_INTERVAL   control.poll_interval    (default: 1s)
#   REOPEN_COOLDOWN control.reopen_cooldown (default: 2s)
#   AZ_MIN/AZ_MAX   az travel limits      (defaults: 0 / 360 — set the real
#   EL_MIN/EL_MAX   el travel limits       mechanical range at bench bring-up)
#   AZ_PARK/EL_PARK park positions        (default: 0)
#
#   SERIAL_GROUP      group owning the tty devices (default: dialout)
#   SERIAL_USB_VENDORS space-separated USB vendor ids for the udev serial-group
#                     rules (default: "0403" = FTDI). TWO adapters usually mean
#                     TWO vendors or two distinct by-id paths — list every
#                     vendor the adapters use, e.g. SERIAL_USB_VENDORS="0403 067b".
#                     Set empty to skip installing the udev rules.
#
# Configuration lives in a 0600 TOML file on the target
# (/etc/spid-ercm-rotator-bridge/config.toml); the MQTT password is NOT in the
# TOML (a `password` key there is a hard parse error) — it is loaded from an
# EnvironmentFile (/etc/spid-ercm-rotator-bridge/spid-ercm-rotator-bridge.env,
# 0600) so it never appears in the unit file or process command line. Both files
# are SEEDED ONCE on first deploy from the variables above; subsequent deploys
# leave the on-device files untouched so the Pi owns its own settings. To change
# a setting after the first deploy, edit the file on the device (or delete it
# and redeploy to re-seed).
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-spid-ercm-rotator-bridge}"
SERVICE_USER="${SERVICE_USER:-spid-ercm-rotator-bridge}"
INSTALL_DIR="${INSTALL_DIR:-/opt/spid-ercm-rotator-bridge}"
CONFIG_DIR="${CONFIG_DIR:-/etc/spid-ercm-rotator-bridge}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
ENV_FILE="${ENV_FILE:-${CONFIG_DIR}/spid-ercm-rotator-bridge.env}"
BINARY="${BINARY:-spid-ercm-rotator-bridge}"
PKG="./cmd/spid-ercm-rotator-bridge"

HOST_NAME="${HOST_NAME:-shari}"
LOCATION="${LOCATION:-bauwagen}"
LOG_LEVEL="${LOG_LEVEL:-info}"
MQTT_BROKER="${MQTT_BROKER:-tcp://127.0.0.1:1883}"
MQTT_SITE="${MQTT_SITE:-muehle}"
MQTT_STATION="${MQTT_STATION:-uhf}"
MQTT_USER="${MQTT_USER:-hf}"
MQTT_PASSWORD="${MQTT_PASSWORD:-}"

# Per-axis slots. Empty ports seed MOCK mode (KTD7) so the first deploy can
# land before the bench bring-up pins the real /dev/serial/by-id/ identities.
AZ_PORT="${AZ_PORT:-}"
AZ_BAUD="${AZ_BAUD:-1200}"
AZ_DEVICE_MODEL="${AZ_DEVICE_MODEL:-SPID Rotor (Rot1Prog)}"
EL_PORT="${EL_PORT:-}"
EL_BAUD="${EL_BAUD:-9600}"
EL_DEVICE_MODEL="${EL_DEVICE_MODEL:-ERC-M / GS-500}"

ROTCTLD_PORT="${ROTCTLD_PORT:-4534}"
PSTROTATOR_PORT="${PSTROTATOR_PORT:-12041}"
POLL_INTERVAL="${POLL_INTERVAL:-1s}"
REOPEN_COOLDOWN="${REOPEN_COOLDOWN:-2s}"
AZ_MIN="${AZ_MIN:-0}"
AZ_MAX="${AZ_MAX:-360}"
AZ_PARK="${AZ_PARK:-0}"
EL_MIN="${EL_MIN:-0}"
EL_MAX="${EL_MAX:-90}"
EL_PARK="${EL_PARK:-0}"

SERIAL_GROUP="${SERIAL_GROUP:-dialout}"
# Space-separated USB vendor ids — one udev rule per vendor (the repo template
# pins a single vendor; two adapters may be two vendors, plan Outstanding
# Questions). Default 0403 = FTDI. Empty = skip the udev rules.
SERIAL_USB_VENDORS="${SERIAL_USB_VENDORS:-0403}"

# Allow "user@host" in SSH_HOST; otherwise prepend SSH_USER.
if [[ "$SSH_HOST" == *"@"* ]]; then
  SSH_TARGET="$SSH_HOST"
else
  SSH_TARGET="${SSH_USER}@${SSH_HOST}"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# --- TOML/env escaping helper ------------------------------------------------
# Escape backslash and double quotes for a TOML basic string or a systemd
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
trap 'rm -f "$SEED_CONFIG" "$SEED_ENV" "${UNIT_FILE:-}" "${UDEV_FILE:-}"' EXIT
{
  echo "# spid-ercm-rotator-bridge configuration. Sensitive values are NOT here — the MQTT"
  echo "# password lives in the EnvironmentFile (spid-ercm-rotator-bridge.env) and a"
  echo "# \`password\` key in this file is a hard parse error. Keep both 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change settings."
  echo ""
  echo "host = \"$(toml_escape "$HOST_NAME")\""
  echo ""
  echo "[mqtt]"
  echo "# client_id defaults to \"<site>-<station>-<slot>\" PER slot (each [[slot]] gets"
  echo "# its own client and LWT) when empty."
  echo "broker   = \"$(toml_escape "$MQTT_BROKER")\""
  echo "client_id = \"\""
  echo "site     = \"$(toml_escape "$MQTT_SITE")\""
  echo "station  = \"$(toml_escape "$MQTT_STATION")\""
  echo "location = \"$(toml_escape "$LOCATION")\""
  echo "user     = \"$(toml_escape "$MQTT_USER")\""
  echo "# password is loaded from SPID_ERCM_ROTATOR_BRIDGE_MQTT_PASSWORD in the EnvironmentFile, not here."
  echo ""
  echo "[log]"
  echo "level = \"$(toml_escape "$LOG_LEVEL")\""
  echo ""
  echo "[rotctld]"
  echo "# rotctld-compatible TCP server (pelcobridge2 dialect, KTD10). No-auth LAN"
  echo "# posture is the reviewed station decision (KTD4)."
  echo "bind = \"0.0.0.0\""
  echo "port = ${ROTCTLD_PORT}"
  echo ""
  echo "[pstrotator]"
  echo "# PstRotator native UDP listener (KTD11); replies go to the source IP at"
  echo "# listen-port+1."
  echo "bind = \"0.0.0.0\""
  echo "port = ${PSTROTATOR_PORT}"
  echo ""
  echo "[[slot]]"
  echo "axis         = \"az\""
  echo "slot         = \"az-rotator\""
  echo "device_model = \"$(toml_escape "$AZ_DEVICE_MODEL")\""
  echo "link         = \"serial\""
  echo "  [slot.serial]"
  if [[ -n "$AZ_PORT" ]]; then
    echo "  # Stable by-id path — the self-heal reopen re-resolves it (KTD7)."
    echo "  port = \"$(toml_escape "$AZ_PORT")\""
  else
    echo "  # Empty port = in-process mock device (KTD7) — set the real by-id path here."
    echo "  port = \"\""
  fi
  echo "  baud = ${AZ_BAUD}"
  echo ""
  echo "[[slot]]"
  echo "axis         = \"el\""
  echo "slot         = \"el-rotator\""
  echo "device_model = \"$(toml_escape "$EL_DEVICE_MODEL")\""
  echo "link         = \"serial\""
  echo "  [slot.serial]"
  if [[ -n "$EL_PORT" ]]; then
    echo "  # Stable by-id path — the self-heal reopen re-resolves it (KTD7)."
    echo "  port = \"$(toml_escape "$EL_PORT")\""
  else
    echo "  # Empty port = in-process mock device (KTD7) — set the real by-id path here."
    echo "  port = \"\""
  fi
  echo "  baud = ${EL_BAUD}"
  echo ""
  echo "[control]"
  echo "poll_interval   = \"$(toml_escape "$POLL_INTERVAL")\""
  echo "reopen_cooldown = \"$(toml_escape "$REOPEN_COOLDOWN")\""
  echo ""
  echo "[control.az]"
  echo "min      = ${AZ_MIN}"
  echo "max      = ${AZ_MAX}"
  echo "deadband = 1.0"
  echo "park     = ${AZ_PARK}"
  echo ""
  echo "[control.el]"
  echo "min      = ${EL_MIN}"
  echo "max      = ${EL_MAX}"
  echo "deadband = 1.0"
  echo "park     = ${EL_PARK}"
} > "$SEED_CONFIG"

{
  echo "# spid-ercm-rotator-bridge EnvironmentFile (read by the systemd unit). Keep 0600."
  echo "# Seeded by deploy.sh on first deploy; edit here to change the password."
  if [[ -n "$MQTT_PASSWORD" ]]; then
    echo "SPID_ERCM_ROTATOR_BRIDGE_MQTT_PASSWORD=\"$(toml_escape "$MQTT_PASSWORD")\""
  else
    echo "# SPID_ERCM_ROTATOR_BRIDGE_MQTT_PASSWORD=\"...\"   # set on the device (copy from another hf service config)"
  fi
} > "$SEED_ENV"

# --- generate the udev rules (one rule per USB vendor, KTD3) -----------------
# The repo template pins a single vendor; this bridge may have two different
# USB-serial adapters (SPID + ERC-M), so the vendor list drives the rules.
UDEV_FILE=""
if [[ -n "$SERIAL_USB_VENDORS" ]]; then
  UDEV_FILE="$(mktemp)"
  for vendor in $SERIAL_USB_VENDORS; do
    printf 'SUBSYSTEM=="tty", SUBSYSTEMS=="usb", ATTRS{idVendor}=="%s", GROUP="%s", MODE="0660"\n' \
      "$vendor" "$SERIAL_GROUP" >> "$UDEV_FILE"
  done
fi

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
Description=SPID az + ERC-M el rotator bridge (spid-ercm-rotator-bridge)
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
# systemd owns /etc/spid-ercm-rotator-bridge (created 0755, owned by the service user).
ConfigurationDirectory=${SERVICE_NAME}
StateDirectory=${SERVICE_NAME}

# Hardening (plan KTD3 — a new combination): serial access forbids
# PrivateDevices (unlike the network-only bridges), so the unit takes
# ultrabridge's serial grants, atr1k's remaining hardening with
# MemoryMax/TasksMax, and wrc's RestrictAddressFamilies — the bridge both
# opens /dev/ttyUSB* and LISTENS on TCP :${ROTCTLD_PORT} + UDP :${PSTROTATOR_PORT}.
SupplementaryGroups=${SERIAL_GROUP}
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
DeviceAllow=char-ttyUSB rw
DeviceAllow=char-ttyACM rw
DeviceAllow=char-tty rw
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
if [[ -n "$UDEV_FILE" ]]; then
  scp "$UDEV_FILE" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.udev.rules"
fi

# --- install remotely -------------------------------------------------------
echo ">> Installing on ${SSH_TARGET}..."
ssh "$SSH_TARGET" "INSTALL_DIR='${INSTALL_DIR}' BINARY='${BINARY}' SERVICE_NAME='${SERVICE_NAME}' SERVICE_USER='${SERVICE_USER}' SERIAL_GROUP='${SERIAL_GROUP}' INSTALL_UDEV='$([[ -n "$UDEV_FILE" ]] && echo yes || echo no)' CONFIG_DIR='${CONFIG_DIR}' CONFIG_FILE='${CONFIG_FILE}' ENV_FILE='${ENV_FILE}' bash -s" <<'REMOTE'
set -euo pipefail
SEED_CFG="/tmp/${SERVICE_NAME}.config.seed"
SEED_ENV="/tmp/${SERVICE_NAME}.env.seed"
UDEV_RULES="/tmp/${SERVICE_NAME}.udev.rules"
# Always remove the transferred seeds (the env one carries the secret) when done.
trap 'rm -f "$SEED_CFG" "$SEED_ENV" "$UDEV_RULES"' EXIT
# Create a dedicated system user/group (no login, no home) if missing.
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi
# Make sure the serial group exists and the service user is a member of it.
getent group "$SERIAL_GROUP" >/dev/null 2>&1 || sudo groupadd --system "$SERIAL_GROUP"
sudo usermod -aG "$SERIAL_GROUP" "$SERVICE_USER"
# Install the udev rules so every USB-serial adapter's tty lands in SERIAL_GROUP.
# Some distros assign tty devices to a group the service user isn't in, which
# would deny access — the rules pin them. deploy.sh generates ONE rule PER USB
# vendor (two adapters may be two vendors).
if [ "$INSTALL_UDEV" = "yes" ]; then
  sudo install -m 0644 "$UDEV_RULES" "/etc/udev/rules.d/99-${SERVICE_NAME}-serial.rules"
  sudo udevadm control --reload-rules
  sudo udevadm trigger --subsystem-match=tty
  echo "   installed udev rules: /etc/udev/rules.d/99-${SERVICE_NAME}-serial.rules."
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
  echo "   !! Set SPID_ERCM_ROTATOR_BRIDGE_MQTT_PASSWORD in $ENV_FILE before relying on the bridge."
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
echo ">> Done. spid-ercm-rotator-bridge deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Logs:    ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Config:  ${CONFIG_FILE}"
echo "   Secret:  ${ENV_FILE}  (set SPID_ERCM_ROTATOR_BRIDGE_MQTT_PASSWORD if not seeded)"
echo "   Topics:  muehle/uhf/az-rotator/{meta,state,status,cmd} + muehle/uhf/el-rotator/{meta,state,status,cmd}"
echo "   Listeners: rotctld TCP :${ROTCTLD_PORT}, PstRotator UDP :${PSTROTATOR_PORT}"
echo "   NOTE: this is the U1 scaffold — the binary logs its config and waits; no"
echo "   device behavior is wired yet (U2-U7)."