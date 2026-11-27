# MQTT schema — winpc-power-bridge

This document describes the MQTT interface of **winpc-power-bridge**, the
sleep agent on the Windows shack PC (`bwpc`, 192.168.1.197). It implements the
slot `muehle/power/shack-pc` of the station integration model
(`../../docs/station-integration-model.md`). It is the on-the-wire contract,
derived from `internal/bridge/bridge.go` and `cmd/winpc-power-bridge/main.go`.

The agent puts the PC to **sleep**, and it starts, checks and ends the
station programs of its `[[program]]` list (DAX, AetherSDR, WSJT-X, 75auto) for
the powerseq start chain of 75auto (section 7). It cannot wake the PC: a
sleeping PC runs no code. The wake path is Wake-on-LAN, sent by `powerseq` (`kind = "wol"`
step with the PC's wired NIC MAC).

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1, plain TCP (default `tcp://192.168.1.178:1883`, the scmino station broker) |
| Authentication | user `hf`; password from env `WINPC_POWER_BRIDGE_MQTT_PASSWORD` (TOML fallback) |
| Client ID | `<site>-<station>-<slot>` = `muehle-power-shack-pc` |
| Clean session | **true** — nothing is queued for the agent while the PC sleeps |
| Keep-alive | 30 s (the broker fires the LWT ~45 s after an unannounced sleep or crash) |
| Auto-reconnect / connect-retry | yes; connect retries every 5 s until the network is up |
| LWT | `muehle/power/shack-pc/status` = `offline`, QoS 1, retained |

---

## 2. Topics

| Topic | Retained | Direction | Purpose |
|-------|----------|-----------|---------|
| `muehle/power/shack-pc/meta` | yes | agent → bus | birth certificate |
| `muehle/power/shack-pc/state` | yes | agent → bus | live snapshot |
| `muehle/power/shack-pc/status` | yes | agent → bus | `online` \| `offline` (LWT + explicit) |
| `muehle/power/shack-pc/cmd` | **no** | bus → agent | `sleep`, `ensure_program`, `stop_program` |
| `muehle/hf/power-seq/state` | yes | bus → agent | read: `chain.state` and `chain.ts` for the power request |
| `muehle/hf/power-seq/status` | yes | bus → agent | read: while `offline`, the powerseq chain does not count for the power request |
| `muehle/hf/auto-op/state`, `muehle/hf/auto-op/beat` | yes / no | bus → agent | read: `session.state`, `session.source` and heartbeat for the power request and the `stop_program` guard |

All agent publishes are QoS 1.

---

## 3. `/meta`

Published on every (re)connect.

```json
{
  "schema": "1.0",
  "role": "pc-power",
  "device": { "model": "Windows PC", "serial": "bwpc" },
  "link": "ethernet",
  "location": "bauwagen",
  "host": "bwpc",
  "capabilities": {
    "actions": ["sleep", "ensure_program", "stop_program"],
    "min_user_idle_minutes": 30,
    "programs": ["aethersdr", "dax", "wsjtx", "75auto"]
  },
  "expose": {
    "device": { "name": "Shack PC (bwpc)", "model": "Windows PC" },
    "fields": [
      { "key": "power", "name": "Power", "type": "enum", "options": ["awake"] },
      { "key": "user_idle_s", "name": "User idle", "type": "number", "unit": "s",
        "class": "duration", "state_class": "measurement" }
    ],
    "actions": [
      { "key": "sleep", "name": "Sleep", "command": { "action": "sleep" } }
    ]
  }
}
```

`capabilities.programs` lists the configured program names (the
`ensure_program` values); it is left out when no program is configured.

---

## 4. `/state`

Published on (re)connect, every `state.interval_s` (default 30 s, no dedup —
`user_idle_s` changes on every refresh) and after every `sleep` command.

```json
{ "ts": "2026-10-10T09:41:00Z", "power": "awake", "user_idle_s": 1834, "device_online": true,
  "program_aethersdr": "failed", "program_aethersdr_error": "check: reply \"RPRT -1\"",
  "program_dax": "ready", "program_wsjtx": "stopped", "program_75auto": "stopped" }
```

Programs also publish `/state` on every `ensure_program` (once with
`starting`, once with the result) and every `stop_program`.

| Field | Type | Meaning |
|-------|------|---------|
| `ts` | RFC 3339 UTC | publish time |
| `power` | `"awake"` | always `awake`: a sleeping PC publishes nothing. Asleep = `/status` `offline`. |
| `user_idle_s` | int | seconds since the last keyboard/mouse input in the interactive user session (`GetLastInputInfo`). Remote input (ssh) does not count. |
| `device_online` | bool | always `true` (the agent runs on the device) |
| `error` | string, optional | see below |
| `program_<name>` | string | one per configured program: `stopped`, `starting`, `ready`, `failed` (section 7) |
| `program_<name>_error` | string, optional | the reason of `failed` |

`error` values:

| Value | When | Cleared |
|-------|------|---------|
| `user active, sleep refused` | `sleep` received, `user_idle_s` < `min_user_idle_minutes` | on the next `/state` publish (refresh or next command) |
| `user idle time unavailable, sleep refused` | `GetLastInputInfo` failed on `sleep` | on the next `/state` publish |
| `sleep failed: <reason>` | the PC cannot suspend (no S3 / Modern Standby) or `SetSuspendState` failed | on the next `/state` publish |
| `user idle time unavailable: <reason>` | `GetLastInputInfo` fails during a refresh | when it works again |
| `ensure_program refused: unknown program: <name>` | `ensure_program` with a name that is not configured; nothing starts | on the next `/state` publish |
| `stop_program <name> refused: <reason>` | reason `75auto session active`, `not started by the chain`, `unknown program`, or `end: <error>` | on the next `/state` publish |

A command outcome is carried in exactly one successfully published `/state`
(a refusal is an event, not a lasting condition). Consumers that want to show
it should latch it.

**Liveness (two layers).** The agent is the device, so `/status` alone carries
"PC awake and reachable". A consumer must treat a retained `/state` as stale
whenever `/status` is `offline`.

---

## 5. `/cmd`

```json
{ "action": "sleep" }
```

- Subscribed at **QoS 0** with a clean session: a command published while the
  PC is asleep or the agent is offline is lost, never delivered late.
- **Send it non-retained.** A delivery with the MQTT retain flag set (a broker
  replay on subscribe — at agent start or after resume) is **dropped** with a
  `Warn` log line, so a stale retained `sleep` can never put the PC to sleep at
  boot or right after a wake. In `powerseq`, set `retain = false` on the step.
  If a retained `sleep` is left on the broker, clear it:
  `mosquitto_pub -h 192.168.1.178 -u hf -P "$MQTT_PASSWORD" -t muehle/power/shack-pc/cmd -r -n`.
- `value` is ignored (powerseq's `cmd` step requires one; any string is fine).
- Unknown actions and malformed JSON are logged and ignored; `/state` is not
  touched. An empty payload is ignored silently.

- `ensure_program` and `stop_program` take the program name under `value`:
  `{"action":"ensure_program","value":"dax"}`. They are unsigned, like
  `sleep`: the program paths come only from the agent configuration, so a
  client with bus access can at worst start the programs or end the copies
  that the agent started outside a session.

### Sleep sequence

1. **Guard:** read the user idle time. If it is below
   `min_user_idle_minutes` → refuse (`/state.error`, `Warn` log). Done.
2. **Pre-check:** `IsPwrSuspendAllowed`. If the machine has no S1–S3 sleep
   state → `sleep failed: …`, the agent stays online. Done.
3. Publish `/status` = `offline` (retained, explicit — a clean disconnect
   suppresses the LWT), disconnect cleanly.
4. Enable `SeShutdownPrivilege`, call `SetSuspendState(FALSE, FALSE, FALSE)`
   (sleep, not hibernate; not forced; wake events enabled so WOL works). The
   call returns on resume.
5. Reconnect with a **fresh** paho client (retry every 5 s until the NIC is
   up), publish `online`, `/meta`, `/state` (no `error`).

If `SetSuspendState` itself fails after step 3, the agent reconnects at once
and publishes `sleep failed: …` (a short `/status` offline flap).

---

## 6. powerseq integration (example)

```toml
# idle standby (same step fits [[shutdown]]): put the shack PC to sleep — non-retained!
[[standby]]
name   = "shack PC sleep"
kind   = "cmd"
slot   = "power/shack-pc"
action = "sleep"
value  = "now"        # ignored by the agent; powerseq requires a value
retain = false

# wake (same steps fit [[startup]]): Wake-on-LAN, then wait for the agent
[[wake]]
name = "shack PC wake"
kind = "wol"
mac  = "<bwpc wired NIC MAC>"

[[wake]]
name      = "shack PC online"
kind      = "wait_status"
slots     = ["power/shack-pc"]
timeout_s = 120
```

A sleep that the guard refuses does not change `/status`; a sequencer that
waits for `/status` = `offline` after the sleep step times out in that case.

---

## 7. Programs

The `[[program]]` list of the configuration names the programs (see
`config.example.toml`). The agent starts them in the logged-on desktop session
in which it runs, with `CREATE_BREAKAWAY_FROM_JOB`, so a restart of the agent
task does not end them.

### Owned copies

A copy that the agent started is **owned**. The agent records it (PID and
process start time) in `programs-owned.json` next to the configuration, so an
agent restart keeps the record. At start the agent drops every entry whose PID
does not run with the recorded start time (the PID was reused). A copy that
the operator started is never ended and never restarted.

### `ensure_program`

The agent publishes `program_<name>` = `starting` at once, in every case, then
one of:

| Situation | Action | Result |
|---|---|---|
| owned copy runs, `restart_owned = true` | end it, start it again, check | `ready` or `failed` |
| a copy runs and passes its check | none (an operator copy stays not owned) | `ready` |
| owned copy runs and fails its check | end it, start it again, check | `ready` or `failed` |
| operator copy runs and fails its check | none | `failed`, `not started by the chain: <check reason>` |
| no copy runs | start it, record it as owned, check | `ready` or `failed` |

Ready checks (`check`):

| Kind | Ready when |
|---|---|
| `process` | after a start: the process still runs after `settle_s`. A copy that already ran: it runs. |
| `tcp` | a connect to `check_address` succeeds, the agent sends `probe` plus a newline, and the first reply line matches `reply_pattern`. After a start it tries once per second until `check_timeout_s`; a copy that already ran gets one try. For AetherSDR: probe `f` on the rigctl port; `RPRT -1` means no radio connection (for example, another client holds the last GUI client slot of the radio). |
| `none` | at once (75auto: powerseq waits for its `/status`). |

`failed` reasons include `start: <error>`, `end: <error>`,
`check: process exited within <n> s`, `check: reply "<line>"`,
`check: connect <addr>: <error>`.

The work runs on the jobs worker of the agent, so a long check delays the
next `/state` refresh and later commands until it ends.

### `stop_program`

Ends an owned copy: `WM_CLOSE` to its visible windows, so the program can
close cleanly (WSJT-X removes its lock file), and `TerminateProcess` if it
still runs after 10 s. Then `program_<name>` = `stopped`. It is refused:

- while a 75auto session is active (`session.source` = `live`,
  `session.state` `connecting` or `running`, and a `/beat` or live `/state`
  of `hf/auto-op` younger than 30 s). A replay session (`source` `replay`)
  drives no radio and does not count;
- for a copy that the operator started (`not started by the chain`).

With no copy running the result is `stopped`.

### Refresh

Every `/state` tick (30 s) the agent checks the programs again: an owned copy
that no longer runs leaves the record and shows `stopped`, and so does a
`ready` operator copy whose process is gone. Before its first
`ensure_program`, a program shows `stopped` (an owned copy that runs after an
agent restart shows `ready`).

### Power request

The agent holds a Windows power request (`PowerCreateRequest`, type
`PowerRequestSystemRequired`) while one of these is true:

- `muehle/hf/power-seq/state` has `chain.state` = `running`;
- `chain.state` = `ready` and the chain is younger than 30 min. The age comes
  from `chain.ts`; if `ts` does not parse, from the time the agent received
  that `/state`. powerseq holds a ready chain for 30 min while the console
  sends the session start;
- a 75auto session is active (definition above).

While `muehle/hf/power-seq/status` is `offline`, the chain does not count: a
retained `/state` of a powerseq that stopped can show `running` for ever. A retained `hf/auto-op` `/state` that arrives at subscribe does not
count as a heartbeat. In all other cases the request is released, also with
owned programs still running, so the PC sleeps as before. The agent checks
this every 5 s and on every message of those topics. `powercfg /requests`
shows the request on the PC.

### powerseq chain steps (example)

```toml
[[auto_start]]
name   = "dax"
kind   = "cmd"
slot   = "power/shack-pc"
action = "ensure_program"
value  = "dax"
retain = false          # the agent drops retained commands
```

followed by a `wait_state` on `program_dax` = `ready` that ignores a `/state`
from before the step and fails on `failed`.
