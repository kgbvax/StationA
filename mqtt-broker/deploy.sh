#!/usr/bin/env bash
#
# Deploy the shack-local Mosquitto broker to shari and install it as a systemd
# service. This is infrastructure, not a Go component — no cross-compile, just
# apt + seed-once config + password files.
#
# See README.md for the two-broker topology (shack broker on shari authoritative
# for muehle/#, HA broker at 192.168.1.50 untouched, a mosquitto bridge between).
#
# Usage:
#   ./deploy.sh                       # deploy to default host "shari"
#   SSH_HOST=io@192.168.1.139 ./deploy.sh
#
# Configurable via environment variables (with defaults):
#   SSH_HOST        SSH target            (default: 192.168.1.139)
#   SSH_USER        SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   CONFIG_DIR      mosquitto config dir  (default: /etc/mosquitto)
#   CONF_FILE       mosquitto.conf path   (default: /etc/mosquitto/mosquitto.conf)
#   ACL_FILE        acl.conf path         (default: /etc/mosquitto/acl.conf)
#   PASSWD_FILE     password db path      (default: /etc/mosquitto/passwd)
#   HA_BROKER       bridge remote address (default: 192.168.1.50:1883)
#   HA_REMOTE_USER  bridge remote user   (default: stationa-bridge)
#
#   HF_MQTT_PASSWORD      password for the `hf` station account (prompted if empty
#   BRIDGE_MQTT_PASSWORD  password for the `bridge` account   and the passwd file
#   CONSOLE_MQTT_PASSWORD password for the `console` account does not exist yet)
#   DIAL_MQTT_PASSWORD    password for the `dial` account
#
# Secrets handling follows the repo convention: the password db (/etc/mosquitto/passwd)
# and the bridge remote_password (in /etc/mosquitto/mosquitto.conf) are SEEDED ONCE
# on first deploy and never appear in the repo. Subsequent deploys leave the
# on-device files untouched so shari owns its own settings. To change a password
# after the first deploy, run `mosquitto_passwd` on the device (or delete the
# passwd file and redeploy to re-seed).
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
CONFIG_DIR="${CONFIG_DIR:-/etc/mosquitto}"
CONF_FILE="${CONF_FILE:-${CONFIG_DIR}/mosquitto.conf}"
ACL_FILE="${ACL_FILE:-${CONFIG_DIR}/acl.conf}"
PASSWD_FILE="${PASSWD_FILE:-${CONFIG_DIR}/passwd}"
HA_BROKER="${HA_BROKER:-192.168.1.50:1883}"
HA_REMOTE_USER="${HA_REMOTE_USER:-stationa-bridge}"
HF_MQTT_PASSWORD="${HF_MQTT_PASSWORD:-}"
BRIDGE_MQTT_PASSWORD="${BRIDGE_MQTT_PASSWORD:-}"
CONSOLE_MQTT_PASSWORD="${CONSOLE_MQTT_PASSWORD:-}"
DIAL_MQTT_PASSWORD="${DIAL_MQTT_PASSWORD:-}"

if [[ "$SSH_HOST" == *"@"* ]]; then
  SSH_TARGET="$SSH_HOST"
else
  SSH_TARGET="${SSH_USER}@${SSH_HOST}"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# --- prepare seed config (substitute the HA bridge address/user) -----------
# Written with umask 077 so the local temp copy is never world-readable.
SEED_CONF="$(umask 077; mktemp)"
SEED_ACL="$(umask 077; mktemp)"
trap 'rm -f "$SEED_CONF" "$SEED_ACL"' EXIT

# Substitute HA_BROKER / HA_REMOTE_USER into the example conf (only the bridge
# connection block references them; everything else is copied verbatim).
sed \
  -e "s|^address .*|address ${HA_BROKER}|" \
  -e "s|^remote_username .*|remote_username ${HA_REMOTE_USER}|" \
  mosquitto.conf.example > "$SEED_CONF"
cp acl.conf.example "$SEED_ACL"

# --- install remotely -------------------------------------------------------
echo ">> Installing mosquitto on ${SSH_TARGET}..."
# Ship the seed files to restrictive temp paths; the remote installs them only
# if the on-device files do not yet exist, then removes the temp copies.
scp "$SEED_CONF" "${SSH_TARGET}:/tmp/mosquitto.conf.seed"
scp "$SEED_ACL" "${SSH_TARGET}:/tmp/acl.conf.seed"

ssh "$SSH_TARGET" "CONF_FILE='${CONF_FILE}' ACL_FILE='${ACL_FILE}' PASSWD_FILE='${PASSWD_FILE}' CONFIG_DIR='${CONFIG_DIR}' HF_MQTT_PASSWORD='${HF_MQTT_PASSWORD}' BRIDGE_MQTT_PASSWORD='${BRIDGE_MQTT_PASSWORD}' CONSOLE_MQTT_PASSWORD='${CONSOLE_MQTT_PASSWORD}' DIAL_MQTT_PASSWORD='${DIAL_MQTT_PASSWORD}' HA_BROKER='${HA_BROKER}' HA_REMOTE_USER='${HA_REMOTE_USER}' bash -s" <<'REMOTE'
set -euo pipefail
SEED_CONF="/tmp/mosquitto.conf.seed"
SEED_ACL="/tmp/acl.conf.seed"
trap 'rm -f "$SEED_CONF" "$SEED_ACL"' EXIT

# Ensure mosquitto + clients are installed.
if ! command -v mosquitto >/dev/null 2>&1; then
  sudo apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y mosquitto mosquitto-clients
fi

# Ensure the mosquitto user/group exist (apt creates them; be safe).
if ! id -u mosquitto >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin mosquitto
fi

sudo install -d -o mosquitto -g mosquitto -m 0755 "$CONFIG_DIR"
sudo install -d -o mosquitto -g mosquitto -m 0755 /var/lib/mosquitto
sudo install -d -o mosquitto -g mosquitto -m 0755 /var/log/mosquitto

# Seed mosquitto.conf ONCE (0600 — it will hold remote_password after the
# operator edits it, so treat it as a secret file from the start).
if [ -e "$CONF_FILE" ]; then
  echo "   mosquitto.conf exists at $CONF_FILE -- leaving it untouched (seed-once)."
  echo "   !! If this is the first deploy, set 'remote_password' under [bridge-to-ha] in $CONF_FILE."
else
  sudo install -o mosquitto -g mosquitto -m 0600 "$SEED_CONF" "$CONF_FILE"
  echo "   seeded mosquitto.conf at $CONF_FILE (0600, owner mosquitto)."
  echo "   !! Set 'remote_password <value>' under [bridge-to-ha] in $CONF_FILE (HA-side bridge account password)."
fi

# Seed acl.conf ONCE (not secret, but still seed-once so device edits survive).
if [ -e "$ACL_FILE" ]; then
  echo "   acl.conf exists at $ACL_FILE -- leaving it untouched (seed-once)."
else
  sudo install -o mosquitto -g mosquitto -m 0644 "$SEED_ACL" "$ACL_FILE"
  echo "   seeded acl.conf at $ACL_FILE (0644, owner mosquitto)."
fi

# Seed the password db ONCE. mosquitto_passwd creates the file; subsequent
# deploys leave it so passwords set on the device survive.
if [ -e "$PASSWD_FILE" ]; then
  echo "   password db exists at $PASSWD_FILE -- leaving it untouched (seed-once)."
  echo "   !! If this is the first deploy and the file is empty/missing users, add them with:"
  echo "      sudo mosquitto_passwd $PASSWD_FILE hf"
  echo "      sudo mosquitto_passwd $PASSWD_FILE bridge"
  echo "      sudo mosquitto_passwd $PASSWD_FILE console"
  echo "      sudo mosquitto_passwd $PASSWD_FILE dial"
else
  # Helper: add a user with a given password non-interactively (only if the
  # password was supplied via env; otherwise skip that user with a note).
  add_user() {
    local user="$1" pass="$2"
    if [ -n "$pass" ]; then
      sudo mosquitto_passwd -b "$PASSWD_FILE" "$user" "$pass"
      echo "   seeded user '$user'."
    else
      echo "   !! skipped user '$user' (no password supplied). Add it with: sudo mosquitto_passwd $PASSWD_FILE $user"
    fi
  }
  # Create the file with the first user, then append the rest.
  if [ -n "$HF_MQTT_PASSWORD" ]; then
    sudo mosquitto_passwd -b -c "$PASSWD_FILE" hf "$HF_MQTT_PASSWORD"
    echo "   seeded user 'hf'."
  else
    sudo touch "$PASSWD_FILE"
    echo "   !! no HF_MQTT_PASSWORD supplied — empty password db created."
    echo "      Add users with: sudo mosquitto_passwd -c $PASSWD_FILE hf  (then -b for the rest)"
  fi
  add_user bridge "$BRIDGE_MQTT_PASSWORD"
  add_user console "$CONSOLE_MQTT_PASSWORD"
  add_user dial "$DIAL_MQTT_PASSWORD"
  sudo chmod 0600 "$PASSWD_FILE"
  sudo chown mosquitto:mosquitto "$PASSWD_FILE"
  echo "   password db at $PASSWD_FILE (0600, owner mosquitto)."
fi

# (Re)enable and restart. mosquitto ships with a systemd unit from apt.
sudo systemctl daemon-reload
sudo systemctl enable mosquitto
sudo systemctl restart mosquitto
echo "--- service status ---"
sudo systemctl --no-pager --full status mosquitto || true
REMOTE

echo ""
echo ">> Done. Shack Mosquitto broker deployed to ${SSH_TARGET}."
echo "   Config:  ${CONF_FILE}  (set 'remote_password' under [bridge-to-ha] for the HA bridge)"
echo "   ACL:     ${ACL_FILE}"
echo "   Secrets: ${PASSWD_FILE}  (hf / bridge / console / dial users)"
echo "   Logs:    ssh ${SSH_TARGET} 'journalctl -u mosquitto -f'"
echo "   Next:    reconfigure the HA Mosquitto add-on with the 'stationa-bridge' account + ACL"
echo "            (see README.md 'HA-side setup'), then repoint station components at 127.0.0.1:1883."