#!/bin/sh
# Deploy winpc-power-bridge to the Windows shack PC (bwpc) over OpenSSH.
#
# Deviation from the deployment convention (recorded in CLAUDE.md): no systemd
# unit. The agent must run in the INTERACTIVE user session (GetLastInputInfo
# only sees input of its own session), so it is a hidden Task Scheduler task
# with a logon trigger, never a Windows service (session 0).
#
# This script: cross-compiles windows/amd64 as a GUI-subsystem exe (no console
# window), stops a running instance, copies the binary, seeds the config ONCE
# (never overwrites an existing one), (re)registers the logon task from an XML
# definition (idempotent, /f) and starts it.
set -eu
cd "$(dirname "$0")"

HOST="${WINPC_HOST:-192.168.1.197}"
USER="${WINPC_USER:-iotte}"
DEST="${WINPC_DEST:-C:/Users/${USER}/winpc-power-bridge}"
TASK="winpc-power-bridge"
EXE="winpc-power-bridge.exe"

echo "==> cross-compiling windows/amd64 (GUI subsystem: no console window)"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -H=windowsgui" \
    -o "dist/winpc-power-bridge-windows-amd64.exe" ./cmd/winpc-power-bridge

echo "==> seeding config (once; an existing config.toml is never touched)"
if [ ! -f .deploy-seed.toml ]; then
    cp config.example.toml .deploy-seed.toml
    echo "    created .deploy-seed.toml from config.example.toml — review it,"
    echo "    then re-run deploy.sh. Set the MQTT password on the PC as the user"
    echo "    environment variable WINPC_POWER_BRIDGE_MQTT_PASSWORD (CLAUDE.md)."
    exit 0
fi

# The shack PC's sshd runs commands through (German) cmd.exe; `if not exist`
# with forward-slash paths trips its parser ("Syntaxfehler"). An explicit
# `cmd /c` with backslash paths works (pelcobridge2, verified 2026-08-30).
WDEST=$(printf '%s' "$DEST" | tr '/' '\\')
ssh "${USER}@${HOST}" "cmd /c \"if not exist ${WDEST} mkdir ${WDEST}\""

echo "==> stopping a running instance (the exe is locked while it runs)"
ssh "${USER}@${HOST}" "schtasks /end /tn ${TASK}" >/dev/null 2>&1 || true
ssh "${USER}@${HOST}" "taskkill /f /im ${EXE}" >/dev/null 2>&1 || true
sleep 2

echo "==> copying binary to ${USER}@${HOST}:${DEST}/"
scp "dist/winpc-power-bridge-windows-amd64.exe" "${USER}@${HOST}:${DEST}/${EXE}"

ssh "${USER}@${HOST}" "cmd /c \"if not exist ${WDEST}\\config.toml (echo seed) else (echo exists)\"" | grep -q seed &&
    scp .deploy-seed.toml "${USER}@${HOST}:${DEST}/config.toml" ||
    echo "    config.toml already present — left alone (seed-once)"

echo "==> registering logon task ${TASK}"
# The task runs as the logged-on user, non-elevated (LeastPrivilege), only
# while that user is logged on (InteractiveToken = in the desktop session).
# XML instead of `schtasks /sc onlogon /tr …` because the command-line form
# cannot switch off the 72 h ExecutionTimeLimit (the agent would be killed
# after three days up) nor the "AC power only" start condition.
WHO=$(ssh "${USER}@${HOST}" whoami | tr -d '\r')
cat > .deploy-task.xml <<EOF
<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>stationa winpc-power-bridge: MQTT sleep agent for muehle/power/shack-pc</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>${WHO}</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>${WHO}</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>true</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>${WDEST}\\${EXE}</Command>
      <Arguments>-config "${WDEST}\\config.toml"</Arguments>
      <WorkingDirectory>${WDEST}</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
EOF
# schtasks /xml wants UTF-16 (LE with BOM) to match the declaration.
{ printf '\377\376'; iconv -f UTF-8 -t UTF-16LE .deploy-task.xml; } > dist/task.xml
scp dist/task.xml "${USER}@${HOST}:${DEST}/task.xml"
ssh "${USER}@${HOST}" "schtasks /create /tn ${TASK} /xml \"${WDEST}\\task.xml\" /f"

echo "==> starting ${TASK}"
ssh "${USER}@${HOST}" "schtasks /run /tn ${TASK}" ||
    echo "    could not start now (user not logged on?) — it starts at the next logon"

echo "==> done. Check on the bus:"
echo "    mosquitto_sub -h 192.168.1.50 -u hf -P \"\$MQTT_PASSWORD\" -t 'muehle/power/shack-pc/#' -v"
echo "    Log on the PC: ${DEST}/winpc-power-bridge.log"
echo
echo "==> NIC MAC addresses (the wired one goes into powerseq's wol step):"
ssh "${USER}@${HOST}" "getmac /v /fo list" || true
