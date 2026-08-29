# pelcobridge2 — UHF rotator console (Pelco pan/tilt head, interactive TUI + rotctld)

This document specifies the station's UHF antenna rotator component: an interactive,
terminal-based operator console that drives a PTS-303Z/3050DZ pan/tilt head (a
motorized mount that steers an antenna in azimuth, the horizontal compass angle, and
elevation, the vertical angle) over an RS-485 serial line using the Pelco-D and
Pelco-P protocols, and simultaneously exposes the head to satellite-tracking and
rotator-control clients through a rotctld-compatible TCP server. Its defining trait —
and the reason it deviates from every other station component's daemon deployment —
is its safety model: the head may only be moved remotely after a human physically at
the keyboard has manually *armed* it; arming is never possible over any network
interface and never survives a restart. A re-implementation must reproduce the wire
protocol, the serialized engine, the exact rotctld text responses, the MQTT presence,
and above all the arming rules, byte for byte where marked normative.

Terminology is defined at first use; §1 collects the recurring vocabulary.

---

## 1. Glossary

- **Amateur radio**: the licensed hobby of two-way radio communication. The station
  this PRD reconstructs ("Mühle", bus prefix `muehle`) is such a station.
- **UHF**: ultra-high frequency, roughly 300 MHz–3 GHz; the station's UHF antenna is
  mounted on a movable pan/tilt head.
- **Rotator**: a motorized apparatus that points an antenna. A *pan/tilt head*
  rotates in azimuth (pan) and elevation (tilt).
- **Azimuth (az)**: horizontal pointing angle. This system distinguishes **true**
  azimuth (compass-true angle) from **physical** azimuth (the head's own raw
  readback); their difference is the *arm offset* (§8).
- **Elevation (el / tilt)**: vertical pointing angle; 0° = horizon, 90° = zenith for
  this head.
- **PTS-303Z/3050DZ**: the physical Pelco-family pan/tilt head being driven.
- **Pelco-D / Pelco-P**: two byte-level serial protocols originally defined for
  CCTV pan/tilt/zoom cameras. D uses 7-byte frames with an additive checksum; P
  uses 8-byte frames with an XOR checksum (§3).
- **RS-485**: a multi-drop serial electrical bus standard; here a USB–RS-485
  adapter connects the host computer to the head.
- **8N1**: serial parameters — 8 data bits, no parity bit, 1 stop bit.
- **rotctld**: the TCP server of *Hamlib*, an open-source radio/rotator control
  library. It accepts one-line text commands from clients. Clients connect with
  *Hamlib model 901* (`rotctl -m 901 -r host:port`), which designates "speak the
  rotctld text protocol over TCP".
- **gpredict**: a satellite-tracking program that drives rotators through rotctld;
  it is the primary intended remote client.
- **RPRT n**: rotctld's reply convention — `RPRT 0` means success; negative numbers
  are Hamlib error codes (`-9` = rejected/command not allowed, `-11` = no usable
  position readback, `-1` = rejected/invalid parameter, `-4` = unknown command,
  `-6` = operation timed out).
- **TUI**: terminal user interface — a full-screen interactive terminal application.
- **Jog**: continuous motion in one direction at a set speed, started by one
  command and stopped by another (contrast with an absolute *set* to a position).
- **Set ladder**: this component's verified absolute-position sequence: send set →
  quiet window → one readback query → tolerance check → retry or converge (§7).
- **MQTT**: a lightweight publish/subscribe message protocol; the station's central
  message bus. A **slot** is one component's topic namespace
  (`<site>/<station>/<slot>`); this component owns slot `muehle/uhf/rotator`.
- **Retained message**: an MQTT message the broker stores and delivers to any
  future subscriber immediately on subscription.
- **LWT** (Last Will and Testament): a message the broker publishes on a client's
  behalf if that client disconnects uncleanly; here the string `offline` on the
  slot's `/status` topic.
- **Arming**: this component's manual safety gate — a keyboard-only act in the TUI
  that must happen before remote (rotctld) clients may move the head (§8).
- **E-stop**: emergency stop — a stop-all-movement frame that works from any
  source, in any state.
- **Seed-once**: the station's deployment convention — the installer writes the
  configuration file only if it is absent and never overwrites it; the operator
  owns it afterwards.
- **shari**: the Raspberry Pi (192.168.1.139) that hosts the station's background
  services. This component deliberately does *not* run there (§13).
- **shack-pc**: the Windows PC in the radio shack where this interactive component
  runs.
- **Broker**: the MQTT server, at `192.168.1.50:1883` in the deployed system (see
  02-interface-spec.md for the station-wide picture and the pending broker
  migration decision).

---

## 2. Role and shape of the component

pelcobridge2 is a **single-process interactive console application**, not a daemon.
It:

1. Drives the PTS-303Z/3050DZ pan/tilt head over RS-485 (Pelco-D or Pelco-P).
2. Presents the operator a full-screen TUI for manual motion, position queries,
   arming, and a live log of raw wire traffic ("wire log").
3. Serves the same head to Hamlib clients (`rotctl`, `gpredict`) as a
   rotctld-compatible TCP server — **but only once the operator has armed it from
   the TUI** (§8).
4. Publishes the station-standard four-plane MQTT presence (see
   02-interface-spec.md §3) on slot `muehle/uhf/rotator`, accepting exactly one
   remote command: **stop**.

It is the `muehle/uhf/rotator` slot. Unlike every other station bridge, it runs
interactively on the shack PC rather than as a service, because its primary
interface is a human at a keyboard and its core safety rule is that a human must
arm it locally.

**Requirements (normative):**

- REQ-ROLE-1: The component SHALL be a single interactive process started
  manually by an operator; it SHALL NOT install any auto-start mechanism, service
  definition, or watchdog.
- REQ-ROLE-2: The component SHALL provide three command entry points — TUI
  keyboard, rotctld TCP server, MQTT `/cmd` — all funneling into one serialized
  engine (§6).
- REQ-ROLE-3: A companion mock-head binary SHALL be provided that simulates the
  head over a TCP socket (or pseudo-terminal) reproducing the hardware quirks of
  §3.4, so the component can be exercised without hardware.

Reference-implementation note: in the reference codebase the component is written
in Go (TUI via the Bubble Tea library, serial via go.bug.st/serial, MQTT via paho);
the mock head is `pelcobridge2-mock` simulating over TCP. None of these choices are
requirements — only the behaviors in this document are.

---

## 3. Wire protocol — the pan/tilt head

### 3.1 Transport

- Physical link: RS-485 via a USB-serial adapter. Serial parameters SHALL be 8N1;
  baud is configurable with **default 2400** (the head family is documented
  1200–9600; the bench link runs 2400).
- The configured port may be a real serial device (`COM3`, `/dev/ttyUSB0`,
  `/dev/serial/by-id/...`) or the string `tcp:host:port`, which connects to the
  mock head over TCP (this also enables bench testing on Windows, which has no
  pseudo-terminal facility).
- **Opening the link at startup is fatal on failure**: the process prints the
  error, holds the console open with a "press Enter to exit…" prompt (so a
  double-clicked Windows executable's error is readable), and exits. It SHALL NOT
  present a UI over a dead link.
- Every write SHALL be a single complete frame and atomic from the engine's
  perspective; a short write (fewer bytes than the frame) SHALL be treated as an
  error (`short write: N of M bytes`).
- Only one frame is on the wire at a time; after every transmit the engine holds
  the line quiet for the frame gap (§6.2) before allowing the next transmit.

### 3.2 Pelco-D framing (default transmit envelope)

A 7-byte frame: `FF addr cmd1 cmd2 d1 d2 sum`, where:

- `FF` (hex) is the fixed start byte;
- `addr` is the head's DIP-switch address (**default 1**, configurable);
- `cmd1` is always `0x00` for every function this head documents — the action
  lives in `cmd2`;
- `d1`, `d2` are data bytes (speed byte, or halves of a 16-bit position word);
- `sum` = `(addr + cmd1 + cmd2 + d1 + d2) & 0xFF` — an 8-bit additive checksum.

### 3.3 Pelco-P framing (optional transmit envelope)

Configured by a "use Pelco-P for transmit" boolean (default false). An 8-byte
frame: `A0 addr cmd1 cmd2 d1 d2 AF xor` — the same logical fields, with start byte
`A0`, end byte `AF`, and checksum `xor` = the **XOR of the seven bytes preceding
it** (the start byte through the end byte inclusive, indices 0–6 of the frame).
The same address byte is used in both protocols: this head matches one DIP code
regardless of protocol (strict Pelco-P gear is zero-indexed; this unit is not —
this equivalence is a bench assumption, see §16).

**Reception is always protocol-adaptive.** The head answers in whichever envelope a
frame arrived in, and the component's receive assembler SHALL accept both D and P
frames at all times, regardless of the configured transmit envelope. The engine
records the envelope of the last received frame and reports it as `protocol` =
`"D"` or `"P"`.

### 3.4 Opcodes (cmd2 values) — normative table

| cmd2 | Name | Meaning | Data bytes |
|---|---|---|---|
| `0x00` | stop | stop all movement | 0, 0 |
| `0x02` | jog right | pan jog | speed byte in `d1` |
| `0x04` | jog left | pan jog | speed byte in `d1` |
| `0x08` | jog up | tilt jog | speed byte in `d2` |
| `0x10` | jog down | tilt jog | speed byte in `d2` |
| `0x4B` | set pan | absolute pan set — **lands only on a quiet line** | target word |
| `0x4D` | set tilt | absolute tilt set — same quiet-line rule | target word |
| `0x51` | query pan | pan angle request | — |
| `0x53` | query tilt | tilt angle request | — |
| `0x59` | pan response | pan position reply | position word |
| `0x5B` | tilt response | tilt position reply | position word |
| `0x03` | preset set | "set preset N" / extended selector | N in `d2` |
| `0x07` | preset call | "call preset N" / extended selector | N in `d2` |

Two preset selectors are load-bearing:

- **Preset set 105 (`0x69`)** = *disable the head's periodic self-check*. The
  head otherwise periodically re-homes itself (drives to its reference position)
  unprompted. This frame SHALL be sent once at process start, before anything
  else uses the line (§6.3). Re-enabling would be preset call 105 — the component
  never sends it.
- **Preset call 125 (`0x7D`)** = factory self-test: restores defaults and re-homes
  the head (physically swings it). DANGEROUS — can rip cables if the head is
  pointed such that the travel pulls them. Reachable only from the TUI, only
  while disarmed, behind a two-stage confirmation (§9.2).

**Speed byte**: jog speed lives in `d1` for pan-axis operations and `d2` for
tilt-axis operations; documented range `0x00`–`0x3F` (hex, i.e. 0–63); default jog
speed **`0x12`** (decimal 18).

**Position encoding**: `d1` is the high byte and `d2` the low byte of a big-endian
16-bit word; **degrees = word / 100** (hundredths of a degree, "degrees×100"),
word range 0–655.35. This is used for both absolute-set targets and readback
replies. Per the 2026-08-28 bench session it applies to both the pan reply
(`0x59`) and the tilt reply (`0x5B`) — superseding the earlier ptest-era finding
that the tilt word's meaning was unknown (§16, open decision). Target shaping: a
pan set wraps its argument into [0, 360); a tilt set **clamps** its argument to
[0, 90] (overshooting tilt travel is the dangerous direction). NaN and infinity
targets SHALL be rejected before encoding in every entry path (§9.4).

### 3.5 Measured head behavior (bench facts — normative input to design)

These facts were established by bench measurement against the actual head and are
normative; a re-implementation must not assume the vendor manual's idealized
behavior:

1. **Absolute sets (`0x4B`/`0x4D`) work only on a quiet line.** The head ignores an
   absolute set if any other frame is transmitted too close to it. The engine's
   settle window and transmit gate (§6.2, §7) exist to guarantee that quiet line.
   (An earlier bench tool observed these opcodes as simply "ignored" — see §16 for
   the contradiction record; the later pelcobridge2 bench session, confirmed by the
   working set ladder, attributes the earlier observation to the missing
   quiet-line discipline.)
2. **Readback is garbage while a motor runs.** Mid-motion the head returns
   checksum-valid, position-unrelated words. No checksum test can filter them.
   Consequences: user position queries are refused while moving; the set ladder
   only verifies after the settle window; the physical position reported in state
   is deliberately stale during a jog.
3. **Reception is protocol-adaptive per frame** (§3.3).
4. **The head is silent on undecodable or wrong-address frames** — there is no
   error reply; timeouts are the only failure signal.
5. **There is no connection handshake.** The link is a raw serial line; the
   engine's "connect" ritual is a single disable-self-check frame (preset set 105)
   sent at engine start.
6. **At most one outstanding query.** The engine tracks the expected response
   opcode (`0x59` or `0x5B`) for the single in-flight query and matches incoming
   readbacks against it. A query with no usable answer within the **reply wait
   (default 400 ms)** fails with "no valid readback", after which any stalled
   partial bytes are flushed from the assembler and the engine resumes.

### 3.6 RX assembler (byte stream → frames)

The receive path SHALL be a byte-level resynchronizer with two load-bearing rules:

1. **Sync on the start byte**: `0xFF` → expect a 7-byte D frame; `0xA0` → expect
   an 8-byte P frame; any other byte is collected into a noise run. The matching
   checksum is validated; on failure the leading byte is dropped and scanning
   resumes one byte later. Noise runs are emitted to the wire log **before** the
   frame that follows them, in wire order, and are capped at 256 bytes per run
   within a single feed (a wrong-baud flood stays readable).
2. **Incomplete frames are bounded by a receive gap.** A partial frame is held
   only until the next receive gap; when the reply wait expires the engine flushes
   it as a "partial" noise event. Without this bound, a truncated reply merges
   with the next reply and can produce a checksum-valid frame carrying a
   **fabricated position word** — an observed bench failure, and the reason this
   rule exists.

Wire-log rendering per receive event: a raw hex line, a decoded-fields line
(`raw: addr=.. cmd1=.. cmd2=.. d1=.. d2=..`), a checksum line
(`word=XXXX (n)  chk=XX ok|BAD (want XX)`), and for position frames a
`pan %.2f°` / `tilt %.2f°` line; each line truncated at 50 columns (the log pane
does not wrap, so long lines lose their tail silently — an accepted cosmetic
behavior).

---

## 4. Serial transport failure handling (self-heal)

- **Read side**: a dedicated blocking reader owns the port. ANY read error (a
  pulled or re-enumerated USB adapter, a dropped TCP peer) marks the head offline
  (`device_online=false` in state, an error string recorded), triggers an automatic
  reopen of the port **throttled to at most one attempt per 2 s**, and starts a
  fresh reader generation. Late errors from a superseded reader generation are
  ignored by generation tag. A TUI key (`ctrl+r`) performs the same reopen
  manually and always works.
- **Write side**: a failed write MUST unwind rather than wedge: the in-flight
  query is failed ("no valid readback"), an active set ladder is killed with
  status `"failed"`, jog state is cleared, the transmit gate is re-opened (no
  frame went out, so no timer was armed), and the error is recorded. Without this
  rule, state stored before a failed transmit would wait forever on a timer that
  was never set.
- The TCP (mock) transport bounds every write with a 3 s deadline — a wedged peer
  must not wedge the engine, because the e-stop rides on writes — and reconnects
  with a 3 s dial timeout.

Known deviation to preserve as documented behavior: the automatic reopen gives up
permanently if the reopen attempt fails (no reader is then running, so no further
read errors arrive to retry on); recovery requires the manual `ctrl+r`, which
always works. A re-implementation MAY improve this (continuous throttled retry)
but MUST keep the manual reopen working from the TUI. See §16.

---

## 5. MQTT presence — slot `muehle/uhf/rotator`

MQTT is **optional** (default disabled) and **never fatal**: broker unavailability
or authentication failure only degrades an indicator in the TUI header; the TUI
and the rotctld server keep working. Defaults: broker `tcp://192.168.1.50:1883`,
user `hf`, password from environment variable `PELCOBRIDGE2_MQTT_PASSWORD`
(fallback: a config-file password key — see §16 for the documented-vs-README
disagreement), client ID `muehle-uhf-rotator`. Auto-reconnect SHALL be enabled
with a **5 s retry interval**. On every (re)connect the component SHALL: publish
birth (see `/status`), refresh the retained `/meta`, re-subscribe `/cmd`, and
**republish the last retained `/state` payload** — a quiescent head emits no
snapshots, so without this republish, consumers after a broker restart would see
no rotator state at all.

Four planes, exact topic strings:

| Topic | Direction | Retained | QoS | Payload |
|---|---|---|---|---|
| `muehle/uhf/rotator/meta` | publish | yes | 1 | birth-certificate JSON (§5.2) |
| `muehle/uhf/rotator/state` | publish | yes | 1 | state snapshot JSON (§5.3) |
| `muehle/uhf/rotator/status` | publish (LWT) | yes | 1 | `"online"` / `"offline"` |
| `muehle/uhf/rotator/cmd` | subscribe | — | 1 | `{"action":"stop"}` only (§5.4) |

### 5.1 `/status`

Literal body `online` on connect; `offline` as the MQTT Last Will (published by
the broker if the process dies uncleanly). This reports the **component's**
availability and is deliberately distinct from `/state.device_online`, which
reports the **head's serial link**. Station-wide caveat (see 02-interface-spec.md):
on a clean shutdown no will fires, so a retained `online` can outlive a stopped
process; consumers must not trust `/status` alone.

### 5.2 `/meta` (retained birth certificate)

```json
{
  "schema": "1.0",
  "role": "rotator",
  "device": { "model": "PTS-303Z/3050DZ" },
  "link": "rs485",
  "host": "shack-pc",
  "capabilities": { "axes": ["az", "el"] },
  "expose": {
    "device": { "name": "UHF Rotator", "model": "PTS-303Z/3050DZ", "manufacturer": "Pelco" },
    "fields": [
      { "key": "az", "name": "Azimuth", "type": "number", "unit": "°", "class": "azimuth", "state_class": "measurement", "min": 0.0, "max": 360.0 },
      { "key": "el", "name": "Elevation", "type": "number", "unit": "°", "class": "elevation", "state_class": "measurement", "min": 0.0, "max": 90.0 },
      { "key": "target_az", "name": "Target Azimuth", "type": "number", "unit": "°", "class": "azimuth", "state_class": "measurement" },
      { "key": "target_el", "name": "Target Elevation", "type": "number", "unit": "°", "class": "elevation", "state_class": "measurement" },
      { "key": "moving", "name": "Moving", "type": "boolean" },
      { "key": "armed", "name": "Armed", "type": "boolean" },
      { "key": "device_online", "name": "Device Online", "type": "boolean" }
    ],
    "actions": [ { "key": "stop", "name": "Stop", "command": { "action": "stop" } } ]
  }
}
```

All identity strings (`device.model`, `expose.device.name`/`model`, `link`,
`host`) come from configuration. The field min/max values are **fixed 0–360 (az)
and 0–90 (el)** in the reference implementation and do not track the configured
rotctld limits — an inconsistency recorded in §16.

### 5.3 `/state` (retained, change-deduplicated snapshot)

One JSON document per engine state change. Exact field names, types, units:

| Field | Type | Semantics |
|---|---|---|
| `ts` | string | UTC timestamp, RFC 3339, stamped at publish time |
| `az`, `el` | number or `null` | **true** (offset-corrected, §8.3) azimuth/elevation in degrees, rounded to 0.01; `null` when no readback yet |
| `phys_az`, `phys_el` | number or `null` | raw head readback in degrees, rounded to 0.01; `null` when none |
| `readback_valid` | bool | both axes have a readback at all (not a freshness claim) |
| `readback_age_s` | number | age of the pan readback in seconds, rounded to 0.1 |
| `armed` | bool | arm state; false at every start |
| `az_offset_deg` | number or `null` | the arm offset (physical − true) in degrees, 0.01 rounding |
| `moving` | bool | a jog is commanded or a set ladder is active |
| `target_az`, `target_el` | number or absent | current ladder target in **true** degrees; **omitted entirely** when none |
| `set_status` | string | absent, `"setting"`, `"converged"`, or `"failed"` |
| `jog_speed` | int | current jog speed byte value (0–63) as an integer |
| `protocol` | string | envelope of the last received frame, `"D"` or `"P"`; omitted when none yet |
| `rotctld_clients` | int | number of currently connected rotctld TCP clients |
| `device_online` | bool | a checksum-valid frame has been received since the link was last known-dead |
| `link` | string | `"ok"` when `device_online`, else `"down"` |
| `error` | string | last link/engine error text; omitted when empty |

**Publish cadence and deduplication**: the engine emits a snapshot event on every
state change (readback arrival, arm/disarm, jog start/stop, ladder phase change,
link error). The publisher computes a dedup key from the payload with the
timestamp blanked and `readback_age_s` zeroed, and publishes only when a real
field changed — a stationary, parked head generates no bus traffic. The last
published payload is cached for republish on reconnect.

### 5.4 `/cmd` — stop only, normative safety surface

Subscribed at QoS 1. The handler SHALL accept **only** the JSON object
`{"action":"stop"}` (the station-wide `/cmd` convention, 02-interface-spec.md §4,
defines an optional `value` key — irrelevant for stop). Any other action, and any
unparseable payload, is logged as a warning and ignored. There SHALL be **no** MQTT
path to motion, arming, calibration, or self-test — this is a hard safety
requirement (§8.4, 06-safety.md).

Command handling SHALL be enqueued onto a bounded job queue (capacity 32) drained
by a single worker, never executed on the MQTT client's message-dispatch thread.
This is a library-independent constraint: in the reference MQTT library, handlers
run on the connection's dispatch thread and a handler that blocks or publishes
synchronously deadlocks the client — an incident that occurred live in another
component. See 03-components/common-runtime-library.md.

---

## 6. Architecture: the serialized engine and the no-polling rule

### 6.1 Single serializer

**One engine owns the serial wire and all mutable rotator state; no other actor
mutates state directly.** All other actors (TUI, rotctld server, MQTT handler)
submit typed intents over a channel (capacity 64; submit is non-blocking — a full
channel returns the error "engine busy") and receive results on a one-slot reply
channel with a caller timeout (**2 s convention**). Engine events (snapshots and
wire-log lines) fan out on a sink channel (capacity 512); a slow consumer's events
are dropped — the latest snapshot is what matters, and the wire log is not promised
to be complete.

### 6.2 No timer polling — one-shot gates only

The engine's only timers are re-armed as one-shot **gate releases** of exactly
three kinds. No timer ever transmits by itself; each expiry only releases the next
queued action:

1. **Frame gap** — after every transmit, the transmit gate closes for
   `IdleGap(baud) = 1.5 × (7 bytes × 10 bits) / baud`, minimum 20 ms
   (**≈43.75 ms at 2400 baud**). While the gate is closed, motion and query intents
   are queued (FIFO, capacity 16 — overflow returns "engine busy"); the Stop
   intent is the only thing that cuts through a closed gate.
2. **Reply wait** — armed when the frame gap releases with a query outstanding;
   **400 ms** default. On expiry the in-flight query fails with "no valid
   readback", stalled partial bytes are flushed, and a set ladder in its verify
   phase retries.
3. **Settle** — the quiet-line window around absolute sets, **2000 ms** default
   (§7).

The single recorded deviation is the TUI's hold-to-move stop tick (§9.3), which
lives outside the engine.

### 6.3 Startup sequence

1. Resolve configuration (§12 resolution order). An **explicitly named** missing
   config file is a fatal error; only the no-path case falls back to built-in
   defaults — a mistyped path must not quietly run on defaults.
2. Apply command-line flag overrides.
3. Open the transport; failure is fatal (exit).
4. Start the engine. It immediately: sends the **disable-self-check frame**
   (preset set 105) once, *before anything else uses the line*; and publishes one
   initial snapshot (so the TUI and MQTT have state even with no traffic — a
   quiescent head emits nothing).
5. Start the rotctld listener (unless disabled). A bind failure is logged into the
   TUI wire log and is **non-fatal**.
6. If MQTT is enabled: connect in the background (never fatal; 5 s retry),
   start the single jobs worker, subscribe `/cmd`.
7. Start the event pump (engine events → MQTT `/state` and TUI).
8. Run the TUI. When it exits, the engine sends one final all-stop frame on
   shutdown.

### 6.4 Intent vocabulary and source gates

All command surfaces reduce to engine intents, each attributable to its source
(`tui`, `rotctld`, `mqtt`, `engine`):

| Intent | Argument | Source gate | Behavior |
|---|---|---|---|
| `QueryPan` / `QueryTilt` | — | any source | one readback; refused while moving ("rotator is moving") or if another query is outstanding ("engine busy") |
| `Jog` | direction | TUI allowed **disarmed**; rotctld/MQTT require armed (MQTT never issues it) | start motion at jog speed; stopped by Stop or TUI hold expiry |
| `Stop` | — | any source, any state (armed/disarmed, mid-prompt, mid-ladder, gate open or closed) | immediate all-stop frame; kills the ladder; **cancels every queued, not-yet-executed motion request** with "request cancelled (all-stop)" |
| `SetPan` / `SetTilt` | true degrees | armed only; finite values only | starts the verification ladder (§7) |
| `GotoPhysZero` | — | TUI allowed disarmed; other sources armed | ladder to physical (0°, 0°); offset never applied |
| `Arm` | true azimuth | **TUI only, enforced twice** (§8.1) | computes the offset; arms |
| `Disarm` | — | TUI only | drops armed state |
| `SelfTest` | — | TUI only, **refused while armed** | sends preset call 125 (re-homes head) |
| `JogSpeed` | speed byte | no engine-level source check (TUI is the only caller in practice) | clamps to 0x00–0x3F |
| `Reopen` | — | any source (TUI `ctrl+r`) | reopen serial transport, restart reader |

Exact engine error strings (normative; callers and tests match on them):
`engine busy`, `request cancelled (all-stop)`, `rotator is not armed`,
`rotator is moving`, `no valid readback`, `readback too old to arm`,
`intent not allowed from this source`, `set did not converge`.

---

## 7. The set ladder — absolute positioning without polling

The head offers no reliable way to read position while moving, and absolute sets
are only accepted on a quiet line; the ladder reconciles both. Phases per step:
**1** = set transmit due after settle; **2** = verify transmit due after settle;
**3** = verify query in flight.

1. On an accepted set intent, the engine transmits the absolute-set frame
   immediately (the line is quiet by rule), closes the transmit gate, and arms the
   settle timer (**`settle_ms`, default 2000 ms**).
2. When settle elapses (head stopped, line quiet), the engine sends **one** verify
   query.
3. The reply is compared to the target within **`set_tolerance_deg` (default
   0.3°)** — pan compared with wraparound distance (the minimum of |Δ| and
   360−|Δ|), tilt absolutely.
4. Within tolerance → the step converges. A multi-step ladder (goto-physical-zero
   = pan 0 then tilt 0) starts the next step with a fresh settle window; when all
   steps converge the ladder ends with `set_status = "converged"` and queued sets
   may proceed.
5. Out of tolerance or no readback → retry: decrement `tries` (initial
   **`set_attempts` = 3**), wait out another settle window, re-send the set frame;
   when attempts are exhausted the ladder ends with `set_status = "failed"` and
   the queue drains anyway.

Cancellation semantics (normative): any stop, jog, or new set **cancels** an
active ladder — a human always wins over an in-flight verification. The all-stop
also cancels motion that is merely **queued** (behind a gate window or an
outstanding query) by replying to every pending request with
`request cancelled (all-stop)` — nothing may start moving after an all-stop frame.
While a query is outstanding or a ladder runs, queued set-type intents are held
back (FIFO head-of-line blocking) so a new set can neither clobber the in-flight
query reply nor drop the ladder's remaining steps.

Jog behavior: jog frames carry the speed byte (§3.4). Jogging clears any active
ladder. `moving` = jog active OR ladder active. Readback while moving is refused
(§3.5), so `phys_az` in the snapshot is stale during a jog — by design.

---

## 8. Arming — the safety model (normative)

### 8.1 What "manual, never remote" means

A human must be physically present at the console running the TUI on the shack PC,
look at the head, and press `A`. No rotctld command, no MQTT message, no
configuration key, and no command-line flag can arm the head. This SHALL be
enforced twice, structurally:

- REQ-ARM-1: The Arm intent SHALL have **no construction path outside the TUI
  code** — no other component, server, or handler can even build the request.
- REQ-ARM-2: The engine SHALL independently reject an Arm (and SelfTest) request
  whose source is not the TUI, with `intent not allowed from this source`.
- REQ-ARM-3: Armed state SHALL NOT be persisted. **Every process start is
  disarmed.** A crash, reboot, or restart removes remote motion capability until a
  human re-arms.
- REQ-ARM-4: Disarm (TUI-only) SHALL re-lock remote motion immediately.

### 8.2 Arm flow

1. `A` opens a prompt: "enter the TRUE azimuth the head points at now", pre-filled
   with the last-confirmed value from the state file (§12.1). Pre-fill only — the
   operator must confirm or correct it every run; it is never auto-loaded into the
   engine.
2. The entered value must be a finite number in **0..360**. NaN and infinity SHALL
   be explicitly rejected (the reference platform's float parser accepts them, and
   they would arm a NaN offset).
3. A fresh pan readback is fetched first; the engine refuses to arm without one.
4. The engine refuses if the head is moving (`rotator is moving`) or if the pan
   readback is older than **`arm_max_readback_age_s` (default 10 s)**
   (`readback too old to arm`).
5. On success: `offset = physical_azimuth − entered_true_azimuth`; the head is
   ARMED. The entered value is saved to the state file as the next pre-fill.

### 8.3 Coordinate frames

- All user-facing pan degrees are **true**: readback replies (rotctld `get_pos`,
  TUI queries) apply `true = Norm360(physical − offset)`; set arguments convert as
  `physical = Norm360(true + offset)`. Elevation has no offset. A read-then-set
  client therefore works in one consistent frame.
- The set ladder's *internal* verification queries keep raw physical degrees (its
  targets are physical).
- Goto-physical-zero bypasses the offset in both directions.
- An un-armed head therefore reports physical position (offset is 0).

### 8.4 What arming gates — and what it does not

- **Gated**: the rotctld *motion* path only. `set_pos` and any rotctld-sourced
  goto-zero answer `RPRT -9` while disarmed.
- **Not gated**: TUI manual motion (jog, goto-0 — deliberately usable disarmed,
  because that is how the head is positioned before arming); position queries from
  any source; and the all-stop from any source.
- **MQTT can never move the head, armed or not**: `/cmd` parses exactly
  `{"action":"stop"}`; anything else is dropped (§5.4). No motion, arm, or
  calibration intent exists on the MQTT path under any circumstances.

### 8.5 Self-test

The factory self-test (preset call 125) re-homes the head and can rip cables. It
SHALL be reachable only from the TUI, only while disarmed, behind a two-stage
confirmation (press `y`, then type the word `RIPCABLES`), and the engine SHALL
additionally refuse it while armed.

---

## 9. Command surfaces

### 9.1 rotctld TCP server (default `0.0.0.0:4533`, Hamlib model 901)

Any number of concurrent clients; line-oriented text protocol; responses pinned
byte-exactly against Hamlib's parser (`rotctl_parse.c` / `netrotctl.c`) by tests.
The re-implementation SHALL reproduce these responses exactly:

| Input | Reply | Behavior |
|---|---|---|
| `p` or `get_pos` | two lines `"%.2f\n%.2f\n"` (azimuth then elevation), **no RPRT line on success** | queries pan then tilt; errors → `RPRT -11` (no usable readback; moving counts, since readback is garbage mid-motion) |
| `P` or `set_pos` with az el args | `RPRT 0` | parses two floats (decimal comma accepted: `,`→`.`); NaN/infinity and parse failures → `RPRT -1` (rejected up-front — they would otherwise park the head at 0°); disarmed → `RPRT -9`; engine timeout → `RPRT -6`; any other refusal (busy, cancelled) → `RPRT -1`; missing args → `RPRT -1` |
| `S` or `stop` | `RPRT 0` (always) | all-stop, every state; cancels ladder and queued motion |
| `_` or `get_info` | info line | `"pelcobridge2 · <device name>"` (default `pelcobridge2 · UHF Rotator`) |
| `\dump_state` | multi-line block below | protocol v1 block; gpredict sends this at connection open |
| `q` or `Q` | nothing | close connection |
| `#comment`, empty line | nothing | ignored |
| `+` prefix on any get command | `RPRT 0` line prepended | extended-response mode |
| unknown command | `RPRT -4` | |

`set_pos` details: the pan intent is submitted first; if it fails, the tilt intent
is not submitted and the mapped error is returned. Both arguments are **true**
degrees (§8.3). `RPRT 0` is returned as soon as both intents are *accepted by the
engine* — the verification ladder then runs asynchronously. **Clients must be
documented this**: `RPRT 0` does not mean "positioned"; convergence and failure
are observable only via `/state.set_status` or the TUI. This is a shape
constraint of the rotctld protocol, not a defect.

`\dump_state` exact body (protocol v1; min/max from configured rotctld limits,
defaults 0/360/0/90; `rot_model` fixed at 901 = NET_ROTCTL):

```
1
rot_model=901
min_az=0.000000
max_az=360.000000
min_el=0.000000
max_el=90.000000
south_zero=0
rot_type=AzEl
done
```

Each command round-trip uses a **2 s caller timeout** against the engine. A
command-line flag (`-no-rotctld`) suppresses the listener even when configuration
enables it. A bind failure is logged in the TUI and is non-fatal.

### 9.2 TUI keyboard (the human surface)

Full-screen layout: header bar, 3-line position pane, wire-log viewport, prompt
line, status line, and a `?` help overlay.

| Key | Action |
|---|---|
| arrow keys / `hjkl` | hold-to-move jog at jog speed — **works disarmed** (this is how the head is positioned before arming) |
| `SPACE` / `ESC` | **global e-stop** — all-stop frame, cancels prompts, allowed from every state |
| `a` / `e` | query azimuth / elevation (one-shot; result in status line, `%.2f°`, true degrees) |
| `A` | arm flow (§8.2) |
| `0` | goto **physical** zero (both axes), offset never applied — works disarmed |
| `+` / `-` (also `=` / `_`) | jog speed ±1, clamped 0x00–0x3F |
| `s` | self-test — disarmed only; two-stage confirm (`y`, then type `RIPCABLES`) |
| `d` | disarm |
| `?` | toggle help overlay |
| Tab / Shift-Tab | scroll wire log half a page |
| `ctrl+r` | reopen serial port (manual USB-re-enumeration heal) |
| `ctrl+l` | clear wire log |
| `ctrl+c` / `ctrl+q` | quit — best-effort all-stop first |

Prompt discipline: an open prompt (arm entry, self-test confirm) **owns the
keyboard** — no motion key fires mid-prompt; only the e-stop (space/esc) and
ctrl chords cut through.

Header bar (exact format): `pelcobridge2 · <short port name> · <baud> 8N1 · addr
<n> · proto D|P · jog 0xNN · rotctl:<client count> · mqtt: on|off · head:
online|offline · DISARMED | ● ARMED`.

Position pane: line 1 = `TRUE AZ <deg> (<age>)   EL <deg> (<age>)   readback
ok|down`; line 2 = `PHYS AZ <deg>   PHYS EL <deg>   offset <±deg>`; line 3 =
`TARGET <az> / <el>   <idle|setting|converged|FAILED>   [· MOVING] [· <error>]`.
Angles render as `%7.2f`, `---.--` when no readback; ages as `%.1fs` or `no fix`.

Wire log: capped at 2000 lines (trimmed to the last 1000); the viewport stays
pinned to the tail only if the operator was already at the tail. Log lines can be
lost under load (§6.1) — the wire log is operator-facing, not machine-consumed.

The TUI e-stop is stronger than a fire-and-forget submit: it performs a
**blocking round-trip into the engine (2 s timeout)**, so a saturated intent
queue can never silently swallow the stop while the status line claims it was
sent. Jogs, by contrast, are non-blocking submits.

### 9.3 Hold-to-move mechanics (recorded deviation, open question)

Terminals have no key-release event. Every jog keypress increments a sequence
number and arms a one-shot **`jog_hold_ms` timer (default 250 ms)**; terminal
auto-repeat refreshes the timer with each repeated keypress; a tick firing with
the *current* sequence number (no fresh keypress since) sends exactly one stop.
If the terminal does not auto-repeat, motion stops after `jog_hold_ms`. **Windows
Terminal auto-repeat behavior is bench-unverified** — see §16; the designed
fallback (a toggle-jog: first press starts, second press stops) is NOT
implemented.

### 9.4 Input hardening

Every entry path SHALL reject NaN and infinity targets before they reach the wire
(rotctld parse, TUI arm prompt, engine finite checks) — they would otherwise park
the head at 0° or arm a NaN offset. The rotctld parser accepts a decimal comma
(`45,5` → 45.5) per Hamlib convention.

### 9.5 Command-line flags

`-config <path>`, `-port <name>` (overrides the configured serial port;
`tcp:host:port` selects the TCP transport), `-addr <n>`, `-baud <n>`,
`-list-ports` (enumerate serial ports with USB identity — vid:pid, product,
serial number — and exit), `-no-rotctld`, `-print-config` (print resolved
configuration and exit).

---

## 10. Error paths (normative summary)

| Trigger | Required effect |
|---|---|
| Serial read error | head marked offline, error recorded, auto-reopen after ≥2 s cooldown, fresh reader generation; if the reopen fails, healing stops until manual `ctrl+r` (§4, §16) |
| Serial write error (incl. short write) | in-flight query failed, ladder killed `"failed"`, jog cleared, transmit gate re-opened, error recorded — never wedges |
| Reply-wait expiry (400 ms) | query failed "no valid readback", partials flushed, ladder verify retried |
| Verify out of tolerance ×3 | ladder `"failed"`, queue drains |
| Stop while motion queued | all pending requests replied "cancelled"; nothing drains later |
| Intent queue > 16 pending | new requests refused "engine busy" |
| rotctld bind failure | logged in TUI, process continues |
| MQTT connect failure | logged, TUI header shows `mqtt: off`, everything else continues |
| Config file explicitly named but missing | fatal at startup |
| Serial port missing at startup | fatal at startup |

---

## 11. Safety invariants (must be preserved by any re-implementation)

1. Arming is a TUI-only, human act, twice enforced (no construction path outside
   the TUI AND an engine-side source check); never persisted; every start
   disarmed; no remote arming under any circumstances (§8.1).
2. MQTT can never move the head: `/cmd` accepts exactly `{"action":"stop"}` (§5.4).
3. The all-stop always works: any source, any state, and it also cancels *queued*
   motion so nothing starts moving after the stop frame (§6.4).
4. Self-test is disarmed-only, double-confirmed, and engine-refused while armed
   (§8.5).
5. At most one outstanding query; never query while moving (readback is garbage
   mid-motion) (§3.5).
6. Absolute sets get a quiet line: the settle window brackets every absolute-set
   transmission and re-send; no other traffic interleaves with an active ladder
   (§7).
7. A failed write unwinds rather than wedges; no state may wait on a timer that
   was never armed (§4).
8. The engine is a single serializer for the wire and all mutable state; the
   pending queue is capped (16) so a stuck line cannot grow it unboundedly (§6.1).
9. Coordinate-frame consistency: user-facing pan values are true degrees in both
   directions; goto-physical-zero bypasses the offset; arming requires a fresh
   (≤10 s) pan readback and a stationary head (§8).
10. NaN/infinity targets rejected before the wire in every entry path (§9.4).
11. The head's periodic self-check must be disabled once at start (preset set 105)
    before anything else uses the line (§6.3; per-reconnect improvement — §16).
12. A tilt set clamps to [0, 90]; a pan set wraps into [0, 360) (§3.4).
13. No timer polling: timers are one-shot gate releases only; a stationary head
    generates no bus traffic (state dedup excludes timestamp and readback age)
    (§6.2, §5.3).
14. Two-layer liveness is kept distinct: `/status` = component availability (LWT);
    `/state.device_online` = the head's serial link (§5.1, 02-interface-spec.md).
15. MQTT handlers never block: command handling is serialized on a single worker
    queue, never on the client's dispatch thread; the engine never blocks on a
    slow event consumer (§5.4, §6.1).

---

## 12. Configuration

TOML file, seed-once, stored with 0600 permissions; secrets never on the command
line. Path resolution order (highest first): `-config` flag >
`PELCOBRIDGE2_CONFIG` environment variable > `config.toml` in the executable's
directory (Windows double-click friendly) > `./config.toml` > built-in defaults.
An explicitly named missing file is an error, never a silent fallback.

| Key | Default | Meaning |
|---|---|---|
| `[serial] port` | `""` (must be set or `-port` given; fatal otherwise) | `COM3`, `/dev/ttyUSB0`, `/dev/serial/by-id/...`, or `tcp:host:port` |
| `[serial] baud` | `2400` | 8N1 always |
| `[serial] addr` | `1` | head's Pelco DIP address |
| `[serial] pelco_p` | `false` | transmit envelope; reception is always adaptive |
| `[rotctld] enabled` | `true` | serve rotctld |
| `[rotctld] bind` | `"0.0.0.0"` | `127.0.0.1` keeps it local |
| `[rotctld] port` | `4533` | TCP listen port |
| `[control] jog_speed` | `18` (0x12) | jog speed byte 0x00–0x3F; values outside the range are replaced by the default (0x12), not clamped |
| `[control] settle_ms` | `2000` | quiet-line window around absolute sets; negative → default |
| `[control] set_attempts` | `3` | ladder re-sends; `< 1` clamped to 1 |
| `[control] set_tolerance_deg` | `0.3` | verify tolerance in degrees; 0 → default |
| `[control] arm_max_readback_age_s` | `10` | pan readback freshness required to arm; negative → default |
| `[control] jog_hold_ms` | `250` | TUI hold-to-move stop timer |
| `[control] min_az / max_az / min_el / max_el` | `0 / 360 / 0 / 90` | advertised rotctld limits (`\dump_state`); **not enforced** on set arguments — targets are wrapped/clamped at the frame layer instead (§3.4, §16) |
| `[mqtt] enabled` | `false` | MQTT off by default |
| `[mqtt] broker` | `"tcp://192.168.1.50:1883"` | broker URL |
| `[mqtt] client_id` | `""` → `muehle-uhf-rotator` | |
| `[mqtt] user` | `"hf"` | |
| `[mqtt] password` | `""` | fallback only, for GUI-launched processes with no environment; the environment variable always wins |
| `[mqtt] site / station / slot` | `muehle / uhf / rotator` | topic namespace |
| `[mqtt] device_model` | `"PTS-303Z/3050DZ"` | /meta device model |
| `[mqtt] device_name` | `"UHF Rotator"` | /meta expose name and rotctld `get_info` |
| `[mqtt] device_link` | `"rs485"` | /meta link |
| `[mqtt] host` | `"shack-pc"` | /meta compute host |
| `[log] file` | `""` | optional log file; empty disables |

Secrets: the MQTT password is supplied via environment variable
`PELCOBRIDGE2_MQTT_PASSWORD` (preferred) or, as a documented accommodation for a
double-clicked executable with no environment, the 0600 config file — never a
command-line flag. (The project README claims the config file is never used; the
code does use it as fallback. A re-implementation must pick one contract — see
§16.)

### 12.1 State file (`state.toml`)

Stored next to the config file (or next to the executable on a flagless first
run), 0600. Holds exactly one key: `last_offset_deg` — **which despite its name
stores the last *entered true azimuth*** (the value the operator typed at the arm
prompt), used only to pre-fill the next arm prompt. Nothing else is persisted;
armed state never survives a restart. A re-implementation is free to fix the
misleading key name, but the stored *meaning* (entered true azimuth, not the
computed offset) is the contract.

---

## 13. Deployment — recorded deviation from station convention

- **Target host: shack-pc**, a Windows machine at `192.168.1.197` (SSH user
  `iotte`), destination `C:/Users/iotte/pelcobridge2/`. This is a deliberate,
  recorded deviation from the station's service-on-shari convention
  (05-deployment-ops.md): the component is an interactive TUI, so there is **no
  service, no auto-start, no service-manager unit**. A human starts it by hand
  whenever the UHF rotator is to be used, and arming is a further manual step
  inside it. This deployment shape is inseparable from the safety model (§8) — a
  human must be at the keyboard to arm.
- The deploy script cross-compiles for Windows, copies the binary, and seeds
  `config.toml` **once** (only if the remote file does not exist) from a local
  seed template derived from the example config; the operator must edit
  `[serial] port` and re-run. Host/user/destination overridable via environment
  variables `PELCO2_HOST` / `PELCO2_USER` / `PELCO2_DEST`. It prints the manual
  start line and an arming reminder.
- Build targets: Windows, Linux, macOS (both the main and mock binaries). Runtime
  dependencies on the target: a USB–RS-485 adapter at the configured port;
  network reachability to the MQTT broker (optional) and rotctld clients.
- The fatal-error path holds the console open with "press Enter to exit…" so a
  double-clicked executable's error is readable.
- The deploy had **not yet been executed to the shack PC** as of 2026-08-29; the
  component was built and green in CI/bench tests only (see the status note in
  §16).

Reference-implementation note: the deploy script cross-compiles with
`GOOS=windows`, `CGO_ENABLED=0`, `-trimpath -ldflags "-s -w"` and copies via
`scp`; the service-manager convention would be systemd, which does not apply here.

---

## 14. Reference-implementation notes (non-normative)

- The reference implementation is Go: a Bubble Tea TUI, a goroutine-per-connection
  rotctld server, a channel-and-intent engine, paho MQTT. The mock head
  (`pelcobridge2-mock`) simulates the head's quirks (silence-required sets,
  garbage readback while moving, D/P-adaptive answers) over a pseudo-terminal or
  TCP, toggleable per quirk.
- Engine tests require the settle window scaled above the simulated travel time —
  the first verify readback is garbage while the (simulated) head is still moving.
- The MQTT state dedup is implemented by marshaling the payload with the
  timestamp blanked and comparing JSON; any change-detection with the same effect
  is acceptable.
- The component evolved from a one-time bench tool (`pelcotest`/`ptest`) used to
  reverse-engineer the head's serial behavior; the knowledge that tool produced is
  folded into §3 and is contract for this bridge. The tool itself does not need
  reconstruction.

---

## 15. Cross-references

- Station bus planes, `/cmd` JSON convention, two-layer liveness:
  02-interface-spec.md.
- Station-wide safety philosophy and the operator-presence principle:
  06-safety.md.
- MQTT connect/job-queue library constraint (handler never blocks):
  03-components/common-runtime-library.md.
- Deployment conventions this component deliberately deviates from:
  05-deployment-ops.md.

---

## 16. Open decisions & unresolved facts

1. **Bench facts needing hardware re-confirmation — earlier tool vs later bench
   contradiction.** The earlier bench tool (`ptest`, findings dated up to
   2026-08-27) measured: absolute-set opcodes `0x4B`/`0x4D` *ignored* by the head;
   tilt reply (`0x5B`) word *meaning unknown* (readings far outside the 0–90°
   travel; a "raw encoder count" hypothesis was fitted and then contradicted). The
   later pelcobridge2 bench session (2026-08-28) established: absolute sets *do*
   land when the line is quiet (the set ladder verifies to 0.3° against
   readback), and both pan and tilt replies are plain degrees×100. The working
   hypothesis is that the earlier "ignored" observation was an artifact of the
   missing quiet-line discipline. Evidence for each variant is recorded in the
   two research specs (`_research/pelcobridge2.md` §2, `_research/aux-projects.md`
   §3.2). **A re-implementation must re-confirm on hardware before trusting tilt
   readback as an elevation readout**; if the earlier finding holds for tilt, the
   ladder's tilt verification and the `el` field semantics need redesign.
2. **Bench-tuned control defaults need hardware re-tuning.** `settle_ms` 2000,
   `set_tolerance_deg` 0.3, reply wait 400 ms, and `jog_hold_ms` 250 were tuned
   against the *simulated* head; interactive bench smoke and control-default
   tuning on the real head were still pending as of 2026-08-29, as was the first
   deploy to the shack PC. Treat all four as defaults pending hardware
   validation.
3. **Windows Terminal auto-repeat for hold-to-move is unverified.** If arrow-key
   holds do not auto-repeat there, each jog stops after `jog_hold_ms` (250 ms).
   The designed fallback — a toggle-jog (press to start, press again to stop) — is
   NOT implemented. Decision required after bench check on the shack PC.
4. **Self-check disable cadence.** Preset set 105 (disable periodic self-check)
   is sent once per *process start*, not per *link reconnect*. If the head itself
   power-cycles (or is power-cycled) during a run, its self-check is re-enabled
   and the head can re-home itself unprompted; the engine never re-sends the
   frame after a serial reopen. A re-implementation SHOULD send it after every
   successful (re)open — an improvement over current behavior — but this needs
   confirmation against the head's firmware (whether the disable is volatile
   across head power loss, and whether re-sending is idempotent).
5. **Auto-heal gives up permanently on a failed reopen.** On a read error the
   reader dies; if the throttled reopen then fails, no reader is running, so no
   further read errors arrive and the automatic retry stops entirely; recovery
   requires manual `ctrl+r` (which always works). Preserved as documented
   behavior; a continuous throttled retry would be an improvement, not a
   regression.
6. **MQTT password source: README vs code.** The README says the password never
   comes from the config file; the code falls back to the `[mqtt] password` key
   when the environment variable is empty (a documented accommodation for a
   double-clicked Windows executable with no environment). A re-implementation
   must pick one contract and document it.
7. **`state.toml` key name lies.** `last_offset_deg` stores the last *entered
   true azimuth*, not the computed offset (§12.1). Behavior is correct; the name
   invites a wrong re-implementation. Rename in a re-implementation, keep the
   meaning.
8. **Advertised limits are inconsistent and unenforced.** The `/meta` expose
   min/max are fixed 0–360 / 0–90 and do not track `[control]
   min_az/max_az/min_el/max_el`, which feed only rotctld `\dump_state`; and
   neither set is enforced on `set_pos` arguments (a `set_pos 400 95` wraps pan to
   40° and clamps tilt to 90° at the frame layer instead of being range-rejected).
   A re-implementation should either enforce the configured limits consistently on
   both surfaces or document the wrap/clamp behavior to clients.
9. **Pelco-P addressing is an unverified assumption.** Strict Pelco-P gear is
   zero-indexed (address byte n = unit n+1); this head is assumed to use the same
   one-indexed address in both protocols. If wrong, every P-mode frame is
   addressed to a different unit and silently ignored — which would masquerade as
   "does not answer Pelco-P". Unresolved; harmless while the default transmit
   envelope is D.
10. **`JogSpeed` has no engine-level source gate** (unlike Arm/Disarm/SelfTest).
    Harmless today — only the TUI constructs it — but the "twice enforced"
    discipline is not uniform; a re-implementation may want the gate for
    symmetry.
11. **A misbehaving rotctld client can starve the queue.** The rotctld server
    accepts unlimited concurrent clients, all funneling into the one 16-slot
    engine queue; a flood from one client can get everyone else "engine busy"
    (the e-stop still cuts through). Rotctld per-client fairness/throttling is an
    open design decision.
12. **`-port sim` does not exist.** A package doc in the reference code mentions
    an in-process simulator mode (`-port sim`); the shipped binary supports only
    real serial ports and `tcp:`. Documentation drift, not behavior.
13. **Broker topology.** The default broker URL is `tcp://192.168.1.50:1883`
    (current production). A planned migration of station components to a broker
    on shari (192.168.1.139) exists on an unmerged branch and is not deployed;
    this component's broker URL is configuration and follows whatever the station
    decides (see 02-interface-spec.md).
14. **UHF-side capture gap.** The station's MQTT traffic recorder defaults to the
    HF subtree only, so this slot's bus traffic is not recorded by default
    (`_research/aux-projects.md` §1.2) — an operational note for debugging, not a
    component decision.