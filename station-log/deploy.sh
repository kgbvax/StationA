#!/usr/bin/env bash
#
# Deploy station-log to scmino as a hardened systemd service (the standard
# stationa pattern, docs/conventions/deployment.md). Deploy only when the
# operator asks: a deploy restarts the service.
#
# Config:  /etc/station-log/config.toml            (0600, seed-once)
# Secrets: /etc/station-log/station-log.env        (0600, seed-once)
#            STATION_LOG_MQTT_PASSWORD
# Data:    /var/lib/station-log (StateDirectory): the SQLite log (personal data)
#
# Usage:
#   ./deploy.sh
#   SSH_HOST=io@scmino MQTT_PASSWORD=... ./deploy.sh   # first deploy
#   ssh io@scmino 'sudo -u station-log /opt/station-log/station-log -config /etc/station-log/config.toml -import-adif /tmp/log.adi'
#   curl --data-binary @log.adi 'http://127.0.0.1:8096/import?source=log4om-file'   # on scmino
#
set -euo pipefail

SSH_HOST="${SSH_HOST:-192.168.1.178}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME=station-log
SERVICE_USER=station-log
INSTALL_DIR=/opt/station-log
CONFIG_DIR=/etc/station-log
CONFIG_FILE="${CONFIG_DIR}/config.toml"
ENV_FILE="${CONFIG_DIR}/station-log.env"
BINARY=station-log
PKG=./cmd/station-log
DATA_DIR=/var/lib/station-log
MQTT_PASSWORD="${MQTT_PASSWORD:-}"

if [[ "$SSH_HOST" == *"@"* ]]; then SSH_TARGET="$SSH_HOST"; else SSH_TARGET="${SSH_USER}@${SSH_HOST}"; fi
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# single-quoted for systemd's EnvironmentFile and for sourcing; no $ or backtick expansion
esc() { local s="$1"; case "$s" in *$'\n'*) echo "secret contains a newline" >&2; exit 1;; esac; printf '%s' "${s//\'/\'\\\'\'}"; }

SEED_ENV="$(umask 077; mktemp)"
UNIT_FILE="$(mktemp)"
trap 'rm -f "$SEED_ENV" "$UNIT_FILE"' EXIT
{
  echo "# station-log EnvironmentFile (read by the systemd unit). Keep 0600."
  if [[ -n "$MQTT_PASSWORD" ]]; then echo "STATION_LOG_MQTT_PASSWORD='"$(esc "$MQTT_PASSWORD")"'"; else echo "# STATION_LOG_MQTT_PASSWORD=\"...\""; fi
} > "$SEED_ENV"

echo ">> Building ${BINARY} for linux/arm64..."
OUT="dist/${BINARY}-linux-arm64"
mkdir -p dist
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$OUT" "$PKG"

cat > "$UNIT_FILE" <<UNIT
[Unit]
Description=Station log: QSO record with ADIF import and duplicate merge (station-log)
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
StateDirectory=${SERVICE_NAME}
StateDirectoryMode=0750
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
ReadWritePaths=${DATA_DIR}
MemoryMax=1G
TasksMax=64
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

[Install]
WantedBy=multi-user.target
UNIT

echo ">> Copying files to ${SSH_TARGET}..."
scp -q "$OUT" "${SSH_TARGET}:/tmp/${BINARY}.new"
scp -q "$UNIT_FILE" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.service"
scp -q config.example.toml "${SSH_TARGET}:/tmp/${SERVICE_NAME}.config.seed"
scp -q "$SEED_ENV" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.env.seed"

echo ">> Installing on ${SSH_TARGET}..."
ssh "$SSH_TARGET" "INSTALL_DIR='${INSTALL_DIR}' BINARY='${BINARY}' SERVICE_NAME='${SERVICE_NAME}' SERVICE_USER='${SERVICE_USER}' CONFIG_DIR='${CONFIG_DIR}' CONFIG_FILE='${CONFIG_FILE}' ENV_FILE='${ENV_FILE}' DATA_DIR='${DATA_DIR}' bash -s" <<'REMOTE'
set -euo pipefail
trap 'rm -f /tmp/${SERVICE_NAME}.config.seed /tmp/${SERVICE_NAME}.env.seed /tmp/${SERVICE_NAME}.service /tmp/${BINARY}.new' EXIT
id -u "$SERVICE_USER" >/dev/null 2>&1 || sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$INSTALL_DIR" "$CONFIG_DIR"
sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$DATA_DIR"
if [ -e "$CONFIG_FILE" ]; then echo "   config exists -- untouched (seed-once)."; else
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "/tmp/${SERVICE_NAME}.config.seed" "$CONFIG_FILE"; echo "   seeded $CONFIG_FILE"; fi
if [ -e "$ENV_FILE" ]; then echo "   env file exists -- untouched (seed-once)."; else
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "/tmp/${SERVICE_NAME}.env.seed" "$ENV_FILE"; echo "   seeded $ENV_FILE"; fi
sudo install -o root -g root -m 0755 "/tmp/${BINARY}.new" "$INSTALL_DIR/$BINARY.next"
# check the new binary against the existing config before it replaces the running one
if sudo bash -c "set -a; . '$ENV_FILE'; set +a; exec '$INSTALL_DIR/$BINARY.next' -config '$CONFIG_FILE' -check"; then
  sudo mv -f "$INSTALL_DIR/$BINARY.next" "$INSTALL_DIR/$BINARY"
  sudo install -o root -g root -m 0644 "/tmp/${SERVICE_NAME}.service" "/etc/systemd/system/${SERVICE_NAME}.service"
  sudo systemctl daemon-reload
  sudo systemctl enable "$SERVICE_NAME" >/dev/null
  sudo systemctl restart "$SERVICE_NAME"; echo "   service restarted."
else
  sudo rm -f "$INSTALL_DIR/$BINARY.next"
  echo "!! config check FAILED; nothing was replaced and the service was NOT restarted. Fix $CONFIG_FILE / $ENV_FILE and re-run."; exit 1
fi
REMOTE
echo ">> done. Logs: ssh ${SSH_TARGET} journalctl -u ${SERVICE_NAME} -f"
