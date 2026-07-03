#!/usr/bin/env bash
# Install flexbridge on a Raspberry Pi (or any Linux host) as a systemd
# service. Run this on the Pi after copying the cross-compiled binary here.
#
# Usage:
#   scp bin/flexbridge-linux-arm64 pi@raspberrypi:/tmp/flexbridge
#   scp -r deploy pi@raspberrypi:/tmp/flexbridge-deploy
#   ssh pi@raspberrypi 'sudo bash /tmp/flexbridge-deploy/install.sh'
set -euo pipefail

BINARY="${1:-/tmp/flexbridge}"
DEPLOY_DIR="${2:-/tmp/flexbridge-deploy}"
INSTALL_BIN="/usr/local/bin/flexbridge"
CONFIG_DIR="/etc/flexbridge"
STATE_DIR="/var/lib/flexbridge"

if [[ "$(id -u)" -ne 0 ]]; then
  echo "must run as root (try: sudo bash $0)" >&2
  exit 1
fi

echo "==> Installing binary"
install -m 0755 "$BINARY" "$INSTALL_BIN"

echo "==> Creating flexbridge user/group"
if ! getent group flexbridge >/dev/null; then
  groupadd --system flexbridge
fi
if ! getent passwd flexbridge >/dev/null; then
  useradd --system --gid flexbridge --no-create-home --shell /usr/sbin/nologin flexbridge
fi

echo "==> Creating state directory"
install -d -o flexbridge -g flexbridge -m 0755 "$STATE_DIR"

echo "==> Installing config"
install -d -m 0755 "$CONFIG_DIR"
if [[ ! -f "$CONFIG_DIR/config.toml" ]]; then
  install -m 0644 "$DEPLOY_DIR/config.example.toml" "$CONFIG_DIR/config.toml"
  echo "    wrote $CONFIG_DIR/config.toml (EDIT THIS before starting)"
else
  echo "    existing config.toml preserved"
fi
# Empty env file so the unit's EnvironmentFile=- doesn't error; user fills it.
touch "$CONFIG_DIR/flexbridge.env"
chmod 0600 "$CONFIG_DIR/flexbridge.env"
chown flexbridge:flexbridge "$CONFIG_DIR/flexbridge.env"

echo "==> Installing systemd unit"
install -m 0644 "$DEPLOY_DIR/flexbridge.service" /etc/systemd/system/flexbridge.service
systemctl daemon-reload

echo
echo "Done. Next steps:"
echo "  1. Edit $CONFIG_DIR/config.toml (broker, and radio_serial if you have"
echo "     more than one FlexRadio)."
echo "  2. Put secrets in $CONFIG_DIR/flexbridge.env if you prefer not to use"
echo "     the toml, e.g.:"
echo "        FLEXBRIDGE_MQTT_PASSWORD=hunter2"
echo "  3. Enable and start:"
echo "        sudo systemctl enable --now flexbridge"
echo "  4. Watch logs:"
echo "        journalctl -u flexbridge -f"
