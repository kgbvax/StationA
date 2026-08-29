# 03 — flexbridge: HF radio bridge (FLEX-8400)

## 0. Purpose

flexbridge is a small always-on daemon that mirrors the state of the station's HF transceiver — a **FLEX-8400**, a software-defined amateur-radio transceiver manufactured by FlexRadio ("amateur radio" is the licensed hobby of two-way radio communication on internationally allocated frequency bands) — onto the station's **MQTT** message bus (a lightweight publish/subscribe protocol), and accepts a small set of remote commands in the opposite direction. In the station model every device owns a **slot**: four MQTT topics `<site>/<station>/<slot>/{meta,state,status,cmd}`, where `meta` is a static "birth certificate", `state` is a live retained JSON snapshot, `status` is bridge-process liveness signaled via the MQTT **Last Will and Testament** (LWT — a message the broker publishes automatically when a client disappears uncleanly), and `cmd` carries one-shot command intents. flexbridge is the **radio slot** `muehle/hf/radio`. Downstream consumers — the antenna-selection policy service, the power-amplifier bridge, the operator console — subscribe to its `/state` to learn the tuned frequency, band, mode and transmit state, and drive band changes, voice-keyer playback, and microphone-profile loads through `/cmd`. The bridge is deliberately **read-only toward the radio except three narrow command families**: native band changes, digital-voice-keyer play/stop, and mic-profile load. Nothing on the bus can put the radio on an arbitrary frequency, mode, or power level.

This document is the complete normative specification for a re-implementation. The existing Go implementation in `flexbridge/` is the reference; its known defects are catalogued in §9 and must not be reproduced.

## 1. Vocabulary

- **SmartSDR** — the TCP/IP control protocol spoken by FlexRadio 6000-series radios (TCP port 4992, newline-delimited ASCII text).
- **Slice** — one receive channel ("VFO") inside the radio. The FLEX-8400 can run several; exactly one — the *active slice*, selected deterministically (§5.4) — drives the published state.
- **Panadapter** — the on-screen spectrum-display object in SmartSDR; each panadapter hosts slices and is identified by a **pan handle** (a hex stream id, e.g. `0x40000000`). Band changes are addressed to panadapters, not to slices.
- **DVK (Digital Voice Keyer)** — a SmartSDR v4+ feature: 12 recordable voice memories whose playback also keys the transmitter (puts it on the air).
- **Band** — an amateur frequency allocation, named by approximate wavelength ("20m" ≈ 14 MHz). The radio does not report a band; the bridge **derives** it from frequency (§5.4).
- **PTT / interlock** — the radio's transmit-keying path. A status frame `interlock state=TRANSMITTING` means the radio is on the air.
- **ATU (antenna tuner)** — a device that matches the antenna's impedance to the radio's; the radio's internal ATU reports a `tuning` status while it is matching.
- **PA (power amplifier)** — a device that boosts the radio's transmit signal; its *arm* relay in this station depends on this bridge's heartbeat (§3.4).
- **Retained message** — an MQTT message the broker stores and delivers to every future subscriber, giving new consumers the last known state without waiting for a change.

For the bus-wide contracts (planes, slot addressing, `/cmd` payload convention, liveness model) see `02-interface-spec.md`; for how the radio slot fits the station see `01-architecture.md` and `00-system-overview.md`.

## 2. Upstream interface — the FLEX-8400 over SmartSDR

### 2.1 Radio discovery (UDP, optional)

If no radio host is configured, the bridge SHALL discover the radio:

- It SHALL broadcast the literal ASCII payload `discovery` as a UDP datagram to `255.255.255.255:4992` from an ephemeral port.
- The radio replies with a whitespace-separated `key=value` text payload, e.g.:
  `version=3.4.1 serial=1234-5678-8400.12345 nickname=Flex6400 model=FLEX-8400 ip=192.168.1.50 port=4992 status=Available`
- The bridge SHALL parse keys (case-insensitive) `serial`, `model`, `nickname`, `ip`, `port`, `version`, `status`, assuming reply port 4992 when absent.
- Read deadline: the caller's context deadline, else 3 s (host-configured path) or 5 s (autodiscovery path). These are defaults.
- If a wanted serial number is configured, replies from other radios SHALL be skipped (case-insensitive serial compare) until the wanted one arrives or the timeout expires.
- If no serial is configured: the first reply is kept provisionally; if a reply with `status=Available` arrives, it SHALL be returned immediately. On timeout with only a non-Available reply seen, the first reply SHALL be returned.
- Failure modes: bind/broadcast failure, or read timeout with no reply at all (both are errors for the connect cycle).

### 2.2 Connection and framing (TCP, port 4992)

- The bridge SHALL connect via TCP to `<host>:4992`.
- The protocol is **newline-delimited ASCII**. Three line kinds, identified by the first character:
  - `C<handle>|<command> <args...>` — client → radio. The bridge always uses handle **1**, so its commands look like `C1|version`.
  - `R<handle>|<seq>|<body>` — reply to a command; body typically `0|OK` or `0|<error>`.
  - `S<handle>|<topic> <topic-args...> <key=value> <key=value> ...` — asynchronous status frames. The handle is usually `0` (the radio's own frames).
- Frame parsing rules (normative): trim `\r\n`; the first byte must be `R`, `S` or `C` (anything else → line skipped); everything up to the first `|` is the handle, the rest is the body. For `S` frames: the first whitespace-delimited word is the **topic**; the remaining raw text is split into topic-args (leading words that are not `key=value` tokens) and a key→value field map. Tokenization SHALL preserve double-quoted substrings (quotes retained, not stripped) so values containing spaces round-trip.
- Every command send SHALL carry a write deadline of 5 s (a default; shorter if a context deadline is set). Handshake reply reads SHALL carry a 5 s deadline likewise.

### 2.3 Handshake (exactly once per connection)

After TCP connect, the bridge SHALL perform this sequence, sending each command and reading lines until a matching `R1|` reply arrives. Interleaved `S|` status frames arriving during the await SHALL be dispatched to the status handler — none may be lost or buffered indefinitely.

1. `version` — connection probe. Failure aborts the connect cycle.
2. `sub slice all`
3. `sub radio all`
4. `sub interlock all`
5. `sub atu all`
6. `sub pan all` — panadapter status (pan handles + band/center); needed to address band changes. (Failure of any of 2–6 aborts the connect cycle.)
7. `info` — awaited but **non-fatal** if it fails. Reply body is comma-separated `key="value"` pairs; fields extracted: `model` (e.g. `FLEX-8400`), `chassis_serial` (e.g. `1126-1213-8400-3564`), `firmware_version` (e.g. `3.8.19`). A leading `<seq>|` prefix is stripped.
8. `sub dvk all` — **fire-and-forget** (no await). SmartSDR v4+/licensed only; a v3 or unlicensed radio rejects it, which must not break the handshake. Its reply, if any, is consumed by the read loop and dropped. It is sent *after* the awaited commands so its reply cannot be misattributed.
9. `profile mic info` — **fire-and-forget**. An undocumented one-shot (used by FlexLib); the radio replies asynchronously with a `profile mic list=...` status frame (§2.5.7). Best-effort; on older radios the mic-profile list stays empty.

The reference matcher ignores the reply sequence number (any `R1|...` line ends the await). This is safe only because the handshake serializes one command at a time; a re-implementation with any command concurrency SHALL match on real sequence numbers (see §9, defect 13).

The handshake SHALL NOT send `client udpport` or `meter list`; no meter telemetry is part of this contract (the reference implementation retains meter/VITA-49 parsing code that is dead — §9, defect 1).

### 2.4 Commands the bridge sends to the radio (complete list)

All runtime commands are fire-and-forget on the wire; confirmation comes via the status stream ("fire-and-observe"):

| Purpose | Wire command (appended after `C1|`) |
|---|---|
| DVK play memory N (also keys TX) | `dvk playback_start id=<N>` (N in 1–12) |
| DVK stop memory N (unkeys TX) | `dvk playback_stop id=<N>` |
| Band change via native band-stacking | `display pan s <panHandle> band=<bandNumber>`, where bandNumber is the wavelength in meters (`20m`→`20`, `160m`→`160`, `6m`→`6`) and panHandle is the verbatim hex handle string |
| Mic-profile load | `profile mic load "<name>"` (name double-quoted; may contain spaces) |

There SHALL deliberately be **no** set_freq, set_mode, or set_power command anywhere in the bridge: the bridge never tunes the radio except by band (a hard safety boundary — invariant 9, §8).

### 2.5 Status frames consumed

The bridge routes on `frame.Topic`:

#### 2.5.1 `slice` — per-slice receiver state

- Topic args: first = slice index (integer), second = receiver index (may be absent).
- Slice status frames are **incremental**: only changed fields appear. Each frame SHALL be merged onto the previously stored state for that slice index; absent fields carry over.
- Fields parsed:
  - `RF_frequency=<MHz float>` (primary; e.g. `3.800000` → 3,800,000 Hz). Conversion SHALL use rounding, not truncation: `round(mhz × 1e6)`. Truncation produces a 1-Hz-low value for ≈1.2 % of 10-Hz-step frequencies — this is a correctness bug, not a nicety.
  - Legacy fallback `freq=<dotted Hz>` (e.g. `14.100.000`, dots stripped).
  - `mode` — raw firmware string (`USB`, `LSB`, `CW`, `DIGU`, …), normalized per §5.6 before publication.
  - `active=1|0`, `tx=1|0`, `agc_mode=` (older firmware: `agc=`), `filter_lo`/`filter_hi` (Hz ints), `pan=<handle>` (hex pan stream id the slice sits on).
- A malformed frequency value SHALL be non-fatal: the previous frequency is retained and the frame's remaining fields are still applied (warn log).
- **Slice removal** SHALL delete the tracked slice entry. Removal is detected by any of: a trailing topic-arg `removed` (frame `S|slice <n> <r> removed`), `in_use=0`, or `removed=1`. Missing this leaves a phantom slice that flips the published state (live-observed).
- Per-slice band with hysteresis is tracked per slice index (§5.4).

#### 2.5.2 `display` — panadapter status. **Two-word-topic quirk**

The SmartSDR panadapter status topic is the **two-word "display pan"**: the frame arrives with `Topic == "display"` and the literal word `pan` as the first topic arg. The bridge SHALL gate on `Topic == "display"` **and then** on topic-args starting with `pan`. Frames with other leading args (`panafall`, `panf`) SHALL be ignored. Routing on the single word `pan` alone (or treating the topic as one word) is the classic re-implementation error here.

- Pan handle = the topic arg after the literal `pan` (kept as the raw hex string).
- Fields: `band=<wavelength>` (often absent — pan status carries `center`, not band), `center=<MHz float>` → Hz (rounded).
- Removal detection identical to slice removal (`removed` arg / `in_use=0` / `removed=1`).
- A tracked pan reporting `band == <target band number>` SHALL confirm a commanded band change and release the band-transition hold early (§5.5).

#### 2.5.3 `interlock` — transmit state

- `state=<STATE>`, uppercase: `RECEIVING`, `TRANSMITTING`, `READY`, `PTT_REQUESTED`, `ERROR`, else `UNKNOWN`. Only `state=TRANSMITTING` sets the published `tx="tx"`. The `cause=` field is captured but unused downstream.

#### 2.5.4 `atu` — antenna-tuner status

- `status=<value>` (case-insensitive; e.g. `tuned`, `bypass`, `tuning`). The published `tuning` flag SHALL be true iff `status == "tuning"`. `active=1` is also parsed.

#### 2.5.5 `radio` — radio-wide status

- `drive=<0-100>` → published `drive`. `tuning=1|0` → drives the same published `tuning` flag (shared with the ATU state; see defect 14 in §9 for the last-writer-wins semantics). `tx_power`/`tune_power` parsers exist but are unused — not part of the contract.

#### 2.5.6 `dvk` — Digital Voice Keyer status (SmartSDR v4+; flows only after `sub dvk all`)

- Frame shape: `S<h>|dvk status=<idle|recording|preview|playback|disabled> [id=<N>] [enabled=1|0]`.
- `added`/`deleted` memory-library frames carry no `status=` key and SHALL be ignored (no state change).
- `idle` or `disabled` SHALL force the active memory id to 0 (cleared) even if the frame carries an id.

#### 2.5.7 `profile` — mic-profile list. **Caret-list quirk**

Mic profiles are **not broadcast** (`sub radio all` does not include them); the list arrives only as the asynchronous reply to the one-shot `profile mic info` sent during the handshake. The reply is a status frame:

`S<h>|profile mic list=Default^Default FHM-1^…^RTTYDefault^`

Parsing rules (normative):

- Names are **caret-delimited** (`^`), **names contain spaces**, and there is a **trailing caret** that must not yield an empty entry. The value cannot be space-split; the parser SHALL read the raw body after the word `mic` by hand and take everything up to end-of-line after `list=`.
- The list is an **authoritative full snapshot**: the known set SHALL be rebuilt (replaced) from it, sorted lexicographically, and published as the `/state` field `mic_profiles`. If the client-tracked active name is no longer in the list, it SHALL be cleared.
- Only profile type `mic` is tracked; `global` and `transmit` profile frames and `importing=`/`exporting=` flags SHALL be ignored.
- **The radio never reports the active mic profile.** Mic profiles are load-only presets with no "current" pointer (unlike global profiles, which emit `current=<name>`). A `current=` frame SHALL be honored defensively should firmware ever emit one, but as of firmware v4.2.20 (observed) it never arrives; the active name is tracked client-side (§4, `set_mic_profile`).

### 2.6 Connection-loss detection

- The read loop blocks on reading lines from the TCP connection; any read error (EOF, closed connection, or other) SHALL terminate the read loop and trigger the disconnect path (§5.3). There is no retry-within-loop.
- Process shutdown (context cancellation) SHALL close the TCP connection, which unblocks the read.
- There is **no application-level keepalive** toward the radio in the reference implementation: a silently dead TCP path (e.g. a black-holed route with no traffic) is detected only when a read fails. See §9, defect 5 — a re-implementation MAY add a read-timeout watchdog; if it does, the timing and behavior SHALL be specified.

## 3. MQTT presence

### 3.1 Slot address and broker

- Topics: `muehle/hf/radio/{meta,state,status,cmd}` (deployed values; site/station/slot are configurable, §7).
- Broker: `tcp://192.168.1.50:1883` in the deployed seed config (config key `mqtt.broker`; the shipped example points at `homeassistant.local`). **Broker-topology decision point:** deployed code points at the workstation-side broker at 192.168.1.50; a migration to a broker on shari (192.168.1.139) exists on an unmerged, undeployed feature branch. The re-implementation SHALL treat 192.168.1.50 as current production and make the broker address a deployment-time setting, not code.
- Credentials: user `hf`; password supplied via environment file, never in the config file (§7).
- MQTT client behavior (library-independent): auto-reconnect enabled, connect retry every 5 s; LWT configured as topic `<status>`, payload `offline`, QoS 1, retained; on every (re)connect the bridge publishes `online` to `<status>` (QoS 1, retained) and re-subscribes `<cmd>` (QoS 1). `/cmd` is intentionally not retained, so re-subscribing on every reconnect is mandatory.
- The initial MQTT connect SHALL be abortable by process shutdown (SIGTERM during a broker outage must not hang the stop; see §5.2).
- Publish QoS policy: retained topics at QoS 1, non-retained at QoS 0. In practice every topic this bridge publishes is retained, hence effectively all QoS 1.

### 3.2 `muehle/hf/radio/meta` — retained, QoS 1, JSON

The birth certificate, published **once per successful radio connect** — only after the handshake and only with the radio's real identity from the `info` reply (see the placeholder guard, §5.3, invariant 1). Exact payload shape:

```json
{
  "schema": "1.0",
  "role": "radio",
  "device": { "model": "<from info>", "serial": "<chassis_serial>", "firmware": "<firmware_version, omitted if empty>" },
  "link": "ethernet",
  "location": "<config mqtt.location, e.g. bauwagen; omitted if unset>",
  "capabilities": {
    "bands": ["160m","80m","60m","40m","30m","20m","17m","15m","12m","10m","6m"],
    "modes": ["cw","usb","lsb","am","fm","data"],
    "receivers": 1,
    "diversity": false,
    "amp_key": true,
    "tune": true,
    "bias_t": false,
    "rx_inputs": ["ant1","ant2","rx_a"],
    "tx_outputs": ["ant1","ant2"]
  },
  "expose": {
    "device": { "name": "FlexRadio <model>", "model": "<model>", "manufacturer": "FlexRadio Systems", "sw_version": "<firmware>", "area": "<location>" },
    "fields": [
      { "key": "device_online", "name": "Device online", "type": "boolean" },
      { "key": "freq_hz", "name": "Frequency", "type": "number", "unit": "Hz", "class": "frequency", "state_class": "measurement" },
      { "key": "band", "name": "Band", "type": "enum", "options_ref": "bands", "writable": true,
        "command": { "action": "set_band", "value_key": "value", "value_type": "string" } },
      { "key": "mode", "name": "Mode", "type": "enum", "options_ref": "modes" },
      { "key": "drive", "name": "Drive", "type": "number", "unit": "%" },
      { "key": "tx", "name": "Transmitting", "type": "boolean", "on": "tx", "off": "rx" },
      { "key": "tuning", "name": "Tuning", "type": "boolean" },
      { "key": "dvk_status", "name": "DVK Status", "type": "string" },
      { "key": "dvk_id", "name": "DVK Memory", "type": "number" },
      { "key": "mic_profile", "name": "Mic Profile", "type": "string", "writable": true,
        "command": { "action": "set_mic_profile", "value_key": "value", "value_type": "string" } }
    ],
    "actions": [
      { "key": "dvk_play_1", "name": "DVK Play 1", "command": { "action": "dvk_play_1" } },
      "... dvk_play_2 through dvk_play_12, one per memory, same shape ...",
      { "key": "dvk_stop", "name": "DVK Stop", "command": { "action": "dvk_stop" } }
    ]
  }
}
```

That is 13 actions total (12 DVK play buttons + stop). The `expose` block is the consumer-neutral discovery surface rendered by the separate `hadiscovery` service — it is not consumed by this bridge. The mic-profile **list** is deliberately absent from `expose.fields` (the expose schema has no array type); it lives only on `/state` as `mic_profiles`.

### 3.3 `muehle/hf/radio/state` — retained, QoS 1, JSON

A single complete JSON snapshot republished **on every state change**. No partial updates, no per-field topics; unchanged state produces no publish — *except* for the mandatory heartbeat republish of §3.4 and the forced disconnect publish of §5.3.

Exact field set:

| Field | JSON key | Type | Semantics |
|---|---|---|---|
| timestamp | `ts` | string | RFC3339 UTC (e.g. `2026-07-06T12:34:56Z`); always present |
| frequency | `freq_hz` | int64 | Hz as an integer — never kHz/MHz; 0 while unknown; always present |
| band | `band` | string, **omitted when empty** | derived from `freq_hz` (§5.4); canonical labels |
| mode | `mode` | string, **omitted when empty** | canonical (`cw`/`usb`/`lsb`/`am`/`fm`/`data`); raw firmware modes with no canonical mapping are never published |
| tx state | `tx` | string | `"tx"` or `"rx"`; always present |
| tuning | `tuning` | bool | true while ATU status is `tuning` or radio status `tuning=1` |
| drive | `drive` | int | 0–100 transmit drive level; always present |
| radio link | `device_online` | bool | true from handshake success until disconnect; always present (explicit `true`, not omitted — consumers may rely on either form, see `02-interface-spec.md`) |
| DVK status | `dvk_status` | string, omitted when empty | `idle` / `recording` / `preview` / `playback` / `disabled` |
| DVK memory | `dvk_id` | int, omitted when 0 | active memory id 1–12; 0 (omitted) on idle/disabled |
| mic profile active | `mic_profile` | string, omitted when empty | client-side tracked "last loaded" name |
| mic profiles available | `mic_profiles` | string array, omitted when empty | lexicographically sorted list from `profile mic info`; `/state`-only dynamic field |

Change-triggered publish set (any one → one full-snapshot publish): active/TX slice frequency, mode or band change; interlock TX↔RX transition; ATU tuning in/out; drive change; radio `tuning` flag change; DVK status or id change; mic-profile list change or active-name change; successful device identification (`device_online=true`); disconnect reset (`device_online=false`, all values zeroed — §5.3).

### 3.4 State heartbeat — HARD requirement

The reference implementation publishes `/state` **only on change**. This starves a live safety dependency: the station's PA arm relay (in the `m5stamp-hf-ctrl` firmware) de-energizes if its enabling inputs — including this bridge's radio `/state` — are not refreshed within 10 s. A change-only publisher lets the arm relay drop while the radio is idle-but-healthy, which happened live (see `06-safety.md`).

**The re-implementation SHALL republish the radio `/state` snapshot at least every 5 s while `device_online=true`, even when nothing has changed.** The republished payload may be byte-identical apart from `ts`. Five seconds is a default ceiling, derived from the 10 s relay deadline with a 2× safety margin; it must be documented as such. An equivalent liveness mechanism (e.g. a dedicated heartbeat topic consumed by the arming chain) is acceptable only if the PA-arm input refresh contract is updated accordingly — this is a cross-component decision, flagged in §11.

### 3.5 `muehle/hf/radio/status` — retained, QoS 1, plain string (not JSON)

Payload `online` after every MQTT (re)connect; payload `offline` via the broker LWT on unclean loss. **Actual-behavior contract:** on a *clean* process shutdown the reference bridge makes a clean MQTT disconnect, so the broker does **not** fire the LWT and the retained `online` remains on the broker while the service is stopped. A re-implementation SHALL either reproduce this behavior *and* document it, or better, explicitly publish `offline` before a clean disconnect (the reference's schema docs claim this but it was never implemented — §9, defect 3). Either way:

- `/status` alone means only "the bridge process is running (or was, at last unclean loss)".
- Consumers SHALL AND `/status` with `/state.device_online` (two-layer liveness — see `02-interface-spec.md`). `/state.device_online` is the layer that actually tracks the radio link.

### 3.6 `muehle/hf/radio/cmd` — subscribed, QoS 1, NOT retained, JSON

One-shot command intents from the bus to the bridge (§4). The subscription SHALL be re-established on every MQTT reconnect. `/cmd` is not retained so that a stale command cannot re-fire on bridge or broker restart.

### 3.7 Home Assistant discovery (legacy, gated OFF by default)

If `mqtt.publish_ha_discovery = true`, on first successful device identification per connect cycle the bridge also publishes retained HA discovery configs to `homeassistant/<component>/flexradio-<sanitized-serial>/<object_id>/config` for 7 entities reading `/state` via `value_template`: sensors Frequency (`{{ value_json.freq_hz }}`, Hz), Band, Mode, Drive (%); binary_sensors Transmitting (on `tx` / off `rx`), Tuning (template `{{ value_json.tuning | lower }}`, on `true`/off `false`), Device online (same pattern). `unique_id` = `flexradio-<sanitized-serial>_<object_id>`; availability topic = the `/status` topic with `availability_mode: all`; binary entities get `device_class: running`. The serial is sanitized: lower-cased, non `[a-z0-9_-]` → `_`. The deployed default is `false`; the preferred discovery path is the external `hadiscovery` service rendering from `/meta.expose` under node id `muehle-hf-radio`. Note the two paths produce different HA node ids (§9, defect 10).

## 4. Command surface (`/cmd`)

Payload convention (shared across the station, see `02-interface-spec.md`): JSON `{"action": "<action>", "value": "<argument>"}` — the argument always rides under the key **`value`**, never under a key named after the action; `value` is a JSON string (booleans as `"true"`/`"false"`). Not retained; **no acknowledgment is ever published** — callers confirm by observing `/state` (fire-and-observe). All commands SHALL be executed serially by a single worker (§5.2). Complete accepted-action list:

| `action` | `value` | Behavior |
|---|---|---|
| `set_band` | band label, e.g. `"20m"` (case/whitespace tolerant) | Map label to band number: `160m→160, 80m→80, 60m→60, 40m→40, 30m→30, 20m→20, 17m→17, 15m→15, 12m→12, 10m→10, 6m→6`. Anything else (`2m`, `70cm`, `23cm`, `gen`, `unknown`, garbage) SHALL be rejected with a warn log and no radio write. Resolve the target pan handle: the active slice's `pan` handle if that handle is tracked; else the lexicographically lowest tracked handle; else no-op with warn ("no panadapter tracked"). Send `display pan s <handle> band=<n>`. Then arm the band-transition hold (750 ms, §5.5). |
| `dvk_play` | memory id string `"1"`..`"12"` | Validate id in 1–12 (else drop). If the current mode is not a voice mode (`usb`/`lsb`/`am`/`fm`), log a warning that the radio may refuse — **not blocked**. Send `dvk playback_start id=<N>`. |
| `dvk_play_<N>` | (none; N in the action name) | Same as `dvk_play` for N; invalid N dropped. This form exists so UI buttons need no value injection. |
| `dvk_stop` | `"N"`, optional | With value: stop memory N (validated 1–12). Without value: stop the currently active memory, resolved from the live `/state` id; if none active, warn and drop. Send `dvk playback_stop id=<N>`. |
| `set_mic_profile` | profile name, e.g. `"Default ProSet HC6"` | Trim; validate: non-empty, ≤64 chars, no `"`, no `\`, no control chars (the name is embedded in a double-quoted wire string — this is also an injection guard); else drop with warn. If the tracked profile list is non-empty and the name is not in it, drop as a likely typo (an empty list does NOT block — it only means `profile mic info` hasn't answered yet). Send `profile mic load "<name>"`. Then, optimistically, set `/state.mic_profile` to the name client-side (the radio reports no active mic profile; see §9, defect 4). |

Any other action, unparseable JSON, or any command arriving while the radio link is down SHALL be logged at warn and dropped. Validation drops are silent on the bus (no error topic).

Explicitly **not** commands — this is not a general radio control channel: `set_freq_hz`, `set_mode`, `set_drive`, mic-profile save (obsolete on SmartSDR v4+, §9 defect 9), antenna/routing control. A re-implementation SHALL NOT add any of these; the absence of arbitrary-frequency tuning is a deliberate safety boundary (§8, invariant 9).

## 5. Behavior & state machine

### 5.1 Startup sequence

1. Parse flags (`-config <path>`, `-log.level <level>`); load TOML config (defaults → file → env overrides → flag overrides). Exit code 2 on config read/decode failure or if `mqtt.site`/`mqtt.station` are empty (slot addressing is mandatory).
2. Install a signal context (SIGINT, SIGTERM).
3. Construct the bridge core and the `/cmd` worker **before** connecting MQTT, so the subscription callback can dispatch immediately.
4. Connect MQTT with LWT (§3.1). Startup connect failure → exit 1. Success → publish `online`, subscribe `/cmd`.
5. Enter the radio connect/reconnect loop (§5.3).

### 5.2 Command dispatch model — library-independent constraint

**Incoming MQTT handlers must never block.** In the reference implementation's MQTT library, message callbacks run on the connection's dispatch thread; a handler that blocks or publishes synchronously deadlocks the client. This happened live in another component of this station. Regardless of the library used:

- The `/cmd` callback SHALL copy the payload bytes (the reference library reuses its buffer after the handler returns) and enqueue a closure onto a bounded queue of capacity **32** (default).
- A single worker SHALL drain the queue serially — command handling performs a blocking TCP write to the radio and must be off the receive path.
- On queue saturation the command SHALL be dropped, never block. The reference drops silently; a re-implementation SHOULD log the drop (§9, defect 6).
- The initial MQTT connect SHALL be abortable by process shutdown (SIGTERM during a broker outage must produce a clean exit, not a hang until the init system's kill timeout).

Radio TCP reads run on one goroutine/thread feeding the status handler; radio writes are serialized by a write mutex. The command-sender surface is injected into the bridge after each successful handshake and cleared on disconnect, so `/cmd` while the radio link is down is a logged no-op.

### 5.3 Radio connect/reconnect loop

- Backoff: initial **2 s**, multiplied by **1.5** after each failed cycle, capped at **60 s** (all defaults). Backoff sleeps SHALL be interruptible by process shutdown.
- Each cycle:
  1. **Resolve the radio**: if `radio_host` is configured, use it (if the serial is configured too, attempt a 3 s discovery to learn/verify the serial; on failure use the placeholder string `"flexradio"`). If no host is configured, run UDP autodiscovery with a 5 s timeout; failure → warn, backoff, retry.
  2. **Placeholder-meta guard — never publish placeholders.** `/meta` (and any identity) SHALL be (re)written only from the handshake `info` reply, with the radio's real serial and model. The bridge SHALL NOT publish identity derived from discovery or from a configured serial. Violating this clobbers the retained birth certificate with a bogus identity on every retry while the radio is off (live-observed incident). The placeholder serial is used only for logging ("radio resolved").
  3. Dial TCP, install the status handler, run the handshake (§2.3).
  4. After handshake: record the device identity; set `device_online=true`; publish `/meta` (once per connect) and a `/state` snapshot; install the command sender. If the `info` reply carried no serial, skip identity publication entirely (publish nothing).
  5. Run the read loop until disconnect.
  6. On disconnect (and not shutdown): warn log, `Reset`, backoff, retry.
- `Reset` SHALL clear: the slice map, per-slice band map, pan map, mic-profile set, interlock state, ATU status, the entire published state, the command sender, and any pending band transition — then **publish** a `/state` snapshot with `device_online=false` and all values zeroed/omitted. This forced publish is mandatory: `/state` is otherwise change-only and would freeze on the last live values, hiding the disconnect from consumers that watch only `/state`.

### 5.4 Band derivation (the core derived-state contract)

The radio reports no band. The bridge SHALL derive it from `freq_hz` with this table (edges inclusive; IARU Region 1 / Germany allocations):

| Band | Low Hz | High Hz |
|---|---|---|
| 160m | 1,800,000 | 2,000,000 |
| 80m | 3,500,000 | 4,000,000 |
| 60m | 5,351,500 | 5,366,500 |
| 40m | 7,000,000 | 7,300,000 |
| 30m | 10,100,000 | 10,150,000 |
| 20m | 14,000,000 | 14,350,000 |
| 17m | 18,068,000 | 18,168,000 |
| 15m | 21,000,000 | 21,450,000 |
| 12m | 24,890,000 | 24,990,000 |
| 10m | 28,000,000 | 29,700,000 |
| 6m | 50,000,000 | 54,000,000 |
| 2m | 144,000,000 | 146,000,000 |
| 70cm | 430,000,000 | 440,000,000 |
| 23cm | 1,240,000,000 | 1,300,000,000 |

- Frequency inside an allocation → that label. Frequency in HF general coverage 1.8–30 MHz but outside all allocations → `gen`. Anything else (VHF/UHF gaps, ≤0) → `unknown`.
- **Band-edge hysteresis** (2000 Hz, a default): each slice tracks its own previous band. If the new frequency's candidate band is `gen` but the frequency lies within 2 kHz just past the previous band's upper edge or just below its lower edge, the previous band is kept. Transitions into another ham band are never delayed — hysteresis guards only exits into the general-coverage gap. Per-slice (not global) previous-band state is load-bearing: with a single global previous, switching the active slice between two slices clobbered an edge-dwelling slice's held band.
- A nonzero frequency deriving to `unknown`/`gen` after hysteresis SHALL log a warning.

**Active-slice selection** — deterministic, must be preserved: among slices with `tx=1`, the **lowest index** wins; if none, among slices with `active=1`, the **lowest index** wins; if none, no state update. Lowest-index tie-breaking is not incidental: with unordered iteration, "first match found" makes freq/band/mode a coin flip per frame when two slices match — the exact live defect that produced flip-flopping published state.

**Invariant:** `/state.band` SHALL always be derived from `/state.freq_hz` — even after `set_band` (which uses native band-stacking; the radio picks the frequency). Band is never accepted from the radio or stored as an independent setpoint.

### 5.5 Band-transition hold (suppresses the set_band transient)

After a successful `set_band`, the bridge arms a transition `{target band, deadline = now + 750 ms}` (750 ms is a default). Rationale: SmartSDR switches the panadapter's band immediately but retunes the slice asynchronously, emitting intermediate slice frames still carrying the old frequency (outside the target band); without suppression the bridge would publish a transient wrong band and the antenna-selection consumer would chatter to the fallback antenna.

- While armed, unconfirmed, and before the deadline: slice-derived band changes whose derived band ≠ target SHALL be suppressed (the previously published band is kept); frequency and mode updates that land inside the target band are still applied.
- The hold SHALL be released early when any tracked panadapter reports `band == <target's number>`.
- At deadline expiry the hold is dropped and the next derived band is accepted as-is.
- A second `set_band` during a hold replaces the transition (new target/deadline). Disconnect abandons any pending transition.

### 5.6 Mode normalization (raw firmware → canonical)

`USB→usb, LSB→lsb, CW/CW-U/CW-L→cw, AM/SAM→am, FM/NFM/DFM→fm, DIGU/DIGL/DATA-U/DATA-L/FDV/FDVU/FDVL/RTTY-U/RTTY-L/PKTUSB/PKTLSB/DSTR→data`. Anything else → empty → the `/state.mode` field is omitted entirely. Raw firmware mode strings SHALL never be published.

### 5.7 Error paths summary

| Failure | Required behavior |
|---|---|
| Config unreadable/invalid; site/station empty | exit code 2 |
| MQTT connect failure at startup | exit 1 after logging |
| MQTT connection lost later | auto-reconnect (5 s retry interval default); `online` + `/cmd` resubscribe on return; LWT `offline` if the loss is unclean |
| Shutdown signal during initial MQTT connect | connect aborted, clean exit (§5.2) |
| Radio dial/handshake failure | cycle ends → `Reset` (publishes `device_online=false` zeroed snapshot) → backoff 2 s ×1.5 → max 60 s → retry |
| Radio TCP read error / EOF | same as above |
| Status frame parse failure | line skipped; slice frames with malformed frequency keep the previous frequency and still apply other fields (warn log) |
| Unknown `/cmd` action / bad value / radio offline | warn log, drop; no bus feedback |
| `set_band` with no panadapter tracked | warn log, no-op |
| `sub dvk all` / `profile mic info` rejected by the radio | ignored (fire-and-forget); DVK/mic fields stay empty — graceful degradation |

## 6. Configuration

File: TOML at `/etc/flexbridge/config.toml` (flag `-config` overrides). An empty or missing file is not an error (defaults used). Unknown keys are ignored.

| Key | Default | Meaning |
|---|---|---|
| `radio_host` | `""` | Radio IP/hostname. Empty = UDP autodiscovery. |
| `radio_serial` | `""` | Selects a specific discovered radio (case-insensitive); empty = first found. With `radio_host` set, used as an identity label — but never published to `/meta` (only the handshake `info` serial is). |
| `mqtt.broker` | `tcp://homeassistant.local:1883` | MQTT broker URL. Deployed value: `tcp://192.168.1.50:1883`. |
| `mqtt.client_id` | `flexbridge` | MQTT client id. If empty, derived `<site>-<station>-<slot>`. (The shipped default pre-fills `flexbridge`, making the derivation dead in practice — see §9, defect 2.) |
| `mqtt.user` / `mqtt.password` | `""` | Credentials; applied only if user is non-empty. |
| `mqtt.discovery_prefix` | `homeassistant` | HA discovery topic prefix (legacy embedded path only). |
| `mqtt.publish_ha_discovery` | `false` | Gates legacy embedded HA discovery; preferred path is external hadiscovery reading `/meta.expose`. |
| `mqtt.site` / `mqtt.station` / `mqtt.slot` | `""` / `""` / `radio` | Slot address `<site>/<station>/<slot>`; site+station mandatory (exit 2 if empty). Deployed: `muehle`/`hf`/`radio`. |
| `mqtt.location` | `""` | Physical location label (deployed: `bauwagen`); carried in `/meta` (`location`, `expose.device.area`). |
| `log.level` | `info` | `debug`/`info`/`warn`/`error`; flag `-log.level` overrides. |

Environment overrides (applied after the file; this is the secret-injection path): `FLEXBRIDGE_MQTT_BROKER`, `FLEXBRIDGE_MQTT_CLIENT_ID`, `FLEXBRIDGE_MQTT_USER`, `FLEXBRIDGE_MQTT_PASSWORD`, `FLEXBRIDGE_RADIO_HOST`, `FLEXBRIDGE_RADIO_SERIAL`, `FLEXBRIDGE_SITE`, `FLEXBRIDGE_STATION`, `FLEXBRIDGE_SLOT`. Non-empty values override the file.

Secrets convention: the MQTT password SHALL NOT be in the config file. It lives in `/etc/flexbridge/flexbridge.env` (mode 0600, owned by the service user), referenced by the service unit's `EnvironmentFile=`, injected as `FLEXBRIDGE_MQTT_PASSWORD`. Never on the command line, in shell history, or in the unit file. See `05-deployment-ops.md`.

The reference README documents `radio_udp_port = 4991` and a `[rates]` config section — **neither exists in the code** (stale documentation of the unwired meter feature, §9, defect 1). They are not part of this contract.

## 7. Deployment

Target host: `shari`, a Raspberry Pi at `192.168.1.139` (see `05-deployment-ops.md`). The reference `deploy.sh` (env-tunable: `SSH_HOST`, `SSH_USER`, `SERVICE_NAME`, `SERVICE_USER`, `INSTALL_DIR`, `BINARY`, and seed values `RADIO_HOST`, `RADIO_SERIAL`, `LOCATION`, `LOG_LEVEL`, `MQTT_BROKER`, `MQTT_SITE/STATION/SLOT/USER/PASSWORD`, `DISCOVERY_PREFIX`):

1. Cross-builds for `linux/arm64` (static binary, stripped) to `dist/flexbridge-linux-arm64`.
2. Generates the seed config TOML and the seed env file into `mktemp` files under `umask 077` (secrets never ride with the non-secret TOML).
3. Generates the service unit inline (below).
4. Copies binary, unit, and seeds to the target.
5. Remotely: creates a dedicated system user `flexbridge` (no home, no login shell) if absent; creates `/etc/flexbridge` (0755, service-user owned); **seed-once**: config and env files installed 0600 service-user owned only if they don't already exist — later deploys never touch them (the device owns its settings; change by editing on the device, or delete and redeploy). Stops the service, moves the binary into place (0755), installs the unit, reloads, enables, restarts.
6. Removes the transferred seed temp files.

Systemd unit (exact reference):

```
[Unit]
Description=FlexRadio 6000-series to MQTT bridge (flexbridge)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/flexbridge/flexbridge -config /etc/flexbridge/config.toml
EnvironmentFile=/etc/flexbridge/flexbridge.env
Restart=on-failure
RestartSec=5
User=flexbridge
Group=flexbridge
ConfigurationDirectory=flexbridge
StateDirectory=flexbridge
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
RestrictRealtime=true
RestrictSUIDSGID=true
RemoveIPC=true
CapabilityBoundingSet=
AmbientCapabilities=
ReadWritePaths=/var/lib/flexbridge
StandardOutput=journal
StandardError=journal
SyslogIdentifier=flexbridge

[Install]
WantedBy=multi-user.target
```

The unit has no stop-timeout override (systemd default 90 s) and no kill-signal overrides. Runtime dependencies: outbound TCP only (radio 4992 + MQTT 1883) and outbound/local UDP (discovery broadcast); fully unprivileged; no serial port, no disk writes (the state directory is reserved but unused). Any deployment achieving equivalent sandboxing, seed-once config, and secret isolation is acceptable.

## 8. Invariants & safety rules

Any re-implementation SHALL satisfy all of the following (testable statements):

1. **Never publish `/meta` (or any identity) before a completed handshake, and never with a placeholder serial.** `/meta` is written solely from the radio's `info` reply. (Live incident: publishing before the handshake clobbered the retained birth certificate with bogus identity on every retry while the radio was off.)
2. **Two-layer liveness**: `/status` = MQTT LWT layer (bridge process); `/state.device_online` = radio TCP link layer. `device_online` SHALL flip true on handshake success and false — with a forced republish of a zeroed snapshot — on disconnect. Consumers AND both layers; the contract keeps both present.
3. **`freq_hz` is an integer in Hz**, never kHz/MHz, always present in the snapshot (0 = unknown).
4. **Band and frequency can never disagree**: `/state.band` is always derived from `/state.freq_hz`, even after `set_band`. Band is never accepted from the radio as an independent value.
5. **Canonical vocabulary only**: modes normalized to `cw`/`usb`/`lsb`/`am`/`fm`/`data`; raw firmware mode strings never published (unknown mode → field omitted). Band labels per the table in §5.4; `gen` for HF general coverage; `unknown` otherwise.
6. **`/state` is a single complete retained JSON document**, republished on change (and per the heartbeat, §3.4); never per-field topics, never partial updates.
7. **`/cmd` is not retained** — one-shot intents; a stale command must not re-fire on restart. The argument rides under the `value` key.
8. **No command acknowledgment is published.** Results are observed on `/state`. No cmd-reply topic exists.
9. **Read-only toward the radio except** `set_band` (→ `display pan s …`), DVK play/stop, and `set_mic_profile` (→ `profile mic load`). No set_freq/set_mode/set_power command may exist — nothing on the bus can put the radio on an arbitrary frequency directly.
10. **Deterministic active-slice selection**: lowest-index TX slice, else lowest-index active slice. Randomized selection between equal candidates is a correctness bug (live-observed flip-flop).
11. **Slice/pan removal must delete tracked state** (else a phantom TX/active slice freezes the published frequency and jumps the state between phantom and real).
12. **Blocking work never runs in the MQTT message callback**: payload copied; dispatch via a bounded queue; drop-not-block on saturation.
13. **Mic-profile names are validated before wire-send** (no quotes, no backslash, no control chars; ≤64 chars) — the name is embedded in a double-quoted protocol string; this is an injection guard.
14. **Incremental slice frames merge onto previous state**; a single malformed field (especially frequency) must not drop the frame's other fields.
15. **Frequency conversion uses rounding, not truncation** (`round(mhz × 1e6)`); truncation is wrong by 1 Hz for ≈1.2 % of 10-Hz-step frequencies.
16. **`/status` clean-shutdown caveat**: a clean stop leaves retained `online` on the broker (no will fires on a graceful disconnect). Nothing may rely on `/status` alone for "radio reachable". Either reproduce this behavior and document it, or publish `offline` before clean disconnect — but consumers must never be required to trust `/status` alone.
17. **Heartbeat**: while `device_online=true`, the bridge SHALL republish `/state` at least every 5 s even without changes (§3.4) — derived from the PA-arm 10 s relay deadline; see `06-safety.md`.
18. **Shutdown signal during any connect or backoff** (MQTT or radio) SHALL produce a prompt, clean exit — no hang waiting for a network timeout or the init system's kill.

## 9. Known defects & fragilities of the reference implementation

These describe the reference (Go) implementation as observed on 2026-08-29. A re-implementation SHALL NOT reproduce items marked "do not reproduce"; the rest are documented fragilities to be aware of or improve on.

1. **README is substantially stale — do not reproduce the behavior it describes.** It documents a long-gone per-field topic scheme (`flexbridge/<serial>/state/...`), meter topics, `[rates]` and `radio_udp_port` config, per-slice entities, and "frequency in MHz". The actual bridge publishes the station-model slot topics with a single JSON snapshot in Hz and no meter topics at all. The UDP/VITA-49 meter machinery (`meters.go`, `vita49.go`, meter-list parser) is retained and tested but **dead code** — no UDP listener is wired, the handshake sends no `client udpport`/`meter list`. Where this PRD and the reference README disagree, this PRD (code-derived) wins.
2. **Client-id derivation is dead in practice.** The code derives `<site>-<station>-<slot>` only when `mqtt.client_id` is empty, but the built-in default config pre-fills `"flexbridge"`, so the deployed client id is `flexbridge` while the deploy script comments claim the derivation is the default. Consequence: duplicate-connection diagnosis sees `flexbridge`, not `muehle-hf-radio`. A re-implementation should pick one scheme deliberately (see §11).
3. **Clean shutdown leaves retained `online`** on `/status` (a graceful MQTT disconnect fires no will). The schema documentation's claim that the bridge publishes `offline` on clean shutdown was never implemented. Mitigated only by the `device_online` layer; a trap for new consumers.
4. **`set_mic_profile` active-name tracking is best-effort and unconfirmed**: the bridge optimistically sets `/state.mic_profile` right after sending the load command, assuming success. A profile switched directly in the radio's GUI is never reflected (the radio reports no active mic profile). A failed load leaves a wrong name published until the next list snapshot drops it.
5. **No radio-side keepalive**: a silently black-holed TCP path (no traffic, no read error) is not detected — no ping, no read-timeout watchdog. Reconnect waits for the OS to notice. A re-implementation SHOULD add a watchdog; if so, specify its timing.
6. **`/cmd` worker saturation drops commands silently** (queue of 32, no drop logging). One burst of >32 commands before the worker drains loses intents without a trace. Do not reproduce: log drops.
7. **`dvk_play` is not gated on voice mode** — only an advisory debug log; the radio refuses in CW/data. A consumer can trigger a TX-keying action expecting an effect in a mode where it cannot work.
8. **Band change requires a panadapter to be open in the radio's GUI**; with none tracked the command is a warn-log no-op. The manual precondition is invisible on the bus. (Design note: any UI commanding `set_band` should surface "no panadapter tracked" as an error the operator can act on.)
9. **`profile mic save` is obsolete on SmartSDR v4+** (the radio returns a malformed reply); profile creation uses a file-transfer mechanism out of scope. Re-implementations must not add a save action against the v4 wire protocol.
10. **Legacy embedded HA discovery vs the external hadiscovery service produce different HA node ids** (`flexradio-<serial>` vs `muehle-hf-radio`); switching paths leaves orphaned HA entities unless old discovery topics are cleared.
11. **Band-edge hysteresis guards only `gen` exits** (2 kHz). A frequency landing in another allocation's `unknown` gap switches immediately. Fine in practice; the asymmetry is deliberate, not accidental.
12. **The slice-status `pan=<handle>` field is best-effort** (confirmed live for this radio); if a firmware variant omits it, `set_band` silently falls back to the lowest tracked pan handle, which may target the wrong panadapter on multi-pan setups.
13. **Reply sequence numbers are ignored** in the reference matcher (first `R1|` wins). Safe only because the handshake serializes one command at a time. Do not reproduce under concurrency — track real sequence numbers.
14. **`tuning` is a last-writer-wins OR of two independent sources** (ATU `status=tuning` and radio `tuning=1`), with each handler writing the same published bool. A radio `tuning=0` frame immediately after an ATU `tuning` frame can clear the flag while the ATU is still tuning (narrow race, never observed live). A re-implementation should OR the two live source states instead.

## 10. Reference-implementation notes (non-normative)

The reference is a Go daemon (`flexbridge/`) using the Eclipse paho MQTT client and a hand-written SmartSDR client (`internal/flexradio/`). Implementation details free to change: the language and MQTT library, goroutine/channel structure, the `Commander` interface injection pattern, mutex-guarded state, TOML as config format (any equivalent config + env override is fine as long as secrets stay off the process command line), the frame struct/parser layout, the `sendAwaitReply` mechanics (a re-implementation should track real sequence numbers), the dead meter/VITA-49 code (drop it unless meter telemetry becomes a PRD requirement, in which case the README's meter table is aspirational, not current behavior), the legacy embedded HA discovery path, and the deploy-script mechanics (any deployment achieving equivalent sandboxing, seed-once config, and secret isolation). Shared plumbing (context-aware MQTT connect, topic helpers) lives in a shared library — see `03-components/common-runtime-library.md`.

What must be preserved verbatim is the behavior contract: all topic strings, retention/QoS policy, exact JSON field names/units/omitempty semantics, plain-string `/status`, the `/cmd` action grammar and validation rules, the SmartSDR wire surface (port 4992, `C1|` prefix, handshake order, the four command strings, the two-word "display pan" quirk, caret-delimited mic-list parsing, incremental slice merging, the removal encodings), all timing constants (backoff 2 s ×1.5 → 60 s; MQTT retry 5 s; handshake deadlines 5 s; discovery 3 s/5 s; band-transition hold 750 ms; hysteresis 2000 Hz; MQTT disconnect quiesce 500 ms; `/cmd` queue capacity 32), the band/mode tables, deterministic slice selection, per-slice hysteresis, the `device_online` lifecycle, and the never-publish-placeholder rule.

## 11. Open decisions & unresolved facts

1. **Broker topology** (station-wide): deployed code points at 192.168.1.50:1883; a migration to a broker on shari (192.168.1.139) exists on an unmerged, undeployed branch as of 2026-08-29. The re-implementation must decide the production broker address at deployment time; see `05-deployment-ops.md`.
2. **`device_online` wire form**: this bridge publishes explicit `device_online: true`/`false`; the integration model's text says "omitted when true". Consumers must treat absence as true, or the contract must mandate explicit values. Station-wide decision — see `02-interface-spec.md`.
3. **Clean-shutdown `/status`**: keep the reference behavior (clean disconnect leaves retained `online`, consumers rely on `device_online`) or require an explicit `offline` publish before graceful disconnect. The latter is cleaner for consumers; the reference never implemented it. Station-wide consistency matters (other bridges share the behavior).
4. **Radio `/state` heartbeat mechanism**: §3.4 mandates ≤5 s republish. Alternative designs (dedicated heartbeat topic consumed by the PA-arm chain, or moving the freshness requirement out of `/state` entirely) change the cross-component contract with `m5stamp-hf-ctrl` and `powerseq`; decide before freezing `02-interface-spec.md`. Evidence: live incident — change-only radio state starved the PA-arm 10 s heartbeat while the radio was idle-but-healthy.
5. **MQTT client id**: derived slot address (`muehle-hf-radio`) vs the pre-filled `flexbridge` (deployed). Derivation is more diagnosable; pick one and make the deploy seed match.
6. **Radio-side keepalive**: the reference has none (defect 5). Should the re-implementation add a read-timeout watchdog / periodic `info` ping? No timing evidence exists for how quickly a black-holed path is detected today; any added watchdog timing is a new default needing a decision.
7. **`dvk_play` voice-mode gating**: keep advisory-only (reference) or reject non-voice modes with an error? The radio already refuses; rejecting in the bridge would make the failure visible on the bus, but no feedback channel exists today (no-ack contract).
8. **Meter telemetry**: the README describes UDP VITA-49 meter streaming (SWR, power) that is dead code in the reference. No station consumer requires radio meters today (the PA bridge measures power at the amplifier). Decide whether meters are PRD scope; if yes, treat the README's meter table as the aspirational spec, not current behavior.
9. **Mic-profile "active" tracking**: the radio reports no active mic profile (firmware v4.2.20 observed), so `/state.mic_profile` is a client-side guess. Options: keep the optimistic guess, or omit the field and let consumers track loads themselves. The defensive `current=` frame handling should be kept either way (costless if firmware never emits it).
10. **Firmware-version assumptions**: the SmartSDR quirks documented here (two-word pan topic, caret mic list, no active-mic pointer, `profile mic save` malformed reply) were confirmed on firmware 3.8.19/v4.2.20-class radios. A re-implementation targeting substantially different firmware must re-verify each quirk against the actual radio; the two-word pan topic and the caret list in particular are undocumented behaviors confirmed only by live observation.