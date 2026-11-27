# CLAUDE.md — winpc-power-bridge

winpc-power-bridge is a small agent on the **Windows shack PC** (`bwpc`,
`192.168.1.197`, OpenSSH server, German `cmd.exe` shell). It publishes the
station integration-model slot **`muehle/power/shack-pc`** and lets the station
put the PC to **sleep (S3)** over MQTT. Waking is not its job: a sleeping PC
runs no code. `powerseq` wakes it with Wake-on-LAN (`kind = "wol"` step, with `poke` for
Modern Standby, see below).

- Role `pc-power`; actions `sleep`, `ensure_program`, `stop_program`;
  `/state` = `{ts, power:"awake", user_idle_s, device_online:true, error?,
  program_<name>, program_<name>_error?}`; asleep = `/status` `offline`.
- **Programs** (`[[program]]`, 2026-11-27): the bwpc actuator of the powerseq
  75auto start chain. `ensure_program <name>` uses a copy that runs and passes
  its ready check (`process`, `tcp` probe, or `none`), starts a missing one and
  records it as owned (PID + process start time in `programs-owned.json` next
  to the config; a reused PID is not owned), restarts an owned one that fails
  its check or has `restart_owned`, and fails an operator copy that fails its
  check with `not started by the chain`. It never ends an operator copy.
  `/state` shows `starting` first in every case. `stop_program` ends only
  owned copies (WM_CLOSE, then TerminateProcess after 10 s) and is refused
  while a 75auto session is active. Programs start with
  `CREATE_BREAKAWAY_FROM_JOB`, so `schtasks /end` of the agent leaves them
  running. The commands are unsigned like `sleep`; the program paths come
  only from the config.
- **Power request**: the agent holds a `PowerRequestSystemRequired` power
  request while `hf/power-seq` `chain.state` = `running` or a 75auto session
  (`hf/auto-op` `session.state` connecting/running) has a `/beat` or live
  `/state` younger than 30 s. Otherwise it is released, also with owned
  programs running. Whether this keeps bwpc out of Modern Standby is a bench
  check.
- **Sleep guard:** a `sleep` is refused (`/state.error = "user active, sleep
  refused"`, `Warn` log) when the last keyboard/mouse input in the user session
  is newer than `[sleep] min_user_idle_minutes` (default 30).
- **Retained `/cmd` deliveries are dropped** (`Warn`), so a stale retained
  `sleep` can never put the PC to sleep at boot or right after a wake. Send
  `sleep` non-retained (powerseq: `retain = false`).

- **Tailscale after a resume** (`[tailscale] ping_after_resume`, default on,
  2026-11-22): bwpc goes to Modern Standby (S0 low-power idle) after one
  hour without use, not to S3. Windows pauses Tailscale then. After the wake,
  a remote computer (the operator's Mac) cannot open a tunnel to bwpc, but
  bwpc can open one to it. The main loop notices a resume when the `/state`
  tick comes more than 3 intervals late (`internal/tailnet.ResumeDetector`,
  wall clock without the monotonic reading) and runs `tailscale ping --tsmp`
  to every online peer (`tailnet.PingPeers`). A missing Tailscale program
  only logs a warning. A magic packet does not wake bwpc from Modern
  Standby, a TCP connect to it does: powerseq's `wol` step has the `poke`
  key for that.

Full wire contract: `docs/winpc-power-bridge-mqtt-api.md`.

---

## Commands

```bash
go build ./cmd/winpc-power-bridge                 # local build (stubs on macOS)
go test ./...                                     # unit tests (no network, no Windows)
go test ./... -race
go vet ./...
GOOS=windows GOARCH=amd64 go build ./...          # Windows cross-build check
GOOS=windows go vet ./...                         # vets the //go:build windows files
./deploy.sh                                       # build, ship, register + start the logon task
```

The module is in the root `go.work` (since 2026-10-10). `GOWORK=off` also
works: `go.mod` carries the `replace … => ../shared`.

CLI flags: `-config <path>` (default: `config.toml` next to the exe; an
explicitly named missing file is an error), `-log.level debug|info|warn|error`.

---

## Architecture

1. `cmd/winpc-power-bridge/main.go` — config, logger, signal ctx, the jobs
   worker (`shared/mqtt` `RunJobs`), the 30 s `/state` ticker, and the
   **`session`** that owns the current paho client. On (re)connect `OnConnect`
   publishes `online`, enqueues `/meta` + `/state`, and subscribes `/cmd`
   (QoS 0). The paho handler only enqueues (paho handlers must never publish
   blocking — stationa memory, hadiscovery deadlock).
2. `internal/bridge` — the slot model, platform-free: `Decide` (pure guard),
   `HandleCommand(retained, payload)`, `PublishState`, `PublishMeta`. The idle
   source (`IdleFunc`) and the sleep sequence (`SuspendFunc`) are injected, so
   all of it is unit-tested on macOS.
3. `internal/winpower` — the Windows layer (`//go:build windows`):
   `UserIdle` (`user32!GetLastInputInfo` + `kernel32!GetTickCount64`, 32-bit
   wrap-safe difference), `CanSuspend` (`powrprof!IsPwrSuspendAllowed`),
   `Suspend` (enable `SeShutdownPrivilege` via `golang.org/x/sys/windows`, then
   `powrprof!SetSuspendState(FALSE,FALSE,FALSE)`). `winpower_other.go` stubs
   return `ErrUnsupported` elsewhere.
4. `internal/programs` — `Manager` (the ensure/stop decisions, the owned
   record, the ready checks), `Activity` + `PowerHold` (the power request).
   The process table (`Proc`), clock and power request are injected, so the
   decisions are tested on macOS. `proc_windows.go`: Toolhelp snapshot
   (`CreateToolhelp32Snapshot`/`Process32*`), creation time
   (`OpenProcess` + `GetExitCodeProcess` + `GetProcessTimes`), start
   (`os.StartProcess` with `CREATE_BREAKAWAY_FROM_JOB`), end (`EnumWindows` +
   `PostMessageW(WM_CLOSE)`, then `TerminateProcess`), power request
   (`kernel32!PowerCreateRequest`/`PowerSetRequest`/`PowerClearRequest`).
   `proc_other.go` stubs return `ErrUnsupported`. `ensure_program` runs on
   the jobs worker (it can take until `check_timeout_s`); the power hold runs
   on its own goroutine, fed from the paho handlers of the powerseq and
   75auto topics (parse and record only, no publish).
5. `internal/config` — TOML + `WINPC_POWER_BRIDGE_MQTT_*` env overrides.

### Sleep and reconnect — design choice

The sleep sequence runs on the jobs worker: guard → `CanSuspend` (refuse
without a `/status` flap) → publish `/status` `offline` explicitly (a clean
DISCONNECT suppresses the LWT) → `Disconnect(500)` → `SetSuspendState` (returns
on resume) → **reconnect in-process with a fresh paho client**.

The client is rebuilt rather than re-`Connect()`ed because paho documents that
reusing a client after `Disconnect` may panic. The bridge publishes through the
`session` (it implements `bridge.Publisher`), so it never holds a stale client.
`SetConnectRetry(true)` keeps the connect pending until the NIC is up after
resume; `shared/mqtt.Connect` makes that wait ctx-aware. The alternative
(exit after resume and let Task Scheduler restart the process) was rejected:
Task Scheduler's restart-on-failure does not reliably fire on a non-zero exit
code, so the agent could stay down until the next logon.

If the PC sleeps by other means (power plan timeout, Start menu), the broker
fires the LWT after ~45 s (30 s keep-alive) and paho auto-reconnects on resume.

Clean session + QoS 0 `/cmd`: nothing is queued for the agent while it is
offline, so a `sleep` can never arrive late, after a wake.

---

## Configuration and secrets

Config: `config.toml` in the install dir (`C:\Users\iotte\winpc-power-bridge\`),
seed-once from `config.example.toml`. Keys: `host`, `location`, `[mqtt]`,
`[device]`, `[sleep] min_user_idle_minutes`, `[state] interval_s`,
`[tailscale]`, `[log] level`, `[log] file`, `[[program]]` (name, exe, args,
workdir, process, check, check_address, probe, reply_pattern, settle_s,
check_timeout_s, restart_owned; see `config.example.toml`). The config is
seed-once: on bwpc add the `[[program]]` entries by hand.

**MQTT password** — same rule as pelcobridge2 (the earlier shack-PC component,
decommissioned 2026-10-10):
the env var `WINPC_POWER_BRIDGE_MQTT_PASSWORD` wins; the TOML `[mqtt] password`
is the fallback. There is no systemd `EnvironmentFile` on Windows. Preferred:
set it as a **user** environment variable on bwpc in the GUI
(`rundll32 sysdm.cpl,EditEnvironmentVariables` → "Benutzervariablen"), so it is
not on an ssh command line or in shell history. The logon task inherits the
user environment. The fallback is acceptable because the config file is in the
user profile, which only the user, SYSTEM and Administrators can read. No
password flag exists. Other env overrides: `WINPC_POWER_BRIDGE_MQTT_BROKER`,
`_CLIENT_ID`, `_USER`, `_SITE`.

Default broker: `tcp://192.168.1.178:1883` (the scmino station broker).

---

## Logging — recorded exception

slog text handler with the constant `component=winpc-power-bridge` attr and a
`slot` attr (logging convention §1–§3). **Exception to §4 (no log files):** the
exe is built with `-H=windowsgui` and runs hidden, so stderr goes nowhere and
there is no journald. With `[log] file` set (default in the example:
`winpc-power-bridge.log` next to the config) all output goes to that file; it
is moved to `<file>.1` at start when above 5 MB. For an interactive debug run,
build without `-H=windowsgui`, set `file = ""`, and run it in a console.

---

## Deployment — recorded deviation

No systemd unit and no Windows service: `GetLastInputInfo` only sees input in
the caller's own session, and a service runs in session 0. The agent is a
**hidden Task Scheduler task with a logon trigger**, running as the logged-on
user, non-elevated (`LeastPrivilege`; a standard user holds
`SeShutdownPrivilege`), only while that user is logged on (`InteractiveToken`).

```bash
./deploy.sh        # first run creates .deploy-seed.toml and stops — review it, re-run
WINPC_HOST=192.168.1.197 WINPC_USER=iotte ./deploy.sh   # defaults shown
```

deploy.sh: cross-compiles `CGO_ENABLED=0 GOOS=windows GOARCH=amd64` with
`-ldflags "-s -w -H=windowsgui"`; ends a running instance (`schtasks /end`,
`taskkill`; the exe is locked while it runs); copies the exe; seeds
`config.toml` once; registers task `winpc-power-bridge` from an XML definition
(`schtasks /create /xml … /f`, idempotent) and starts it (`schtasks /run`);
prints the NIC MACs (`getmac`) for powerseq.

The task is defined in XML (UTF-16) rather than with
`schtasks /sc onlogon /tr …`, because the command-line form leaves the default
72 h execution time limit (the agent would be killed after three days up) and
the "start only on AC power" condition. The XML sets `ExecutionTimeLimit PT0S`,
no battery conditions, `MultipleInstancesPolicy IgnoreNew`, restart on failure
every minute.

Check: `schtasks /query /tn winpc-power-bridge /v /fo list` on the PC, the log
file in the install dir, and the bus:
`mosquitto_sub -h 192.168.1.178 -u hf -P "$MQTT_PASSWORD" -t 'muehle/power/shack-pc/#' -v`.

---

## Prerequisites on bwpc

1. **S3 sleep available.** `powercfg /a` must list "Standby (S3)" as
   available. On a **Modern Standby (S0 low-power idle)** machine
   `SetSuspendState` does not enter S3 (and `IsPwrSuspendAllowed` may be false):
   the agent then answers `sleep failed: …` and stays online.
2. **Wake-on-LAN** for powerseq's wake step:
   - BIOS/UEFI: enable "Wake on LAN" / "Power on by PCI-E".
   - Device Manager → wired NIC → *Energieverwaltung*: "Gerät kann den Computer
     aus dem Ruhezustand aktivieren" + "Nur Magic Packet …"; *Erweitert*:
     "Wake on Magic Packet" = enabled.
   - The wired NIC MAC goes into powerseq's `wol` step (`mac = …`). deploy.sh
     prints it.
   - WOL from S3 needs the wired NIC; Wi-Fi does not wake.
3. A user is logged on (auto-logon, or a locked session). With nobody logged on
   the task does not run and the slot stays `offline`.
4. Hibernation does not matter: `SetSuspendState(FALSE, …)` asks for sleep. If
   "hybrid sleep" is on, the PC still sleeps (and also writes hiberfil).

---

## Test plan on the PC

1. Deploy; `/status` = `online`, `/meta`, `/state` every 30 s with rising
   `user_idle_s` while nobody touches the PC.
2. Move the mouse, send `{"action":"sleep"}` (non-retained) → `/state.error =
   "user active, sleep refused"`, PC stays on, `Warn` line in the log.
3. Set `min_user_idle_minutes = 1`, restart the task, wait > 1 min, send
   `sleep` → `/status` `offline`, PC sleeps. WOL (powerseq or `wakeonlan
   <mac>`) → `/status` `online` again within ~1 min, `/state` without `error`.
4. Publish a **retained** `sleep`, restart the task → `Warn` "dropping retained
   /cmd replay", PC stays on. Clear the retained message afterwards
   (`mosquitto_pub … -r -n`).
5. Configure the programs. With the screen locked, send
   `{"action":"ensure_program","value":"dax"}` (non-retained) → the program
   opens on the desktop, `/state` `program_dax` `starting` then `ready`, an
   entry in `programs-owned.json`. `schtasks /end` + `/run` of the agent →
   the program keeps running and is still owned.
6. During a 75auto session, `powercfg /requests` lists the agent's request
   under SYSTEM; after the session it is gone. `stop_program` during the
   session is refused.

---

## Station model and shared conventions

| Document | Path |
|---|---|
| Station integration model | `../docs/station-integration-model.md` |
| Config and secrets convention | `../docs/conventions/config-and-secrets.md` |
| Deployment convention | `../docs/conventions/deployment.md` |
| Logging convention | `../docs/conventions/logging.md` |
| Bridge-naming convention | `../docs/conventions/naming.md` |

Naming: `<devtag>-<function>-bridge` with devtag `winpc` (the device family /
control interface: a Windows PC) and function `power`.
