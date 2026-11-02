#!/bin/sh
# Deploy chaseq-ui to the Windows shack PC (bwpc) over OpenSSH. Run it only when
# the operator says so: it restarts the window the operator may be working in.
#
# Same shape as winpc-power-bridge/deploy.sh (hidden at-logon Task Scheduler task from
# XML, seed-once config, cmd /c with backslash paths for the German cmd.exe) plus
# qslotter's clean quit (POST /api/quit), SHA-256 check and health poll.
# The exe is a GUI app, so the task must run in the interactive session (never a service).
set -eu
cd "$(dirname "$0")"

HOST="${CHASEQ_HOST:-192.168.1.197}"
USER="${CHASEQ_USER:-iotte}"
DEST="${CHASEQ_DEST:-C:/Users/${USER}/chaseq-ui}"
TASK="chaseq-ui"
EXE="chaseq-ui.exe"
PORT="${CHASEQ_PORT:-8480}"
SSH="ssh -o ConnectTimeout=25 ${USER}@${HOST}"
WDEST=$(printf '%s' "$DEST" | tr '/' '\\')

echo "==> tests (GOWORK=off: this module is outside go.work)"
GOWORK=off go test ./... -race

echo "==> cross-compiling windows/amd64 (GUI subsystem)"
mkdir -p dist
GOWORK=off CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -H=windowsgui" -o "dist/${EXE}" ./cmd/chaseq-ui
WANT=$(shasum -a 256 "dist/${EXE}" | cut -d' ' -f1)

echo "==> seeding config (once; an existing config.toml is never touched)"
if [ ! -f .deploy-seed.toml ]; then
    cp config.example.toml .deploy-seed.toml
    echo "    created .deploy-seed.toml from config.example.toml - review it (Log4OM port!),"
    echo "    then re-run deploy.sh. Set the MQTT password on the PC as the user environment"
    echo "    variable CHASEQ_UI_MQTT_PASSWORD (rundll32 sysdm.cpl,EditEnvironmentVariables)."
    exit 0
fi

$SSH "cmd /c \"if not exist ${WDEST} mkdir ${WDEST}\""

echo "==> clean quit of a running instance"
$SSH "powershell -NoProfile -c \"try { Invoke-WebRequest -UseBasicParsing -Method Post -Headers @{'X-Chaseq'='1'} http://127.0.0.1:${PORT}/api/quit | Out-Null } catch {}\"" >/dev/null 2>&1 || true
sleep 2
$SSH "taskkill /f /im ${EXE}" >/dev/null 2>&1 || true

echo "==> backing up the old exe"
$SSH "cmd /c \"if exist ${WDEST}\\${EXE} copy /y ${WDEST}\\${EXE} ${WDEST}\\${EXE}.bak\"" >/dev/null 2>&1 || true

echo "==> copying binary"
scp "dist/${EXE}" "${USER}@${HOST}:${DEST}/${EXE}"
GOT=$($SSH "powershell -NoProfile -c \"(Get-FileHash -Algorithm SHA256 '${WDEST}\\${EXE}').Hash.ToLower()\"" | tr -d '\r')
[ "$GOT" = "$WANT" ] || { echo "SHA-256 mismatch: $GOT != $WANT" >&2; exit 1; }

$SSH "cmd /c \"if not exist ${WDEST}\\config.toml (echo seed) else (echo exists)\"" | grep -q seed &&
    scp .deploy-seed.toml "${USER}@${HOST}:${DEST}/config.toml" ||
    echo "    config.toml already present - left alone (seed-once)"

echo "==> registering logon task ${TASK}"
WHO=$($SSH whoami | tr -d '\r')
cat > .deploy-task.xml <<XML
<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>stationa chaseq-ui: chase-queue list, QSY and logger fill</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled><UserId>${WHO}</UserId></LogonTrigger></Triggers>
  <Principals><Principal id="Author"><UserId>${WHO}</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings><StopOnIdleEnd>false</StopOnIdleEnd><RestartOnIdle>false</RestartOnIdle></IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure><Interval>PT1M</Interval><Count>999</Count></RestartOnFailure>
  </Settings>
  <Actions Context="Author"><Exec>
    <Command>${WDEST}\\${EXE}</Command>
    <Arguments>-config "${WDEST}\\config.toml"</Arguments>
    <WorkingDirectory>${WDEST}</WorkingDirectory>
  </Exec></Actions>
</Task>
XML
{ printf '\377\376'; iconv -f UTF-8 -t UTF-16LE .deploy-task.xml; } > dist/task.xml
scp dist/task.xml "${USER}@${HOST}:${DEST}/task.xml"
$SSH "schtasks /create /tn ${TASK} /xml \"${WDEST}\\task.xml\" /f"

echo "==> starting ${TASK}"
$SSH "schtasks /run /tn ${TASK}" || echo "    could not start now (user not logged on?) - it starts at the next logon"

echo "==> waiting for /healthz"
i=0
while [ $i -lt 20 ]; do
    CODE=$($SSH "powershell -NoProfile -c \"try { (Invoke-WebRequest -UseBasicParsing http://127.0.0.1:${PORT}/healthz -TimeoutSec 3).StatusCode } catch { 0 }\"" 2>/dev/null | tr -d '\r' || echo 0)
    [ "$CODE" = "200" ] && { echo "    up"; break; }
    i=$((i + 1)); sleep 2
done
[ "$CODE" = "200" ] || { echo "chaseq-ui did not come up; see ${DEST}/chaseq-ui.log on the PC" >&2; exit 1; }

echo "==> done. First run: if Log4OM parts say 'sent, no reply', allow chaseq-ui in Windows Defender"
echo "    Firewall (Log4OM answers on its inbound port + 1 as a broadcast)."
