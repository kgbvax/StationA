#!/usr/bin/env bash
#
# Migrate the legacy upstream AF6SA `rotint` rotor bridge on shari to the
# stationa `wrc-rotator-bridge`. One-time, idempotent. Run this BEFORE the first
# `./deploy.sh` of wrc-rotator-bridge so deploy.sh's seed-once sees the migrated
# real MQTT password and the new service starts with the actual credentials
# first time.
#
# The legacy rotint install is the upstream AF6SA project, NOT a stationa
# bridge. It is structured completely differently from the stationa bridges:
#   - service: rotor-bridge.service at /etc/systemd/system/rotor-bridge.service
#   - binary:  /home/io/rotint/rotor-bridge (the rotint Go project)
#   - runs as user `io` (the deploy user — NOT a dedicated service user)
#   - NO TOML config: the MQTT password is passed on the ExecStart command line
#     (-password="..."), the GS-232 port via -gsport, everything else hardcoded
#     in source. No /etc config dir, no /var/lib, no udev.
#   - publishes the `rotor2mqtt/...` topic tree + embedded HA discovery under
#     `homeassistant/...` (NOT the stationa three-plane muehle/hf/rotator/*).
#
# The stationa wrc-rotator-bridge defaults match rotint's hardcoded values for
# everything except the broker: WRC ws://192.168.1.108/wsrotor, user hf,
# GS-232 0.0.0.0:7373 are unchanged, but the broker is now the shack-local
# Mosquitto on shari (tcp://127.0.0.1:1883, see docs/conventions/mqtt-topology.md)
# rather than rotint's tcp://192.168.1.50:1883. So deploy.sh's seed-once config.toml
# is functionally equivalent modulo the broker repoint. The ONLY thing deploy.sh
# cannot seed is the MQTT password — so this
# migration extracts it from the old unit's command line and writes it to the new
# EnvironmentFile.
#
# What this does, all over SSH on the target:
#   1. Creates the new service user `wrc-rotator-bridge` and the new config dir
#      /etc/wrc-rotator-bridge (so deploy.sh only has to drop the binary + unit).
#   2. Extracts the MQTT password from the old unit's `-password="..."` arg and
#      writes /etc/wrc-rotator-bridge/wrc-rotator-bridge.env (0600, owner new user)
#      as WRC_ROTATOR_BRIDGE_MQTT_PASSWORD="...". The extraction + rewrite happen
#      ON THE DEVICE — the password never crosses to the workstation and never
#      appears in shell history here. (This also fixes rotint's pre-existing
#      secret exposure: the password moves from a world-readable unit file's
#      command line into a 0600 EnvironmentFile owned by a dedicated user.)
#   3. Stops + disables the old `rotor-bridge` (rotint) systemd service.
#   4. Removes the old unit + daemon-reload.
#
# It deliberately does NOT:
#   - delete user `io` (rotint ran as io, which is the deploy user — leave it).
#   - delete /home/io/rotint (the rotint source + binary — the user's files;
#     only the service is stopped. Remove manually later if desired:
#     `rm -rf /home/io/rotint`).
#   - touch any udev/serial config (rotint was network-only, like the new bridge).
#
# Every step is guarded, so re-running is safe: if the old install is already
# gone, the steps report "not present" and no-op. If a new env file already
# exists at the target, it is backed up to *.pre-migration first.
#
# Usage:
#   ./migrate-from-rotint.sh                  # migrate on default host "shari"
#   SSH_HOST=pi@shari.local ./migrate-from-rotint.sh
#
# Configurable via environment variables (defaults shown):
#   SSH_HOST        SSH target            (default: 192.168.1.139)
#   SSH_USER        SSH user              (default: io)  [used only if SSH_HOST has no user@]
#   SERVICE_USER    new service user       (default: wrc-rotator-bridge)
#   CONFIG_DIR      new config dir         (default: /etc/wrc-rotator-bridge)
#
#   OLD_SERVICE_NAME old systemd service  (default: rotor-bridge)
#   OLD_UNIT          old unit file         (default: /etc/systemd/system/rotor-bridge.service)
#
set -euo pipefail

# --- configuration ----------------------------------------------------------
SSH_HOST="${SSH_HOST:-192.168.1.139}"
SSH_USER="${SSH_USER:-io}"
SERVICE_USER="${SERVICE_USER:-wrc-rotator-bridge}"
CONFIG_DIR="${CONFIG_DIR:-/etc/wrc-rotator-bridge}"
ENV_FILE="${CONFIG_DIR}/wrc-rotator-bridge.env"

# Legacy rotint install.
OLD_SERVICE_NAME="${OLD_SERVICE_NAME:-rotor-bridge}"
OLD_UNIT="${OLD_UNIT:-/etc/systemd/system/rotor-bridge.service}"

# Allow "user@host" in SSH_HOST; otherwise prepend SSH_USER.
if [[ "$SSH_HOST" == *"@"* ]]; then
  SSH_TARGET="$SSH_HOST"
else
  SSH_TARGET="${SSH_USER}@${SSH_HOST}"
fi

echo ">> Migrating rotint (rotor-bridge) -> wrc-rotator-bridge on ${SSH_TARGET}..."
echo "   old: service=${OLD_SERVICE_NAME} unit=${OLD_UNIT}"
echo "   new: user=${SERVICE_USER} config=${CONFIG_DIR}"
echo ""

ssh "$SSH_TARGET" \
  "NEW_USER='${SERVICE_USER}' NEW_CONFIG_DIR='${CONFIG_DIR}' NEW_ENV_FILE='${ENV_FILE}' OLD_SERVICE_NAME='${OLD_SERVICE_NAME}' OLD_UNIT='${OLD_UNIT}' bash -s" <<'REMOTE'
set -euo pipefail

# 1. Create the new service user (network-only: no serial group needed).
if ! id -u "$NEW_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$NEW_USER"
  echo "   created service user $NEW_USER"
fi

# 2. Ensure the new config dir exists (0755, owned by the new user).
sudo install -d -o "$NEW_USER" -g "$NEW_USER" -m 0755 "$NEW_CONFIG_DIR"

# 3. Extract the MQTT password from the old unit's ExecStart command line and
#    write it to the new EnvironmentFile ON THE DEVICE. The password never
#    leaves the Pi and never enters the workstation's shell history. Only the
#    `-password="..."` argument is parsed; rotint has no other secrets.
if [ -e "$OLD_UNIT" ]; then
  PW="$(sudo sed -nE 's/.*-password="([^"]*)".*/\1/p' "$OLD_UNIT" | head -1)"
  if [ -n "$PW" ]; then
    if [ -e "$NEW_ENV_FILE" ]; then
      sudo cp -a "$NEW_ENV_FILE" "${NEW_ENV_FILE}.pre-migration"
      echo "   backed up existing new env -> ${NEW_ENV_FILE}.pre-migration"
    fi
    # Write via a root-owned 0600 temp, then chown to the service user.
    TMP="$(umask 077; mktemp)"
    printf 'WRC_ROTATOR_BRIDGE_MQTT_PASSWORD="%s"\n' "$PW" > "$TMP"
    sudo install -o "$NEW_USER" -g "$NEW_USER" -m 0600 "$TMP" "$NEW_ENV_FILE"
    rm -f "$TMP"
    echo "   extracted MQTT password from $OLD_UNIT -> $NEW_ENV_FILE (0600, owner $NEW_USER)"
  else
    echo "   !! could not parse -password=\"...\" from $OLD_UNIT -- skipping env migration"
    echo "      set WRC_ROTATOR_BRIDGE_MQTT_PASSWORD in $NEW_ENV_FILE manually before deploy"
  fi
else
  echo "   no old unit at $OLD_UNIT -- skipping password extraction (set WRC_ROTATOR_BRIDGE_MQTT_PASSWORD on device)"
fi

# 4. Stop + disable the old rotint service (disable --now stops it too; no-op if absent).
if sudo systemctl disable --now "${OLD_SERVICE_NAME}.service" 2>/dev/null; then
  echo "   stopped + disabled old service ${OLD_SERVICE_NAME}"
else
  echo "   old service ${OLD_SERVICE_NAME} not running/present -- nothing to stop"
fi
sudo systemctl reset-failed "${OLD_SERVICE_NAME}.service" 2>/dev/null || true

# 5. Remove the old systemd unit + reload. (/home/io/rotint is left in place —
#    the user's source + binary; only the service is retired. Remove manually
#    later if desired: rm -rf /home/io/rotint. User io is also left alone.)
if [ -e "/etc/systemd/system/${OLD_SERVICE_NAME}.service" ]; then
  sudo rm -f "/etc/systemd/system/${OLD_SERVICE_NAME}.service"
  sudo systemctl daemon-reload
  echo "   removed old unit /etc/systemd/system/${OLD_SERVICE_NAME}.service"
fi

echo ""
echo ">> Migration complete."
echo "   /home/io/rotint left in place (user's files); only the service was retired."
echo "   Next: run ./deploy.sh to install wrc-rotator-bridge."
echo "         (seed-once seeds config.toml with defaults that match rotint's values;"
echo "          the migrated env file supplies the real MQTT password.)"
REMOTE

echo ""
echo ">> Migration finished on ${SSH_TARGET}. Now run: ./deploy.sh"