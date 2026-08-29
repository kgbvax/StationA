# flexbridge — research spec for re-implementation

Source analyzed: `/Users/ingomar.otter/dev/stationa/flexbridge` (Go), commit context: branch `hf_console/review-fixes`, 2026-08-29. All statements below were verified against code, not READMEs. Where README/CLAUDE.md and code disagree, the code wins and the disagreement is flagged.

---

## 1. Purpose & role

**flexbridge** is a small always-on daemon that mirrors the state of a **FLEX-8400** (a software-defined amateur-radio transceiver manufactured by FlexRadio; "amateur radio" = licensed two-way radio hobby service on internationally allocated frequency bands) onto an **MQTT** message bus (a lightweight publish/subscribe protocol), and accepts a small set of remote commands.

Vocabulary used throughout this document:

- **SmartSDR** — the TCP/IP control protocol spoken by FlexRadio 6000-series radios (port 4992). Commands are newline-delimited ASCII.
- **slice** — one receive channel ("VFO") inside the radio. The FLEX-8400 can run several; exactly one drives the published state.
- **panadapter** — the on-screen spectrum display "waterfall" object in SmartSDR; each panadapter hosts slices and is identified by a **pan handle** (hex stream id, e.g. `0x40000000`). Band changes are addressed to panadapters, not slices.
- **DVK (Digital Voice Keyer)** — SmartSDR v4+ feature: 12 recordable voice memories whose playback also keys the transmitter.
- **band** — amateur frequency allocation, named by wavelength ("20m" ≈ 14 MHz). The radio does not report a band; the bridge **derives** it from frequency.
- **PTT / interlock** — the radio's transmit keying path; `interlock state=TRANSMITTING` means the radio is on the air.
- **station model** — this station's MQTT convention: every device ("slot") owns four topics `<site>/<station>/<slot>/{meta,state,status,cmd}` (meta = static birth certificate, state = live JSON snapshot, status = bridge liveness via MQTT Last Will, cmd = one-shot command intents).

Role in the station: flexbridge is the **radio slot** `muehle/hf/radio`. Downstream consumers (antenna-selection policy, PA bridge, operator console) subscribe to its `/state` to know the tuned frequency/band/mode and TX state, and drive band changes, DVK, and mic-profile loads through `/cmd`. It is **read-only toward the radio** except three narrow command families: band changes (native band-stacking), DVK play/stop, and mic-profile load.

---

## 2. Upstream interface — the FLEX-8400 over SmartSDR

### 2.1 Radio discovery (UDP, optional)

`flexradio.Discover(ctx, wantSerial)` in `internal/flexradio/discovery.go`:

- Broadcasts the literal ASCII payload `discovery` as a UDP datagram to `255.255.255.255:4992` (sent from an ephemeral wildcard-bound port).
- The radio replies with a whitespace-separated `key=value` text payload, e.g.:
  `version=3.4.1 serial=1234-5678-8400.12345 nickname=Flex6400 model=FLEX-8400 ip=192.168.1.50 port=4992 status=Available`
- Parsed keys (case-insensitive): `serial`, `model`, `nickname`, `ip`, `port`, `version`, `status`. Default reply port assumed 4992.
- Read deadline: the caller's context deadline, else 3 s. Callers use 3 s (host-configured path) or 5 s (autodiscovery path).
- If `wantSerial` is non-empty, replies from other radios are skipped (case-insensitive serial compare) until the wanted one arrives or timeout.
- If `wantSerial` is empty: the first reply is kept provisionally, but collection continues; a radio with `status=Available` is returned immediately. On timeout with only a non-Available reply seen, the first reply is returned.
- Errors: bind/broadcast failure, or read timeout with no reply at all.

### 2.2 Connection and framing (TCP, port 4992)

`internal/flexradio/client.go`:

- TCP connect to `<host>:4992` (constant `CommandPort = 4992`), using a context-aware dialer.
- Protocol is **newline-delimited ASCII**. Three line kinds, identified by the first character:
  - `C<handle>|<command> <args...>` — client → radio (commands). flexbridge always uses handle **1** (counter starts at 1), so its commands look like `C1|version`.
  - `R<handle>|<seq>|<body>` — reply to a command. Body typically `0|OK` or `0|<error>` (leading sequence number).
  - `S<handle>|<topic> <topic-args...> <key=value> <key=value> ...` — asynchronous status frames. `handle` is the connection handle (usually `0` for the radio's own frames).
- Parsing (`status.go ParseFrame`): trims `\r\n`; first byte must be `R`, `S` or `C` (anything else → line skipped); everything up to the first `|` is the handle; the rest is body. For `S` frames: first whitespace-delimited word is the **topic**; the remaining text (`RawBody`) is split into `TopicArgs` (leading words that are not `key=value` tokens) and `Fields` (map of `key=value`). Tokenization preserves double-quoted substrings (quotes retained, not stripped) so values containing spaces round-trip.
- Write deadline on every command send: 5 s (or the context deadline if one is set).
- Read deadline during handshake awaits: 5 s (or context deadline).

### 2.3 Handshake (exactly once per connection, `Client.Handshake`)

In order, each awaited (send then read lines until the matching `R1|` reply; interleaved `S|` frames are dispatched to the status handler so none are lost):

1. `version` — connection probe. Failure aborts the connect cycle.
2. `sub slice all`
3. `sub radio all`
4. `sub interlock all`
5. `sub atu all`
6. `sub pan all` — panadapter status (pan handles + band/center), needed to address band changes.
   (Any of 2–6 failing aborts the connect cycle with `subscribe %q: %v`.)
7. `info` — awaited but **non-fatal** if it fails. Reply body is comma-separated `key="value"`; fields extracted: `model` (e.g. `FLEX-8400`), `chassis_serial` (e.g. `1126-1213-8400-3564`), `firmware_version` (e.g. `3.8.19`). A leading `<seq>|` prefix is stripped.
8. `sub dvk all` — **fire-and-forget** (no await). SmartSDR v4+/licensed only; a v3 or unlicensed radio rejects it and that must not break the handshake. Its reply, when any, is consumed by the read loop and dropped. Sent *after* the awaited commands so its reply cannot be misattributed.
9. `profile mic info` — **fire-and-forget**. Undocumented one-shot (used by FlexLib); the radio replies asynchronously with a `profile mic list=...` status frame (see 2.5.4). Best-effort; older radios leave the mic-profile list empty.

The reply matcher ignores the sequence number: any `R1|...` line ends the await (flexbridge sends one command at a time during the handshake).

**Note**: the handshake does NOT send `client udpport`, does NOT send `meter list`, and no UDP meter listener is wired in `main.go`. The VITA-49 meter machinery (see §9) is currently dead code.

### 2.4 Commands flexbridge sends to the radio (complete list)

All fire-and-forget (`Client.Send`, no reply wait; confirmation comes via the status stream — "fire-and-observe"):

| Purpose | Wire command (appended after `C1|`) |
|---|---|
| DVK play memory N (also keys TX) | `dvk playback_start id=<N>` (N 1–12) |
| DVK stop memory N (unkeys TX) | `dvk playback_stop id=<N>` |
| Band change via native band-stacking | `display pan s <panHandle> band=<bandNumber>` where bandNumber is wavelength in meters (`20m`→`20`, `160m`→`160`, `6m`→`6`); panHandle verbatim hex string |
| Mic-profile load | `profile mic load "<name>"` (name double-quoted; may contain spaces) |

Handshake commands (2.3) are the only others. There is deliberately **no** set_freq/set_mode/set_power: the bridge never tunes the radio except by band.

### 2.5 Status frames consumed (topic → behavior)

`Bridge.HandleStatus` routes on `frame.Topic`:

#### `slice` — per-slice receiver state
- Topic args: first = slice index (int), second = receiver index (may be absent).
- Incremental updates: only changed fields appear; each frame is **merged onto the previously stored slice state** (absent fields carry over).
- Fields parsed: `RF_frequency=<MHz float>` (primary; e.g. `3.800000` → 3,800,000 Hz using `math.Round(mhz*1e6)` — rounding, not truncation, is load-bearing: `int64(mhz*1e6)` truncates ~1.2 % of 10-Hz-step frequencies 1 Hz low); legacy fallback `freq=<dotted Hz>` (e.g. `14.100.000`, dots stripped); `mode` (raw firmware string: `USB`, `LSB`, `CW`, `DIGU`, …); `active=1|0`; `tx=1|0`; `agc_mode=` (older firmware `agc=`); `filter_lo`/`filter_hi` (Hz ints); `pan=<handle>` (hex pan stream id the slice sits on).
- A malformed/unparseable frequency value is **non-fatal**: previous frequency retained, remaining fields of the frame still applied.
- **Slice removal** must delete the map entry; detected by any of: a bare `removed` trailing topic-arg (`S|slice <n> <r> removed`), `in_use=0`, or `removed=1`. Missing this leaves a phantom slice that flips the published state.
- Per-slice band with hysteresis is tracked per slice index (see §5.4).

#### `display` — panadapter status. **Quirk:** the SmartSDR panadapter status topic is the **two-word "display pan"**; the frame arrives with `Topic == "display"` and the literal word `pan` as the first topic arg, so the handler must gate on `Topic=="display"` **and then** `TopicArgs` starting with `pan`. Frames for `panafall`/`panf` (same `display` topic, other first arg) are ignored.
- Handle = topic arg after the literal `pan` (kept as raw hex string).
- Fields: `band=<wavelength>` (often absent — pan status carries `center`, not band), `center=<MHz float>` → Hz (rounded).
- Removal detection identical to slice removal (`removed` arg / `in_use=0` / `removed=1`).
- A tracked pan reporting `band == <target band number>` **confirms** a commanded band change and releases the transition hold early.

#### `interlock` — TX state
- `state=<STATE>` uppercase: `RECEIVING`, `TRANSMITTING`, `READY`, `PTT_REQUESTED`, `ERROR`, else `UNKNOWN`. Only `state=TRANSMITTING` sets the published `tx="tx"`. `cause=` field captured (unused downstream).

#### `atu` — antenna tuner status
- `status=<value>` (case-insensitive; e.g. `tuned`, `bypass`, `tuning`); published `tuning=true` iff `status == "tuning"`. `active=1` also parsed.

#### `radio` — radio-wide status
- `drive=<0-100>` → published `drive`. `tuning=1|0` → also drives the same published `tuning` flag (shared with ATU state). `tx_power`/`tune_power` parsers exist but are **unused** by the bridge.

#### `dvk` — Digital Voice Keyer status (SmartSDR v4+; only flows after `sub dvk all`)
- `S<h>|dvk status=<idle|recording|preview|playback|disabled> [id=<N>] [enabled=1|0]`.
- `added`/`deleted` memory-library frames carry no `status=` key and are ignored (no state change).
- `idle` or `disabled` force the active memory id to 0 (cleared) even if the frame carries an id.

#### `profile` — mic-profile list. **Quirk:** profiles are NOT broadcast (`sub radio all` does not include them); the list arrives only as the asynchronous reply to the one-shot `profile mic info` sent in the handshake. The reply is a status frame `S<h>|profile mic list=Default^Default FHM-1^…^RTTYDefault^`:
  - **caret-delimited** names; **names contain spaces**; **trailing caret** must not yield an empty entry. The value cannot be space-split — the parser reads the raw body after the word `mic` by hand, takes everything up to end-of-line after `list=`.
  - The list is an **authoritative full snapshot**: the known set is rebuilt (replaced) from it, sorted lexicographically, published as `/state.mic_profiles`. If the client-tracked active name is no longer in the list, it is cleared.
  - Only type `mic` is tracked; `global` and `transmit` profile frames, and `importing=`/`exporting=` flags, are ignored.
  - **The radio never reports the active mic profile** (mic profiles are load-only presets with no "current" pointer, unlike global profiles which emit `current=<name>`). A `current=` frame is honored defensively should firmware ever emit one for mic, but on current firmware (v4.2.20 observed) it never arrives.

### 2.6 Connection-loss detection

The read loop (`Client.Run`) blocks on `ReadString('\n')`; it returns (ending the connect cycle) on EOF, "use of closed network connection" / `net.ErrClosed`, or any other read error (no retry-within-loop: any read error terminates the run). Context cancellation also terminates it (the dial's derived context is cancelled on parent exit, and a goroutine calls `client.Close()` on context done, which unblocks the read with a closed-connection error). There is **no application-level keepalive/ping** toward the radio; a silently dead TCP path is detected only when a read fails.

---

## 3. MQTT presence

Broker: `tcp://192.168.1.50:1883` in the deployed seed config (config key `mqtt.broker`; the shipped example points at `homeassistant.local`). Credentials: user `hf`, password via environment file, never in TOML.

Client options (`main.go connectMQTT`):
- ClientID: from `mqtt.client_id` config; **if empty** derived as `<site>-<station>-<slot>` (would be `muehle-hf-radio`). Note the built-in default config sets `client_id = "flexbridge"`, so in practice the deployed client id is `flexbridge` unless the TOML explicitly clears it — see §9 (stale-default defect).
- `AutoReconnect=true`, `ConnectRetry=true`, `ConnectRetryInterval=5s`.
- LWT (Last Will and Testament — a message the broker publishes on unclean client loss): topic `<status>`, payload `offline`, QoS 1, retained.
- On every (re)connect (`OnConnect`): publish `online` to `<status>` (QoS 1, retained) and re-subscribe `<cmd>` (QoS 1). `/cmd` is intentionally not retained, so resubscribing on every reconnect is required.
- Connect is wrapped in `sharedmqtt.Connect` (see §5.2) because the paho client's `Connect().Wait()` ignores context cancellation.
- All bridge publishes go through a `Publisher` adapter that maps: retained → QoS 1, non-retained → QoS 0; publish tokens are **not** awaited (fire-and-forget; paho queues QoS 1). In practice every topic the bridge publishes is retained, so effectively everything is QoS 1.

Exact topics (site/station/slot configurable; deployed values in parentheses):

### `muehle/hf/radio/meta` — retained, QoS 1, JSON
Birth certificate, published **once per successful radio connect** (only after the handshake, only with the radio's real identity). Static capabilities for the FLEX-8400. Exact payload shape:

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
That is 13 actions total (12 play buttons + stop). `expose` is the consumer-neutral surface consumed by the separate `hadiscovery` service (not this bridge). The mic-profile **list** is deliberately not in `expose.fields` (the expose schema has no array type); it lives only on `/state`.

### `muehle/hf/radio/state` — retained, QoS 1, JSON
Single complete JSON snapshot republished **on every state change** (no partial updates, no periodic refresh; unchanged state produces no publish). Exact field set:

| Field | JSON key | Type | Semantics |
|---|---|---|---|
| timestamp | `ts` | string | RFC3339 UTC, e.g. `2026-07-06T12:34:56Z`; always present |
| frequency | `freq_hz` | int64 | Hz as an integer — never kHz/MHz; 0 while unknown; always present |
| band | `band` | string, **omitted when empty** | derived from freq_hz (see §5.4); canonical labels |
| mode | `mode` | string, **omitted when empty** | canonical (`cw`/`usb`/`lsb`/`am`/`fm`/`data`); raw firmware modes with no canonical mapping are **never** published (omitted) |
| tx state | `tx` | string | `"tx"` or `"rx"`; always present |
| tuning | `tuning` | bool | true while ATU status is `tuning` **or** radio status `tuning=1` |
| drive | `drive` | int | 0–100 transmit drive level; always present |
| radio link | `device_online` | bool | true from handshake success until disconnect; **always present** so consumers can tell a live radio from a frozen snapshot |
| DVK status | `dvk_status` | string, omitted when empty | `idle` / `recording` / `preview` / `playback` / `disabled` |
| DVK memory | `dvk_id` | int, omitted when 0 | active memory id 1–12; 0 (omitted) on idle/disabled |
| mic profile active | `mic_profile` | string, omitted when empty | client-side tracked "last loaded" name |
| mic profiles available | `mic_profiles` | string array, omitted when empty | sorted list from `profile mic info`; `/state`-only dynamic field |

Publish triggers (any → one full-snapshot publish): active/TX slice freq/mode/band change; interlock TX↔RX transition; ATU tuning in/out; drive change; radio `tuning` flag change; DVK status/id change; mic-profile list change or active-name change; `SetDevice` (device_online=true); `Reset` (device_online=false, all values zeroed).

### `muehle/hf/radio/status` — retained, QoS 1, plain string (not JSON)
`online` after every MQTT (re)connect; `offline` via broker LWT on unclean loss. **BEHAVIOR CONTRACT NOTE:** on a *clean* process shutdown the bridge calls `mqttClient.Disconnect(500ms)` — a clean MQTT disconnect — so the LWT does **not** fire and the retained `online` remains on the broker while the service is stopped. Consumers must therefore AND `/status` with `/state.device_online` (two-layer liveness); `/status` alone means only "the bridge process is running".

### `muehle/hf/radio/cmd` — subscribed, QoS 1, NOT retained, JSON
One-shot intents from the bus to the bridge (see §4). Subscription is re-established on every MQTT reconnect.

### Home Assistant discovery (legacy, gated OFF by default)
If `mqtt.publish_ha_discovery = true`, on first `SetDevice` per connect cycle the bridge also publishes retained HA discovery configs to `homeassistant/<component>/flexradio-<sanitized-serial>/<object_id>/config` for 7 entities reading the `/state` topic via `value_template`: sensors Frequency (`{{ value_json.freq_hz }}`, Hz), Band, Mode, Drive (%); binary_sensors Transmitting (on `tx` / off `rx`), Tuning (template `{{ value_json.tuning | lower }}`, on `true`/off `false`), Device online (same pattern). `unique_id` = `flexradio-<sanitized-serial>_<object_id>`; availability topic = the `/status` topic with `availability_mode: all`; binary entities get `device_class: running`. Serial is sanitized: lower-cased, non `[a-z0-9_-]` → `_`. Deployed default is `false`; the preferred discovery path is the external `hadiscovery` service rendering from `/meta.expose` under node id `muehle-hf-radio`.

---

## 4. Command surface (`/cmd`)

Payload JSON convention (shared across the station): `{"action": "<action>", "value": "<argument>"}` — the argument always rides under the key **`value`**, never under a key named after the action. `value` is a JSON string. Not retained; no ack is ever published; callers confirm by observing `/state` (fire-and-observe).

All commands are executed serially by a single worker (see §5.2). Complete accepted-action list (`HandleCommand`):

| `action` | `value` | Behavior |
|---|---|---|
| `set_band` | band label, e.g. `"20m"` (case/whitespace tolerant) | Look up band number (`160m→160, 80m→80, 60m→60, 40m→40, 30m→30, 20m→20, 17m→17, 15m→15, 12m→12, 10m→10, 6m→6`; anything else — `2m`, `70cm`, `23cm`, `gen`, `unknown`, garbage — is rejected with a warn log, no radio write). Resolve target pan handle: the active slice's `pan` handle if that handle is tracked; else the lexicographically-lowest tracked handle; else **no-op with warn** ("no panadapter tracked"). Then send `display pan s <handle> band=<n>`. Then arm the **band-transition hold** (750 ms; §5.5). |
| `dvk_play` | memory id string `"1"`..`"12"` | Validate id in 1–12 (else drop). If current mode is not a voice mode (`usb`/`lsb`/`am`/`fm`), debug-log a warning that the radio may refuse — **not blocked**. Send `dvk playback_start id=<N>`. |
| `dvk_play_<N>` | (none; N in the action name) | Same as above for N; invalid N dropped. This form exists so UI buttons need no value injection. |
| `dvk_stop` | `"N"` optional | With value: stop memory N (validated 1–12). Without value: stop the currently active memory, resolved from the live `/state` id; if none active, warn and drop. Send `dvk playback_stop id=<N>`. |
| `set_mic_profile` | profile name, e.g. `"Default ProSet HC6"` | Trim; validate: non-empty, ≤64 chars, no `"`, no `\`, no control chars (name is embedded in a double-quoted wire string — this is also an injection guard); else drop with warn. If the tracked profile list is **non-empty** and the name is not in it, drop as a likely typo (empty list does NOT block — it only means `profile mic info` hasn't answered yet). Send `profile mic load "<name>"`. Then, optimistically, set `/state.mic_profile` to the name client-side (the radio reports no active mic profile). |

Any other action, unparseable JSON, or any command arriving while the radio link is down (no commander installed) is logged at warn and dropped. Unknown-name/invalid-id drops are silent on the bus.

Explicitly **not** commands (not a general control channel): `set_freq_hz`, `set_mode`, `set_drive`, mic-profile save (obsolete on v4+, see §9), antenna/routing control.

---

## 5. Behavior & state machine

### 5.1 Startup sequence

1. Parse flags (`-config <path>`, `-log.level <level>`); load TOML config (defaults → file → env overrides → flag overrides). Exit code 2 on config read/decode failure or if `mqtt.site`/`mqtt.station` are empty (station-model addressing is mandatory).
2. Set up signal context (SIGINT, SIGTERM).
3. Construct the Bridge and the `/cmd` worker **before** connecting MQTT (so the subscription callback can dispatch immediately).
4. Connect MQTT with LWT (§3). On failure: exit with error. Success → `online` published, `/cmd` subscribed.
5. Enter `radioLoop`.

### 5.2 Threading / command dispatch model (behavior-relevant)

- paho runs message callbacks on its own dispatch goroutine; a callback must never block (a DVK command does a blocking TCP write to the radio). Therefore the `/cmd` callback **copies the payload** (paho reuses its buffer after the handler returns) and enqueues a closure onto a bounded channel of capacity **32**; a single worker drains it serially. If the buffer is full, the command is **dropped** (drop, never block).
- The MQTT connect itself is made context-aware by `sharedmqtt.Connect`: it starts paho's connect, waits on a side goroutine, and `select`s against context cancellation — so SIGTERM during a broker outage aborts the connect instead of hanging until systemd SIGKILL. (This was a live defect in acom1200s-pa-bridge; flexbridge now routes through the shared helper.)
- Radio TCP reads run on one goroutine feeding `HandleStatus`; writes are serialized by a write mutex. The Commander surface is injected into the Bridge after each successful handshake and cleared on disconnect (`Reset`), so `/cmd` while offline is a logged no-op.

### 5.3 Radio connect/reconnect loop (`radioLoop`)

- Backoff: initial **2 s**, multiplied by **1.5** after each failed cycle, capped at **60 s**. Sleeps are context-aware (SIGTERM during a backoff exits immediately).
- Cycle:
  1. **Resolve radio**: if `radio_host` configured → use it; serial from config, else a 3 s discovery attempt to learn it, else the placeholder string `"flexradio"` (see next point). If no host configured → discovery with a **5 s** timeout; failure → warn, backoff, retry.
  2. **Placeholders are never published.** The code carries an explicit guard: do **not** call `SetDevice` with a resolved/placeholder serial — `/meta` is (re)written only from the handshake's `info` reply with the radio's real identity. Publishing before the handshake clobbered the retained `/meta` with a bogus FLEX-6000/"flexradio" identity on every retry while the radio was off; the placeholder is used only for logging "radio resolved".
  3. Dial TCP, install status handler, run handshake (§2.3).
  4. After handshake: `SetDevice` (records identity; sets `device_online=true`; publishes `/meta` + `/state` snapshot; optionally HA discovery) and install the Commander. If the `info` reply had no serial, `SetDevice` is skipped (nothing published).
  5. `Run` the read loop until disconnect.
  6. On disconnect (and not ctx-cancelled): warn log, `Bridge.Reset()`, backoff, retry.
- `Reset()` clears: slice map, per-slice band map, pan map, mic-profile set, interlock, ATU status, the entire published state, the Commander, any pending band transition, the discovery-once flag — then **publishes** a `/state` snapshot with `device_online=false` and all values zeroed/omitted. This forced publish matters: `/state` is otherwise on-change-only and would freeze on the last live values, hiding the disconnect from consumers that only watch `/state`.

### 5.4 Band derivation (the core derived-state contract)

The radio reports no band. Exact derivation (`BandForFreq`), edges inclusive (IARU Region 1 / Germany allocations):

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
- **Band-edge hysteresis** (constant `BandEdgeHysteresisHz = 2000` Hz): each slice tracks its own previous band. If the new frequency's candidate band is `gen` but the frequency lies within 2 kHz just past the previous band's upper edge or just below its lower edge, the previous band is kept. Transitions *into* a ham band (candidate is another ham band) are never delayed — hysteresis guards only exits into the general-coverage gap. Per-slice (not global) previous-band state is load-bearing: with a single global prev, switching the active slice between two slices clobbered an edge-dwelling slice's held band.
- Out-of-band sanity: a nonzero frequency deriving to `unknown`/`gen` after hysteresis logs a warning.

**Active-slice selection** (`resolveActiveSlice`) — deterministic, must be preserved: among slices with `tx=1`, the **lowest index** wins; if none, among slices with `active=1`, the **lowest index** wins; if none, no update. Lowest-index tie-breaking is not incidental: with randomized map iteration, "first match found" makes freq/band/mode a coin flip per frame when two slices match — the exact live defect that produced flip-flopping state.

### 5.5 Band-transition hold (suppresses the set_band transient)

After a successful `set_band`, the bridge arms `bandTransition{target, deadline = now + 750 ms}`. Rationale: SmartSDR switches the panadapter's band immediately but retunes the slice asynchronously, emitting intermediate slice frames still carrying the old frequency (outside the target band); without suppression the bridge would publish a transient wrong band and the antenna-selection consumer would chatter to the fallback antenna.

- While armed and unconfirmed and before the deadline: slice-derived band changes whose derived band ≠ target are suppressed (the previously published band is kept); frequency/mode updates are still applied when they land inside the target band.
- The hold is released early when any tracked panadapter reports `band == <target's number>`.
- At deadline expiry the hold is dropped and the next derived band is accepted as-is.
- A second `set_band` during a hold replaces the transition (new target/deadline). Disconnect (`Reset`) abandons any pending transition.

### 5.6 Mode normalization (raw firmware → canonical)

`USB→usb, LSB→lsb, CW/CW-U/CW-L→cw, AM/SAM→am, FM/NFM/DFM→fm, DIGU/DIGL/DATA-U/DATA-L/FDV/FDVU/FDVL/RTTY-U/RTTY-L/PKTUSB/PKTLSB/DSTR→data`; anything else → `""` → the `/state.mode` field is omitted entirely (raw firmware mode strings must never be published).

### 5.7 Error paths summary

| Failure | Behavior |
|---|---|
| Config unreadable/invalid, site/station empty | exit 2 |
| MQTT connect failure at startup | exit 1 (after logging) |
| MQTT connection lost later | paho auto-reconnect (5 s retry interval); `online` + resubscribe on return; LWT `offline` if the loss is unclean |
| SIGTERM during initial MQTT connect | connect aborted via ctx-aware wrapper, clean exit |
| Radio dial/handshake failure | cycle ends → `Reset` (publishes device_online=false) → backoff 2 s ×1.5 → max 60 s → retry |
| Radio TCP read error/EOF | same as above |
| Status frame parse failure | line skipped; slice frames with malformed freq keep previous freq and still apply other fields (warn log) |
| Unknown `/cmd` action / bad value / radio offline | warn log, drop; no bus feedback |
| `set_band` with no panadapter tracked | warn log, no-op |
| `sub dvk all` / `profile mic info` rejected by radio | ignored (fire-and-forget); DVK/mic fields stay empty — graceful degradation |

---

## 6. Configuration

File: TOML at `/etc/flexbridge/config.toml` (flag `-config` to override). An empty/missing file is not an error (defaults used). Undecoded TOML keys are ignored.

| Key | Default | Meaning |
|---|---|---|
| `radio_host` | `""` | Radio IP/hostname. Empty = UDP autodiscovery. |
| `radio_serial` | `""` | Selects a specific discovered radio (case-insensitive); empty = first found. With `radio_host` set, used as the identity label — but never published to `/meta` (only the handshake `info` serial is). |
| `mqtt.broker` | `tcp://homeassistant.local:1883` | MQTT broker URL (deployed: `tcp://192.168.1.50:1883`). |
| `mqtt.client_id` | `flexbridge` | paho client id. If **empty**, derived `<site>-<station>-<slot>`. (See §9 — the shipped default makes the derivation dead in practice.) |
| `mqtt.user` / `mqtt.password` | `""` | Credentials; set only if non-empty user. |
| `mqtt.discovery_prefix` | `homeassistant` | HA discovery topic prefix (legacy embedded path only). |
| `mqtt.publish_ha_discovery` | `false` | Gates legacy embedded HA discovery; preferred path is external hadiscovery reading `/meta.expose`. |
| `mqtt.site` / `mqtt.station` / `mqtt.slot` | `""`/`""`/`radio` | Slot address `<site>/<station>/<slot>`; site+station mandatory (exit 2 if empty). Deployed: `muehle`/`hf`/`radio`. |
| `mqtt.location` | `""` | Physical location label (deployed: `bauwagen`); carried in `/meta` (`location`, `expose.device.area`). |
| `log.level` | `info` | `debug`/`info`/`warn`/`error`; flag `-log.level` overrides. |

Environment overrides (applied after the file; used for the systemd EnvironmentFile secret flow): `FLEXBRIDGE_MQTT_BROKER`, `FLEXBRIDGE_MQTT_CLIENT_ID`, `FLEXBRIDGE_MQTT_USER`, `FLEXBRIDGE_MQTT_PASSWORD`, `FLEXBRIDGE_RADIO_HOST`, `FLEXBRIDGE_RADIO_SERIAL`, `FLEXBRIDGE_SITE`, `FLEXBRIDGE_STATION`, `FLEXBRIDGE_SLOT`. Non-empty values override.

Secrets convention: the MQTT password is **not** in the TOML. It lives in `/etc/flexbridge/flexbridge.env` (0600, owned by the service user), referenced by the unit's `EnvironmentFile=`, injected as `FLEXBRIDGE_MQTT_PASSWORD`. Never on the command line or in the unit file.

The README documents `radio_udp_port = 4991` and a `[rates]` section — **these do not exist in the code** (stale; the meter feature they configured is unwired — see §9).

---

## 7. Deployment

Target host: `shari`, a Raspberry Pi at `192.168.1.139` (SSH user `io`). `deploy.sh` (env-tunable: `SSH_HOST`, `SSH_USER`, `SERVICE_NAME`, `SERVICE_USER`, `INSTALL_DIR`, `BINARY`, and seed values `RADIO_HOST`, `RADIO_SERIAL`, `LOCATION`, `LOG_LEVEL`, `MQTT_BROKER`, `MQTT_SITE/STATION/SLOT/USER/PASSWORD`, `DISCOVERY_PREFIX`):

1. Builds for `linux/arm64` (`CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"`) to `dist/flexbridge-linux-arm64`.
2. Generates the seed config TOML and seed env file into `mktemp` files with `umask 077` (secrets never ride with the non-secret TOML).
3. Generates the systemd unit inline (below).
4. scp's binary, unit, and seeds to `/tmp` on the target.
5. Remotely: creates a dedicated system user `flexbridge` (`useradd --system --no-create-home --shell /usr/sbin/nologin`) if absent; `install -d /etc/flexbridge` (0755, service-user owned); **seed-once**: config and env files installed 0600 service-user owned only if they don't already exist — later deploys never touch them (the device owns its settings; change by editing on the device or deleting and redeploying); stops the service, moves the binary into place (0755), installs the unit, `daemon-reload`, `enable`, `restart`; prints status.
6. Removes the transferred seed temp files.

Systemd unit (exact):

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

The unit has no `TimeoutStopSec` override (systemd default 90 s) and no `KillSignal` overrides. Dependencies: outbound TCP only (radio 4992 + MQTT 1883), outbound/local UDP (discovery broadcast); fully unprivileged; no serial, no disk writes (the `StateDirectory` is reserved but unused).

Build/run locally: `make build` / `make run` (runs against `config.example.toml`) / `make test`.

---

## 8. Invariants & safety rules (must hold in any re-implementation)

1. **Never publish `/meta` (or any identity) before a completed handshake**, and never with a placeholder serial. `/meta` is written solely from the radio's `info` reply. Violating this clobbers the retained birth certificate with bogus identity on every retry while the radio is off (this happened live).
2. **Two-layer liveness**: `/status` = MQTT/bridge LWT only; `/state.device_online` = radio TCP link. `/state.device_online` must flip true on handshake and false (with a forced republish of a zeroed snapshot) on disconnect. Consumers AND both — the contract must keep both layers present.
3. **`freq_hz` is an integer in Hz**, never kHz/MHz, always a field of the snapshot (0 = unknown).
4. **Band and frequency can never disagree**: `/state.band` is always derived from `/state.freq_hz` — even after a `set_band` command (which uses native band-stacking; the radio picks the frequency). Band is never accepted from the radio or stored as an independent setpoint.
5. **Canonical vocabulary only**: modes normalized (`cw`/`usb`/`lsb`/`am`/`fm`/`data`), raw firmware mode strings never published (unknown → field omitted). Band labels per the canonical table; `gen` for HF general coverage; `unknown` otherwise.
6. **`/state` is a single complete retained JSON document**, republished only on change; never per-field topics, never partial updates.
7. **`/cmd` is not retained** — one-shot intents; a stale command must not re-fire on bridge/broker restart. The argument rides under the `value` key.
8. **No command acknowledgment is published.** Results are observed on `/state` (fire-and-observe). Consumers must not expect a cmd-reply topic.
9. **Read-only toward the radio except** `set_band` (→ `display pan s …`), DVK play/stop, and `set_mic_profile` (→ `profile mic load`). No set_freq/set_mode/set_power exists — this is a deliberate safety boundary: nothing on the bus can put the radio on an arbitrary frequency directly.
10. **Deterministic active-slice selection**: lowest-index TX slice, else lowest-index active slice. Randomized selection between equal candidates is a correctness bug (live-observed state flip-flop).
11. **Slice/pan removal must delete tracked state** (else a phantom TX/active slice freezes published freq and jumps the state between phantom and real).
12. **Blocking work never runs in the MQTT message callback** (payload copied; dispatch via a bounded queue; drop-not-block on saturation).
13. **Mic-profile name validation before wire-send** (no quotes/backslash/control chars; ≤64 chars) — the name is embedded in a double-quoted protocol string; this is an injection guard.
14. **Incremental slice frames merge onto previous state**; a single malformed field (esp. frequency) must not drop the frame's other fields.
15. **Frequency conversion uses round, not truncate** (`math.Round(mhz*1e6)`); truncation produces wrong 1-Hz-low values for ~1.2 % of 10-Hz-step frequencies.
16. **`/status` clean-shutdown caveat**: clean stop leaves retained `online` on the broker; nothing may rely on `/status` alone for "radio reachable".

---

## 9. Known defects & fragilities

1. **README is substantially stale.** It describes the long-gone per-field topic scheme (`flexbridge/<serial>/state/...`, meter topics, `[rates]` config, `radio_udp_port`, per-slice entities, "publishes frequency in MHz"). The code publishes the station-model slot topics with a single JSON snapshot and no meter topics at all. CLAUDE.md's architecture section also still mentions UDP meter streaming and a "one-shot meter list" reply handler — neither is wired: `main.go` has no UDP listener, the handshake sends no `client udpport` and no `meter list`, and `Bridge` has no meter handler. `internal/flexradio/meters.go` + `vita49.go` + the meter-list parser are retained, tested, but **dead code** in the current wiring.
2. **Client-id derivation is dead in practice.** `main.go` derives `<site>-<station>-<slot>` only when `mqtt.client_id` is empty, but the built-in default config pre-fills `"flexbridge"`, so the deployed client id is `flexbridge` (the deploy.sh comment claims the derivation is the default). A duplicate-connection diagnosis therefore sees `flexbridge`, not the slot address.
3. **Clean shutdown leaves retained `online`** on `/status` (paho clean DISCONNECT does not fire the will). The schema doc's claim "bridge publishes [offline] on clean shutdown" is not implemented. Mitigated only by the `device_online` layer; still a trap for new consumers.
4. **`set_mic_profile` active-name tracking is best-effort and unconfirmed** — the bridge optimistically sets `/state.mic_profile` right after sending the load command, assuming success. A profile switched directly in the SmartSDR GUI is never reflected (the radio reports no active mic profile). A failed load (e.g. race with a radio-side change) leaves a wrong name published until the next list snapshot drops it.
5. **No radio-side keepalive**: TCP connection death that doesn't produce a read error (e.g. silently black-holed path without traffic) is not detected; there is no `info` ping or read-timeout watchdog in the run loop. Reconnect waits for the OS to notice.
6. **`/cmd` worker saturation drops commands silently** (bounded queue of 32; drop, no log of the drop). One burst of >32 commands before the worker drains loses intents with no trace.
7. **`dvk_play` is not gated on voice mode** (advisory debug log only; the radio refuses). A consumer can trigger a TX-keying action in CW/data expecting an effect.
8. **Band-change requires a panadapter to be open in SmartSDR**; with none tracked the command is a warn-log no-op. The operator must open one via the radio GUI first — a manual precondition invisible on the bus.
9. **`profile mic save` is obsolete on SmartSDR v4+** (the radio returns a malformed reply); profile creation uses a file-transfer mechanism out of scope. Re-implementors must not add a save action against the v4 wire protocol.
10. **Legacy embedded HA discovery vs hadiscovery produce different HA node ids** (`flexradio-<serial>` vs `muehle-hf-radio`); switching paths leaves orphaned HA entities unless old discovery topics are cleared.
11. **Band-edge hysteresis only guards `gen`** exits (2 kHz). A frequency that jumps into another allocation's gap (candidate `unknown`) switches immediately — fine in practice, but the asymmetry is deliberate, not accidental.
12. **The `pan` field name on slice status** (`pan=<handle>`) is described in-code as best-effort/confirmed-live-for-their-radio; if a firmware variant omits it, `set_band` silently falls back to the lowest tracked pan handle, which may target the wrong panadapter on multi-pan setups.
13. **Sequence numbers are ignored on replies**: `sendAwaitReply` matches the first `R1|` line regardless of the echoed sequence. Safe only because the handshake serializes one command at a time; any concurrency in a reimplementation must not reuse this matcher.
14. **`tuning` is an OR of two independent sources** (ATU `status=tuning` and radio `tuning=1`) into one published bool, with the two handlers each writing the same field — a radio `tuning=0` frame immediately after an ATU `tuning` frame can clear the flag while the ATU is still tuning (narrow race; never observed live, but the semantics are last-writer-wins per source, not a true OR of live source states).

---

## 10. Re-implementation notes

**Must be preserved verbatim (behavior contract):**
- All MQTT topic strings (`<site>/<station>/<slot>/{meta,state,status,cmd}`), retention/QoS policy (meta/state/status retained QoS 1; cmd not retained), the exact JSON field names and units of `/state` and `/meta` (including omitempty semantics), and the plain-string `online`/`offline` `/status`.
- The full `/cmd` action grammar including validation rules, the `value`-key convention, the no-ack fire-and-observe discipline, and the serial single-worker dispatch with drop-not-block on saturation.
- The SmartSDR wire surface: port 4992, `C1|` command prefix, handshake command order (`version`, the five `sub` commands, `info`, then fire-and-forget `sub dvk all` and `profile mic info`), the four command strings (`dvk playback_start id=N`, `dvk playback_stop id=N`, `display pan s <handle> band=<n>`, `profile mic load "<name>"`), the two-word "display pan" status quirk (gate on Topic `display` then arg `pan`), the caret-delimited mic-list parsing (spaces in names, trailing caret), incremental slice-frame merging, and the removal encodings (`removed` arg / `in_use=0` / `removed=1`).
- All timing constants: reconnect backoff 2 s ×1.5 → max 60 s; MQTT retry interval 5 s; handshake I/O deadlines 5 s; discovery timeouts 3 s (host-configured path) / 5 s (autodiscovery); band-transition hold 750 ms; band-edge hysteresis 2000 Hz; MQTT disconnect quiesce 500 ms; `/cmd` queue capacity 32.
- Band table, band-number mapping, mode-normalization table, `gen`/`unknown` fallbacks, deterministic lowest-index slice selection, per-slice hysteresis state, device_online lifecycle (true on handshake, false + forced zeroed-snapshot publish on disconnect), and the never-publish-placeholder-identity rule.
- Static `/meta` capabilities and the expose surface (fields + 13 actions) exactly as given in §3.

**Free to change (implementation detail):**
- Go, paho, goroutines, the `Commander` interface injection pattern, `sync.RWMutex` state guarding, TOML as config format (any equivalent config + env override mechanism is fine as long as secrets stay out of the process command line), slog text logging.
- The internal frame struct / parser layout, `sendAwaitReply` mechanics (a reimplementation may track real sequence numbers — in fact it should, given defect 13).
- The dead meter/VITA-49 code (meters.go, vita49.go) — unless meter telemetry is a PRD requirement, in which case treat the README's meter table as the aspirational spec, not current behavior.
- The legacy embedded HA discovery (default-off); the PRD can decide whether a new implementation needs it at all, given the external `hadiscovery` consumer renders from `/meta.expose`.
- The systemd unit generator/deploy script mechanics (any deployment achieving equivalent sandboxing, seed-once config, and secret isolation is acceptable).