#!/bin/sh
# Deploy logger-spot-bridge to the Windows shack PC over OpenSSH (ssh/scp).
#
# The shack PC is INTERACTIVE (the loggers are desktop apps) — like
# pelcobridge2 there is no systemd service (recorded deviation from the
# deployment convention). This script: cross-compiles windows/amd64, copies
# the binary, seeds the config ONCE (never overwrites an existing one), and
# prints the start line. To start it with the loggers, register a schtasks
# logon trigger (one-liner printed at the end) or start it by hand.
set -eu
cd "$(dirname "$0")"

HOST="${LSB_HOST:-192.168.1.197}"
USER="${LSB_USER:-iotte}"
DEST="${LSB_DEST:-C:/Users/${USER}/logger-spot-bridge}"

echo "==> cross-compiling windows/amd64"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w" -o dist/logger-spot-bridge-windows-amd64.exe ./cmd/logger-spot-bridge

echo "==> copying binary to ${USER}@${HOST}:${DEST}/"
# The shack PC's sshd runs commands through (German) cmd.exe; `if not exist`
# with forward-slash paths trips its parser. An explicit `cmd /c` with
# backslash paths works — same finding as pelcobridge2 (2026-08-30).
WDEST=$(printf '%s' "$DEST" | tr '/' '\\')
# Windows locks a running exe: stop the task BEFORE scp, restart after.
ssh "${USER}@${HOST}" "schtasks /end /tn logger-spot-bridge" >/dev/null 2>&1 || true
ssh "${USER}@${HOST}" "taskkill /f /im logger-spot-bridge.exe" >/dev/null 2>&1 || true
ssh "${USER}@${HOST}" "cmd /c \"if not exist ${WDEST} mkdir ${WDEST}\""
scp dist/logger-spot-bridge-windows-amd64.exe "${USER}@${HOST}:${DEST}/logger-spot-bridge.exe" || {
    echo "!! scp failed — is the exe still running/locked on the shack PC?"
    exit 1
}

echo "==> seeding config (once; an existing config.toml is never touched)"
if [ ! -f .deploy-seed.toml ]; then
    cp config.example.toml .deploy-seed.toml
    echo "    created .deploy-seed.toml from config.example.toml — edit it first!"
    echo "    (set station_locator + listeners, then re-run deploy.sh)"
    exit 0
fi
ssh "${USER}@${HOST}" "cmd /c \"if not exist ${WDEST}\\config.toml (echo seed) else (echo exists)\"" | grep -q seed &&
    scp .deploy-seed.toml "${USER}@${HOST}:${DEST}/config.toml" ||
    echo "    config.toml already present — left alone (seed-once)"

echo "==> restarting the logon task"
ssh "${USER}@${HOST}" "schtasks /run /tn logger-spot-bridge" || true

echo "==> done. On the shack PC, set the broker password and start it:"
echo "    cd ${DEST}"
echo "    set LOGGER_SPOT_BRIDGE_MQTT_PASSWORD=<password>"
echo "    logger-spot-bridge.exe"
echo
echo "    Optional autostart at logon (schtasks):"
echo "    schtasks /create /tn logger-spot-bridge /tr \"${DEST}\\logger-spot-bridge.exe\" /sc onlogon /rl limited"
echo
echo "    Watch the bus from anywhere: mosquitto_sub -h hassio.kgbvax.net -u hf -P ... -t 'muehle/hf/spots/#' -v"
