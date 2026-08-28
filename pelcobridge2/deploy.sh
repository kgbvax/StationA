#!/bin/sh
# Deploy pelcobridge2 to the Windows shack PC over OpenSSH (ssh/scp).
#
# NOTE: the shack PC is INTERACTIVE (TUI) — there is no service, no
# auto-start, by design (recorded deviation from the deployment convention).
# This script: cross-compiles windows/amd64, copies the binary, seeds the
# config ONCE (never overwrites an existing one), and prints the start line.
set -eu
cd "$(dirname "$0")"

HOST="${PELCO2_HOST:-192.168.1.197}"    # NB: the brief said 197.168.1.197 —
                                        # treated as a typo for 192.168.1.197.
                                        # Override with PELCO2_HOST if wrong.
USER="${PELCO2_USER:-iotte}"
DEST="${PELCO2_DEST:-C:/Users/${USER}/pelcobridge2}"

echo "==> cross-compiling windows/amd64"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w" -o dist/pelcobridge2-windows-amd64.exe ./cmd/pelcobridge2

echo "==> copying binary to ${USER}@${HOST}:${DEST}/"
ssh "${USER}@${HOST}" "if not exist ${DEST} mkdir ${DEST}"
scp dist/pelcobridge2-windows-amd64.exe "${USER}@${HOST}:${DEST}/pelcobridge2.exe"

echo "==> seeding config (once; an existing config.toml is never touched)"
if [ ! -f .deploy-seed.toml ]; then
    cp config.example.toml .deploy-seed.toml
    echo "    created .deploy-seed.toml from config.example.toml — edit it first!"
    echo "    (set [serial] port, then re-run deploy.sh)"
    exit 0
fi
ssh "${USER}@${HOST}" "if not exist ${DEST}\\config.toml (echo seed) else (echo exists)" | grep -q seed &&
    scp .deploy-seed.toml "${USER}@${HOST}:${DEST}/config.toml" ||
    echo "    config.toml already present — left alone (seed-once)"

echo "==> done. On the shack PC, start it interactively:"
echo "    cd ${DEST} && pelcobridge2.exe"
echo
echo "    ARMING IS MANUAL: run the TUI, press A, and enter the TRUE azimuth"
echo "    the head is pointing at. Disarmed at every start; never persisted."