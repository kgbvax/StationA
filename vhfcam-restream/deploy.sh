#!/usr/bin/env bash
#
# Deploy vhfcam-restream to the Raspberry Pi (shari) and install it as a
# systemd service.
#
# vhfcam-restream is a network-only ffmpeg supervisor: it pulls the camera's
# RTSPS stream and pushes it to YouTube Live over RTMP. No serial device, no
# HTTP server, no local disk writes — the unit can run under a strict sandbox.
#
# Usage:
#   SOURCE_URL=... YT_STREAM_KEY=... ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST        SSH target            (default: 192.168.1.139)
#   SSH_USER        SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_NAME    systemd service name  (default: vhfcam-restream)
#   SERVICE_USER    system user to run as (default: vhfcam-restream)
#   INSTALL_DIR     remote install dir    (default: /opt/vhfcam-restream)
#   BINARY          binary name           (default: vhfcam-restream)
#
#   SOURCE_URL      source_url value      (default: empty -> placeholder, set on device)
#   SOURCE_HD_URL   source_hd_url value   (default: empty -> hd profile not configured)
#   QUALITY         quality value         (default: sd)
#   YT_URL          youtube_url value     (default: rtmp://a.rtmp.youtube.com/live2)
#   YT_STREAM_KEY   stream_key value      (default: empty -> placeholder, set on device)
#   FFMPEG_BIN      ffmpeg_bin value      (default: /usr/bin/ffmpeg)
#   LOG_LEVEL       log_level value       (default: info)
#
# Configuration lives in a single 0600 TOML file on the target
# (/etc/vhfcam-restream/config.toml). The RTSPS path token and the YouTube
# stream key are secrets and live in that file — never on any command line or
# in the unit file. The file is SEEDED ONCE on first deploy; subsequent deploys
# leave it untouched so the Pi owns its own settings. To change a setting after
# the first deploy, edit the file on the device (or delete it and redeploy to
# re-seed). A quality switch needs no redeploy: edit quality on the device and
# `sudo systemctl kill -s SIGHUP vhfcam-restream`.
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_NAME="${SERVICE_NAME:-vhfcam-restream}"
SERVICE_USER="${SERVICE_USER:-vhfcam-restream}"
INSTALL_DIR="${INSTALL_DIR:-/opt/vhfcam-restream}"
CONFIG_DIR="${CONFIG_DIR:-/etc/vhfcam-restream}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.toml}"
BINARY="${BINARY:-vhfcam-restream}"
PKG="./cmd/vhfcam-restream"

SOURCE_URL="${SOURCE_URL:-}"
SOURCE_HD_URL="${SOURCE_HD_URL:-}"
QUALITY="${QUALITY:-sd}"
YT_URL="${YT_URL:-rtmp://a.rtmp.youtube.com/live2}"
YT_STREAM_KEY="${YT_STREAM_KEY:-}"
FFMPEG_BIN="${FFMPEG_BIN:-/usr/bin/ffmpeg}"
LOG_LEVEL="${LOG_LEVEL:-info}"

# Allow "user@host" in SSH_HOST; otherwise prepend SSH_USER.
if [[ "$SSH_HOST" == *"@"* ]]; then
  SSH_TARGET="$SSH_HOST"
else
  SSH_TARGET="${SSH_USER}@${SSH_HOST}"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# --- TOML escaping helper ----------------------------------------------------
# Escape backslashes and double quotes for a TOML basic string.
toml_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '%s' "$s"
}

# --- generate the seed config file (used only if none exists on target) -----
# Written with umask 077 so the local temp copy is never world-readable. The
# seed carries the secrets (source token, stream key) — that is the design: a
# single 0600 file, per docs/conventions/config-and-secrets.md §3.
SEED_CONFIG="$(umask 077; mktemp)"
trap 'rm -f "$SEED_CONFIG" "${UNIT_FILE:-}"' EXIT
{
  echo "# vhfcam-restream configuration. Contains SECRETS (source tokens and"
  echo "# the YouTube stream key). Keep 0600. Seeded by deploy.sh on first"
  echo "# deploy; edit here to change settings."
  echo "# Camera sources (UniFi Protect RTSPS). quality picks the profile;"
  echo "# switch live with: sudo systemctl kill -s SIGHUP ${SERVICE_NAME}"
  echo "source_url    = \"$(toml_escape "$SOURCE_URL")\""
  echo "source_hd_url = \"$(toml_escape "$SOURCE_HD_URL")\""
  echo "quality       = \"$(toml_escape "$QUALITY")\""
  echo ""
  echo "# YouTube RTMP ingest."
  echo "youtube_url = \"$(toml_escape "$YT_URL")\""
  echo "stream_key  = \"$(toml_escape "$YT_STREAM_KEY")\""
  echo ""
  echo "# ffmpeg. Both camera profiles emit H.264 video + AAC and Opus audio;"
  echo "# audio_map pins the AAC track (FLV/RTMP cannot carry Opus), and both"
  echo "# streams are copied — no transcoding on the Pi."
  echo "ffmpeg_bin     = \"$(toml_escape "$FFMPEG_BIN")\""
  echo "rtsp_transport = \"tcp\""
  echo "video_map      = \"0:v:0\""
  echo "audio_map      = \"0:a:m:aac\""
  echo "video_codec    = \"copy\""
  echo "audio_codec    = \"copy\""
  echo ""
  echo "# Supervision."
  echo "stall_timeout_s = 60"
  echo "restart_min_s   = 5"
  echo "restart_max_s   = 300"
  echo "stable_run_s    = 120"
  echo ""
  echo "log_level = \"$(toml_escape "$LOG_LEVEL")\""
} > "$SEED_CONFIG"

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
Description=VHF cam to YouTube Live restreamer (vhfcam-restream)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# All settings — including the source tokens and the YouTube stream key — come
# from the 0600 config file. No secrets on the command line or in the unit.
ExecStart=${INSTALL_DIR}/${BINARY} -config ${CONFIG_FILE}
Restart=on-failure
RestartSec=5
User=${SERVICE_USER}
Group=${SERVICE_USER}
# systemd owns /etc/vhfcam-restream (created 0755, owned by the service user).
ConfigurationDirectory=${SERVICE_NAME}
# A writable state dir (unused today, reserved for future on-disk state).
StateDirectory=${SERVICE_NAME}

# Hardening. vhfcam-restream needs only outbound TCP (camera 7441 TLS/SRTP,
# YouTube 1935 RTMP) and /usr/bin/ffmpeg — no serial, no disk, no elevated
# capabilities — so the sandbox can be strict.
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
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

[Install]
WantedBy=multi-user.target
EOF

# --- copy artifacts to the Pi ----------------------------------------------
echo ">> Copying files to ${SSH_TARGET}..."
scp -q "$OUT" "${SSH_TARGET}:/tmp/${BINARY}.new"
scp -q "$UNIT_FILE" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.service"
# Transfer the seed to a restrictive temp path; the remote installs it only if
# no config exists yet, then removes the temp copy.
scp -q "$SEED_CONFIG" "${SSH_TARGET}:/tmp/${SERVICE_NAME}.config.seed"

# --- install remotely -------------------------------------------------------
echo ">> Installing on ${SSH_TARGET}..."
ssh "$SSH_TARGET" "SERVICE_NAME='${SERVICE_NAME}' SERVICE_USER='${SERVICE_USER}' INSTALL_DIR='${INSTALL_DIR}' CONFIG_DIR='${CONFIG_DIR}' CONFIG_FILE='${CONFIG_FILE}' BINARY='${BINARY}' FFMPEG_BIN='${FFMPEG_BIN}' bash -s" <<'REMOTE'
set -euo pipefail
SEED_CFG="/tmp/${SERVICE_NAME}.config.seed"
trap 'rm -f "$SEED_CFG"' EXIT
# ffmpeg is the actual stream engine — refuse to install without it.
if ! command -v "$FFMPEG_BIN" >/dev/null 2>&1 && [ ! -x "$FFMPEG_BIN" ]; then
  echo "!! ffmpeg not found at '$FFMPEG_BIN' — apt install ffmpeg first." >&2
  exit 1
fi
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
echo ">> Done. vhfcam-restream deployed to ${SSH_TARGET} as systemd service '${SERVICE_NAME}'."
echo "   Logs:    ssh ${SSH_TARGET} 'journalctl -u ${SERVICE_NAME} -f'"
echo "   Config:  ${CONFIG_FILE}"
if [[ -z "$SOURCE_URL" || -z "$YT_STREAM_KEY" ]]; then
  echo "   !! Seed ran with placeholders — set source_url / stream_key in $CONFIG_FILE on the device."
fi
