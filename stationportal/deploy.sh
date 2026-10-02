#!/usr/bin/env bash
#
# Deploy stationportal (the station landing page, binary `stationportal`) to
# scmino (192.168.1.178) as a hardened systemd service on port 80.
#
# stationportal is a passive bus consumer (no slot, no publishes) plus an HTTP
# server. It needs: inbound TCP 80, outbound TCP to the MQTT broker and to the
# probed service links. Binding port 80 as an unprivileged user takes exactly
# one capability, CAP_NET_BIND_SERVICE — nothing else.
#
# Usage:
#   ./deploy.sh                       # deploy to default host scmino
#   SSH_HOST=io@192.168.1.178 ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST        SSH target            (default: 192.168.1.178 = scmino)
#   SSH_USER        SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME    systemd service name  (default: stationportal)
#   SERVICE_USER    system user to run as (default: stationportal)
#   INSTALL_DIR     remote install dir    (default: /opt/stationportal)
#   HTTP_ADDR       http_addr value       (default: :80)
#   MQTT_BROKER     mqtt.broker value     (default: tcp://192.168.1.50:1883 — the live
#                   broker; NOT 127.0.0.1, which on scmino is the not-yet-used mirror)
#   MQTT_USER       mqtt.user value       (default: hf)
#
# Config is a 0600 TOML (/etc/stationportal/config.toml); the MQTT password is NOT
# in it — it lives in an EnvironmentFile (/etc/stationportal/stationportal.env,
# 0600), pulled on-device from an existing hf service env so it never leaves the
# host. Both files are SEEDED ONCE; later deploys leave them untouched. The
# inventory (links, hosts, slots, software) is built into the binary from
# internal/inventory/inventory.toml — a redeploy updates it.
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.178}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-stationportal}"
SERVICE_USER="${SERVICE_USER:-stationportal}"
INSTALL_DIR="${INSTALL_DIR:-/opt/stationportal}"
CONFIG_DIR="${CONFIG_DIR:-/etc/stationportal}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
ENV_FILE="${ENV_FILE:-${CONFIG_DIR}/stationportal.env}"
BINARY="${BINARY:-stationportal}"
PKG="./cmd/stationportal"

HTTP_ADDR="${HTTP_ADDR:-:80}"
MQTT_BROKER="${MQTT_BROKER:-tcp://192.168.1.50:1883}"
MQTT_USER="${MQTT_USER:-hf}"

if [[ "$SSH_HOST" == *"@"* ]]; then
  SSH_TARGET="$SSH_HOST"
else
  SSH_TARGET="${SSH_USER}@${SSH_HOST}"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

toml_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '%s' "$s"
}

# --- seed config (used only if none exists on target) ------------------------
SEED_CONFIG="$(umask 077; mktemp)"
trap 'rm -f "$SEED_CONFIG" "${UNIT_FILE:-}"' EXIT
{
  echo "# stationportal configuration. The MQTT password is NOT here — it lives in"
  echo "# stationportal.env (STATIONPORTAL_MQTT_PASSWORD). Seeded once by deploy.sh."
  echo "http_addr        = \"$(toml_escape "$HTTP_ADDR")\""
  echo "site             = \"muehle\""
  echo "probe_interval_s = 30"
  echo "probe_timeout_s  = 3"
  echo "log_level        = \"info\""
  echo "# inventory = \"/etc/stationportal/inventory.toml\"   # optional override of the built-in inventory"
  echo ""
  echo "[mqtt]"
  echo "broker    = \"$(toml_escape "$MQTT_BROKER")\""
  echo "client_id = \"stationportal\""
  echo "user      = \"$(toml_escape "$MQTT_USER")\""
} > "$SEED_CONFIG"

# --- build for the Pi (Linux arm64) -----------------------------------------
echo ">> Running tests..."
go vet ./... && go test ./... >/dev/null
echo ">> Building ${BINARY} for linux/arm64..."
OUT="dist/${BINARY}-linux-arm64"
mkdir -p dist
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$OUT" "$PKG"
echo "   built $OUT"

# --- systemd unit -----------------------------------------------------------
UNIT_FILE="$(mktemp)"
cat > "$UNIT_FILE" <<EOF
[Unit]
Description=Mühle station landing page (stationportal)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/${BINARY} -config ${CONFIG_FILE}
EnvironmentFile=${ENV_FILE}
Restart=on-failure
RestartSec=5
User=${SERVICE_USER}
Group=${SERVICE_USER}
ConfigurationDirectory=${SERVICE_NAME}

# Port 80 as an unprivileged user: exactly CAP_NET_BIND_SERVICE, nothing else.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

# Hardening. No disk writes, no devices; inbound HTTP + outbound TCP only.
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
MemoryMax=64M
TasksMax=64
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

[Install]
WantedBy=multi-user.target
EOF

# --- copy + install ---------------------------------------------------------
echo ">> Copying files to ${SSH_TARGET}..."
scp -q "$OUT" "${SSH_TARGET}:/tmp/${BINARY}.new"
scp -q "$UNIT_FILE" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.service"
scp -q "$SEED_CONFIG" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.config.seed"

echo ">> Installing on ${SSH_TARGET}..."
ssh "$SSH_TARGET" "INSTALL_DIR='${INSTALL_DIR}' BINARY='${BINARY}' SERVICE_NAME='${SERVICE_NAME}' SERVICE_USER='${SERVICE_USER}' CONFIG_DIR='${CONFIG_DIR}' CONFIG_FILE='${CONFIG_FILE}' ENV_FILE='${ENV_FILE}' bash -s" <<'REMOTE'
set -euo pipefail
SEED_CFG="/tmp/${SERVICE_NAME}.config.seed"
trap 'rm -f "$SEED_CFG"' EXIT
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi
sudo mkdir -p "$INSTALL_DIR"
sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$CONFIG_DIR"
if [ -e "$CONFIG_FILE" ]; then
  echo "   config exists at $CONFIG_FILE -- leaving it untouched (seed-once)."
else
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$SEED_CFG" "$CONFIG_FILE"
  echo "   seeded config at $CONFIG_FILE (0600)."
fi
# Seed the EnvironmentFile ONCE with the shared hf password, pulled on-device
# from an existing station service env (the secret never leaves the host).
if [ -e "$ENV_FILE" ]; then
  echo "   env file exists at $ENV_FILE -- leaving it untouched (seed-once)."
else
  pw=""
  for f in /etc/testui/testui.env \
           /etc/flexbridge/flexbridge.env \
           /etc/acom1200s-pa-bridge/acom1200s-pa-bridge.env \
           /etc/hadiscovery/hadiscovery.env ; do
    sudo test -r "$f" || continue
    v=$(sudo grep -hE '^[A-Z0-9_]*MQTT_PASSWORD=' "$f" 2>/dev/null | head -1 | sed -E 's/^[^=]*=//; s/^"(.*)"$/\1/')
    [ -n "$v" ] && pw="$v" && break
  done
  tmp=$(umask 077; mktemp)
  {
    echo "# stationportal EnvironmentFile. Keep 0600."
    if [ -n "$pw" ]; then
      printf 'STATIONPORTAL_MQTT_PASSWORD="%s"\n' "$pw"
    else
      echo "# STATIONPORTAL_MQTT_PASSWORD=\"...\"   # no hf service env found to copy from"
    fi
  } > "$tmp"
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$tmp" "$ENV_FILE"
  rm -f "$tmp"
  if [ -n "$pw" ]; then
    echo "   seeded env file at $ENV_FILE (0600) — hf password pulled on-device."
  else
    echo "   !! seeded env file WITHOUT a password — set STATIONPORTAL_MQTT_PASSWORD in $ENV_FILE."
  fi
fi
sudo systemctl stop "${SERVICE_NAME}.service" 2>/dev/null || true
sudo install -m 0755 "/tmp/${BINARY}.new" "${INSTALL_DIR}/${BINARY}"
rm -f "/tmp/${BINARY}.new"
sudo mv "/tmp/${SERVICE_NAME}.service" "/etc/systemd/system/${SERVICE_NAME}.service"
sudo systemctl daemon-reload
sudo systemctl enable "${SERVICE_NAME}.service"
sudo systemctl restart "${SERVICE_NAME}.service"
echo "--- service status ---"
sudo systemctl --no-pager --full status "${SERVICE_NAME}.service" || true
REMOTE

echo ""
echo ">> Done. stationportal deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Page:    http://${SSH_HOST##*@}/"
echo "   Logs:    ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Config:  ${CONFIG_FILE}   Secret: ${ENV_FILE}"
