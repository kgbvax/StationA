#!/usr/bin/env bash
# Install flex2mqtt on a Raspberry Pi (or any Linux host) as a systemd
# service. Run this on the Pi after copying the cross-compiled binary here.
#
# Usage:
#   scp bin/flex2mqtt-linux-arm64 pi@raspberrypi:/tmp/flex2mqtt
#   scp -r deploy pi@raspberrypi:/tmp/flex2mqtt-deploy
#   ssh pi@raspberrypi 'sudo bash /tmp/flex2mqtt-deploy/install.sh'
set -euo pipefail

BINARY="${1:-/tmp/flex2mqtt}"
DEPLOY_DIR="${2:-/tmp/flex2mqtt-deploy}"
INSTALL_BIN="/usr/local/bin/flex2mqtt"
CONFIG_DIR="/etc/flex2mqtt"
STATE_DIR="/var/lib/flex2mqtt"

if [[ "$(id -u)" -ne 0 ]]; then
  echo "must run as root (try: sudo bash $0)" >&2
  exit 1
fi

echo "==> Installing binary"
install -m 0755 "$BINARY" "$INSTALL_BIN"

echo "==> Creating flex2mqtt user/group"
if ! getent group flex2mqtt >/dev/null; then
  groupadd --system flex2mqtt
fi
if ! getent passwd flex2mqtt >/dev/null; then
  useradd --system --gid flex2mqtt --no-create-home --shell /usr/sbin/nologin flex2mqtt
fi

echo "==> Creating state directory"
install -d -o flex2mqtt -g flex2mqtt -m 0755 "$STATE_DIR"

echo "==> Installing config"
install -d -m 0755 "$CONFIG_DIR"
if [[ ! -f "$CONFIG_DIR/config.toml" ]]; then
  install -m 0644 "$DEPLOY_DIR/config.example.toml" "$CONFIG_DIR/config.toml"
  echo "    wrote $CONFIG_DIR/config.toml (EDIT THIS before starting)"
else
  echo "    existing config.toml preserved"
fi
# Empty env file so the unit's EnvironmentFile=- doesn't error; user fills it.
touch "$CONFIG_DIR/flex2mqtt.env"
chmod 0600 "$CONFIG_DIR/flex2mqtt.env"
chown flex2mqtt:flex2mqtt "$CONFIG_DIR/flex2mqtt.env"

echo "==> Installing systemd unit"
install -m 0644 "$DEPLOY_DIR/flex2mqtt.service" /etc/systemd/system/flex2mqtt.service
systemctl daemon-reload

echo
echo "Done. Next steps:"
echo "  1. Edit $CONFIG_DIR/config.toml (broker, and radio_serial if you have"
echo "     more than one FlexRadio)."
echo "  2. Put secrets in $CONFIG_DIR/flex2mqtt.env if you prefer not to use"
echo "     the toml, e.g.:"
echo "        FLEX2MQTT_MQTT_PASSWORD=hunter2"
echo "  3. Enable and start:"
echo "        sudo systemctl enable --now flex2mqtt"
echo "  4. Watch logs:"
echo "        journalctl -u flex2mqtt -f"
