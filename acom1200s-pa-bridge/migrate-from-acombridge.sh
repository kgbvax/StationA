#!/usr/bin/env bash
#
# Migrate the legacy `acombridge` install on shari to the renamed
# `acom1200s-pa-bridge` layout. One-time, idempotent. Run this BEFORE the first
# `./deploy.sh` of acom1200s-pa-bridge so deploy.sh's seed-once sees the migrated
# real config+env and skips seeding defaults (the new service then starts with
# the actual serial port + MQTT password first time, not the defaults).
#
# What this does, all over SSH on the target:
#   1. Creates the new service user `acom1200s-pa-bridge` (+ dialout membership)
#      and the new config dir /etc/acom1200s-pa-bridge, so deploy.sh only has to
#      drop the binary + unit.
#   2. Copies the old config  /etc/acombridge/config.toml
#        -> /etc/acom1200s-pa-bridge/config.toml  (0600, owner new user).
#   3. Copies the old env     /etc/acombridge/acombridge.env
#        -> /etc/acom1200s-pa-bridge/acom1200s-pa-bridge.env (0600, owner new user),
#      rewriting the env-var prefix ACOMBRIDGE_ -> ACOM1200S_PA_BRIDGE_ in place
#      (so ACOMBRIDGE_MQTT_PASSWORD becomes ACOM1200S_PA_BRIDGE_MQTT_PASSWORD).
#      The secret is rewritten ON THE DEVICE — it never crosses to the workstation.
#   4. Stops + disables the old `acombridge` systemd service.
#   5. Removes the old unit, /opt/acombridge, /etc/acombridge, /var/lib/acombridge,
#      and the old udev rule 99-acombridge-serial.rules.
#   6. Best-effort removes the old `acombridge` user.
#
# Every step is guarded, so re-running is safe: if the old install is already
# gone, the steps just report "not present" and no-op. If a new config/env
# already exists at the target, it is backed up to *.pre-migration first.
#
# Usage:
#   ./migrate-from-acombridge.sh                  # migrate on default host "shari"
#   SSH_HOST=pi@shari.local ./migrate-from-acombridge.sh
#
# Configurable via environment variables (defaults shown):
#   SSH_HOST        SSH target            (default: 192.168.1.139)
#   SSH_USER        SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_USER    new service user       (default: acom1200s-pa-bridge)
#   SERIAL_GROUP    group owning serial    (default: dialout)
#   CONFIG_DIR      new config dir         (default: /etc/acom1200s-pa-bridge)
#
#   OLD_SERVICE_NAME old systemd service  (default: acombridge)
#   OLD_USER          old service user     (default: acombridge)
#   OLD_INSTALL_DIR   old install dir      (default: /opt/acombridge)
#   OLD_CONFIG_DIR    old config dir       (default: /etc/acombridge)
#   OLD_UDEV_RULE     old udev rule file    (default: 99-acombridge-serial.rules)
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_USER="${SERVICE_USER:-acom1200s-pa-bridge}"
SERIAL_GROUP="${SERIAL_GROUP:-dialout}"
CONFIG_DIR="${CONFIG_DIR:-/etc/acom1200s-pa-bridge}"
CONFIG_FILE="${CONFIG_DIR}/config.toml"
ENV_FILE="${CONFIG_DIR}/acom1200s-pa-bridge.env"

# Legacy acombridge install (fixed defaults it always deployed with).
OLD_SERVICE_NAME="${OLD_SERVICE_NAME:-acombridge}"
OLD_USER="${OLD_USER:-acombridge}"
OLD_INSTALL_DIR="${OLD_INSTALL_DIR:-/opt/acombridge}"
OLD_CONFIG_DIR="${OLD_CONFIG_DIR:-/etc/acombridge}"
OLD_CONFIG_FILE="${OLD_CONFIG_DIR}/config.toml"
OLD_ENV_FILE="${OLD_CONFIG_DIR}/acombridge.env"
OLD_UDEV_RULE="${OLD_UDEV_RULE:-99-acombridge-serial.rules}"

# Allow "user@host" in SSH_HOST; otherwise prepend SSH_USER.
if [[ "$SSH_HOST" == *"@"* ]]; then
  SSH_TARGET="$SSH_HOST"
else
  SSH_TARGET="${SSH_USER}@${SSH_HOST}"
fi

echo ">> Migrating acombridge -> acom1200s-pa-bridge on ${SSH_TARGET}..."
echo "   old: service=${OLD_SERVICE_NAME} user=${OLD_USER} config=${OLD_CONFIG_DIR} install=${OLD_INSTALL_DIR}"
echo "   new: user=${SERVICE_USER} config=${CONFIG_DIR}"
echo ""

ssh "$SSH_TARGET" \
  "NEW_USER='${SERVICE_USER}' SERIAL_GROUP='${SERIAL_GROUP}' NEW_CONFIG_DIR='${CONFIG_DIR}' NEW_CONFIG_FILE='${CONFIG_FILE}' NEW_ENV_FILE='${ENV_FILE}' OLD_SERVICE_NAME='${OLD_SERVICE_NAME}' OLD_USER='${OLD_USER}' OLD_INSTALL_DIR='${OLD_INSTALL_DIR}' OLD_CONFIG_DIR='${OLD_CONFIG_DIR}' OLD_CONFIG_FILE='${OLD_CONFIG_FILE}' OLD_ENV_FILE='${OLD_ENV_FILE}' OLD_UDEV_RULE='${OLD_UDEV_RULE}' bash -s" <<'REMOTE'
set -euo pipefail

# 1. Create the new service user + serial-group membership (mirrors deploy.sh;
#    deploy.sh will redo these idempotently, so doing it here is harmless and
#    lets us chown the migrated files before deploy.sh runs).
if ! id -u "$NEW_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$NEW_USER"
  echo "   created service user $NEW_USER"
fi
getent group "$SERIAL_GROUP" >/dev/null 2>&1 || sudo groupadd --system "$SERIAL_GROUP"
sudo usermod -aG "$SERIAL_GROUP" "$NEW_USER"

# 2. Ensure the new config dir exists (0755, owned by the new user).
sudo install -d -o "$NEW_USER" -g "$NEW_USER" -m 0755 "$NEW_CONFIG_DIR"

# 3. Migrate the config (TOML content is identical + portable; copy verbatim).
if [ -e "$OLD_CONFIG_FILE" ]; then
  if [ -e "$NEW_CONFIG_FILE" ]; then
    sudo cp -a "$NEW_CONFIG_FILE" "${NEW_CONFIG_FILE}.pre-migration"
    echo "   backed up existing new config -> ${NEW_CONFIG_FILE}.pre-migration"
  fi
  sudo install -o "$NEW_USER" -g "$NEW_USER" -m 0600 "$OLD_CONFIG_FILE" "$NEW_CONFIG_FILE"
  echo "   migrated config: $OLD_CONFIG_FILE -> $NEW_CONFIG_FILE"
else
  echo "   no old config at $OLD_CONFIG_FILE -- skipping (deploy.sh will seed defaults)"
fi

# 4. Migrate the env file, rewriting the var-name prefix ON THE DEVICE so the
#    secret never leaves the Pi. Only the uppercase ACOMBRIDGE_ prefix is
#    rewritten (never lowercase, in case a password value happened to contain
#    "acombridge"). Comments mentioning the old name are left as-is.
if [ -e "$OLD_ENV_FILE" ]; then
  if [ -e "$NEW_ENV_FILE" ]; then
    sudo cp -a "$NEW_ENV_FILE" "${NEW_ENV_FILE}.pre-migration"
    echo "   backed up existing new env -> ${NEW_ENV_FILE}.pre-migration"
  fi
  sudo install -o "$NEW_USER" -g "$NEW_USER" -m 0600 "$OLD_ENV_FILE" "$NEW_ENV_FILE"
  sudo sed -i 's/ACOMBRIDGE_/ACOM1200S_PA_BRIDGE_/g' "$NEW_ENV_FILE"
  echo "   migrated env: $OLD_ENV_FILE -> $NEW_ENV_FILE (ACOMBRIDGE_ -> ACOM1200S_PA_BRIDGE_)"
else
  echo "   no old env at $OLD_ENV_FILE -- skipping (set ACOM1200S_PA_BRIDGE_MQTT_PASSWORD on device)"
fi

# 5. Stop + disable the old service (disable --now stops it too; no-op if absent).
if sudo systemctl disable --now "${OLD_SERVICE_NAME}.service" 2>/dev/null; then
  echo "   stopped + disabled old service ${OLD_SERVICE_NAME}"
else
  echo "   old service ${OLD_SERVICE_NAME} not running/present -- nothing to stop"
fi
sudo systemctl reset-failed "${OLD_SERVICE_NAME}.service" 2>/dev/null || true

# 6. Remove the old systemd unit + reload.
if [ -e "/etc/systemd/system/${OLD_SERVICE_NAME}.service" ]; then
  sudo rm -f "/etc/systemd/system/${OLD_SERVICE_NAME}.service"
  sudo systemctl daemon-reload
  echo "   removed old unit /etc/systemd/system/${OLD_SERVICE_NAME}.service"
fi

# 7. Remove the old install dir, config dir, and systemd state dir.
if [ -d "$OLD_INSTALL_DIR" ]; then
  sudo rm -rf "$OLD_INSTALL_DIR"
  echo "   removed old install dir $OLD_INSTALL_DIR"
fi
if [ -d "$OLD_CONFIG_DIR" ]; then
  sudo rm -rf "$OLD_CONFIG_DIR"
  echo "   removed old config dir $OLD_CONFIG_DIR"
fi
if [ -d "/var/lib/${OLD_SERVICE_NAME}" ]; then
  sudo rm -rf "/var/lib/${OLD_SERVICE_NAME}"
  echo "   removed old state dir /var/lib/${OLD_SERVICE_NAME}"
fi

# 8. Remove the old udev rule (deploy.sh installs the new 99-acom1200s-pa-bridge one).
if [ -e "/etc/udev/rules.d/${OLD_UDEV_RULE}" ]; then
  sudo rm -f "/etc/udev/rules.d/${OLD_UDEV_RULE}"
  sudo udevadm control --reload-rules
  sudo udevadm trigger --subsystem-match=tty 2>/dev/null || true
  echo "   removed old udev rule ${OLD_UDEV_RULE}"
fi

# 9. Best-effort remove the old service user. After removing /opt and /etc
#    above it owns nothing of consequence; userdel fails only if a process or
#    file we missed still references it, in which case we report and move on.
if id -u "$OLD_USER" >/dev/null 2>&1; then
  if sudo userdel "$OLD_USER" 2>/dev/null; then
    echo "   removed old user $OLD_USER"
  else
    echo "   !! could not remove old user $OLD_USER (still referenced?) -- remove manually if desired: sudo userdel $OLD_USER"
  fi
fi

echo ""
echo ">> Migration complete."
echo "   Next: run ./deploy.sh to install acom1200s-pa-bridge."
echo "         (seed-once will keep the migrated config+env; the new service starts with the real settings.)"
REMOTE

echo ""
echo ">> Migration finished on ${SSH_TARGET}. Now run: ./deploy.sh"