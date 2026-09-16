#!/bin/sh
# Deploy logger-spot-bridge to the Windows shack PC over OpenSSH (ssh/scp).
#
# The shack PC is INTERACTIVE (the loggers are desktop apps) — like
# pelcobridge2 there is no systemd service (recorded deviation from the
# deployment convention). This script: cross-compiles windows/amd64, copies
# the binary, seeds config.toml and start-bridge.cmd ONCE (never overwrites
# either — config and secrets are host state and must survive updates),
# registers the logon task against the wrapper, verifies the live config
# with `-check` (broker DNS + password presence, redacted printout), and
# only then restarts the task.
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
seed_state=$(ssh "${USER}@${HOST}" "cmd /c \"if not exist ${WDEST}\\config.toml (echo seed) else (echo exists)\"")
case "$seed_state" in
    *seed*)
        scp .deploy-seed.toml "${USER}@${HOST}:${DEST}/config.toml"
        echo "    config.toml seeded from .deploy-seed.toml"
        ;;
    *exists*)
        echo "    config.toml already present — left alone (seed-once)"
        ;;
    *)
        echo "!! could not verify config.toml on ${HOST} (got: ${seed_state:-<empty>})"
        echo "   refusing to guess — fix ssh access and re-run."
        exit 1
        ;;
esac

echo "==> seeding start-bridge.cmd (once; holds the MQTT password as env)"
if [ ! -f start-bridge.cmd.example ]; then
    echo "!! start-bridge.cmd.example missing from the repo — cannot seed the wrapper"
    exit 1
fi
wrapper_state=$(ssh "${USER}@${HOST}" "cmd /c \"if not exist ${WDEST}\\start-bridge.cmd (echo seed) else (echo exists)\"")
case "$wrapper_state" in
    *seed*)
        scp start-bridge.cmd.example "${USER}@${HOST}:${DEST}/start-bridge.cmd"
        echo "    start-bridge.cmd seeded — edit it on the shack PC and paste"
        echo "    the real MQTT password (PASTE_MQTT_PASSWORD_HERE placeholder)"
        ;;
    *exists*)
        echo "    start-bridge.cmd already present — left alone (secrets survive updates)"
        ;;
    *)
        echo "!! could not verify start-bridge.cmd on ${HOST} (got: ${wrapper_state:-<empty>})"
        exit 1
        ;;
esac

echo "==> registering the logon task against the wrapper (repairs stale registrations)"
# The task must run start-bridge.cmd — the wrapper carries the MQTT password
# env. A task pointing at the exe directly starts the bridge without it (the
# 2026-09-16 outage: "can not resolve mqtt host / no mqtt password" after a
# redeploy). /f is deliberate: the task definition is ours, the secrets live
# in the seed-once wrapper, and /f repairs older direct-exe registrations.
ssh "${USER}@${HOST}" "schtasks /create /f /tn logger-spot-bridge /tr \"${WDEST}\\start-bridge.cmd\" /sc onlogon /rl limited" || {
    echo "!! schtasks registration failed — start the bridge by hand (see below)"
}

echo "==> verifying the live config on the shack PC (-check)"
if ssh "${USER}@${HOST}" "cmd /c \"cd /d ${WDEST} && logger-spot-bridge.exe -check\""; then
    :
else
    echo "!! config check FAILED on the shack PC — fix the problem above and re-run."
    echo "   Typical causes: PASTE_MQTT_PASSWORD_HERE still in start-bridge.cmd,"
    echo "   or a stale broker host in config.toml. config.toml and start-bridge.cmd"
    echo "   are seed-once; edit them in place on the shack PC."
    exit 1
fi

echo "==> restarting the logon task"
ssh "${USER}@${HOST}" "schtasks /run /tn logger-spot-bridge" || true

echo "==> done. Binary, config and wrapper updated; config + secrets were preserved."
echo "    Watch the bridge log:  ssh ${USER}@${HOST} type ${WDEST}\\bridge.log"
echo "    Watch the bus:         mosquitto_sub -h hassio.kgbvax.net -u hf -P ... -t 'muehle/hf/spots/#' -v"
