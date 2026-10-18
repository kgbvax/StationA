# MQTT schema — winpc-power-bridge

This document describes the MQTT interface of **winpc-power-bridge**, the
sleep agent on the Windows shack PC (`bwpc`, 192.168.1.197). It implements the
slot `muehle/power/shack-pc` of the station integration model
(`../../docs/station-integration-model.md`). It is the on-the-wire contract,
derived from `internal/bridge/bridge.go` and `cmd/winpc-power-bridge/main.go`.

The agent can only put the PC to **sleep**. It cannot wake it: a sleeping PC
runs no code. The wake path is Wake-on-LAN, sent by `powerseq` (`kind = "wol"`
step with the PC's wired NIC MAC).

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1, plain TCP (default `tcp://192.168.1.50:1883`, the live broker) |
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
| `muehle/power/shack-pc/cmd` | **no** | bus → agent | `sleep` request |

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
  "capabilities": { "actions": ["sleep"], "min_user_idle_minutes": 30 },
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

---

## 4. `/state`

Published on (re)connect, every `state.interval_s` (default 30 s, no dedup —
`user_idle_s` changes on every refresh) and after every `sleep` command.

```json
{ "ts": "2026-10-10T09:41:00Z", "power": "awake", "user_idle_s": 1834, "device_online": true }
```

| Field | Type | Meaning |
|-------|------|---------|
| `ts` | RFC 3339 UTC | publish time |
| `power` | `"awake"` | always `awake`: a sleeping PC publishes nothing. Asleep = `/status` `offline`. |
| `user_idle_s` | int | seconds since the last keyboard/mouse input in the interactive user session (`GetLastInputInfo`). Remote input (ssh) does not count. |
| `device_online` | bool | always `true` (the agent runs on the device) |
| `error` | string, optional | see below |

`error` values:

| Value | When | Cleared |
|-------|------|---------|
| `user active, sleep refused` | `sleep` received, `user_idle_s` < `min_user_idle_minutes` | on the next `/state` publish (refresh or next command) |
| `user idle time unavailable, sleep refused` | `GetLastInputInfo` failed on `sleep` | on the next `/state` publish |
| `sleep failed: <reason>` | the PC cannot suspend (no S3 / Modern Standby) or `SetSuspendState` failed | on the next `/state` publish |
| `user idle time unavailable: <reason>` | `GetLastInputInfo` fails during a refresh | when it works again |

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
  `mosquitto_pub -h 192.168.1.50 -u hf -P "$MQTT_PASSWORD" -t muehle/power/shack-pc/cmd -r -n`.
- `value` is ignored (powerseq's `cmd` step requires one; any string is fine).
- Unknown actions and malformed JSON are logged and ignored; `/state` is not
  touched. An empty payload is ignored silently.

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
