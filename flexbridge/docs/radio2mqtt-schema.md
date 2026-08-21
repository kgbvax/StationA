# radio2mqtt MQTT Schema

Reference for the MQTT topics and payloads published by flexbridge. This is
the on-the-wire contract: what a subscriber sees, what every field means, and
what values to expect.

flexbridge implements the `radio` slot of the station integration model
(`station-integration-model.md`). The three planes are:

| Suffix | Retained | Direction | Purpose |
|---|---|---|---|
| `/meta` | yes | bridge → bus | birth certificate: identity + capabilities |
| `/state` | yes | bridge → bus | live radio state (JSON snapshot) |
| `/status` | yes | broker LWT | liveness: `online` / `offline` |
| `/cmd` | no | bus → bridge | intent: band changes (`set_band`) + DVK playback + mic-profile load (`set_mic_profile`) (one-shot, not retained) |

---

## Topic addressing

```
<site>/<station>/<slot>/<suffix>
```

Configured via `[mqtt]` in `config.toml`:

```toml
site     = "muehle"   # physical site
station  = "hf"       # transmitting entity
slot     = "radio"    # role (default: radio)
location = "bauwagen" # included in /meta; optional
```

Example for the default Mühle HF station:

```
muehle/hf/radio/meta
muehle/hf/radio/state
muehle/hf/radio/status
```

---

## `/status` — liveness

Plain string, retained, QoS 1. Written by the MQTT client, not the bridge.

| Value | When |
|---|---|
| `online` | published on every (re)connect |
| `offline` | broker Last Will on unclean disconnect; bridge publishes on clean shutdown |

---

## `/meta` — birth certificate

Retained JSON, published once per connect cycle (after handshake). A new
subscriber or a late-joiner gets this immediately without polling.

```json
{
  "schema": "1.0",
  "role": "radio",
  "device": {
    "model":    "FLEX-8400",
    "serial":   "1126-1213-8400-3564",
    "firmware": "3.8.19"
  },
  "link":     "ethernet",
  "location": "bauwagen",
  "capabilities": {
    "bands":     ["160m","80m","60m","40m","30m","20m","17m","15m","12m","10m","6m"],
    "modes":     ["cw","usb","lsb","am","fm","data"],
    "receivers": 1,
    "diversity": false,
    "amp_key":   true,
    "tune":      true,
    "bias_t":    false,
    "rx_inputs": ["ant1","ant2","rx_a"],
    "tx_outputs":["ant1","ant2"]
  },
  "expose": {
    "device": {
      "name": "FlexRadio 8400",
      "model": "FLEX-8400",
      "manufacturer": "FlexRadio Systems",
      "sw_version": "3.8.19",
      "area": "bauwagen"
    },
    "fields": [
      { "key": "freq_hz", "name": "Frequency", "type": "number", "unit": "Hz", "class": "frequency", "state_class": "measurement" },
      { "key": "band",    "name": "Band",      "type": "enum", "options_ref": "bands" },
      { "key": "mode",    "name": "Mode",      "type": "enum", "options_ref": "modes" },
      { "key": "drive",   "name": "Drive",     "type": "number", "unit": "%" },
      { "key": "tx",      "name": "Transmitting", "type": "boolean", "on": "tx", "off": "rx" },
      { "key": "tuning",  "name": "Tuning",    "type": "boolean" },
      { "key": "dvk_status", "name": "DVK Status", "type": "string" },
      { "key": "dvk_id",     "name": "DVK Memory", "type": "number" },
      { "key": "mic_profile", "name": "Mic Profile", "type": "string", "writable": true,
        "command": { "action": "set_mic_profile", "value_key": "value", "value_type": "string" } }
    ],
    "actions": [
      { "key": "dvk_play_1",  "name": "DVK Play 1",  "command": { "action": "dvk_play_1" } },
      { "key": "dvk_play_2",  "name": "DVK Play 2",  "command": { "action": "dvk_play_2" } },
      { "...": "dvk_play_3 .. dvk_play_12 (one action per memory, ids 1–12)" },
      { "key": "dvk_stop",    "name": "DVK Stop",    "command": { "action": "dvk_stop" } }
    ]
  }
}
```

### The `expose` block — consumer-neutral field surface

`expose` (integration model §3.1, Appendix C) is the **consumer-neutral** description of
this slot's observable field surface. It carries **no consumer vocabulary** — no
`device_class` strings, no Jinja templates, no `payload_on/off`. Consumers (the standalone
`hadiscovery` service for Home Assistant, and any future historian/dashboard/Prometheus
consumer) render their own representation from these neutral primitives.

flexbridge is **read-only for radio tuning state except `band` and `mic_profile`**, both
writable setpoints: `expose.fields[].band` carries `writable: true` with a `command` of
`{action:"set_band", value_key:"value", value_type:"string"}`, and `expose.fields[].mic_profile`
carries `writable: true` with a `command` of
`{action:"set_mic_profile", value_key:"value", value_type:"string"}`. A consumer renders
`band` as a select (enum) over `capabilities.bands`; selecting a band publishes
`{"action":"set_band","value":"<band>"}`. `/state.band` stays **derived from `freq_hz`** —
the `set_band` command triggers SmartSDR native band-stacking, the radio tunes to its own
persisted per-band frequency, and the bridge republishes that `freq_hz` with `band` derived
from it (the model's band/freq-can't-disagree invariant holds; the field is writable as an
input, not as a stored setpoint). `mic_profile` is a writable string setpoint: publishing
`{"action":"set_mic_profile","value":"<name>"}` loads that mic profile (SmartSDR native
`profile mic load`); the active profile is observed back on `/state.mic_profile`. The
**available** mic-profile list is dynamic, so it lives on `/state.mic_profiles` only — the
expose `fields` schema has no array type, so it is not declared in `expose.fields` (a
consumer reads it from `/state`; hadiscovery rendering of the dynamic list is future work).
The other read-write surface is one-shot `actions` (buttons), not writable setpoint
fields: the **Digital Voice Keyer (DVK)** (each play action publishes `{"action":"dvk_play_N"}`
with no value, so a consumer (hadiscovery → HA button) needs no value injection).
The `band`/`mode` enum options are not inlined; they
resolve via `options_ref` against `capabilities.bands` / `capabilities.modes` above (single
source of truth). The `tx` boolean carries `on`/`off` because `/state` holds the strings
`"tx"`/`"rx"`, not a bool; `tuning` omits them because `/state` holds a real bool.

The HA-specific rendering (component choice, `value_template`, unit→`device_class` map,
availability, device block) lives in `hadiscovery`, not in this bridge.

### `/meta` field reference

| Field | Type | Notes |
|---|---|---|
| `schema` | string | Schema version (`"1.0"`) |
| `role` | string | Always `"radio"` |
| `device.model` | string | Radio model from `info` command |
| `device.serial` | string | Chassis serial |
| `device.firmware` | string | Firmware version; omitted if not reported |
| `link` | string | Always `"ethernet"` |
| `location` | string | From config; omitted if unset |
| `capabilities.bands` | string[] | Canonical band names this radio covers |
| `capabilities.modes` | string[] | Canonical modes this radio supports |
| `capabilities.receivers` | int | Number of independent receive paths |
| `capabilities.diversity` | bool | Diversity receive available |
| `capabilities.amp_key` | bool | Hardware amplifier key output present |
| `capabilities.tune` | bool | Internal or external tuner present |
| `capabilities.bias_t` | bool | Bias-T for masthead preamp |
| `capabilities.rx_inputs` | string[] | Named RX antenna inputs |
| `capabilities.tx_outputs` | string[] | Named TX antenna outputs |

---

## `/state` — live radio state

Retained JSON snapshot, QoS 1. Published on every state change. The document
is always complete — no partial updates — so a subscriber always holds a
consistent view.

`/state` represents the **active TX receiver**: the slice or VFO currently
selected for transmit (or the single active receiver on a single-receiver
radio). Downstream devices (PA, antenna switch) always read from this topic
regardless of how many internal receivers the radio has.

```json
{
  "ts":     "2026-07-06T12:34:56Z",
  "freq_hz": 14025000,
  "band":   "20m",
  "mode":   "cw",
  "tx":     "rx",
  "tuning": false,
  "drive":  40,
  "device_online": true,
  "dvk_status": "idle",
  "dvk_id": 0,
  "mic_profile": "Default ProSet HC6",
  "mic_profiles": ["Contest", "Default ProSet HC6", "Ragchew"]
}
```

### `/state` field reference

| Field | Type | Unit | Notes |
|---|---|---|---|
| `ts` | string | — | RFC 3339 UTC timestamp of this publish |
| `freq_hz` | integer | Hz | VFO frequency of the active receiver |
| `band` | string | — | Canonical band name derived from `freq_hz`; see band table |
| `mode` | string | — | Canonical mode; see mode table |
| `tx` | string | — | `"tx"` while transmitting, `"rx"` otherwise |
| `tuning` | bool | — | `true` while the ATU or radio is actively tuning |
| `drive` | integer | % | Transmit drive level, 0–100 |
| `device_online` | bool | — | `true` while the radio TCP link is up; `false` on disconnect. `/status` is the MQTT/LWT bridge liveness, not the radio link |
| `dvk_status` | string | — | DVK operation: `idle` \| `recording` \| `preview` \| `playback` \| `disabled`. SmartSDR v4+; omitted when no DVK status has been reported. `disabled` means no SmartSDR+ license |
| `dvk_id` | integer | — | Active DVK memory id (1–12) while playing/recording/previewing; `0`/omitted when idle |
| `mic_profile` | string | — | Currently-loaded mic profile name (SmartSDR native mic profile). Best-effort: SmartSDR does not report an active mic profile, so the bridge tracks this client-side as the name most recently loaded via `set_mic_profile`; empty until the first load via the bus. Omitted when empty |
| `mic_profiles` | string[] | — | Available mic-profile names (sorted). Populated from the radio's reply to the one-shot `profile mic info` command (`profile mic list=A^B C^…` status frames; queried once in the handshake on connect). `/state`-only (not in `expose.fields`) |

### Mode values

All modes are normalized from firmware strings to the canonical vocabulary
before publishing. Adapters are responsible for this normalization; consumers
always see the canonical form.

| Canonical | Firmware strings mapped here |
|---|---|
| `cw` | `CW`, `CW-U`, `CW-L` |
| `usb` | `USB` |
| `lsb` | `LSB` |
| `am` | `AM`, `SAM` |
| `fm` | `FM`, `NFM`, `DFM` |
| `data` | `DIGU`, `DIGL`, `DATA-U`, `DATA-L`, `FDV`, `FDVU`, `FDVL`, `RTTY-U`, `RTTY-L`, `PKTUSB`, `PKTLSB`, `DSTR` |

Unknown firmware modes are lower-cased and passed through.

### Band values

Derived from `freq_hz` at publish time (IARU Region 1 / DL allocations).

| Band | Low (Hz) | High (Hz) |
|---|---|---|
| `160m` | 1,800,000 | 2,000,000 |
| `80m` | 3,500,000 | 4,000,000 |
| `60m` | 5,060,000 | 5,450,000 |
| `40m` | 7,000,000 | 7,300,000 |
| `30m` | 10,000,000 | 10,200,000 |
| `20m` | 14,000,000 | 14,400,000 |
| `17m` | 18,000,000 | 18,200,000 |
| `15m` | 21,000,000 | 21,500,000 |
| `12m` | 24,890,000 | 25,000,000 |
| `10m` | 28,000,000 | 30,000,000 |
| `6m` | 50,000,000 | 54,000,000 |
| `4m` | 69,000,000 | 71,000,000 |
| `2m` | 144,000,000 | 148,000,000 |
| `1.25m` | 222,000,000 | 225,000,000 |
| `70cm` | 430,000,000 | 440,000,000 |
| `33cm` | 902,000,000 | 928,000,000 |
| `23cm` | 1,240,000,000 | 1,300,000,000 |
| `gen` | 1,800,000 | 30,000,000 (general HF, outside ham allocations) |
| `unknown` | — | anything else, or frequency zero |

---

## `/cmd` — band-change, DVK & mic-profile intent

`/cmd` is **not retained** (QoS 1): all intents are one-shot triggers, and a stale
command must not re-fire on a bridge or broker restart (same rationale as the tuner
slot's `tune`). Payloads are JSON with the shared value-key convention — the argument
rides under `value`, never under a key named after the action.

Accepted actions:

| Action | `value` | Effect |
|---|---|---|
| `set_band` | `"<band>"` (e.g. `"20m"`) | **Native band-stacking.** Changes the band on the active slice's panadapter (`display pan s <handle> band=<wavelength>`, where the wavelength is the band number — `20m`→`20`, `160m`→`160`, `6m`→`6`) and the radio restores the last-used frequency/mode for that band. Only the FLEX-8400's regular bands (`160m`–`6m`) are supported; VHF/UHF XVTR bands and `gen`/`unknown` are rejected. Requires a panadapter to be open; if none is tracked the command is a logged no-op |
| `dvk_play_<N>` | — | Play DVK memory N (1–12) and key the transmitter (SmartSDR keys TX automatically on `playback_start`; no separate `xmit` needed). The per-memory form is what the HA buttons use — no `value` to inject |
| `dvk_play` | `"N"` | Same, value form (for scripts / Node-RED) |
| `dvk_stop` | `"N"` | Stop memory N and unkey |
| `dvk_stop` | omitted | Stop the currently-active memory, resolved by the bridge from `/state.dvk_id` |
| `set_mic_profile` | `"<name>"` | **Native mic-profile load.** Sends `profile mic load "<name>"` (SmartSDR's own mic profiles). The name is double-quoted on the wire, so it must not contain `"`/`\`/control characters (rejected as invalid). If `/state.mic_profiles` is populated and `name` is not in it, the command is dropped as a likely typo; an empty list does not block (the list is empty only before the first `profile mic info` response). The bridge then tracks the loaded name on `/state.mic_profile` client-side (SmartSDR reports no active mic profile) |

Examples:

```json
{"action":"set_band","value":"20m"}
{"action":"dvk_play_3"}
{"action":"dvk_play","value":"3"}
{"action":"dvk_stop"}
{"action":"dvk_stop","value":"3"}
{"action":"set_mic_profile","value":"Default ProSet HC6"}
```

**No command-ack is published.** Consumers observe the result on `/state` —
`freq_hz`/`band`/`mode` for `set_band`, `dvk_status`/`dvk_id` for DVK,
`mic_profile` for mic-profile load — the stationa
fire-and-observe plane discipline: send intent, watch state.

### Band-stacking notes

- The band command goes to the **panadapter handle**, not the slice. Panadapters are
  tracked from `sub pan all` status; `set_band` targets the active slice's panadapter,
  falling back to the single tracked pan (the common one-panadapter case), then the
  lowest handle for determinism. The pan-handle field in slice status (slice→pan
  correlation) is confirmed live; until then a single-pan station works via the
  single-pan fallback.
- The wire command (`display pan s <handle> band=<wavelength>`) and the
  wavelength-in-meters band-number form are from the SmartSDR TCPIP display-pan wiki
  and the FlexRadio community, confirmed live on shari the same way the DVK commands
  were.

### DVK prerequisites and caveats

- **SmartSDR v4+ and a SmartSDR+ license** are required. Without the license the radio
  emits `dvk status=disabled`; `dvk_play` is refused.
- **Voice modes only** (`usb`, `lsb`, `am`, `fm`). The radio refuses DVK in `cw`/`data`.
  The bridge does not block the command in a non-voice mode (it stays dumb; the radio is
  authoritative) — it only debug-logs that the radio may refuse. Consumers should gate the
  UI on `/state.mode` if they want to prevent the attempt.
- **12 memories**, ids 1–12. Out-of-range ids are rejected by the bridge (logged, dropped).
- The `dvk` wire strings were originally third-party-confirmed (AetherSDR impl vs
  FLEX-8600 fw 4.2.18) and are now confirmed against the live FLEX-8400 on shari. The
  official SmartSDR API wiki does not document the `dvk` command family.

### Mic-profile prerequisites and caveats

- flexbridge drives SmartSDR's **native** mic profiles (`profile mic load`), so the radio
  remains the single source of truth — it does not define its own preset/equalizer layer.
  Only `mic` profiles are tracked; `global`/`transmit` profiles are ignored. The load command
  (`profile mic load "<name>"`) is documented in the SmartSDR TCPIP profile wiki; the name is
  double-quoted and may contain spaces (e.g. `"Default ProSet HC6"`).
- **No save.** `profile mic save "<name>"` is obsolete on SmartSDR v4+ (the radio returns a
  malformed-reply error); profile creation/editing now uses a file-transfer mechanism that is
  out of scope for this bridge. So there is no `save_mic_profile` action — load only.
- **Profile list enumeration** is via the one-shot `profile mic info` command (undocumented
  in the SmartSDR wiki, but used by FlexLib and AetherSDR; confirmed live on a FLEX-8400,
  SmartSDR v4.2.20). The radio replies asynchronously with a `profile mic
  list=Default^Default FHM-1^…^RTTYDefault^` status frame — an authoritative full snapshot
  of the available mic profiles (caret-delimited; names may contain spaces; trailing
  caret). The bridge sends `profile mic info` in the handshake (best-effort, fire-and-
  forget like `sub dvk all`), so `/state.mic_profiles` populates on connect. There is no
  `sub profile` and no `profile list`; profiles do NOT arrive via `sub radio all`. The list is
  NOT re-queried after a load (the available set does not change on load); reconnect re-runs
  the handshake and re-populates it.
- **Active mic profile is not reported by the radio.** SmartSDR emits `profile <type>
  current=<name>` for `global` profiles but NOT for `mic` profiles (mic profiles are
  load-only presets with no "current" pointer, unlike global profiles). The bridge therefore
  tracks `/state.mic_profile` client-side as the name most recently loaded via
  `set_mic_profile` (best-effort: it assumes the load succeeded; the known-name typo guard
  makes a wrong load unlikely). A profile switched directly in the SmartSDR GUI will not be
  reflected. The `current=` frame is still honored defensively should a firmware revision
  emit it.

---

## Home Assistant auto-discovery

> **Two paths now exist (integration model §9).** The preferred path is the standalone
> `hadiscovery` consumer, which reads this bridge's `expose` block from `/meta` and renders
> HA discovery centrally. The legacy **embedded** discovery below is retained for
> reversibility but is **gated off** by `[mqtt] publish_ha_discovery = false` (the default).
> Set it `true` only to fall back during migration. Once `hadiscovery` is proven, the
> embedded discovery code will be deleted.
>
> The two paths use **different node IDs**: the embedded path uses `flexradio-<serial>`
> (below); `hadiscovery` uses `muehle-hf-radio` (the sanitized slot address). So switching
> from embedded to `hadiscovery` moves entities to a new device in HA — the old
> `flexradio-*` entities must be removed (clear their discovery topics) to avoid duplicates.

### Embedded discovery (legacy, gated, default off)

flexbridge publishes MQTT discovery payloads under `homeassistant/` (the
`discovery_prefix` in config). HA picks these up automatically; no manual
entity configuration is required.

Discovery node ID: `flexradio-<sanitized-serial>` (lower-cased, non-alphanumeric
characters replaced with `_`).

All entities read from the single `/state` topic using `value_template`.

| Entity | Component | Object ID | Template | Unit |
|---|---|---|---|---|
| Frequency | `sensor` | `frequency` | `{{ value_json.freq_hz }}` | Hz |
| Band | `sensor` | `band` | `{{ value_json.band }}` | — |
| Mode | `sensor` | `mode` | `{{ value_json.mode }}` | — |
| Drive | `sensor` | `drive` | `{{ value_json.drive }}` | % |
| Transmitting | `binary_sensor` | `transmitting` | `{{ value_json.tx }}` | payload on: `tx` / off: `rx` |
| Tuning | `binary_sensor` | `tuning` | `{{ value_json.tuning \| lower }}` | payload on: `true` / off: `false` |

Discovery config topics follow the pattern:

```
<discovery_prefix>/<component>/flexradio-<serial>/<object_id>/config
```

### Standalone discovery via `hadiscovery` (preferred)

With `publish_ha_discovery = false` (default), flexbridge publishes only the `expose`
block in `/meta`. The `hadiscovery` service subscribes to `muehle/+/+/meta`, reads
`expose`, and renders HA discovery under node ID `muehle-hf-radio`:

```
homeassistant/<component>/muehle-hf-radio/<object_id>/config
```

The entity set is the fields above (frequency sensor, band sensor, mode sensor, drive
sensor, transmitting binary_sensor, tuning binary_sensor, plus the DVK status/memory
sensors) **and the one-shot action buttons** — 12 `dvk_play_N` buttons + a `dvk_stop`
button, rendered by `hadiscovery` from the neutral `expose.actions`.
flexbridge itself contains no HA knowledge. See `../hadiscovery/docs/discovery-mqtt-api.md`
for the mapping.

---

## Multi-receiver radios

A radio that declares `receivers > 1` additionally publishes per-receiver
state at:

```
<site>/<station>/<slot>/receiver/<N>/state
```

with the same JSON shape as `/state`. The top-level `/state` still represents
the active TX receiver; the per-receiver sub-topics are for logging and
monitoring consumers only. No core station component (PA, antenna switch)
binds to sub-receiver topics.

The FLEX-8400 with a single active slice declares `receivers: 1` and publishes
only `/state`. If multiple slices are configured in SmartSDR, the TX slice
(or the active slice if none is transmitting) drives `/state`.

---

## Publish trigger summary

| Event | Topic updated |
|---|---|
| Connect / reconnect | `/meta` (incl. `expose`), `/status` (`online`); HA discovery only if `publish_ha_discovery = true` |
| Active/TX slice frequency or mode changes | `/state` |
| Interlock transitions TX ↔ RX | `/state` |
| ATU transitions into or out of Tuning | `/state` |
| Radio drive level changes | `/state` |
| Radio `tuning` flag changes | `/state` |
| DVK status changes (idle ↔ playback ↔ …) | `/state` (`dvk_status`, `dvk_id`) |
| Mic profile list (`profile mic list=` reply to `profile mic info`; queried on connect) | `/state` (`mic_profiles` list) |
| `/cmd` `set_band` received | radio band command sent; radio band-stacks → confirm on `/state` (`freq_hz`, `band`, `mode`) |
| `/cmd` DVK play/stop received | radio command sent; confirm on `/state` (`dvk_status`, `dvk_id`) |
| `/cmd` `set_mic_profile` received | radio `profile mic load` sent; bridge tracks `/state.mic_profile` client-side (radio reports no active mic profile) |
| Disconnect / crash | `/status` → `offline` (broker LWT) |

State is published only when a field actually changes value; unchanged state
does not produce a publish.
