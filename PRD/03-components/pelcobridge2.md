# pelcobridge2 — UHF rotator console (Pelco pan/tilt head, interactive TUI + rotctld)

This document describes the station's UHF antenna rotator component. The component is an
interactive, terminal-based operator console. It drives a PTS-303Z/3050DZ pan/tilt head, a
motorized mount that steers an antenna in azimuth and elevation. Azimuth is the horizontal
compass angle. Elevation is the vertical angle. The console talks to the head over an RS-485
serial line and uses the Pelco-D protocol. It also exposes the head to
satellite-tracking and rotator-control clients through a rotctld-compatible TCP server.

The safety model is its defining trait. This is the reason it deviates from the daemon
deployment of every other station component. The head can move remotely only after a human
physically at the keyboard arms it manually. Arming is never possible over any network
interface. Arming never survives a restart. A re-implementation must reproduce the wire
protocol, the serialized engine, the exact rotctld text responses, and the MQTT presence.
Above all, it must reproduce the arming rules byte for byte where the text marks them
normative.

The text defines each term at first use. §1 collects the recurring vocabulary.

---

## 1. Glossary

- **Amateur radio**: the licensed hobby of two-way radio communication. The station
  this PRD reconstructs ("Mühle", bus prefix `muehle`) is such a station.
- **UHF**: ultra-high frequency, roughly 300 MHz–3 GHz. A movable pan/tilt head carries
  the station's UHF antenna.
- **Rotator**: a motorized apparatus that points an antenna. A *pan/tilt head*
  rotates in azimuth (pan) and elevation (tilt).
- **Azimuth (az)**: horizontal pointing angle. This system distinguishes **true**
  azimuth (compass-true angle) from **physical** azimuth (the head's own raw
  readback). Their difference is the *arm offset* (§8).
- **Elevation (el / tilt)**: vertical pointing angle. 0° = horizon, 90° = zenith for
  this head.
- **PTS-303Z/3050DZ**: the physical Pelco-family pan/tilt head that the console drives.
- **Pelco-D / Pelco-P**: two byte-level serial protocols, originally defined for
  the closed-circuit television (CCTV) surveillance-camera industry to steer
  pan/tilt/zoom cameras. D uses 7-byte frames with an additive checksum. P uses 8-byte
  frames with an XOR checksum (§3).
- **RS-485**: a multi-drop serial electrical bus standard. Here a USB–RS-485
  adapter connects the host computer to the head.
- **Pseudo-terminal (pty)**: a simulated serial port that the operating system
  supplies. Programs use it in place of a real port for testing.
- **8N1**: serial parameters — 8 data bits, no parity bit, 1 stop bit.
- **rotctld**: the TCP server of *Hamlib*, an open-source radio/rotator control
  library. It accepts one-line text commands from clients. Clients connect with
  *Hamlib model 901* (`rotctl -m 901 -r host:port`), which designates "speak the
  rotctld text protocol over TCP".
- **gpredict**: a satellite-tracking program that drives rotators through rotctld.
  It is the primary intended remote client.
- **RPRT n**: rotctld's reply convention — `RPRT 0` means success. Negative numbers
  are Hamlib error codes (`-9` = rejected/command not allowed, `-11` = no usable
  position readback, `-1` = rejected/invalid parameter, `-4` = unknown command,
  `-6` = operation timed out).
- **TUI**: terminal user interface — a full-screen interactive terminal application.
- **Jog**: continuous motion in one direction at a set speed. One command starts the
  motion and another stops it (contrast with an absolute *set* to a position).
- **Set ladder**: this component's checked absolute-position sequence: send set →
  quiet window → one readback query → tolerance check → retry or converge (§7).
- **MQTT**: a lightweight publish/subscribe message protocol. It is the station's
  central message bus. A **slot** is one component's topic namespace
  (`<site>/<station>/<slot>`). This component owns slot `muehle/uhf/rotator`.
- **Retained message**: an MQTT message the broker stores. The broker delivers it to
  any future subscriber immediately on subscription.
- **QoS** (quality of service): the MQTT delivery-guarantee level. QoS 1 =
  at-least-once delivery. The receiver acknowledges, so duplicates are
  possible. This slot publishes and subscribes at QoS 1.
- **LWT** (Last Will and Testament): a message the broker publishes on a client's
  behalf if that client disconnects uncleanly. Here it is the string `offline` on the
  slot's `/status` topic.
- **Arming**: this component's manual safety gate — a keyboard-only act in the TUI
  that must happen before remote (rotctld) clients can move the head (§8).
- **E-stop**: emergency stop — a stop-all-movement frame that works from any
  source, in any state.
- **Seed-once**: the station's deployment convention. The installer writes the
  configuration file only if it is absent and never overwrites it. The operator
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

1. Drives the PTS-303Z/3050DZ pan/tilt head over RS-485 (Pelco-D).
2. Gives the operator a full-screen TUI for manual motion, position queries,
   arming, and a live log of raw wire traffic ("wire log").
3. Serves the same head to Hamlib clients (`rotctl`, `gpredict`) as a
   rotctld-compatible TCP server — **but only once the operator has armed it from
   the TUI** (§8).
4. Publishes the station-standard four-plane MQTT presence (see
   02-interface-spec.md §3) on slot `muehle/uhf/rotator`, and accepts exactly one
   remote command: **stop**.

It is the `muehle/uhf/rotator` slot. It runs interactively on the shack PC, unlike
every other station bridge, which runs as a service. The reasons: its primary
interface is a human at a keyboard, and its core safety rule needs that human to
arm it locally.

**Requirements (normative):**

- REQ-ROLE-1: The component must be a single interactive process that an operator
  starts manually. It must not install any auto-start mechanism, service
  definition, or watchdog.
- REQ-ROLE-2: The component must provide three command entry points — TUI
  keyboard, rotctld TCP server, MQTT `/cmd`. All three funnel into one serialized
  engine (§6).
- REQ-ROLE-3: The component must ship with a companion mock-head binary. The mock
  simulates the head over a TCP socket (or pseudo-terminal). It reproduces the
  hardware quirks of §3.4. Tests can then exercise the component without hardware.
  The mock must serve exactly one client connection at a time, like the real
  RS-485 line. A new connection can take over only after the previous one closes.
  A concurrent mock lets two clients interleave frames. That behavior breaks
  the bench-testing contract of this requirement.

Reference-implementation note: the reference implementation is in Go (Bubble Tea
TUI, go.bug.st/serial serial link, paho MQTT). The mock head is
`pelcobridge2-mock`. It simulates the head over TCP. None of these choices are
requirements — only the behaviors in this document are.

---

## 3. Wire protocol — the pan/tilt head

### 3.1 Transport

- Physical link: RS-485 through a USB-serial adapter. Serial parameters must be 8N1.
  The baud is configurable with **default 2400**. The vendor documents the head
  family at 1200–9600. The bench link runs 2400.
- The configured port can be a real serial device (`COM3`, `/dev/ttyUSB0`,
  `/dev/serial/by-id/...`) or the string `tcp:host:port`. The string form
  connects to the mock head over TCP. This also enables bench testing on Windows,
  which has no pseudo-terminal facility.
- **Opening the link at startup is fatal on failure.** The process prints
  the error. It holds the console open with a "press Enter to exit…"
  prompt, so the operator can read the error of a double-clicked Windows
  executable. Then it exits. It must not present a UI over a dead link.
- Every write must be a single complete frame and, from the engine's
  perspective, atomic. A short write (fewer bytes than the frame) is an error
  (`short write: N of M bytes`).
- Only one frame is on the wire at a time. After every transmit the engine holds
  the line quiet for the frame gap (§6.2). The gate opens for the next transmit
  only after that gap.

### 3.2 Pelco-D framing

A 7-byte frame: `FF addr cmd1 cmd2 d1 d2 sum`, where:

- `FF` (hex) is the fixed start byte.
- `addr` is the head's DIP-switch address (**default 1**, configurable). A DIP
  switch is a row of small on/off configuration switches on the device. The
  operator sets it by hand.
- `cmd1` is always `0x00` for every function this head documents — the action
  lives in `cmd2`.
- `d1`, `d2` are data bytes (speed byte, or halves of a 16-bit position word).
- `sum` = `(addr + cmd1 + cmd2 + d1 + d2) & 0xFF` — an 8-bit additive checksum.

### 3.3 Protocol envelope — Pelco-D only

The component speaks exactly one wire envelope: Pelco-D. It must not send
Pelco-P frames, and its receive assembler must not accept them (an 8-byte P
frame has no `FF` start byte, so it assembles as a noise run, §3.6).

The head itself is protocol-adaptive: measured on the bench, it answers in
whichever envelope a frame arrives in. The component never sends P, so this
device fact never shows on this link. An earlier revision of the component
sent P frames and accepted them on reception. The team removed that
capability on 2026-08-29 (§16.9).

### 3.4 Opcodes (cmd2 values) — normative table

| cmd2 | Name | Meaning | Data bytes |
|---|---|---|---|
| `0x00` | stop | Stop all movement. | 0, 0. |
| `0x02` | jog right | Pan jog. | Speed byte in `d1`. |
| `0x04` | jog left | Pan jog. | Speed byte in `d1`. |
| `0x08` | jog up | Tilt jog. | Speed byte in `d2`. |
| `0x10` | jog down | Tilt jog. | Speed byte in `d2`. |
| `0x4B` | set pan | Absolute pan set. It lands only on a quiet line. | Target word. |
| `0x4D` | set tilt | Absolute tilt set. Same quiet-line rule. | Target word. |
| `0x51` | query pan | Pan angle request. | None. |
| `0x53` | query tilt | Tilt angle request. | None. |
| `0x59` | pan response | Pan position reply. | Position word. |
| `0x5B` | tilt response | Tilt position reply. | Position word. |
| `0x03` | preset set | "Set preset N" / extended selector. | N in `d2`. |
| `0x07` | preset call | "Call preset N" / extended selector. | N in `d2`. |

Two preset selectors are load-bearing:

- **Preset set 105 (`0x69`)** = *disable the head's periodic self-check*. The
  head otherwise periodically re-homes itself (drives to its reference position)
  unprompted. The component must send this frame once at process start, before
  anything else uses the line (§6.3). The engine sends the frame again after
  every successful port reopen (§6.3), because the head can lose power while
  the link is down. Preset call 105 re-enables the
  self-check. Only the TUI can send that frame, as a manual maintenance
  toggle (§8.6). Key `c` disables the self-check. Key `C` enables it behind
  a confirmation, and the engine refuses an enable in the armed state.
- **Preset call 125 (`0x7D`)** = factory self-test. It restores defaults and
  re-homes the head (the head swings physically). DANGEROUS — the frame can rip
  cables. The risk exists when the travel pulls the cables. Only the TUI can
  reach it, only while disarmed, behind a `y/n` confirmation (§8.5).

**Speed byte**: jog speed lives in `d1` for pan-axis operations and in `d2` for
tilt-axis operations. The documented range is `0x00`–`0x3F` (hex, 0–63). The
default jog speed is **`0x12`** (decimal 18).

**Position encoding**: `d1` is the high byte and `d2` the low byte of a big-endian
16-bit word. **degrees = word / 100** (hundredths of a degree, "degrees×100").
Word range 0–655.35. This applies to absolute-set targets and to readback
replies. A bench session on 2026-08-28 confirmed it for the pan reply (`0x59`)
and for the tilt reply (`0x5B`). This supersedes the earlier ptest-era finding
that the tilt word's meaning was unknown (§16, open decision). Target shaping: a
pan set wraps its argument into [0, 360). A tilt set **clamps** its argument to
[0, 90]. Overshooting tilt travel is the dangerous direction. Every entry path
must reject NaN and infinity targets before encoding (§9.4).

### 3.5 Measured head behavior (bench facts — normative input to design)

A bench session measured these facts against the actual head. The facts are
normative. A re-implementation must not assume the vendor manual's idealized
behavior:

1. **Absolute sets (`0x4B`/`0x4D`) work only on a quiet line.** The head ignores
   an absolute set when the component sends another frame too close to it. The
   engine's settle window and transmit gate (§6.2, §7) exist to make that quiet
   line. (An earlier bench tool saw these opcodes as simply "ignored" — §16
   records the contradiction. The later pelcobridge2 bench session attributes
   the earlier observation to the missing quiet-line discipline. The working set
   ladder confirms this.)
2. **Readback is garbage while a motor runs.** Mid-motion the head returns
   checksum-valid words unrelated to position. No checksum test can filter them.
   Consequences: the engine refuses user position queries while the head moves.
   The set ladder checks only after the settle window. The physical position in
   state is deliberately stale during a jog.
3. **The head itself is protocol-adaptive on reception** (§3.3). It answers in
   whichever envelope a frame arrives in. The component speaks only Pelco-D, so
   every answer comes back in the Pelco-D envelope.
4. **The head is silent on undecodable or wrong-address frames.** It gives no
   error reply. Timeouts are the only failure signal.
5. **There is no connection handshake.** The link is a raw serial line. The
   engine's "connect" ritual is a single disable-self-check frame (preset set
   105) that the engine sends at start and after every successful reopen.
6. **At most one outstanding query.** The engine tracks the expected response
   opcode (`0x59` or `0x5B`) for the single in-flight query. It matches incoming
   readbacks against it. A query with no usable answer within the **reply wait
   (default 400 ms)** fails with "no valid readback". The assembler then flushes
   any stalled partial bytes and the engine resumes.

### 3.6 RX assembler (byte stream → frames)

The receive path must be a byte-level resynchronizer. It has two load-bearing
rules:

1. **Sync on the start byte**: `0xFF` → expect a 7-byte D frame. `0xA0` → expect
   an 8-byte P frame. The assembler collects any other byte into a noise run.
   The assembler validates the matching checksum. On failure it drops the
   leading byte and resumes scanning one byte later. The assembler emits noise
   runs to the wire log **before** the frame that follows them, in wire order. The
   assembler caps a run at 256 bytes per single feed (a wrong-baud flood stays
   readable).
2. **A time bound stops incomplete frames.** The reference has no independent
   receive-gap timer. The reply-wait expiry (§6.2, default 400 ms) flushes a
   held partial frame as a "partial" noise event. Without this bound, a truncated
   reply merges with the next reply. The merge can produce a checksum-valid
   frame with a **fabricated position word**. The bench observed this failure.
   It is the reason this rule exists.

   A standalone implementation that bounds partials itself must use the same
   formula as the transmit frame gap (§6.2). That gap is **1.5 frame times at
   the configured baud** (frame bits = 7 bytes × 10 bits), minimum 20 ms. The
   ptest research (`_research/aux-projects.md` §3.4) marks this gap bound as contract for
   any protocol decoder for this head.

Wire-log rendering per receive event: a raw hex line, a decoded-fields line
(`raw: addr=.. cmd1=.. cmd2=.. d1=.. d2=..`), a checksum line
(`word=XXXX (n)  chk=XX ok|BAD (want XX)`), and for position frames a
`pan %.2f°` / `tilt %.2f°` line. The component truncates each line at 50
columns (the log pane does not wrap. Long lines lose their tail silently — an
accepted cosmetic behavior).

---

## 4. Serial transport failure handling (self-heal)

- **Read side**: a dedicated blocking reader owns the port. Any read error (a
  pulled or re-enumerated USB adapter, a dropped TCP peer) marks the head
  offline. It sets `device_online=false` in state and records an error string.
  It drops the self-check model to `"unknown"` (§8.6), because a claim about
  the head cannot survive a dead link. It triggers an automatic port reopen,
  **throttled to at most one try per 2 s**, and starts a fresh reader
  generation. After every successful reopen, the engine sends the
  disable-self-check frame again (§6.3), because the head can lose power
  while the link is down. The engine ignores late errors from a
  superseded reader generation by generation tag. A TUI key (`ctrl+r`) does the
  same reopen manually and always works.
- **Write side**: a failed write must unwind rather than wedge. The engine fails
  the in-flight query ("no valid readback"). It kills an active set ladder with
  status `"failed"`. It clears jog state. It re-opens the transmit gate (no
  frame went out, so the engine armed no timer). It records the error. Without
  this rule, state that the engine stored before a failed transmit can wait
  forever on a timer that it never set.
- The TCP (mock) transport bounds every write with a 3 s deadline. A wedged peer
  must not wedge the engine, because the e-stop rides on writes. It reconnects
  with a 3 s dial timeout.

Preserve this known deviation as documented behavior: the automatic reopen gives
up permanently when a reopen try fails. (No reader then runs, so no further read
errors arrive to retry on.) Recovery then needs the manual `ctrl+r`, which
always works. A re-implementation can improve this (continuous throttled retry).
It must keep the manual reopen working from the TUI. See §16.

---

## 5. MQTT presence — slot `muehle/uhf/rotator`

MQTT is **not necessary** for operation (it is off by default) and **never
fatal**. Broker unavailability or authentication failure only degrades an
indicator in the TUI header. The TUI and the rotctld server keep working.
Defaults: broker `tcp://192.168.1.50:1883`, user `hf`, password from environment
variable `PELCOBRIDGE2_MQTT_PASSWORD` (fallback: a config-file password key —
see §16 for the documented-vs-README disagreement), client ID
`muehle-uhf-rotator`. Auto-reconnect must be on with a **5 s retry interval**.
On every (re)connect the component must publish birth (see `/status`), refresh
the retained `/meta`, and re-subscribe `/cmd`. It must also **republish the
last retained `/state` payload**. A quiescent head emits no snapshots. Without
this republish, consumers after a broker restart can see no rotator state at
all.

Four planes, exact topic strings:

| Topic | Direction | Retained | QoS | Payload |
|---|---|---|---|---|
| `muehle/uhf/rotator/meta` | publish. | yes. | 1 | Birth-certificate JSON (§5.2). |
| `muehle/uhf/rotator/state` | publish. | yes. | 1 | State snapshot JSON (§5.3). |
| `muehle/uhf/rotator/status` | publish (LWT). | yes. | 1 | `"online"` / `"offline"`. |
| `muehle/uhf/rotator/cmd` | subscribe. | — | 1 | `{"action":"stop"}` only (§5.4). |

### 5.1 `/status`

The body is literal `online` on connect. It is `offline` as the MQTT Last Will.
The broker publishes the will if the process dies uncleanly. This reports the
**component's** availability. It is deliberately distinct from
`/state.device_online`, which reports the **head's serial link**. Station-wide
caveat (see 02-interface-spec.md): on a clean shutdown no will fires. A
retained `online` can outlive a stopped process. Consumers must not trust
`/status` alone.

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
      { "key": "self_check", "name": "Self Check", "type": "string" },
      { "key": "device_online", "name": "Device Online", "type": "boolean" }
    ],
    "actions": [ { "key": "stop", "name": "Stop", "command": { "action": "stop" } } ]
  }
}
```

All identity strings (`device.model`, `expose.device.name`/`model`, `link`,
`host`) come from configuration. The reference serializes `host` as
*omitempty*: a run with an empty `[mqtt] host` omits the `host` field from
the `/meta` JSON entirely. The field min/max values are **fixed 0–360
(az) and 0–90 (el)** in the reference implementation. They do not track the
configured rotctld limits. §16 records this inconsistency.

### 5.3 `/state` (retained, change-deduplicated snapshot)

One JSON document per engine state change. Exact field names, types, units:

| Field | Type | Semantics |
|---|---|---|
| `ts` | string | UTC timestamp, RFC 3339. The publisher stamps it at publish time. |
| `az`, `el` | number or `null` | **True** (offset-corrected, §8.3) azimuth/elevation in degrees. Rounded to 0.01. `null` when no readback yet. |
| `phys_az`, `phys_el` | number or `null` | Raw head readback in degrees. Rounded to 0.01. `null` when none. |
| `readback_valid` | bool | Both axes have a readback at all. Not a freshness claim. |
| `readback_age_s` | number | Age of the pan readback in seconds. Rounded to 0.1. |
| `armed` | bool | Arm state. False at every start. |
| `az_offset_deg` | number or `null` | The arm offset (physical − true) in degrees. 0.01 rounding. |
| `moving` | bool | The operator commands a jog, or a set ladder is active. |
| `target_az`, `target_el` | number or absent | Current ladder target in **true** degrees. **Omitted entirely** when none. |
| `set_status` | string | Absent, `"setting"`, `"converged"`, or `"failed"`. |
| `self_check` | string | The engine model of the head's periodic self-check: `"on"`, `"off"`, or `"unknown"` — liveness-gated, verbatim (§8.6). |
| `jog_speed` | int | Current jog speed byte value (0–63), as an integer. |
| `rotctld_clients` | int | Number of now-connected rotctld TCP clients. |
| `device_online` | bool | The link received a checksum-valid frame since it last went dead. |
| `link` | string | `"ok"` when `device_online`, else `"down"`. |
| `error` | string | Last link/engine error text. Omitted when empty. |

**Publish cadence and deduplication**: the engine emits a snapshot event on
every state change (readback arrival, arm/disarm, jog start/stop, ladder phase
change, link error). The publisher computes a dedup key from the payload. It
blanks the timestamp and zeroes `readback_age_s` in that key. It publishes only
when a real field changed — a stationary, parked head generates no bus traffic.
The publisher caches the last published payload for republish on reconnect.

### 5.4 `/cmd` — stop only, normative safety surface

Subscribed at QoS 1. The handler must accept **only** the JSON object
`{"action":"stop"}`. The station-wide `/cmd` convention (02-interface-spec.md
§4) also defines a `value` key. That key is irrelevant for stop. The handler
logs any other action, and any unparseable payload, as a warning and ignores
them. The MQTT path must have **no** route to motion, arming, calibration, or
self-test — this is a hard safety requirement (§8.4, 06-safety.md).

The handler must enqueue command handling onto a bounded job queue (capacity
32). A single worker drains that queue. The MQTT client's message-dispatch
thread must never execute command handling. This is a library-independent
constraint. In the reference MQTT library, handlers run on the connection's
dispatch thread. A handler that blocks or publishes synchronously deadlocks
the client. An incident like this happened live in another component. See
03-components/common-runtime-library.md.

---

## 6. Architecture: the serialized engine and the no-polling rule

### 6.1 Single serializer

**One engine owns the serial wire and all mutable rotator state. No other
actor mutates state directly.** All other actors (TUI, rotctld server, MQTT
handler) submit typed intents over a channel (capacity 64). Submit is
non-blocking. A full channel returns the error "engine busy". Actors receive
results on a one-slot reply channel with a caller timeout (**2 s
convention**). Engine events (snapshots and wire-log lines) fan out on a sink
channel (capacity 512). The engine drops a slow consumer's events. The latest
snapshot is what matters. The wire log does not promise completeness.

### 6.2 No timer polling — one-shot gates only

The engine's only timers are re-armed as one-shot **gate releases** of exactly
three kinds. No timer ever transmits by itself. Each expiry only releases the
next queued action:

1. **Frame gap** — after every transmit, the transmit gate closes for
   `IdleGap(baud) = 1.5 × (7 bytes × 10 bits) / baud`, minimum 20 ms
   (**≈43.75 ms at 2400 baud**). While the gate stays closed, the engine
   queues motion and query intents (FIFO, capacity 16 — overflow returns
   "engine busy"). The Stop intent is the only thing that cuts through a
   closed gate.
2. **Reply wait** — the engine arms it when the frame gap releases with a
   query in flight. **400 ms** default. On expiry the in-flight query fails
   with "no valid readback". The assembler flushes stalled partial bytes. A
   set ladder in its check phase retries.
3. **Settle** — the quiet-line window around absolute sets, **2000 ms**
   default (§7).

The single recorded deviation is the TUI's hold-to-move stop tick (§9.3),
which lives outside the engine.

### 6.3 Startup sequence

1. Resolve configuration (§12 resolution order). An **explicitly named**
   missing config file is a fatal error. Only the no-path case falls back to
   built-in defaults — a mistyped path must not quietly run on defaults.
2. Apply command-line flag overrides.
3. Open the transport. A failure is fatal (exit).
4. Start the engine. It immediately sends the **disable-self-check frame**
   (preset set 105) once, before anything else uses the line. It sends the
   frame again after every successful port reopen (§4). It also
   publishes one first snapshot (so the TUI and MQTT have state even with no
   traffic — a quiescent head emits nothing).
5. Start the rotctld listener (unless disabled). The process logs a bind
   failure into the TUI wire log. The failure is **non-fatal**.
6. If MQTT is on: connect in the background (never fatal, 5 s retry). Start
   the single jobs worker. Subscribe `/cmd`.
7. Start the event pump (engine events → MQTT `/state` and TUI).
8. Run the TUI. When it exits, the engine sends one final all-stop frame on
   shutdown.

### 6.4 Intent vocabulary and source gates

All command surfaces map to engine intents. Each intent names its source
(`tui`, `rotctld`, `mqtt`, `engine`):

| Intent | Argument | Source gate | Behavior |
|---|---|---|---|
| `QueryPan` / `QueryTilt` | — | any source | One readback. The engine refuses it while the head moves (`rotator is moving`). Also refused if another query is in flight (`engine busy`). |
| `Jog` | direction | TUI can use it **disarmed**. Rotctld/MQTT need armed. MQTT never issues it. | Starts motion at jog speed. Stop or TUI hold expiry stops it. |
| `Stop` | — | any source, any state (armed/disarmed, mid-prompt, mid-ladder, gate open or closed) | Immediate all-stop frame. Kills the ladder. **Cancels every queued, not-yet-executed motion request** with "request cancelled (all-stop)". |
| `SetPan` / `SetTilt` | true degrees | Armed only. Finite values only. | Starts the check ladder (§7). |
| `GotoPhysZero` | — | TUI can use it disarmed. Other sources need armed. | Ladder to physical (0°, 0°). Offset never applies. |
| `Arm` | true azimuth | **TUI only, enforced twice** (§8.1) | Computes the offset. Arms the head. |
| `Disarm` | — | TUI only. | Drops armed state. |
| `SelfTest` | — | TUI only. Refused while armed. | Sends preset call 125 (re-homes head). Clears the readback state. Sets the self-check model to `"on"`. |
| `SelfCheck` | enable flag | TUI only. Refused in the armed state (enable only). Refused while the head moves. | Sends preset set 105 (disable) or preset call 105 (enable). Updates the self-check model per §8.6. |
| `JogSpeed` | speed byte | No engine-level source check (TUI is the only caller in practice). | Clamps to 0x00–0x3F. |
| `Reopen` | — | any source (TUI `ctrl+r`) | Reopens serial transport. Restarts reader. |

Exact engine error strings (normative — callers and tests match on them):
`engine busy`, `request cancelled (all-stop)`, `rotator is not armed`,
`rotator is moving`, `no valid readback`, `readback too old to arm`,
`intent not allowed from this source`, `set did not converge`,
`self-test is disarmed-only`,
`enabling the periodic self-check is disarmed-only`,
`frame never reached the wire`.

---

## 7. The set ladder — absolute positioning without polling

The head gives no reliable way to read position while it moves. It accepts
absolute sets only on a quiet line. The ladder reconciles both. Phases per
step: **1** = set transmit due after settle. **2** = check transmit due after
settle. **3** = check query in flight.

1. On an accepted set intent, the engine transmits the absolute-set frame
   immediately (the line is quiet by rule), closes the transmit gate, and arms
   the settle timer (**`settle_ms`, default 2000 ms**).
2. When settle elapses (head stopped, line quiet), the engine sends **one**
   check query.
3. The engine compares the reply to the target within **`set_tolerance_deg`
   (default 0.3°)**. Pan uses wraparound distance (the minimum of |Δ| and
   360−|Δ|). Tilt compares absolutely.
4. Within tolerance → the step converges. A multi-step ladder
   (goto-physical-zero = pan 0 then tilt 0) starts the next step with a fresh
   settle window. When all steps converge, the ladder ends with
   `set_status = "converged"` and queued sets can go on.
5. Out of tolerance or no readback → retry: decrement `tries` (first
   **`set_attempts` = 3**), wait out another settle window, re-send the set
   frame. When the tries run out, the ladder ends with
   `set_status = "failed"` and the queue drains anyway.

Cancellation semantics (normative): any stop, jog, or new set **cancels** an
active ladder. A human always wins over an in-flight check. The all-stop also
cancels motion that is merely **queued** (behind a gate window or an in-flight
query). It replies to every pending request with
`request cancelled (all-stop)`. Nothing can start moving after an all-stop
frame. While a query is in flight or a ladder runs, the engine holds back
queued set-type intents (FIFO head-of-line blocking). A new set can then
neither clobber the in-flight query reply nor drop the ladder's remaining
steps.

Jog behavior: jog frames carry the speed byte (§3.4). Jogging clears any
active ladder. `moving` = jog active OR ladder active. The engine refuses
readback while the head moves (§3.5). `phys_az` in the snapshot is therefore
stale during a jog — by design.

---

## 8. Arming — the safety model (normative)

### 8.1 What "manual, never remote" means

A human must be physically present at the console that runs the TUI on the
shack PC. The human must look at the head and press `A`. No rotctld command,
no MQTT message, no configuration key, and no command-line flag can arm the
head. The design must enforce this twice, structurally:

- REQ-ARM-1: The Arm intent must have **no construction path outside the TUI
  code**. No other component, server, or handler can even build the request.
- REQ-ARM-2: The engine must independently reject an Arm (and SelfTest)
  request whose source is not the TUI, with
  `intent not allowed from this source`.
- REQ-ARM-3: The engine must not persist armed state. **Every process start
  begins disarmed.** A crash, reboot, or restart removes remote motion
  capability until a human re-arms.
- REQ-ARM-4: Disarm (TUI-only) must re-lock remote motion immediately.

### 8.2 Arm flow

1. `A` opens a prompt: "enter the TRUE azimuth the head points at now". The
   prompt pre-fills with the last-confirmed value from the state file
   (§12.1). This is pre-fill only. The operator must confirm or correct it
   every run. The engine never auto-loads it.
2. The entered value must be a finite number in **0..360**. The engine must
   explicitly reject NaN and infinity (the reference platform's float parser
   accepts them, and they can arm a NaN offset).
3. The engine fetches a fresh pan readback first. It refuses to arm without
   one.
4. The engine refuses if the head moves (`rotator is moving`). It also
   refuses if the pan readback is older than
   **`arm_max_readback_age_s` (default 10 s)** (`readback too old to arm`).
5. On success: `offset = physical_azimuth − entered_true_azimuth`. This arms
   the head. The engine saves the entered value to the state file as the
   next pre-fill.

### 8.3 Coordinate frames

- All user-facing pan degrees are **true**. Readback replies (rotctld
  `get_pos`, TUI queries) apply `true = Norm360(physical − offset)`. Set
  arguments convert as `physical = Norm360(true + offset)`. Elevation has no
  offset. A read-then-set client therefore works in one consistent frame.
- The set ladder's *internal* check queries keep raw physical degrees (its
  targets are physical).
- Goto-physical-zero bypasses the offset in both directions.
- An un-armed head therefore reports physical position (offset is 0).

### 8.4 What arming gates — and what it does not

- **Gated**: the rotctld *motion* path only. `set_pos` and any
  rotctld-sourced goto-zero answer `RPRT -9` while disarmed.
- **Not gated**: TUI manual motion (jog, goto-0 — deliberately usable
  disarmed, because that is how the operator positions the head before
  arming). Also not gated: position queries from any source, and the
  all-stop from any source.
- **MQTT can never move the head, armed or not**: `/cmd` parses exactly
  `{"action":"stop"}`. The handler drops anything else (§5.4). No motion,
  arm, or calibration intent exists on the MQTT path under any
  circumstances.

### 8.5 Self-test

The factory self-test (preset call 125) re-homes the head and can rip
cables. Only the TUI can reach it, only while disarmed, behind a `y/n`
confirmation. The engine must additionally refuse it while armed.

The self-test restores the head's factory defaults. Two effects follow. The
engine clears its readback state, so the arm gate demands a new readback.
The head also enables its periodic self-check again, so the engine sets its
self-check model to `"on"`.

### 8.6 Periodic self-check toggle

While the periodic self-check is on, the head re-homes itself without a
command. The engine disables it at start and after every successful reopen
(§6.3). Only the TUI can enable it again. This is a maintenance-only function.

Key `C` opens a `y/n` confirmation. On `y`, the engine sends preset call 105.
The engine refuses this intent in the armed state and while the head moves.
Key `c` sends preset set 105 with no confirmation: this restores the station
default.

Both keys use a blocking round-trip into the engine (2 s timeout). The round
trip retries an `engine busy` answer, because busy means "retry", never a
refusal. The status line shows the engine verdict. A refused intent never
reads as sent. Neither key short-circuits on the pane's current value. The
pane shows a model claim, not proof, so a re-send is always in order.

**The model is liveness-gated.** RS-485 has no link-level confirmation, so a
preset frame that left the adapter proves nothing. The engine models the
self-check as exactly one of `"on"`, `"off"`, or `"unknown"`. A preset frame
that the engine sent parks its claim ("off" or "on") as pending. The claim
lands only when the head proves that it is alive: the link receives a
checksum-valid frame after the preset frame went out. Any link death drops
the model to `"unknown"` and clears the pending claim. Consumers must render
the value verbatim.

## 9. Command surfaces

### 9.1 rotctld TCP server (default `0.0.0.0:4533`, Hamlib model 901)

The server takes any number of concurrent clients. The protocol is one-line
text. Tests pin the responses byte-exactly against Hamlib's parser
(`rotctl_parse.c` / `netrotctl.c`). The re-implementation must reproduce these
responses exactly:

| Input | Reply | Behavior |
|---|---|---|
| `p` or `get_pos` | Two lines `"%.2f\n%.2f\n"` (azimuth then elevation). **No RPRT line on success.** | Queries pan then tilt. Errors → `RPRT -11` (no usable readback — moving counts, since readback is garbage mid-motion). |
| `P` or `set_pos` with az el args | `RPRT 0`. | Parses two floats (decimal comma accepted: `,`→`.`). NaN/infinity and parse failures → `RPRT -1` (rejected up-front — otherwise they park the head at 0°). Disarmed → `RPRT -9`. Engine timeout → `RPRT -6`. Any other refusal (busy, cancelled) → `RPRT -1`. Missing args → `RPRT -1`. |
| `S` or `stop` | `RPRT 0` (always). | All-stop, every state. Cancels the ladder and queued motion. |
| `_` or `get_info` | info line. | `"pelcobridge2 · <device name>"` (default `pelcobridge2 · UHF Rotator`). |
| `\dump_state` | multi-line block, below. | Protocol v1 block. Gpredict sends it at connection open. |
| `q` or `Q` | nothing. | Closes the connection. |
| `#comment`, empty line | nothing. | Ignored. |
| `+` prefix on any get command | `RPRT 0` line prepended. | Extended-response mode. |
| unknown command | `RPRT -4`. | None. |

`set_pos` details: the server submits the pan intent first. If it fails, the
server does not submit the tilt intent and returns the mapped error. Both
arguments are **true** degrees (§8.3). The server returns `RPRT 0` as soon as
the engine accepts both intents. The check ladder then runs asynchronously.
Documentation must tell clients this: `RPRT 0` does not mean "positioned".
Convergence and failure are observable only through `/state.set_status` or
the TUI. This is a shape constraint of the rotctld protocol, not a defect.

`\dump_state` exact body (protocol v1. Min/max from configured rotctld
limits, defaults 0/360/0/90. `rot_model` fixed at 901 = NET_ROTCTL):

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
command-line flag (`-no-rotctld`) suppresses the listener even when the
configuration enables it. The process logs a bind failure in the TUI. The
failure is non-fatal.

### 9.2 TUI keyboard (the human surface)

Full-screen layout: header bar, 3-line position pane, wire-log viewport,
prompt line, status line, and a `?` help overlay.

| Key | Action |
|---|---|
| arrow keys / `hjkl` | Hold-to-move jog at jog speed — **works disarmed** (this is how the operator positions the head before arming). |
| `SPACE` / `ESC` | **Global e-stop** — all-stop frame. Cancels prompts. Works from every state. |
| `a` / `e` | Query azimuth / elevation (one-shot). Result in status line, `%.2f°`, true degrees. |
| `A` | Arm flow (§8.2). |
| `0` | Goto **physical** zero (both axes). Offset never applies — works disarmed. |
| `+` / `-` (also `=` / `_`) | Jog speed ±1. Clamped to 0x00–0x3F. |
| `s` | Self-test — disarmed only. `y/n` confirm (§8.5). |
| `c` | Disable the periodic self-check (preset set 105). No confirm. Always sends — no short-circuit on the pane's value (§8.6). |
| `C` | Enable the periodic self-check (preset call 105) — maintenance only. Disarmed only. `y/n` confirm (§8.6). |
| `d` | Disarm. |
| `?` | Toggle help overlay. |
| Tab / Shift-Tab | Scroll wire log half a page. |
| `ctrl+r` | Reopen serial port (manual USB-re-enumeration heal). |
| `ctrl+l` | Clear wire log. |
| `ctrl+c` / `ctrl+q` | Quit — best-effort all-stop first. |

Prompt discipline: an open prompt (arm entry, self-test confirm) **owns the
keyboard**. No motion key fires mid-prompt. Only the e-stop (space/esc) and
ctrl chords cut through.

Header bar (exact format):
`pelcobridge2 · <short port name> · <baud> 8N1 · addr <n> · proto D|P · jog 0xNN · rotctl:<client count> · mqtt: on|off · head: online|offline · DISARMED | ● ARMED`.

`<short port name>` = the configured port string after its last path separator
(the segment after the final `/`). A `/dev/serial/by-id/usb-FTDI-...` port
therefore shows only its trailing file name. `COM3` and `tcp:host:port`
hold no `/` and render unchanged.

Position pane: line 1 = `TRUE AZ <deg> (<age>)   EL <deg> (<age>)   readback
ok|down`. Line 2 = `PHYS AZ <deg>   PHYS EL <deg>   offset <±deg>`. Line 3 =
`TARGET <az> / <el>   <idle|setting|converged|FAILED>   [· MOVING] [·
SELF-CHECK ON | · self-check off | · self-check ?] [· <error>]`. Angles render as `%7.2f`. They render as `---.--` when no
readback. Ages render as `%.1fs` or `no fix`.

Wire log: capped at 2000 lines (trimmed to the last 1000). The viewport stays
pinned to the tail only if the operator was already at the tail. The
component can lose log lines under load (§6.1). The wire log is
operator-facing, not machine-consumed.

The TUI e-stop is stronger than a fire-and-forget submit. It performs a
**blocking round-trip into the engine (2 s timeout)**. A saturated intent
queue then can never silently swallow the stop while the status line claims
the TUI sent it. The queries, the arm flow, the self-test, and the self-check
toggle also use blocking round-trips, so the status line always shows the
engine verdict. Jogs, by contrast, are non-blocking submits.

### 9.3 Hold-to-move mechanics (recorded deviation, open question)

Terminals have no key-release event. Every jog keypress increments a sequence
number and arms a one-shot **`jog_hold_ms` timer (default 250 ms)**. Terminal
auto-repeat refreshes the timer with each repeated keypress. A tick that
fires with the *current* sequence number (no fresh keypress since) sends
exactly one stop. If the terminal does not auto-repeat, motion stops after
`jog_hold_ms`. **Windows Terminal auto-repeat behavior is bench-unverified**
— see §16. The designed fallback (a toggle-jog: first press starts, second
press stops) is NOT implemented.

### 9.4 Input hardening

Every entry path must reject NaN and infinity targets before they reach the
wire (rotctld parse, TUI arm prompt, engine finite checks). Otherwise they
park the head at 0° or arm a NaN offset. The rotctld parser accepts a decimal
comma (`45,5` → 45.5) per Hamlib convention.

### 9.5 Command-line flags

`-config <path>`. `-port <name>` overrides the configured serial port
(`tcp:host:port` selects the TCP transport). `-addr <n>`. `-baud <n>`.
`-list-ports` enumerates serial ports with USB identity — vid:pid, product,
serial number — and exits. `-no-rotctld`. `-print-config` prints the resolved
configuration and exits.

---

## 10. Error paths (normative summary)

| Trigger | Required effect |
|---|---|
| Serial read error | The component marks the head offline, records an error, and reopens automatically after ≥2 s cooldown. The reopen starts a fresh reader generation. If it fails, healing stops until manual `ctrl+r` (§4, §16). |
| Serial write error (incl. short write) | The engine fails the in-flight query, kills the ladder to `"failed"`, clears jog, and re-opens the transmit gate. It records the error. It never wedges. |
| Reply-wait expiry (400 ms) | The engine fails the query with "no valid readback", flushes partials, and retries the ladder check. |
| Check out of tolerance ×3 | The ladder ends `"failed"`. The queue drains. |
| Stop while motion queued | The engine replies "cancelled" to all pending requests. Nothing drains later. |
| Intent queue > 16 pending | The engine refuses new requests with "engine busy". |
| rotctld bind failure | The process logs it in the TUI. The process continues. |
| MQTT connect failure | The component logs the failure. The TUI header shows `mqtt: off`. Everything else continues. |
| Config file explicitly named but missing | The process exits at startup (fatal). |
| Serial port missing at startup | The process exits at startup (fatal). |

---

## 11. Safety invariants (any re-implementation must preserve them)

1. Arming is a TUI-only, human act. The design enforces it twice (no
   construction path outside the TUI, and an engine-side source check). The
   engine never persists it. Every start begins disarmed. No remote arming
   exists under any circumstances (§8.1).
2. MQTT can never move the head: `/cmd` accepts exactly `{"action":"stop"}`
   (§5.4).
3. The all-stop always works: any source, any state. It also cancels
   *queued* motion, so nothing moves after the stop frame (§6.4).
4. Self-test runs disarmed-only. It needs two confirmations. The engine
   refuses it while armed (§8.5).
5. At most one query is in flight. The engine never queries while the head
   moves (readback is garbage mid-motion) (§3.5).
6. Absolute sets get a quiet line. The settle window brackets every
   absolute-set transmission and re-send. No other traffic interleaves with
   an active ladder (§7).
7. A failed write unwinds rather than wedges. No state can wait on a timer
   that the engine never armed (§4).
8. The engine is a single serializer for the wire and all mutable state. The
   design caps the pending queue at 16. A stuck line then cannot grow it
   unboundedly (§6.1).
9. Coordinate frames stay consistent. User-facing pan values are true
   degrees in both directions. Goto-physical-zero bypasses the offset.
   Arming needs a fresh (≤10 s) pan readback and a stationary head (§8).
10. Every entry path rejects NaN/infinity targets before the wire (§9.4).
11. The component must disable the head's periodic self-check once at start
    (preset set 105), before anything else uses the line (§6.3), and again
    after every successful port reopen (§4). Only the
    TUI can enable it again (§8.6). The engine models the state as `"on"`,
    `"off"`, or `"unknown"`. The model is liveness-gated (§8.6): a claim
    lands only when the head proves that it is alive. Any link death
    drops the model to `"unknown"`. A self-test sets the model to `"on"`
    again.
12. A tilt set clamps to [0, 90]. A pan set wraps into [0, 360) (§3.4).
13. No timer polling. Timers are one-shot gate releases only. A stationary
    head generates no bus traffic (state dedup excludes timestamp and
    readback age) (§6.2, §5.3).
14. Two-layer liveness stays distinct. `/status` = component availability
    (LWT). `/state.device_online` = the head's serial link (§5.1,
    02-interface-spec.md).
15. MQTT handlers never block. Command handling runs serialized on a single
    worker queue, never on the client's dispatch thread. The engine never
    blocks on a slow event consumer (§5.4, §6.1).

---

## 12. Configuration

TOML file, seed-once, stored with 0600 permissions. Secrets never go on the
command line. Path resolution order (highest first): `-config` flag >
`PELCOBRIDGE2_CONFIG` environment variable > `config.toml` in the
executable's directory (Windows double-click friendly) > `./config.toml` >
built-in defaults. An explicitly named missing file is an error, never a
silent fallback.

| Key | Default | Meaning |
|---|---|---|
| `[serial] port` | `""` (the operator must set it, or give `-port`. Fatal otherwise.) | `COM3`, `/dev/ttyUSB0`, `/dev/serial/by-id/...`, or `tcp:host:port` |
| `[serial] baud` | `2400` | 8N1 always. |
| `[serial] addr` | `1` | Head's Pelco DIP address. |
| `[rotctld] enabled` | `true` | Serve rotctld. |
| `[rotctld] bind` | `"0.0.0.0"` | `127.0.0.1` keeps it local. |
| `[rotctld] port` | `4533` | TCP listen port. |
| `[control] jog_speed` | `18` (0x12) | Jog speed byte 0x00–0x3F. Values outside the range get the default (0x12), not a clamp. A configured value of exactly 0 also gets the default 0x12 through engine-level defaulting. 0 is the TOML zero value, so the engine cannot tell it from unset. The operator cannot configure a speed of 0. |
| `[control] settle_ms` | `2000` | Quiet-line window around absolute sets. Negative → default. |
| `[control] set_attempts` | `3` | Ladder re-sends. `< 1` clamps to 1. |
| `[control] set_tolerance_deg` | `0.3` | Check tolerance in degrees. 0 → default. |
| `[control] arm_max_readback_age_s` | `10` | Pan readback freshness that arming needs. Negative → default. |
| `[control] jog_hold_ms` | `250` | TUI hold-to-move stop timer. |
| `[control] min_az / max_az / min_el / max_el` | `0 / 360 / 0 / 90` | Advertised rotctld limits (`\dump_state`). **Not enforced** on set arguments — the frame layer wraps/clamps targets instead (§3.4, §16). |
| `[mqtt] enabled` | `false` | MQTT off by default. |
| `[mqtt] broker` | `"tcp://192.168.1.50:1883"` | Broker URL. |
| `[mqtt] client_id` | `""` → `muehle-uhf-rotator` | |
| `[mqtt] user` | `""` (built-in) → `"hf"` (seeded example config) | The built-in default of the reference binary is empty. The process then sends no username. The value `"hf"` comes from the seeded example config only. |
| `[mqtt] password` | `""` | Fallback only, for GUI-launched processes with no environment. The environment variable always wins. |
| `[mqtt] site / station / slot` | `muehle / uhf / rotator` | Topic namespace. |
| `[mqtt] device_model` | `"PTS-303Z/3050DZ"` | /meta device model. |
| `[mqtt] device_name` | `"UHF Rotator"` | /meta expose name and rotctld `get_info`. |
| `[mqtt] device_link` | `"rs485"` | /meta link. |
| `[mqtt] host` | `""` (built-in) → `"shack-pc"` (seeded example config) | /meta compute host. The built-in default is empty. The publisher then omits the `host` field from `/meta` (§5.2). |
| `[log] file` | `""` | Log file, not necessary. Empty disables it. |

Secrets: the environment variable `PELCOBRIDGE2_MQTT_PASSWORD` supplies the
MQTT password (preferred). As a documented accommodation for a double-clicked
executable with no environment, the 0600 config file is a second source.
Never a command-line flag. (The project README claims the config file is
never used. The code does use it as fallback. A re-implementation must pick
one contract — see §16.)

### 12.1 State file (`state.toml`)

Stored next to the config file (or next to the executable on a flagless
first run), 0600. It holds exactly one key: `last_offset_deg`. Despite its
name, this key stores the last *entered true azimuth* (the value the
operator typed at the arm prompt). The engine uses it only to pre-fill the
next arm prompt. The engine persists nothing else. Armed state never
survives a restart. A re-implementation can fix the misleading key name. But
the stored *meaning* (entered true azimuth, not the computed offset) is the
contract.

---

## 13. Deployment — recorded deviation from station convention

- **Target host: shack-pc**, a Windows machine at `192.168.1.197` (SSH user
  `iotte`), destination `C:/Users/iotte/pelcobridge2/`. This is a deliberate,
  recorded deviation from the station's service-on-shari convention
  (05-deployment-ops.md). The component is an interactive TUI. So there is
  **no service, no auto-start, no service-manager unit**. A human starts it
  by hand whenever the station needs the UHF rotator. Arming is a further
  manual step inside it. This deployment shape is inseparable from the
  safety model (§8) — a human must be at the keyboard to arm.
- The deploy script cross-compiles for Windows. It copies the binary. It
  seeds `config.toml` **once** (only if the remote file does not exist) from
  a local seed template derived from the example config. The operator must
  then edit `[serial] port` and re-run. Environment variables `PELCO2_HOST` /
  `PELCO2_USER` / `PELCO2_DEST` override host/user/destination. The script
  prints the manual start line and an arming reminder.
- Build targets: Windows, Linux, macOS (both the main and mock binaries).
  Runtime dependencies on the target: a USB–RS-485 adapter at the configured
  port. Also: network reachability to the MQTT broker (not necessary) and to
  rotctld clients.
- The fatal-error path holds the console open with "press Enter to exit…"
  so the operator can read a double-clicked executable's error.
- Nobody had yet executed the deploy to the shack PC as of 2026-08-29. The
  component was only built and green in CI/bench tests (see the status note
  in §16).

Reference-implementation note: the deploy script cross-compiles with
`GOOS=windows`, `CGO_ENABLED=0`, `-trimpath -ldflags "-s -w"` and copies
through `scp`. The station-wide service-manager convention is systemd. It
does not apply here.

---

## 14. Reference-implementation notes (non-normative)

- The reference implementation is Go: a Bubble Tea TUI, a
  goroutine-per-connection rotctld server, a channel-and-intent engine,
  paho MQTT. The mock head (`pelcobridge2-mock`) simulates the head's
  quirks (silence-required sets, garbage readback while the head moves)
  over a pseudo-terminal or TCP. Each quirk is
  toggleable.
- Engine tests need the settle window scaled above the simulated travel
  time. The first check readback is garbage while the (simulated) head
  still moves.
- The reference implementation does the MQTT state dedup by marshaling the
  payload with the timestamp blanked and comparing JSON. Any
  change-detection with the same effect is acceptable.
- The component evolved from a one-time bench tool (`pelcotest`/`ptest`).
  The team used that tool to reverse-engineer the head's serial behavior.
  The authors folded the knowledge that the tool produced into §3. That
  knowledge is contract for this bridge. The team deleted the tool from the
  repository on 2026-08-29. It does not need reconstruction.

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

## 16. Open decisions and unresolved facts

1. **Bench facts needing hardware re-confirmation — earlier tool vs later
   bench contradiction.** The earlier bench tool (`ptest`, findings dated up
   to 2026-08-27) found: the head *ignored* the absolute-set opcodes
   `0x4B`/`0x4D`. It also found the tilt reply (`0x5B`) word *meaning
   unknown*, with readings far outside the 0–90° travel. The tool fitted a
   "raw encoder count" hypothesis and then contradicted it. The later
   pelcobridge2 bench session (2026-08-28) established: absolute sets *do*
   land when the line is quiet (the set ladder checks to 0.3° against
   readback). It also established that both pan and tilt replies are plain
   degrees×100. The working hypothesis: the earlier "ignored" observation
   was an artifact of the missing quiet-line discipline. The two research
   specs (`_research/pelcobridge2.md` §2, `_research/aux-projects.md` §3.2)
   record the evidence for each variant. **A re-implementation must
   re-confirm this on hardware** before it trusts tilt readback as an
   elevation readout. If the earlier finding holds for tilt, the ladder's
   tilt check and the `el` field semantics need redesign.
2. **Bench-tuned control defaults need hardware re-tuning.** The team tuned
   `settle_ms` 2000, `set_tolerance_deg` 0.3, reply wait 400 ms, and
   `jog_hold_ms` 250 against the *simulated* head. As of 2026-08-29,
   interactive bench smoke and control-default tuning on the real head were
   still pending. The first deploy to the shack PC was also still pending.
   Treat all four as defaults pending hardware validation.
3. **Windows Terminal auto-repeat for hold-to-move still needs bench
   confirmation.** If arrow-key holds do not auto-repeat there, each jog
   stops after
   `jog_hold_ms` (250 ms). The designed fallback — a toggle-jog (press to
   start, press again to stop) — is NOT implemented. The team must decide
   this after a bench check on the shack PC.
4. **Self-check disable cadence — firmware questions stay open.** The engine
   sends preset set 105 (disable periodic self-check) at process start and
   again after every successful port reopen (§4, §6.3). The model is honest
   about proof: it reads `"unknown"` until the head answers a frame after
   the preset went out (§8.6). Two firmware questions stay open and need
   bench confirmation: does the disable survive head power loss, and is
   re-sending the preset idempotent on the head? Until the bench answers,
   the TUI key `c` (§8.6) lets the operator send the disable frame again
   at any time without a process restart.
5. **Auto-heal gives up permanently on a failed reopen.** On a read error
   the reader dies. If the throttled reopen then fails, no reader runs, so
   no further read errors arrive and the automatic retry stops entirely.
   Recovery then needs manual `ctrl+r` (which always works). The document
   preserves this as documented behavior. A continuous throttled retry is
   an improvement, not a regression.
6. **MQTT password source: README vs code.** The README says the password
   never comes from the config file. The code falls back to the
   `[mqtt] password` key when the environment variable is empty (a
   documented accommodation for a double-clicked Windows executable with no
   environment). A re-implementation must pick one contract and document
   it.
7. **`state.toml` key name lies.** `last_offset_deg` stores the last
   *entered true azimuth*, not the computed offset (§12.1). The behavior is
   correct. The name invites a wrong re-implementation. Rename it in a
   re-implementation, and keep the meaning.
8. **Advertised limits are inconsistent and unenforced.** The design fixes
   the `/meta` expose min/max at 0–360 / 0–90. They do not track
   `[control] min_az/max_az/min_el/max_el`. Those keys feed only rotctld
   `\dump_state`. The engine enforces neither set on `set_pos` arguments.
   (A `set_pos 400 95` wraps pan to 40° and clamps tilt to 90° at the frame
   layer instead of a range rejection.) A re-implementation must either
   enforce the configured limits consistently on both surfaces or document
   the wrap/clamp behavior to clients.
9. **The team removed Pelco-P framing (2026-08-29).** An earlier revision sent
   P frames, accepted P frames on reception, and published a `protocol` state
   field. The config key `[serial] pelco_p` selected that envelope. The removal
   has two reasons. First, the P envelope gave no benefit on this one-head
   link. Second, the head's Pelco-P addressing is an unverified assumption:
   strict Pelco-P gear is zero-indexed (address byte n = unit n+1). A P frame
   with a wrong address goes to a different unit. The head then silently
   ignores it, and the operator sees "does not answer Pelco-P". A
   re-implementation must not add P framing again until a bench session
   checks P addressing on this head. The device fact that the head answers in
   the envelope of the received frame (§3.5.3) stays true. It has no effect
   on a D-only component.
10. **`JogSpeed` has no engine-level source gate** (unlike
    Arm/Disarm/SelfTest). Harmless today — only the TUI constructs it. But
    the "twice enforced" discipline is not uniform. A re-implementation can
    add the gate for symmetry.
11. **A misbehaving rotctld client can starve the queue.** The rotctld
    server accepts unlimited concurrent clients. All funnel into the one
    16-slot engine queue. A flood from one client can get everyone else
    "engine busy" (the e-stop still cuts through). Rotctld per-client
    fairness/throttling is an open design decision.
12. **`-port sim` does not exist.** A package doc in the reference code
    mentions an in-process simulator mode (`-port sim`). The shipped binary
    supports only real serial ports and `tcp:`. Documentation drift, not
    behavior.
13. **Broker topology.** The default broker URL is
    `tcp://192.168.1.50:1883` (current production). A planned migration of
    station components to a broker on shari (192.168.1.139) exists on an
    unmerged branch and is not deployed. This component's broker URL is
    configuration. It follows whatever the station decides (see
    02-interface-spec.md).
14. **UHF-side capture gap.** The station's MQTT traffic recorder defaults
    to the HF subtree only. It does not record this slot's bus traffic by
    default (`_research/aux-projects.md` §1.2). This is an operational note
    for debugging, not a component decision.
