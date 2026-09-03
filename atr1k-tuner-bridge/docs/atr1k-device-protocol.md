# Salvage: atr1k-tuner-bridge.md
> Extracted from PRD/03-components/atr1k-tuner-bridge.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [protocol] ATR-1000 binary WebSocket frame format and decode rules
The ATR-1000 (BTR-1000 / N7DDC device family) is a Wi-Fi-enabled automatic
antenna tuner. It runs a binary WebSocket server on TCP port 60001 and
continuously pushes meter and relay telemetry frames to any connected client.
The repo holds no vendor documentation for this protocol — the original team
reverse-engineered it by observing the device. This frame format is the
authoritative description.

Transport: the bridge connects **outbound** to the tuner over a plain (no TLS)
WebSocket at `ws://<tuner-host>:60001` (default `ws://192.168.1.20:60001`). The
tuner is the server; the bridge always dials out. There is no handshake beyond
the HTTP WebSocket upgrade: no authentication, no hello message. Messages are
WebSocket **binary** messages; each message is exactly one frame.

Frame format (every frame, inbound and outbound):

```
byte 0: 0xFF   flag — every frame starts with 0xFF
byte 1: cmd    command/type byte
byte 2: len    payload length in bytes
byte 3..      payload — multi-byte integers inside the payload are
              little-endian unsigned 16-bit (uint16 LE)
```

Command bytes:

| cmd | Name | Direction | Wire frame | Meaning |
|-----|------|-----------|------------|---------|
| 1 | Sync | bridge → tuner | `FF 01 00` (3 bytes) | request a full state snapshot from the tuner. |
| 2 | Meter | tuner → bridge | `FF 02 len` + payload | telemetry: SWR + forward power. The frame must be ≥ 8 bytes in total to decode. |
| 3 | TuneStatus | bridge → tuner | `FF 03 01 00` (bypass) / `FF 03 01 01` (in line) | put the tuner in line or bypass. |
| 4 | TuneMode | bridge → tuner | `FF 04 01 01` (mem) / `FF 04 01 02` (full) | start a tune cycle. |
| 5 | Relay | tuner → bridge | `FF 05 len` + payload | L/C relay telemetry. The frame must be ≥ 10 bytes in total to decode. |

**Meter frame (cmd 2) decoding.** Offsets are indices in the whole frame (byte 0
is the 0xFF flag). The decoder ignores bytes 2–3.

- `swr_raw = uint16LE(frame[4:6])`.
  - If `swr_raw >= 100`, the published SWR = `swr_raw / 100`. Example: raw 217
    gives SWR 2.17.
  - If `swr_raw < 100`, the published SWR = `swr_raw` **unchanged** (NOT
    divided). In practice SWR ≥ 1.0, so raw ≥ 100 always. This quirk is a known
    defect — a re-implementation must decide the rule explicitly. The
    historical rule is "divide by 100 only when raw ≥ 100".
- `fwd = uint16LE(frame[6:8])` — forward power in whole watts, published
  unchanged.

**Relay frame (cmd 5) decoding.** The decoder ignores bytes 2–5.

- `l_uh = uint16LE(frame[6:8]) / 100` — inductance in µH with two implied
  decimals (raw 1234 → 12.34 µH).
- `c_pf = uint16LE(frame[8:10])` — capacitance in pF, published unchanged
  (raw 56 → 56).

**Inbound frame handling.**

- The bridge silently discards inbound frames with any other cmd byte (including
  a tuner echo of Sync, TuneStatus, or TuneMode) — no log, no state change.
- The bridge silently discards malformed frames (total message shorter than 3
  bytes, or byte 0 ≠ 0xFF).
- The outbound frames carry the length byte (byte 2). The reference
  implementation does **not validate it on receipt**: the decoder takes the
  payload as everything after byte 3 and relies only on the minimum-length
  checks (≥ 8 bytes for Meter, ≥ 10 bytes for Relay). A re-implementation must
  validate the length byte (improvement, not a violation).
- TuneMode wire values also include 0 = Reset and 3 = Fine; the bridge exposes
  only 1 (mem) and 2 (full) over MQTT.

**Sequence on connect.** Dial with a 10 s handshake timeout; on success send a
Sync frame (`FF 01 00`) that requests a full state snapshot (the tuner responds
with Meter and/or Relay frames; a Sync send failure is ignored, not fatal); then
enter the read loop — a blocking read of one binary message at a time.

**Device-link timing constants (normative defaults).**

| Constant | Value | Applies to |
|----------|-------|------------|
| WebSocket handshake timeout | **10 s** | dialing the tuner. |
| Write deadline | **5 s** | each tuner command write (writes serialized under a mutex). |
| Tune-settle timeout | **12 s** | Relay frame must arrive within this of a `tune`, or `fault: "tune timeout"` results. |
| Reconnect backoff | starts **2 s**, ×1.5 after each failed dial, **capped at 60 s** | tuner dial retries. |

## [defect] Pending tune is not stopped on tuner link loss — `device_online` flips back to true
PRD §10.13 / invariant 6: a tuner-link loss must stop a pending tune
(`settling=false`, timer stopped) — `settling=true` must never persist while the
tuner is unreachable.

Evidence (PRD): the device package holds the stop code — `setOnline(false, …)`
stops the 12 s timer and forces `settling=false` — but nothing calls it. The
link-lost path in the entry point calls `bridge.SetDeviceOnline(false, …)`,
which changes only `device_online` and `error`. As-built: a pending tune at link
loss leaves `settling=true` in the published snapshot. The still-armed timer
fires up to 12 s later, and its callback pushes the device's cached state, which
still says `device_online=true` with no `error`. `/state` then shows
`settling: false`, `fault: "tune timeout"`, and `device_online: true` — the
tuner looks reachable while it is down. The next dial failure re-publishes
`device_online: false`.

Code-check verdict: **still open.** `setOnline` is only ever called with
`online=true` (`internal/tuner/device.go:101`); the stop-tune branch inside
`setOnline` (`internal/tuner/device.go:248-250`) is dead code. On link loss
`wsLoop` (`cmd/atr1k-tuner-bridge/main.go:217`) calls
`b.SetDeviceOnline(false, …)` on the bridge-level state copy only; the device's
cached state keeps `DeviceOnline=true`, and a still-armed `onTuneTimeout` timer
pushes it.

## [defect] Tune-timer race: a stale timeout callback can fault a new tune cycle
PRD §10.9: the 12 s timer callback runs on its own thread and races a Relay
frame under the state mutex. The reference implementation handles the settling
check correctly, but the code merely stops the timer — it does not drain it. A
just-fired callback can set `fault: "tune timeout"` microseconds after a Relay
frame cleared settling in a new tune cycle. Never observed live.

Code-check verdict: **still open** — `Tune` re-arms with `timer.Stop()` plus a
new `time.AfterFunc` (`internal/tuner/device.go:154-157`); `time.Timer.Stop()`
does not drain an already-fired callback, so the window remains (the
`if d.state.Settling` guard in `onTuneTimeout` narrows but does not close it).

## [defect] Frame length byte unchecked on receive
PRD §10.4: the decoder takes the payload as everything after byte 3 and ignores
byte 2, relying only on the minimum-length checks (≥ 8 Meter, ≥ 10 Relay). A
frame with a bogus length decodes anyway. A re-implementation must validate the
length byte.

Code-check verdict: **still open** — `parseFrame` (`internal/tuner/protocol.go`)
returns `data[3:]` and never reads `data[2]`.

## [defect] Reconnect backoff never resets after a successful dial
PRD §10.5: the ×1.5 / 60 s-cap backoff grows monotonically for the process
lifetime and does not reset after a successful dial. A long-lived bridge that
has seen many tuner dropouts retries slowly (up to 60 s) even for the next
transient blip. Resetting the backoff on success (keeping start ≈ 2 s and cap ≈
60 s) is an improvement, not a violation.

Code-check verdict: **still open** — `wsLoop` (`cmd/atr1k-tuner-bridge/main.go:210-226`)
scales `backoff` after every failure and never resets it to 2 s on success.

## [defect] No `/state` republish after a broker flush (MQTT reconnect)
PRD §10.6: on MQTT reconnect only `/meta` and `/status` get re-published. If the
broker lost the retained `/state` (for example after a broker restart), the
bridge will not re-publish it until a dedup-defeating telemetry change happens.
A tuner at a rock-steady SWR can leave `/state` stale or absent indefinitely. A
re-implementation must republish `/state` on every MQTT reconnect.

Code-check verdict: **still open** — the reconnect callback
(`publishMetaOnReconnect`, `cmd/atr1k-tuner-bridge/main.go:192`) republishes
only `/meta`; no `/state` republish exists.

## [defect] No WebSocket keepalive / half-open link detection
PRD §10.8: the reference implementation has no ping/pong keepalive and no
idle-read timeout. The operating system detects a silently dead TCP link only
when the OS errors the socket or a write fails. A half-open tuner TCP link (for
example a Wi-Fi drop without a TCP reset) blocks the read loop indefinitely;
`device_online` stays true until then. Adding a keepalive or read-idle timeout
is an improvement to consider — any addition must not change the published
contract.

Code-check verdict: **still open** — the dialer sets only `HandshakeTimeout: 10s`;
there is no ping handler or read deadline anywhere in `internal/tuner/device.go`.

## [defect] `inline` is client-side belief only
PRD §10.3: the protocol has no inbound frame that confirms in-line/bypass (cmd 3
is outbound only, and the decoder ignores inbound cmds other than 2 and 5). If
the operator toggles the tuner's front panel, or the hardware interprets a
`tune` without going in line, `/state.inline` is wrong until the next
`set_inline`. The value also stays true across tuner reconnects, regardless of
what the tuner actually did while the link was down.

Code-check verdict: **still open** (structural) — `handleFrame` decodes only
cmds 2 and 5; `set_inline`/`tune` set `inline` optimistically
(`internal/tuner/device.go`, `SetInline`/`Tune`).

## [defect] SWR raw < 100 is published undivided
PRD §10.2: a raw value of 50 gives SWR 50 instead of 0.5. Benign in practice
(SWR ≥ 1.0 ⇒ raw ≥ 100 always), but the rule must be decided explicitly; what
raw 0 (idle / no RF) renders as is an open hardware question (see the decision
below). The historical rule is "divide by 100 only when raw ≥ 100".

Code-check verdict: **still as-built** — `meter()` (`internal/tuner/protocol.go:36-45`)
implements exactly the historical rule.

## [defect] `fault`/`error`: shipped MQTT API doc shows `""` while the code omits the keys
PRD §10.10: the code omits `fault` and `error` from the `/state` JSON when empty
(`omitempty`); consumers must treat absence as empty. The shipped
`docs/atr1k-tuner-bridge-mqtt-api.md` example still shows `"fault": ""` /
`"error": ""` — the doc lags the code.

Code-check verdict: **still open (doc lag only)** — `tuner.State` uses
`json:"fault,omitempty"` / `json:"error,omitempty"` (`internal/tuner/device.go`);
`docs/atr1k-tuner-bridge-mqtt-api.md` lines 139/141 still show the empty-string
form. Fix the doc.

## [decision] Tuner endpoint IP on the site LAN
The README says `ws://192.168.1.111:60001` in one place; the code,
`config.example.toml`, deploy script, and the README's own config table say
`ws://192.168.1.20:60001`. Code is truth (192.168.1.20); treat `.111` as a
README typo. Code-check verdict: the disagreement is **still present**
(`atr1k-tuner-bridge/README.md:37` vs `config.example.toml:21`). Even so, the
live device's actual IP must be confirmed on the site LAN on-site before any
redeploy that repoints it.

## [decision] Meter-frame push cadence and idle behavior
The code imposes no read timeout. No document gives the tuner's push rate, or
says whether Meter frames pause when idle / no RF. This decides whether the
change-dedup design alone is enough, or whether liveness needs an idle-read
timeout. Check against hardware.

## [decision] SWR raw 0 / raw < 100 rendering
Idle Meter frames can carry SWR raw 0 (which the historical rule publishes as
0). Undecided whether raw < 100 needs division, clamping, or treatment as "no
reading". The historical rule ("divide only when ≥ 100") is the as-built
behavior.

## [decision] Does the tuner ever echo cmd 3 (TuneStatus) inbound?
If it does, the echo can confirm in-line/bypass and fix the client-side-belief
defect. The as-built decoder deliberately discards all inbound cmds except 2 and
5. A hardware capture is needed to decide this.

## [decision] TuneMode 0 (Reset) and 3 (Fine) exist on the wire but are not exposed
The wire protocol defines TuneMode values 0 = Reset and 3 = Fine, but the
bridge does not expose them over MQTT and rejects `{"action":"tune","value":"fine"}`.
If the hardware supports Fine mode, the bridge cannot drive it. Exposing them is
an open design decision requiring a hardware check.

## [decision] `device_online` form: explicit-true vs omit-when-true
The deployed bridge publishes `device_online:true` explicitly (the struct field
has no `omitempty` — verified in `internal/tuner/device.go`). The station
integration-model document says the code can omit the field when true; consumers
must treat absence as true either way. The station-wide choice of one form must
stay consistent across all bridges — this is a station-level open decision, not
settled by this bridge.