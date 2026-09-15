# mqtt-api — the wire contract of slot `muehle/uhf/radio`

This document is the field-for-field contract of the four MQTT planes this
bridge publishes and consumes. It matches the implementation in
`internal/bridge/` (plan U5, requirements R5–R10; KTD-3, KTD-6, KTD-8). Where
this document and the model's generic radio example
(`docs/station-integration-model.md` Appendix A) differ, THIS document wins
for this slot: the IC-9700 is a dual-VFO radio on an on-demand session
(KTD-2/KTD-3), so the flat active-TX state is extended with `main`/`sub`
detail objects and `device_online` means CI-V control-session liveness
(R16), not generic reachability.

All JSON is UTF-8. `meta`, `state`, `status` are retained QoS 1; `cmd` is
one-shot (published not-retained by producers, subscribed by the bridge at
**QoS 0**, cleared with an empty retained payload after every
execute-or-reject — KTD-6; console `cmdRetain['muehle/uhf/radio'] = false`).

---

## `muehle/uhf/radio/meta` — retained birth certificate

Published at every MQTT (re)connect from configuration and constants only —
there is no firmware read in v1 (bench pin), so the startup publish is final
and never clobbered by a later identity discovery.

```json
{
  "schema": "1.0",
  "role": "radio",
  "device": { "model": "Icom IC-9700" },
  "link": "ethernet",
  "location": "bauwagen",
  "capabilities": {
    "bands": ["2m", "70cm", "23cm"],
    "modes": ["cw", "usb", "lsb", "am", "fm", "data"],
    "bias_t": true,
    "satellite": true,
    "vfos": ["main", "sub"]
  },
  "expose": {
    "device": { "name": "muehle/uhf/radio", "model": "Icom IC-9700" },
    "fields": [
      { "key": "freq_hz",       "name": "Frequency",       "type": "number", "unit": "Hz", "class": "frequency", "state_class": "measurement" },
      { "key": "band",          "name": "Band",            "type": "enum",   "options_ref": "bands" },
      { "key": "mode",          "name": "Mode",            "type": "enum",   "options_ref": "modes" },
      { "key": "tx",            "name": "Transmitting",    "type": "boolean", "on": "tx", "off": "rx" },
      { "key": "selected_vfo",  "name": "Selected VFO",    "type": "string" },
      { "key": "satellite",     "name": "Satellite mode",  "type": "boolean" },
      { "key": "session_state", "name": "Session state",   "type": "string" },
      { "key": "device_online", "name": "Device online",   "type": "boolean" },
      { "key": "armed",         "name": "Armed",           "type": "boolean" },
      { "key": "s_meter",       "name": "S-meter",         "type": "number", "state_class": "measurement" },
      { "key": "tx_power",      "name": "TX power",        "type": "number", "state_class": "measurement" },
      { "key": "swr",           "name": "SWR",             "type": "number", "state_class": "measurement" },
      { "key": "alc",           "name": "ALC",             "type": "number", "state_class": "measurement" },
      { "key": "error",         "name": "Last error",      "type": "string" }
    ]
  }
}
```

- `bias_t: true` is **informational** — bias-t is set via the radio menu per
  deploy gate 3; there is no bus action in v1.
- `satellite: true` declares the satellite-mode capability (the `sat_mode`
  action toggles it; see `/cmd` below).
- The expose block is **read-only**: no `writable` fields, no `command`
  objects, no `actions[]`, and deliberately no PTT/arm widgets (the
  sat-rotator posture, R8). The action set lives in this document; a future
  expose `actions[]` is additive without breaking anything, and the
  `armed ∧ live` precondition is derivable from `/state`.
- `tx` declares the `"tx"|"rx"` string enum as a boolean with
  `on: "tx", off: "rx"` (the flexbridge shape).

---

## `muehle/uhf/radio/state` — retained snapshot

One retained JSON document per publish (last-write-wins; every publish is a
full snapshot). The shape is the KTD-3 hybrid: top-level **active-TX fields**
mirroring the TX VFO — the radio's SELECTED band, forced to **SUB in
satellite mode** (SUB is the uplink/TX side there) — plus `main` and `sub`
detail objects.

### Live session (all known fields present)

```json
{
  "ts": "2026-09-15T12:34:56Z",
  "freq_hz": 432100000,
  "band": "70cm",
  "mode": "fm",
  "tx": "rx",
  "main": {
    "band": "70cm",
    "freq_hz": 432100000,
    "mode": "fm",
    "data_mode": false,
    "preamp": 0,
    "attenuator": false
  },
  "sub": {
    "band": "2m",
    "freq_hz": 145200000,
    "mode": "fm"
  },
  "selected_vfo": "main",
  "satellite": false,
  "session_state": "live",
  "device_online": true,
  "armed": true,
  "s_meter": 34,
  "tx_power": 100,
  "swr": 12,
  "alc": 0
}
```

### Idle / connecting / error (radio-measured fields omitted)

```json
{
  "ts": "2026-09-15T12:36:20Z",
  "selected_vfo": "main",
  "session_state": "idle",
  "device_online": false,
  "armed": false
}
```

With an observed failure fact (`session_state: "error"`, or a safety-class
fact surviving the decay — see below):

```json
{
  "ts": "2026-09-15T12:36:20Z",
  "selected_vfo": "main",
  "session_state": "error",
  "device_online": false,
  "armed": false,
  "error": "login refused"
}
```

### Field reference

| key | type | presence | meaning |
|---|---|---|---|
| `ts` | string (RFC3339, UTC) | always | publish stamp; freshness heartbeat republishes an unchanged snapshot every 60 poll ticks so `ts` never goes stale |
| `freq_hz` | integer (Hz) | live only | active-TX VFO's frequency (mirrors `main.freq_hz` or `sub.freq_hz`) |
| `band` | string | live only | derived from `freq_hz` against the canonical table (`2m`, `70cm`, `23cm`); omitted outside it |
| `mode` | string | live only | canonical mode of the TX VFO: `cw`, `usb`, `lsb`, `am`, `fm`, `data`. RTTY/DV/DD mode reports **omit** the field (never published raw, R7) |
| `tx` | string | live only | `"tx"` keyed, `"rx"` receiving — the string enum, not a bool |
| `main`, `sub` | object | live only, when any field known | per-VFO detail, see below |
| `selected_vfo` | string | always | `main` \| `sub` — bridge-held; survives session loss (R6); the SEL-marker source |
| `satellite` | boolean | live only | satellite mode on/off (16 5A read each poll tick); omitted when not live (R6) |
| `session_state` | string | always | `idle` \| `connecting` \| `live` \| `error` |
| `device_online` | boolean | always | **CI-V control-session liveness** — `true` only while `live`; healthy idle reads `false` (R16); `session_state` is the idle-vs-fault discriminator |
| `armed` | boolean | always | the bridge-held PTT permit (KTD-4); drops on session loss and restart (fail-disarmed, R11) |
| `s_meter` | integer 0–255 | live only, when read | raw S-meter level (S0=0, S9=120); ≤1 Hz, dedup-suppressed (KTD-8) |
| `tx_power` | integer 0–255 | live only, when read | RF output power (14 0A) of the selected band's VFO |
| `swr`, `alc` | integer 0–255 | live only, when read | raw SWR / ALC meter levels |
| `error` | string | when a fact is held | observed failure facts only — see taxonomy below; cleared by the next successful operator activity (arm/cmd) or the error-decay for non-safety facts |

### `main` / `sub` detail objects

| key | type | presence | meaning |
|---|---|---|---|
| `band` | string | when the VFO's frequency is in the canonical table | derived |
| `freq_hz` | integer (Hz) | when read | the VFO's frequency |
| `mode` | string | when read and canonical | RTTY/DV/DD omit (R7) |
| `data_mode` | boolean | when read/set | the 1A 06 data-mode modifier |
| `preamp` | integer 0–3 | after a `set_preamp` (cmd echo) | 0 both off, 1 P.AMP, 2 EXT-P.AMP, 3 both. Never polled in v1 — the per-band read scoping of 16 02 is a bench pin |
| `attenuator` | boolean | after a `set_attenuator` (cmd echo) | 11 00 off / 11 10 = 10 dB. Same bench-pin posture as `preamp` |

An empty VFO object is omitted whole; on session loss both objects disappear
(radio-measured fields are omitted, never zeroed or frozen — the next live
session re-reads both VFOs on its first poll tick).

### `/state.error` taxonomy (observed facts only)

| fact | class | source |
|---|---|---|
| `ptt rejected: not armed` | cmd rejection | PTT without the permit (R10, exact string) |
| `ptt rejected: session not live` | cmd rejection | PTT with the permit but no live session (R10, exact string) |
| `sat_mode rejected: tx active` | cmd rejection | satellite toggle while keyed (R10, exact string) |
| `sat_mode rejected: armed` | cmd rejection | satellite toggle while the permit is set (R10, exact string) |
| `freq rejected: out of band for main` / `...for sub` | cmd rejection | local band validation (23 cm is MAIN-only) |
| `mode rejected: unsupported "<value>"` | cmd rejection | non-canonical mode set (RTTY/DV/DD are not settable, R7) |
| `freq|mode|data|preamp|attenuator|power|ptt rejected: invalid value ...` | cmd rejection | malformed value (clipped to 200 runes) |
| `cmd payload too large` | cmd rejection | FIXED string — oversized payloads are never parsed or echoed |
| `stale cmd (age ..., bound ...)` | cmd rejection | ts-gate drop (stamped cmds older/younger than ±30 s) |
| `cmd ts unparseable: ...` | cmd rejection | non-RFC3339 ts |
| `radio: login refused` / `radio: connection refused` / `radio: handshake timeout` / `radio: session lost` / `radio: connect failed` | session fact | the on-demand connect series / session loss (R2/R3) |
| `radio rejected: civ: radio answered NG to <command>` | session fact | the radio refused a command frame |
| `ptt-off undeliverable: session unavailable` | **safety** | the outstanding PTT-off could not be delivered (R2/KTD-5) |

Safety-class facts (the watchdog trip and `ptt-off undeliverable`,
U6) survive the `error` → `idle` decay: `session_state` decays, the fact
stays in `/state.error` until operator ack (the next successful arm/cmd).
Cmd rejections persist until the next admitted cmd.

---

## `muehle/uhf/radio/status` — retained, and the Last Will

```
online
```
Plain string (`online` / `offline`), QoS 1 retained; `offline` is the LWT of
the bridge process. `/status` is the bridge liveness; `/state.device_online`
is the CI-V session liveness (two-layer rule — consumers AND both, with the
R16 carve-out that `device_online:false` is healthy idle here).

---

## `muehle/uhf/radio/cmd` — one-shot intent (bus → bridge)

Payload convention (stationa value-key rule): the argument rides under
`value`, never under a key named after the action. Per-VFO actions take an
optional `vfo` selector (`"main"` | `"sub"`, default: the radio's currently
selected band). An optional `ts` (RFC3339) enables the staleness gate:
stale/far-future stamped cmds are rejected, unstamped are tolerated.

Producers publish **not retained**; the bridge clears the topic with an empty
retained payload after every execute-or-reject.

| action | payload | effect |
|---|---|---|
| `set_freq` | `{"action":"set_freq","value":"432100000","vfo":"main"}` | tune the named VFO (Hz integer); locally validated first (23 cm is MAIN-only — out-of-band rejects with the exact string) |
| `set_mode` | `{"action":"set_mode","value":"usb","vfo":"sub"}` | set a canonical operating mode (`cw`/`usb`/`lsb`/`am`/`fm`); `data` is rejected (use `set_data`); anything else (DV/DD/RTTY) is rejected as unsupported |
| `set_data` | `{"action":"set_data","value":"on","vfo":"main"}` | data-mode modifier (1A 06); `on` carries FIL1 (no bus consumer for the filter choice in v1) |
| `set_preamp` | `{"action":"set_preamp","value":"2","vfo":"main"}` | preamp 0–3, applied to the named VFO's band |
| `set_attenuator` | `{"action":"set_attenuator","value":"on","vfo":"main"}` | ATT off / 10 dB, applied to the named VFO's band |
| `set_power` | `{"action":"set_power","value":"100","vfo":"main"}` | RF output power 0–255 (14 0A), applied to the named VFO's band (select-before-set, KTD-10) |
| `sat_mode` | `{"action":"sat_mode"}` | toggle satellite mode; **rejected** while `tx == "tx"` or `armed == true` (R10) |
| `arm` | `{"action":"arm"}` | set the PTT permit; while no session is held this drives the connect (R1/R2) — the cmd returns only when the series concludes; failure rejects with the observed fact and leaves `armed:false` (fail-disarmed) |
| `disarm` | `{"action":"disarm"}` | drop the permit |
| `ptt` | `{"action":"ptt","value":"on"}` | key/release. **Gated** on `armed ∧ session_state=live` (R10 exact rejection strings). The published `tx` always follows the radio's readback (a PTT read rides every dispatch), never tap optimism |

Any other `action` is warned and dropped (no `/state.error`); oversized
payloads (> 4 KB) get the fixed rejection and are never echoed; a cmd that
fails at the radio (connect refused, NG) rejects into `/state.error` with the
observed fact (R2).

Consumers confirm every action on `/state` — there is no ack plane. A
`set_freq` confirms via `freq_hz`/`band` (transceive broadcast or the next
poll tick), `arm` via `armed`, `ptt` via `tx`.

### U6 preview (safety core, next unit)

PTT dispatch passes through a settable `ArmGate func(armed, live bool) error`
(default: the R10 strings above) and a successful PTT-on fires an
`OnPTTOn` hook — the TX-watchdog seam (KTD-5). The watchdog, the MQTT-loss
PTT-off + disarm rule (R4), and the session-loss reissue land in
`internal/bridge/safety.go` without changing anything in this contract.
