# MQTT schema — m5stamp-hf-ctrl

This document describes the MQTT interface exposed by **m5stamp-hf-ctrl**, the
M5 Stamp PLC #1 firmware. It implements **two** station integration-model
slots — `muehle/hf/switch` and `muehle/hf/pa-arm` — from one physical PLC (a
compound device; the shared `device{model,serial}` is the relationship, model
§3). It is the authoritative on-the-wire contract — derived from
`src/main.cpp`, `src/arm.h`, `src/mqtt_slot.h`, and `src/config.h`.

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1 (plain TCP, `MQTT_HOST` in `secrets.h` — the scmino broker `192.168.1.178:1883` since 2026-10-03) |
| Authentication | Username/password (`hf` user) |
| Clean session | true (PubSubClient 2.8 forces it); every subscription is re-issued on each reconnect and retained `/cmd` + input snapshots are replayed (self-heal) |
| Auto-reconnect | handled in `loop()` (WiFi + two MQTT clients) |
| Client IDs | one per slot: `muehle-switch`, `muehle-pa-arm` |
| Connections | **two** (one PubSubClient per slot, each with its own LWT) |

Two connections (not one) is deliberate: MQTT 3.1.1 allows one Will per client,
and the PLC publishes two slots that each need their own `<slot>/status` LWT.
One connection per slot means a PLC crash fires **both** wills at once — no
stale-online gap. This mirrors the Go compound bridge's one-client-per-slot
decision.

---

## 2. Topic addressing

All topics are addressed `<site>/<station>/<slot>/<suffix>`. The site is
`muehle` (hardcoded in `config.h`); both slots are under `hf/`.

| Suffix | Retained | Direction | Purpose |
|--------|----------|-----------|---------|
| `/meta` | yes | PLC → bus | birth certificate: identity + capabilities + `expose` |
| `/state` | yes | PLC → bus | live state (JSON snapshot) |
| `/status` | yes | broker LWT | liveness of **this PLC, this slot**: `online` / `offline` |
| `/cmd` | **yes** | bus → PLC | desired state (self-healing steady-state, §8) |

`/cmd` is **retained** for both slots: they hold steady-state intent. On
reconnect the broker replays the last command and the firmware re-applies it.

The firmware also **subscribes** the arm-logic inputs on the pa-arm connection
(`/status` + `/state` of `hf/radio`, `hf/ant-switch`, `hf/ant-ctrl`,
`hf/antenna-select`, `hf/pa`, `hf/tuner`, plus `hf/ant-ctrl/cmd`); it never
publishes there.

---

## 3. `muehle/hf/switch` — remote-on relays

Role `switch`. Non-exclusive multi-channel relay (relays 3 & 4 of the PLC).

### `/meta`

```json
{
  "schema": "1.0",
  "role": "switch",
  "device": { "model": "M5Stamp PLC", "serial": "m5stamp-plc-1", "firmware": "76b2291" },
  "link": "wifi",
  "host": "embedded",
  "capabilities": {
    "channels": ["pa", "trx"],
    "exclusive": false,
    "kind": { "pa": "remote_on", "trx": "remote_on" },
    "relay_map": { "pa": 3, "trx": 4 }
  },
  "expose": {
    "device": { "name": "M5Stamp PLC", "model": "M5Stamp PLC", "manufacturer": "M5Stack" },
    "fields": [
      { "key": "pa",  "name": "PA remote-on",  "type": "enum", "options": ["on","off"], "writable": true,
        "command": { "action": "set_pa",  "value_key": "value", "value_type": "string" } },
      { "key": "trx", "name": "TRX remote-on", "type": "enum", "options": ["on","off"], "writable": true,
        "command": { "action": "set_trx", "value_key": "value", "value_type": "string" } }
    ]
  }
}
```

### `/state`

```json
{ "ts": 123456, "pa": "on", "trx": "off", "device_online": true }
```

`pa` / `trx` are the **actual** relay positions, read back from relays 3 & 4
(not the last command). `ts` is PLC uptime ms (no RTC — a monotonic freshness
marker). `device_online` is the PLC itself (true while it is publishing).

### `/cmd`

```json
{ "action": "set_pa",  "value": "on" }
{ "action": "set_trx", "value": "off" }
```

`value` is `"on"`/`"off"` (case-insensitive) or a JSON bool; any other value is
dropped and the relay is left unchanged (it used to switch the relay off).

Each drives the named relay and publishes a new `/state` with the readback
position. `set_pa` drives relay 3; `set_trx` drives relay 4. The argument rides
under the conventional `value` key (stationa /cmd convention).

**PA power defaults off after a reboot.** All relays open at boot, and
`/cmd` is one retained topic holding only the last action, so its replay
would re-power whichever relay was commanded last. On the first connect after
boot the PLC therefore publishes the retained intent `set_pa off` itself and
ignores `set_pa on` until that echo comes back (10 s fallback). PA power then
needs an explicit `set_pa on` (console, powerseq START, button B). A later
reconnect without a reboot replays as before. TRX is not forced: the replay
lands before the boot `set_pa off`, so TRX comes back on if `set_trx on` was
the last retained action at reboot (seen live 2026-10-12) — and stays off if
the last action was anything else, including a previous boot's `set_pa off`.

---

`device.firmware` is the `git describe --always --dirty` of the checkout the
image was built from (`tools/fw_version.py`, both slots) — a `-dirty` or stale
hash on the bus means the PLC is not running a committed `origin/main` build.

## 4. `muehle/hf/pa-arm` — the arm relay

Role `pa-arm`. The PA-enable arm relay (relay 1), **fail-safe-open**, with the
arm logic embedded in the firmware. Same device as `hf/switch` (compound).

### `/meta`

```json
{
  "schema": "1.0",
  "role": "pa-arm",
  "device": { "model": "M5Stamp PLC", "serial": "m5stamp-plc-1", "firmware": "76b2291" },
  "link": "wifi",
  "host": "embedded",
  "capabilities": {
    "fail_safe": "open", "heartbeat": true, "relay": 1,
    "inputs": ["hf/radio", "hf/ant-switch", "hf/ant-ctrl", "hf/antenna-select", "hf/pa", "hf/tuner"],
    "ultrabeam_port": "port4"
  },
  "expose": {
    "device": { "name": "M5Stamp PLC", "model": "M5Stamp PLC", "manufacturer": "M5Stack" },
    "fields": [
      { "key": "armed",   "name": "Armed", "type": "boolean", "writable": false },
      { "key": "enabled", "name": "Enabled (arm permit)", "type": "boolean", "writable": true,
        "command": { "action": "set_enabled", "value_key": "value", "value_type": "string" } },
      { "key": "block",   "name": "Arm blocked by", "type": "string", "writable": false }
    ]
  }
}
```

### `/state`

```json
{ "ts": 123456, "enabled": true, "armed": false, "device_online": true,
  "block": "ultrabeam moving" }
```

| Field | Type | Meaning |
|-------|------|---------|
| `ts` | number | PLC uptime ms (freshness marker) |
| `enabled` | bool | the arm **permit** (last `set_enabled` cmd or front-panel A toggle) |
| `armed` | bool | relay 1 **readback** (AW9523 output level) — derived, never commanded |
| `device_online` | bool | the PLC (true while publishing) |
| `error` | string, omitempty | a **fault** among the gates: `relay mismatch`, `radio offline`, `radio feed stale`, `antenna switch offline`, `ultrabeam offline` (only while the Ultrabeam is in circuit), `pa fault`. Published whether or not `enabled` (D7, decided: it tells the operator why an arm *would* be blocked). Normal transitions are never `error` — the console turns `error` into FAULTS rows |
| `block` | string, omitempty | why the arm is open or about to open: the first failing hazard gate, else the first readiness gate, else `permit off`, `waiting for rx` or `settling` (close dwell). Absent when armed and clear |
| `cmd_error` | string, omitempty | the last rejected `set_enabled` (`set_enabled: bad value`); cleared by the next valid one |

### `/cmd`

```json
{ "action": "set_enabled", "value": "true" }
{ "action": "set_enabled", "value": "false" }
```

`value` is `"true"`/`"false"` (case-insensitive) or a JSON bool. Anything else
leaves the permit unchanged and sets `/state.cmd_error` — never a silent disarm
(D5). There is **no** `arm` action — `armed` is derived, never commanded (§6
software-arm-permit AND hardware key). `set_enabled` only sets the permit; the
firmware arms when safe and drops automatically when not.

**Front panel:** button **A** (leftmost) toggles the permit locally and
publishes it as the retained `set_enabled` intent (the same wire form as
powerseq). A press made while the pa-arm connection is down is re-published on
reconnect, so the broker's older retained intent cannot revert it. Like `/cmd`,
it never forces the relay — every gate still applies.

### Arm logic (embedded, `src/arm.h`)

```
close relay 1  ⇐  every gate clear now
                  ∧ (enabled ∧ every non-move gate clear ∧ radio.tx == "rx") held continuously for ARM_CLOSE_DWELL_MS (1 s)
open  relay 1  ⇐  ¬enabled ∨ any HAZARD gate ∨ (any READINESS gate ∧ radio in RX)
```

*Move gates* are the gates of an Ultrabeam move in flight — `ultrabeam
commanded`, `ultrabeam moving`, `ultrabeam settling`, and `ultrabeam not tuned`
while one of those windows runs. They are timed windows themselves: they block
closing but do not restart the dwell, so the relay closes on the pass the
move's settle ends (e.g. 250 ms after the motors stop) — no second 1 s wait.

**HAZARD** gates open the relay at once, even while transmitting — RF is (or
is about to be) going somewhere unsafe. **READINESS** gates block closing and
open a closed relay only in RX — the station is still following a change or a
supervising feed is down, and hot-unkeying the PA under drive for bookkeeping
is worse than finishing the over (model §6: a slow arm permit ANDed with the
hardware key line).

| Gate | Class | Source | Blocks when | `block` text |
|------|-------|--------|-------------|--------------|
| relay | hazard, fault | relay 1 readback (AW9523 output level, not the contacts) | an open relay still reading closed after 3 rewrites, or 3 confirmed drops of a closed relay (a drop is acted on only after 3 consecutive open readbacks — one failed I²C read never unkeys the PA; a dropped relay re-closes only via dwell + RX). Clears after 20 consistent passes; drops are forgiven after 60 s held closed | `relay mismatch` |
| radio live | hazard, fault | `hf/radio/status` + `/state.device_online` + heartbeat | status ≠ online, or no **new** radio `/state` within 10 s, or device_online false | `radio offline` / `radio feed stale` |
| tuning | hazard | `hf/radio/state.tuning` | true (missing ⇒ true) | `radio tuning` |
| band | hazard | `hf/radio/state.freq_hz` → band | band (derived from freq, flexbridge edges + 2 kHz exit hysteresis) ∉ {160,80,40,30,20,17,15,12,10,6 m} — **60 m excluded** (15 W EIRP) | `band not safe` |
| switch live | readiness, fault | `hf/ant-switch/status` + `/state` | offline or no state since subscribe | `antenna switch offline` |
| grounded | hazard | `hf/ant-switch/state.selected` | `off` or empty | `antenna grounded` |
| switching | hazard | `hf/ant-switch/state.settled` | false (missing ⇒ false) — relays travelling | `antenna switching` |
| Ultrabeam\* retracted | hazard | `hf/ant-ctrl/state.retracted` ∨ a seen `retract` cmd | ultrabridge's latch (set by a successful retract, cleared by the next accepted freq/direction change; ultrabridge publishes /state at once after any accepted command, MQTT or web UI), plus the PLC's own cmd latch — cleared by the next motion cmd or by `retracted:false` after the blind window | `ultrabeam retracted` |
| Ultrabeam\* commanded | hazard | `hf/ant-ctrl/cmd` | 2.5 s after a command that will drive the motors, using ultrabridge's own rules: `frequency` / `band` (band centre) ≥ 25 kHz from current, any `direction`, `retract` — covers ultrabridge's 2 s poll blind spot | `ultrabeam commanded` |
| Ultrabeam\* live | readiness, fault | `hf/ant-ctrl/status` + `/state.device_online` | offline | `ultrabeam offline` |
| Ultrabeam\* moving | hazard | `hf/ant-ctrl/state.moving` | true (missing ⇒ true) | `ultrabeam moving` |
| Ultrabeam\* tuned | hazard | `hf/ant-ctrl/state.freq_hz` vs radio | \|⌊radio kHz⌋ − ub kHz\| ≥ 25 (ultrabridge's own deadband — exactly when it would retune) | `ultrabeam not tuned` |
| Ultrabeam\* settle | readiness | edges | **250 ms** after an observed stop (moving→false; replaces a longer window still running and ends the `commanded` window — unless the latest motion cmd's move has not been seen moving yet); **3 s** after an unobserved change (freq/direction changed with no moving:true seen — a short move can fall between ultrabridge's 2 s polls), a motion cmd, or switching onto the Ultrabeam port | `ultrabeam settling` |
| PA live | readiness | `hf/pa/status` + `/state.{device_online,fault,keyed}` | offline, or no live telemetry yet (empty `fault`/`keyed` — acombridge's open-time snapshot); barefoot is unaffected — the relay is only in the PA key line | `pa offline` |
| PA fault | hazard, fault | `hf/pa/state.fault` | ≠ `none` | `pa fault` |
| ATU tune | hazard | `hf/tuner/status` + `/state.settling` | tuner live and settling (an offline tuner does not block) | `tuner tuning` |
| band change | readiness | radio band edge | 3 s after the radio moves to a different ham band | `band change settling` |
| converged | readiness | `hf/antenna-select/status` + `/state.target` | reconciler online, target set and ≠ `ant-switch.selected` (a port move is pending; empty target = manual mode, no opinion) | `antenna not converged` |

\* Ultrabeam gates apply only while `ant-switch.selected == ULTRABEAM_PORT`
(`port4`, `config.h`; the station docs still say port 3 — known issue O14).

**Liveness.** Only flexbridge heartbeats its `/state` (5 s), so only the radio
feed has an age window; its heartbeat is refreshed only by a `ts` that differs
from the last one seen and is not the first message after a subscribe — a
retained replay (resubscribe, or the `.50`↔`.178` bridge reconnecting) cannot
revive a dead feed. The other producers publish change-only; their liveness is
the two-layer rule (`/status` LWT ∧ `device_online`). On WiFi loss, a pa-arm
disconnect, and each (re)subscribe, every input resets to the closed side, so
the arm only closes on data received over the current connection.

**Residuals.** (1) The `.178`↔`.50` mosquitto bridge (`topic muehle/# both`,
clean session) re-delivers `.50`'s retained topics when it reconnects — a
permit changed on `.178` while the bridge was down can be reverted by `.50`'s
older retained `set_enabled` (true of every retained `/cmd` during the mirror
period). (2) ultrabridge's `retracted` latch is process memory: an ultrabridge
restart after a retract reports `false` until the next retract. (3) The
change-only producers have no age window — a hung-but-connected ultrabridge or
ant-switch keeps its last state until its LWT fires.

**Not gated (deliberately):** PA band vs radio band and ATU in-line state —
antennaselect sends `set_band`/`set_inline` once and never re-sends, so a gate
would deadlock after a PA or ATR power cycle; the ACOM's own drive-frequency
check faults on a band mismatch (→ `pa fault`). Also not gated: rotator motion
(no RF hazard) and `muehle/power/psu-13v8` (R4.9 — the PLC runs from it).

The relay is energized **only** when the logic says close; it is re-driven and
read back every 50 ms loop pass. Loss of 13.8 V (which powers the PLC) or a PLC
crash drops it open by hardware.

---

## 5. `/status` — liveness (both slots)

Retained, driven by the per-slot MQTT LWT: `online` while that slot's
connection is up, `offline` when the PLC dies (or the will fires). This is
liveness of the **PLC**, not the controlled device — the relay/PA reachability
is `device_online` in `/state`.