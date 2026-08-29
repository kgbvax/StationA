# pelcobridge2 — Research Spec (for PRD)

Component: **pelcobridge2** — UHF rotator console for the Mühle station
(`stationa` monorepo). Source of truth: code under
`pelcobridge2/cmd/` and `pelcobridge2/internal/` as of 2026-08-29 (branch
`hf_console/review-fixes`); project `CLAUDE.md` and `README.md` cross-checked.
Where prose and code disagree, the code wins and the disagreement is listed in
§9.

---

## 0. Glossary (every term used below, in one sentence each)

- **Ham radio / amateur radio**: licensed radio communication as a hobby; the
  station ("Mühle", site prefix `muehle`) is such a station.
- **UHF**: ultra-high frequency (~300 MHz–3 GHz); the station's UHF antenna
  sits on a movable mount.
- **Rotator**: motorized apparatus that points an antenna; a *pan/tilt head*
  rotates in azimuth (pan, horizontal angle) and elevation (tilt, vertical
  angle).
- **Azimuth (az)**: horizontal pointing angle; in this system **true azimuth**
  is the compass-true angle, while **physical azimuth** is the head's own
  raw readback; their difference is the *arm offset* (§5.3).
- **Elevation (el / tilt)**: vertical pointing angle, 0° = horizon, 90° =
  zenith for this head.
- **PTS-303Z/3050DZ**: the physical Pelco-family pan/tilt head being driven.
- **Pelco-D / Pelco-P**: two byte-level serial protocols for CCTV pan/tilt
  hardware; D uses 7-byte frames with an additive checksum, P uses 8-byte
  frames with an XOR checksum (§2).
- **RS-485**: a multi-drop serial electrical bus standard; here a USB–RS-485
  adapter connects the host to the head.
- **rotctld**: the TCP server of *Hamlib* (an open-source radio/rotator
  control library) that accepts one-line text commands from clients such as
  `rotctl` and *gpredict* (a satellite-tracking program).
- **hamlib model 901**: the "NET_ROTCTL" model number — a client connecting
  with `rotctl -m 901 -r host:port` speaks the rotctld text protocol.
- **RPRT n**: rotctld's reply convention — `RPRT 0` success, negative numbers
  are Hamlib error codes (e.g. -9 = rejected, -11 = no usable readback).
- **TUI**: terminal user interface — here a full-screen interactive terminal
  application.
- **MQTT**: lightweight publish/subscribe message protocol; the station's
  message bus. A **slot** is one component's topic namespace
  (`<site>/<station>/<slot>`); this component owns slot `muehle/uhf/rotator`.
- **Retained message**: an MQTT message the broker keeps and delivers to any
  future subscriber immediately (used so state survives subscription timing).
- **LWT** (Last Will and Testament): a message the broker publishes on the
  client's behalf if it disconnects uncleanly — here `offline` on `/status`.
- **QoS**: MQTT delivery guarantee level; QoS 1 = at-least-once.
- **Arming**: this component's manual safety gate — a keyboard-only act in the
  TUI that must happen before remote (rotctld) clients may move the head
  (§8).
- **Jog**: continuous motion in one direction at a set speed, started by one
  command and stopped by another (contrast with an absolute *set*).
- **Set ladder**: this component's verified absolute-position sequence: send
  set → quiet window → one readback query → tolerance check → retry or
  converge (§5.4).
- **Seed-once**: deployment convention — the installer writes the config file
  only if absent, never overwrites it; the device/operator owns it afterwards.
- **shari**: the Raspberry Pi that hosts the station's systemd services; this
  component deliberately does NOT run there (§7).
- **shack-pc**: the Windows PC in the radio shack where this interactive
  component runs.

---

## 1. Purpose & role

pelcobridge2 is a **single-process interactive console application** (not a
daemon) that:

1. Drives the PTS-303Z/3050DZ pan/tilt head (the UHF antenna rotator) over
   RS-485 using the Pelco-D protocol.
2. Presents the operator a full-screen terminal UI for manual motion, position
   queries, arming, and a live wire log.
3. Serves the same head to Hamlib clients (`rotctl`, `gpredict`) as a
   **rotctld-compatible TCP server** (model 901) — but only once the operator
   has manually **armed** it from the TUI.
4. Publishes the station-standard four-plane MQTT presence on slot
   `muehle/uhf/rotator`, accepting exactly one remote command: **stop**.

Place in the station: it is the `muehle/uhf/rotator` slot (see the station
integration model). Unlike every other bridge in the monorepo, it runs
**interactively on the shack PC** rather than as a hardened systemd service on
shari, because its primary interface is a human at a keyboard and its core
safety rule is that a human must arm it locally.

A companion binary, **pelcobridge2-mock**, serves a simulated head
(`internal/simhead`) over a pty or TCP socket for bench testing with the
hardware's quirks reproduced (silence-required sets, garbage readback while
moving).

---

## 2. Upstream interface — the pan/tilt head

### 2.1 Transport

- Physical: RS-485 via a USB-serial adapter. Serial parameters: **8 data
  bits, no parity, 1 stop bit (8N1)**, baud configurable — **default 2400**
  (the head family is documented 1200–9600; the bench link runs 2400).
- The config `[serial].port` may also be `tcp:host:port`, which connects to
  the mock head over TCP (also usable on Windows, which has no pty). A real
  port is opened as e.g. `COM3`, `/dev/ttyUSB0`, or `/dev/serial/by-id/...`.
- **Opening the link is fatal on failure at startup** — the process prints the
  error (and holds the console open with "press Enter to exit…" for a
  double-clicked Windows exe) and exits rather than presenting a dead UI.
- Writes are single-frame and atomic from the engine's perspective; a short
  write (fewer bytes than the frame) is an error (`short write: N of M
  bytes`).
- One frame at a time; after every transmit the engine holds the line quiet
  for the **frame gap** (§5.1) before allowing the next transmit.

### 2.2 Pelco-D framing (the TX/RX envelope)

7-byte frame: `FF addr cmd1 cmd2 d1 d2 sum` where

- `FF` is the fixed start byte;
- `addr` is the head's DIP-switch address (**default 1**, configurable);
- `cmd1` is always `0x00` for everything this head documents — the action
  lives in `cmd2`;
- `d1`, `d2` are data bytes (speed or 16-bit position word halves);
- `sum` = `(addr + cmd1 + cmd2 + d1 + d2) & 0xFF` (8-bit additive checksum).

### 2.3 Pelco-P framing (removed from the component 2026-08-29)

Historical record — the component earlier offered Pelco-P as an optional TX
envelope (`[serial].pelco_p = true`) and accepted P frames on RX. It was
removed; the component now speaks Pelco-D only. The bench facts about the
head itself stay valid:

8-byte frame: `A0 addr cmd1 cmd2 d1 d2 AF xor` — same logical fields; the
checksum is the **XOR of the seven bytes preceding it** (indices 0..6,
including STX `A0` and ETX `AF`). Note: this head uses the **same address
byte in both protocols** (it matches one DIP code regardless of protocol;
strict Pelco-P gear is zero-indexed, this unit is not — the equivalence is
an unverified bench assumption).

**The head is protocol-adaptive on RX**: it answers in whichever envelope a
frame arrived in. The component never sends P, so every answer comes back in
the Pelco-D envelope; the receive assembler accepts only D frames (a P
frame's `A0` start byte assembles as noise).

### 2.4 Opcodes used (cmd2 values)

| cmd2 | Name | Meaning | Data bytes |
|---|---|---|---|
| `0x00` | stop | stop all movement | 0, 0 |
| `0x02` | jog right | pan jog, speed in `d1` | speed in d1 |
| `0x04` | jog left | pan jog, speed in `d1` | speed in d1 |
| `0x08` | jog up | tilt jog, speed in `d2` | speed in d2 |
| `0x10` | jog down | tilt jog, speed in `d2` | speed in d2 |
| `0x4B` | set pan | absolute pan set — **only lands on a quiet line** | target word |
| `0x4D` | set tilt | absolute tilt set — same quiet-line rule | target word |
| `0x51` | query pan | pan angle request | — |
| `0x53` | query tilt | tilt angle request | — |
| `0x59` | pan response | pan position reply | position word |
| `0x5B` | tilt response | tilt position reply | position word |
| `0x03` | preset set | "set preset N" / extended selector | N in `d2` |
| `0x07` | preset call | "call preset N" / extended selector | N in `d2` |

Preset selectors used:

- **Preset set 105 (`0x69`)** = *disable the head's periodic self-check*.
  Sent by the engine **once at process start, before anything else uses the
  line** (see §5.2 and the fragility note in §9). The head's periodic
  self-check otherwise re-homes the head unprompted. Re-enabling would be
  preset call 105 — this component never sends it.
- **Preset call 125 (`0x7D`)** = factory self-test: restores defaults and
  re-homes the head (physically swings it). DANGEROUS — can rip cables if
  pointed wrong. Only reachable from the TUI, only while disarmed, behind a
  two-stage confirmation (§4.3).

Speed byte: jog speeds live in `d1` for pan-axis ops (`0x02/0x04/0x4B/0x51/
0x59`) and `d2` for tilt ops; documented range **`0x00`–`0x3F`**; default jog
speed **`0x12`** (decimal 18).

Position encoding: `d1` is the high byte, `d2` the low byte of a big-endian
16-bit word; **degrees = word / 100** (hundredths of a degree, "degrees×100").
This is bench-confirmed for BOTH pan (0x59) and tilt (0x5B) responses. Word
range 0..655.35. Absolute-set targets use the same encoding; a pan set wraps
its argument into [0, 360) first; a tilt set **clamps** its argument to
[0, 90] (overshooting tilt travel is the dangerous direction). NaN/Inf
targets are rejected before encoding (§8).

### 2.5 Query/response contract and handshake

- There is **no connection handshake** with the head; the link is a raw serial
  line. The engine's "connect" ritual is a single **"disable self-check"
  frame** (preset set 105) sent at engine start.
- **At most one outstanding query** at any time. The engine remembers the
  expected *response* opcode (`0x59` or `0x5B`) for the in-flight query and
  matches incoming readbacks against it. A query that gets no (usable) answer
  within the **reply wait (default 400 ms)** is failed with
  "no valid readback"; the engine then flushes any stalled partial bytes
  from the assembler (§2.6) and resumes.
- **Readback is garbage while a motor runs** (bench-confirmed: the head
  returns checksum-valid, position-unrelated words mid-motion). The engine
  therefore refuses user queries while moving (`"rotator is moving"`).
- The head stays **silent** on undecodable or wrong-address frames.

### 2.6 RX assembler (byte-stream → frames)

A byte-level resynchronizer with two load-bearing rules:

1. **Sync** on the start byte: `0xFF` → expect a 7-byte D frame; `0xA0` →
   expect an 8-byte P frame; anything else is dropped into a noise run.
   Validate the matching checksum; on failure drop the leading byte and
   resume scanning. Noise runs are emitted **before** the frame that follows
   them, in wire order, and are capped/chunked at 256 bytes per run within a
   single feed (a wrong-baud flood stays readable).
2. **Incomplete frames** are held only until the next receive gap; when the
   reply-wait expires the engine flushes them as a "partial" noise event.
   Without this bound, a truncated reply merges with the next reply and can
   produce a checksum-valid frame carrying a fabricated position word.

Every RX event is rendered to the wire log as: raw hex line, `raw: addr=..
cmd1=.. cmd2=.. d1=.. d2=..`, `word=XXXX (n)  chk=XX ok|BAD (want XX)`, and
for position frames a `pan %.2f°` / `tilt %.2f°` line; each line truncated at
**50 columns** (the log pane truncates without wrap, so long lines lose their
tail silently).

### 2.7 Connection-loss detection & healing

- **Read side**: a dedicated reader goroutine does blocking reads; ANY read
  error (pulled/re-enumerated USB adapter, dropped TCP peer) marks the head
  offline (`device_online=false`, `error` set) and triggers auto-reopen of
  the port, **throttled to one attempt per 2 s** (`reopenCooldown`), then a
  fresh reader generation. Late errors from a superseded reader generation
  are ignored by generation tag. `ctrl+r` in the TUI performs the same reopen
  manually and always works.
- **Write side**: a failed write must *unwind*, never wedge: the in-flight
  query is failed ("no valid readback"), an active set ladder is killed with
  status "failed", jog state is cleared, the transmit gate is re-opened (no
  frame went out, so no timer was armed), and the error is recorded. Without
  this, state stored before a failed TX would wait forever on a timer that
  was never set.
- The TCP mock transport bounds every write with a 3 s deadline (a wedged
  peer must not wedge the engine — the e-stop rides on writes) and reconnects
  with a 3 s dial timeout.

---

## 3. MQTT presence — slot `muehle/uhf/rotator`

MQTT is **optional** (`[mqtt] enabled = false` by default) and **never
fatal**: broker-down or auth failure only degrades the header indicator; the
TUI and rotctld keep working. Broker default `tcp://192.168.1.50:1883`, user
`hf`, password from env `PELCOBRIDGE2_MQTT_PASSWORD` (fallback: the `[mqtt]
password` TOML key — see §9 for the README/code disagreement). Client ID
default `muehle-uhf-rotator`. Auto-reconnect and connect-retry are on with a
**5 s retry interval**. On every (re)connect: publish birth, refresh retained
/meta, re-subscribe /cmd, and **republish the last retained /state payload**
(a quiescent head emits no snapshots — without this, consumers after a broker
restart would see no rotator state at all).

Four planes, exact topic strings:

| Topic | Direction | Retained | QoS | Payload |
|---|---|---|---|---|
| `muehle/uhf/rotator/meta` | publish | yes | 1 | birth certificate JSON (below) |
| `muehle/uhf/rotator/state` | publish | yes | 1 | state snapshot JSON (below) |
| `muehle/uhf/rotator/status` | publish (LWT) | yes | 1 | `"online"` / `"offline"` |
| `muehle/uhf/rotator/cmd` | subscribe | — | 1 | `{"action":"stop"}` only |

### 3.1 `/status`

Literal body `online` on connect, `offline` as the MQTT Last Will (published
by the broker if the process dies uncleanly). This is the **component's**
availability and is deliberately distinct from `/state.device_online`, which
is the **head's serial link**.

### 3.2 `/meta` (retained birth certificate)

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

All identity strings (`device.model`, `expose.device.name/model`, `link`,
`host`) come from `[mqtt]` config keys; the field min/max are **hardcoded**
0/360 and 0/90 (not derived from `[control] min_az…max_el`, which feed only
the rotctld limits).

### 3.3 `/state` (retained, change-deduped snapshot)

One JSON document per engine state change. Field names, types, units,
semantics:

| Field | Type | Semantics |
|---|---|---|
| `ts` | string | UTC timestamp, RFC 3339, stamped at publish time |
| `az`, `el` | number or `null` | **true** (offset-corrected) azimuth/elevation in degrees, rounded to 0.01; `null` when no readback yet |
| `phys_az`, `phys_el` | number or `null` | raw head readback in degrees, rounded to 0.01; `null` when none |
| `readback_valid` | bool | both axes have a readback at all (not freshness) |
| `readback_age_s` | number | age of the pan readback in seconds, rounded to 0.1 |
| `armed` | bool | arm state (arming is TUI-only; false at every start) |
| `az_offset_deg` | number or `null` | the arm offset (physical − true), degrees, 0.01 rounding |
| `moving` | bool | a jog is commanded or a set ladder is active |
| `target_az`, `target_el` | number or `null` | current ladder target in **true** degrees; **omitted entirely** (omitempty) when none |
| `set_status` | string | `""` (omitted), `"setting"`, `"converged"`, or `"failed"` |
| `jog_speed` | int | current jog speed byte value (0–63) as an integer |
| `protocol` | string | envelope of the last RX frame, `"D"` or `"P"`; omitted when none yet |
| `rotctld_clients` | int | number of currently connected rotctld TCP clients |
| `device_online` | bool | a checksum-valid frame has been received since the link was last known-dead |
| `link` | string | `"ok"` when device_online else `"down"` |
| `error` | string | last link/engine error text; omitted when empty |

**Publish cadence / dedup:** the engine emits a snapshot event on every state
change (readback arrival, arm/disarm, jog start/stop, ladder phase changes,
link errors); the MQTT layer marshals the payload with `ts` blanked and
`readback_age_s` zeroed as a **dedup key** and publishes only when a real
field changed — a stationary, parked head produces no bus churn. The last
published payload is cached for republish on reconnect.

### 3.4 `/cmd`

Subscribed at QoS 1. Handler accepts **only** `{"action":"stop"}` (the
station-wide `/cmd` JSON convention also defines an optional `value` key,
irrelevant for stop). Any other action — and any unparseable payload — is
logged as a warning and ignored; there is **no** MQTT path to motion, arming,
calibration, or self-test. Handling is enqueued onto a bounded (32) job queue
drained by a single worker — never executed on the MQTT client's dispatch
goroutine (a known paho deadlock pattern that hit other components live).

---

## 4. Command surface

Three entry points: MQTT (stop only, §3.4), the rotctld TCP server (§4.1),
and the TUI keyboard (§4.3). All funnel into a single engine (§5).

### 4.1 rotctld TCP server (`[rotctld] enabled/bind/port`, default `0.0.0.0:4533`)

Any number of concurrent clients; line-oriented text protocol. Exact
behaviour (pinned byte-exactly by tests against hamlib's
`rotctl_parse.c`/`netrotctl.c`):

| Input | Reply | Behavior |
|---|---|---|
| `p` or `get_pos` | two lines `"%.2f\n%.2f\n"` (az then el), **no RPRT on success** | queries pan then tilt; errors → `RPRT -11` (no usable readback; moving counts, since readback is garbage mid-motion) |
| `P` or `set_pos` with az el args | `RPRT 0` | parses two floats (decimal comma accepted: `,`→`.`); **NaN/Inf and parse failures → `RPRT -1`** (rejected up-front; they would otherwise park the head at 0°); disarmed → `RPRT -9`; engine timeout → `RPRT -6`; any other refusal (busy, cancelled, etc.) → `RPRT -1`; missing args → `RPRT -1` |
| `S` or `stop` | `RPRT 0` (always) | all-stop, every state, cancels ladder and queued motion |
| `_` or `get_info` | info line | `"pelcobridge2 · <device name>"` (default `pelcobridge2 · UHF Rotator`) |
| `\dump_state` | multi-line, see below | protocol v1 block (gpredict sends this at open) |
| `q` or `Q` | nothing | close connection |
| `#comment`, empty line | nothing | ignored |
| `+` prefix on any get command | `RPRT 0\n` prepended | extended-response mode |
| unknown command | `RPRT -4` | |

`set_pos` details: the pan intent is submitted first; if it fails the tilt
intent is not submitted and the mapped error is returned. Both degrees are
**true** (offset-applied; §5.3). `RPRT 0` is returned as soon as both intents
are *accepted by the engine* — the verification ladder then runs
asynchronously; convergence/failure is visible on `/state.set_status` and in
the TUI, not in the rotctld reply.

`\dump_state` exact body (protocol v1):

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

(min/max values come from `[control] min_az/max_az/min_el/max_el` config
keys, defaults 0/360/0/90; `rot_model` is hardcoded 901 = NET_ROTCTL.)

Each command round-trip uses a **2 s caller timeout** against the engine.
`-no-rotctld` CLI flag suppresses the listener even when config enables it.
A bind failure is logged into the TUI wire log and is **non-fatal**.

### 4.2 Engine intents (internal command vocabulary)

Everything above reduces to these engine intents, each gated by its source
(`tui`, `rotctld`, `mqtt`, `engine`):

| Intent | Arg | Source gate | Behavior |
|---|---|---|---|
| `QueryPan` / `QueryTilt` | — | any source | one readback; refused while moving or if another query is outstanding |
| `Jog` | direction | TUI allowed **disarmed**; rotctld/MQTT require armed (MQTT never issues it) | start motion at jog speed; stopped by Stop or TUI hold-expiry |
| `Stop` | — | any source, any state, disarmed or armed, gate open or closed | immediate all-stop frame; kills ladder; **cancels every queued (not-yet-executed) motion request** with "request cancelled (all-stop)" |
| `SetPan` / `SetTilt` | true degrees | armed only; finite values only | starts the verification ladder |
| `GotoPhysZero` | — | TUI allowed disarmed; others armed | ladder to physical (0°, 0°), offset never applied |
| `Arm` | true azimuth | **TUI only, enforced twice** (source check + no construction path elsewhere) | see §5.3 |
| `Disarm` | — | TUI only | drops armed state |
| `SelfTest` | — | TUI only, **refused while armed** | sends preset call 125 (re-homes head) |
| `JogSpeed` | speed byte | no source check in engine (TUI is the only caller) | clamps to 0x00–0x3F |
| `Reopen` | — | any (TUI ctrl+r) | reopen serial transport, restart reader |

Error strings returned to callers (exact): `engine busy`, `request cancelled
(all-stop)`, `rotator is not armed`, `rotator is moving`, `no valid readback`,
`readback too old to arm`, `intent not allowed from this source`, `set did
not converge`.

### 4.3 TUI keyboard (the human surface)

Full-screen alt-screen layout: header bar, 3-line position pane, wire-log
viewport, prompt line, status line, and a `?` help overlay.

| Key | Action |
|---|---|
| `←→↑↓` / `hjkl` | hold-to-move jog at jog speed — **works disarmed** (this is how the head is positioned before arming) |
| `SPACE` / `ESC` | **global e-stop** — all-stop frame, cancels prompts, always allowed from every state |
| `a` / `e` | query azimuth / elevation (one-shot; result in status line, `%.2f°`; true degrees) |
| `A` | arm flow: text prompt "enter the TRUE azimuth the head points at", prefilled from `state.toml` (§5.3) |
| `0` | goto **physical** zero (both axes), offset never applied — works disarmed |
| `+` / `-` (also `=` / `_`) | jog speed ±1, clamped 0x00–0x3F |
| `s` | self-test — disarmed only; two-stage confirm: `y`, then type `RIPCABLES` |
| `d` | disarm |
| `?` | toggle help overlay |
| `tab` / `shift+tab` | scroll wire log half a page |
| `ctrl+r` | reopen serial port (manual USB-re-enumeration heal) |
| `ctrl+l` | clear wire log |
| `ctrl+c` / `ctrl+q` | quit — best-effort all-stop first |

Prompt discipline: an open prompt (arm entry, self-test confirm) **owns the
keyboard** — no motion key fires mid-prompt; only the e-stop (space/esc) and
ctrl chords cut through.

**Hold-to-move mechanics** (recorded deviation from "no timers"): terminals
have no key-release event. Every jog keypress increments a sequence number
and arms a one-shot **`jog_hold_ms` timer (default 250 ms)**; terminal
auto-repeat refreshes the timer with each repeated keypress; a tick firing
with the *current* sequence number (no fresh keypress since) sends exactly
one stop. If the terminal does not auto-repeat, motion stops after
`jog_hold_ms` (Windows Terminal auto-repeat is bench-unverified; the fallback
design is a toggle-jog).

The TUI e-stop is stronger than a fire-and-forget submit: it performs a
**blocking round-trip (2 s timeout) into the engine**, so a saturated queue
can never silently swallow it while the status line claims it was sent.
Jogs, by contrast, are non-blocking submits.

Header bar (exact format):
`pelcobridge2 · <short port name> · <baud> 8N1 · addr <n> · proto D|P · jog
0xNN · rotctl:<client count> · mqtt: on|off · head: online|offline ·
DISARMED | ● ARMED`.

Position pane: line 1 = `TRUE AZ <deg> (<age>)   EL <deg> (<age>)   readback
ok|down`; line 2 = `PHYS AZ <deg>   PHYS EL <deg>   offset <±deg>`; line 3 =
`TARGET <az> / <el>   <idle|setting|converged|FAILED>   [· MOVING] [·
<error>]`. Angles render as `%7.2f`, `---.--` when no readback; ages as
`%.1fs` or `no fix`.

Wire log: unbounded-ish list capped at 2000 lines (trimmed to the last 1000);
the viewport stays pinned to the tail only if the operator was already at
the tail.

### 4.4 CLI flags

`-config <path>`, `-port <name>` (overrides `[serial].port`; `tcp:host:port`
selects the TCP transport), `-addr <n>`, `-baud <n>`, `-list-ports`
(enumerate serial ports with USB identity vid:pid/product/serial-number and
exit), `-no-rotctld`, `-print-config` (print resolved configuration and
exit).

---

## 5. Behavior & state machine

### 5.1 Architecture invariant (behavior contract)

**One engine goroutine owns the serial wire and all rotator state; no
mutexes, no shared mutation.** All other actors (TUI, rotctld server, MQTT
handler) submit typed intents over a channel (buffered 64; submit is
non-blocking, a full channel returns "engine busy") and receive results on a
one-slot reply channel with a caller timeout (**2 s convention**). Engine
events (snapshots + wire-log lines) fan out on a sink channel (512).

**No timer polling.** The engine's single timer is re-armed as one-shot
"gate releases" of exactly three kinds:

1. `frameGap` — after every TX, the transmit gate closes for
   **`IdleGap(baud)` = 1.5 × (7 bytes × 10 bits) / baud, minimum 20 ms**
   (≈43.75 ms at 2400 baud). While the gate is closed, motion/query intents
   are queued (FIFO, cap 16 — overflow returns "engine busy"); the Stop
   intent is the only thing that cuts through.
2. `replyWait` — armed when a frame-gap tick releases with a query
   outstanding; **400 ms**; on expiry the in-flight query is failed with "no
   valid readback", stalled partial bytes are flushed, and a ladder in its
   verify phase retries.
3. `settle` — the quiet-line window around absolute sets
   (**`settle_ms`, default 2000 ms**).

No timer ever transmits by itself; each expiry only releases the next queued
action. The TUI's hold-to-move tick (§4.3) is the single recorded deviation
and lives outside the engine.

### 5.2 Startup sequence

1. Resolve config (flag > `PELCOBRIDGE2_CONFIG` > `config.toml` next to the
   executable > `./config.toml`; **none found → built-in defaults, but a
   missing explicitly-named file is a fatal error** — a mistyped path must
   not quietly run on defaults).
2. Apply CLI flag overrides.
3. Open the transport (serial port or TCP); failure is **fatal** (exit).
4. Start the engine goroutine. It immediately: sends the
   **disable-self-check frame** (preset set 105) once, *before anything else
   uses the line*, and publishes one initial snapshot (so the TUI and MQTT
   have state even with no traffic — a quiescent head emits nothing).
5. Start the rotctld listener (unless disabled).
6. If MQTT enabled: connect (ctx-aware, non-fatal, background; 5 s retry),
   start the single jobs worker, subscribe `/cmd`.
7. Start the event pump (engine events → MQTT `/state` + TUI).
8. Run the TUI. When it exits, the context is cancelled and the engine sends
   one final all-stop frame on shutdown.

### 5.3 Arming model (the central safety mechanism)

**Operationally, "arming is manual, never remote" means:** a human must be
physically present at the console running the TUI on the shack PC, look at
the head, and press `A`. No rotctld command, no MQTT message, no config key,
and no CLI flag can arm the head — the arm intent has **no construction site
outside the TUI code**, and the engine independently rejects an Arm request
whose source is not `tui` ("intent not allowed from this source"). Armed
state is **never persisted** — every process start is disarmed, so a crash,
reboot, or restart removes remote motion capability until a human re-arms.

Arm flow, step by step:

1. `A` opens a prompt: "enter the TRUE azimuth the head points at now",
   prefilled (`%.1f`) with the last-confirmed value from `state.toml`
   (pre-fill only — the operator must confirm or correct it every run; it is
   never auto-loaded into the engine).
2. On Enter: the value is parsed and must be a finite number in **0..360**
   (`nan`/`inf` explicitly rejected — `ParseFloat` accepts them and they
   would arm a NaN offset).
3. A fresh pan readback is fetched first (the engine refuses to arm without
   one).
4. The engine then refuses if the head is moving (`rotator is moving`) or if
   the pan readback is older than **`arm_max_readback_age_s` (default 10
   s)** (`readback too old to arm`).
5. On success: `offset = physical_azimuth − entered_true_azimuth`; the head
   is ARMED. The entered value is saved to `state.toml` as the next
   pre-fill.

Coordinate frame (behavior contract):

- All user-facing degrees are **true**: rotctld `get_pos` replies and TUI
  `a`/`e` readouts apply `true = Norm360(physical − offset)` to pan replies
  (elevation has no offset), so a read-then-set hamlib client works in one
  consistent frame; `set_pos` arguments are converted to physical targets as
  `physical = Norm360(true + offset)`.
- The set ladder's *internal* verification queries keep raw physical degrees
  (its targets are physical).
- `GotoPhysZero` bypasses the offset in both directions (physical zero).
- An un-armed head therefore reports `get_pos` = physical position (offset
  is 0).

**What arming gates:** the rotctld *motion* path only — `set_pos`,
rotctld-sourced jogs (none exist in practice; jog is TUI-only) and
rotctld-sourced goto-zero answer `RPRT -9` while disarmed. **What it does
not gate:** TUI manual motion (jog, goto-0 — deliberately usable disarmed to
position the head before arming), queries from any source, and the all-stop
from any source. Disarm (`d`, TUI-only) re-locks remote motion immediately.

### 5.4 Set ladder (absolute positioning without polling)

Phases per ladder step: **1** = set TX due after settle; **2** = verify TX
due after settle; **3** = verify query in flight.

1. On an accepted set intent, the engine transmits the absolute-set frame
   immediately (line quiet by rule), closes the gate, and arms the settle
   timer (phase 2).
2. When settle elapses (head stopped moving, line quiet), it sends **one**
   verify query (phase 3).
3. The reply is compared to the target within **`set_tolerance_deg`
   (default 0.3°)**, pan compared with wraparound distance (min of |Δ| and
   360−|Δ|), tilt absolutely.
4. Within tolerance → step converges; for a multi-step ladder
   (goto-physical-zero = pan 0 then tilt 0) the next step starts with a
   fresh settle window; when all steps converge the ladder ends with
   `set_status = "converged"` and queued sets may proceed.
5. Out of tolerance or no readback → retry: decrement `tries`
   (initial **`set_attempts` = 3**), wait out another settle window (phase
   1), re-send the set frame; when tries are exhausted the ladder ends with
   `set_status = "failed"` and the queue drains anyway.

Any stop, jog, or new set **cancels** the ladder (a human always wins over an
in-flight verification). The all-stop also cancels motion that is merely
**queued** (behind a gate window or an outstanding query) by replying every
pending request with `request cancelled (all-stop)` — nothing may start
moving after an all-stop frame. While a query is outstanding or a ladder
runs, queued set-type intents are held back (FIFO head-of-line blocking) so a
new set can neither clobber the in-flight query reply nor drop the ladder's
remaining steps.

### 5.5 Jog behavior

Jog frames carry the speed byte (d1 for pan ops, d2 for tilt ops). Jogging
clears any active ladder (human wins). Motion state is `moving = jog active
OR ladder active`. Readback while moving is refused (garbage per §2.5), so
`phys_az` in the snapshot is stale during a jog — by design. A jog started
while the transmit gate is closed is queued and drains when the gate opens.

### 5.6 Error paths summary

| Trigger | Effect |
|---|---|
| Serial read error | head marked offline, error recorded, auto-reopen after ≥2 s cooldown, fresh reader generation; if reopen fails, error recorded and healing stops until `ctrl+r` (see §9) |
| Serial write error (incl. short write) | in-flight query failed, ladder killed as "failed", jog cleared, gate re-opened, error recorded — never wedges |
| Query reply-wait expiry (400 ms) | query failed "no valid readback", partials flushed, ladder verify retried |
| Verify out of tolerance ×3 | ladder "failed" |
| Stop while queued motion pending | all pending replied "cancelled"; nothing drains later |
| Queue > 16 pending | new requests refused "engine busy" |
| rotctld bind failure | logged in TUI, process continues |
| MQTT connect failure | stderr log line, TUI header shows "mqtt: off", everything else continues |
| Config file explicitly named but missing | fatal at startup |
| Serial port missing at startup | fatal at startup |

---

## 6. Configuration

TOML file, seed-once, stored **0600**; password never on the command line.
Path resolution order (highest first): `-config` flag >
`PELCOBRIDGE2_CONFIG` env > `config.toml` in the executable's directory
(Windows double-click friendly) > `./config.toml` > built-in defaults. An
explicitly named missing file is an error (never a silent fallback).

| Key | Default | Meaning |
|---|---|---|
| `[serial] port` | `""` (must be set or `-port` given; fatal otherwise) | `COM3`, `/dev/ttyUSB0`, `/dev/serial/by-id/...`, or `tcp:host:port` |
| `[serial] baud` | `2400` | 8N1 always |
| `[serial] addr` | `1` | head's Pelco DIP address |
| `[rotctld] enabled` | `true` | serve rotctld |
| `[rotctld] bind` | `"0.0.0.0"` | `127.0.0.1` keeps it local |
| `[rotctld] port` | `4533` | TCP listen port |
| `[control] jog_speed` | `18` (0x12) | jog speed byte, 0x00–0x3F; values outside the range are replaced by the default (0x12), not clamped; note 0 also becomes 0x12 via engine defaulting |
| `[control] settle_ms` | `2000` | quiet-line window around absolute sets; negative → default |
| `[control] set_attempts` | `3` | ladder re-sends; `< 1` clamped to 1 |
| `[control] set_tolerance_deg` | `0.3` | verify tolerance in degrees; 0 → engine default 0.3 |
| `[control] arm_max_readback_age_s` | `10` | pan readback must be fresher to arm; negative → default |
| `[control] jog_hold_ms` | `250` | TUI hold-to-move stop timer |
| `[control] min_az / max_az / min_el / max_el` | `0 / 360 / 0 / 90` | advertised rotctld limits (`\dump_state`); NOT enforced on set arguments (targets are wrapped/clamped at the frame layer, §2.4) |
| `[mqtt] enabled` | `false` | MQTT off by default |
| `[mqtt] broker` | `"tcp://192.168.1.50:1883"` | |
| `[mqtt] client_id` | `""` → `muehle-uhf-rotator` | |
| `[mqtt] user` | `""` (built-in; example config seeds `"hf"`) | binary default empty — no username sent; `"hf"` only via config.example.toml / deploy seed |
| `[mqtt] password` | `""` | fallback only, for hosts with no environment (double-clicked exe); env `PELCOBRIDGE2_MQTT_PASSWORD` always wins |
| `[mqtt] site / station / slot` | `muehle / uhf / rotator` | topic namespace |
| `[mqtt] device_model` | `"PTS-303Z/3050DZ"` | /meta device model |
| `[mqtt] device_name` | `"UHF Rotator"` | /meta expose name and rotctld `get_info` |
| `[mqtt] device_link` | `"rs485"` | /meta link |
| `[mqtt] host` | `""` (built-in; example config seeds `"shack-pc"`) | /meta compute host; serialized omitempty — omitted from /meta entirely when empty |
| `[log] file` | `""` | optional log file; empty disables |

Secrets: MQTT password via environment variable
`PELCOBRIDGE2_MQTT_PASSWORD` (preferred) or, as a documented fallback for a
GUI-launched process, the 0600 config file — never a CLI flag.

`state.toml` (next to the config file, or next to the executable on a
flagless first run; 0600): holds one key, `last_offset_deg` — **despite the
name it stores the last *entered true azimuth*** (the value the operator
typed at the arm prompt), used only to pre-fill the next arm prompt. Nothing
else is persisted; armed state never survives a restart.

Flags override config: `-port`, `-addr`, `-baud` apply only when actually
passed.

---

## 7. Deployment

- **Target host: shack-pc**, a Windows machine at `192.168.1.197` (SSH user
  `iotte`), destination `C:/Users/iotte/pelcobridge2/`. This is a recorded,
  deliberate deviation from the station's systemd-on-shari deployment
  convention: the component is an **interactive TUI**, so there is **no
  service, no auto-start, no systemd unit**. A human starts it by hand
  (`cd C:/Users/iotte/pelcobridge2 && pelcobridge2.exe`) whenever the UHF
  rotator is to be used, and arming is a further manual step inside it.
- `deploy.sh`: cross-compiles `windows/amd64` (`CGO_ENABLED=0`, `-trimpath
  -ldflags "-s -w"`), `scp`s the binary as `pelcobridge2.exe`, and seeds
  `config.toml` **once** — it copies a local `.deploy-seed.toml` (created
  from `config.example.toml` on first run; the operator must edit
  `[serial] port` and re-run) only if the remote file does not exist.
  Host/user/destination overridable via `PELCO2_HOST` / `PELCO2_USER` /
  `PELCO2_DEST`. It prints the manual start line and an arming reminder.
- `build.sh`: cross-compiles to `dist/` — default targets
  `windows-amd64.exe`, `linux-amd64`, `darwin-arm64`; `TARGETS=all` adds
  `linux-arm64`, `darwin-amd64`; both binaries (main + mock) per target.
  Darwin builds need cgo (IOKit for port enumeration); windows/linux are
  static.
- Runtime dependencies on the target: a USB–RS-485 adapter at the configured
  port; network reachability to the MQTT broker (optional) and rotctld
  clients.
- The `fatal()` path holds the console open with "press Enter to exit…" so a
  double-clicked exe's error is readable.

---

## 8. Invariants & safety rules (MUST be preserved by any reimplementation)

1. **Arming is a TUI-only, human act, twice enforced** — the engine rejects
   Arm/SelfTest intents from any non-TUI source, AND no code path outside
   the TUI can even construct them. Armed state is never persisted; every
   start is disarmed. There is no remote arming under any circumstances.
2. **MQTT can never move the head** — `/cmd` parses exactly
   `{"action":"stop"}`; anything else is dropped. No motion, arm, or
   calibration intent exists on the MQTT path.
3. **The all-stop always works**: any source, any state (armed/disarmed,
   mid-prompt, mid-ladder, gate closed), and it also cancels *queued* motion
   so nothing starts moving after the stop frame.
4. **Self-test is disarmed-only and double-confirmed** (press `y`, then type
   `RIPCABLES`); the engine additionally refuses it while armed. It re-homes
   the head and can rip cables.
5. **At most one outstanding query; never query while moving** (readback is
   garbage mid-motion — user queries are refused, the ladder only verifies
   after the settle window).
6. **Absolute sets get a quiet line**: the settle window must bracket every
   absolute-set transmission and re-send, and no other traffic may interleave
   with an active ladder.
7. **A failed write unwinds rather than wedges** — in-flight query failed,
   ladder killed as "failed", jog cleared, gate re-opened; no state may ever
   wait on a timer that was never armed.
8. **The engine (or its equivalent) is a single serializer** for the wire and
   all mutable rotator state; the queue is capped (16) so a stuck line
   cannot grow it unboundedly.
9. **Coordinate-frame consistency**: user-facing pan values are true degrees
   (offset applied in *both* directions — get replies and set arguments);
   goto-physical-zero bypasses the offset; arm requires a fresh (≤10 s)
   pan readback and a stationary head.
10. **NaN/Inf targets are rejected before reaching the wire** in every entry
    path (rotctld parse, TUI arm prompt, engine finite checks) — they would
    otherwise park the head at 0° or arm a NaN offset.
11. **The head's periodic self-check must be disabled once per connect**
    before anything else uses the line (preset set 105) — otherwise the head
    re-homes itself unprompted mid-contact. (See §9 for the once-per-process
    nuance.)
12. **A tilt set clamps to [0, 90]** (overshooting tilt travel is the
    dangerous direction); a pan set wraps into [0, 360).
13. **No timer polling**: timers are one-shot gate releases only; a
    stationary head generates no bus traffic (state dedup excludes
    timestamp and readback age).
14. **Two-layer liveness is kept distinct**: `/status` = component
    availability (LWT); `/state.device_online` = head's serial link
    (a checksum-valid frame seen).
15. **MQTT handlers never block** — command handling is serialized on a
    single worker queue, never on the client's dispatch goroutine; the
    engine never blocks on a slow event consumer (events are dropped, the
    latest snapshot is what matters).

---

## 9. Known defects & fragilities

1. **Self-check disable is sent once per *process start*, not once per
   *link* reconnect.** If the head itself power-cycles (or is power-cycled)
   while the process runs — or after a serial reopen — its periodic
   self-check is re-enabled and the head can re-home itself unprompted; the
   engine never re-sends preset set 105 after a reopen. CLAUDE.md's "once
   per connect" is optimistic relative to the code (`engine.Run` sends it
   once, `ReopenIntent`/auto-reopen do not).
2. **Auto-heal gives up permanently on a failed reopen.** On a read error the
   reader goroutine dies; if the throttled reopen then fails, no reader is
   running, so no further read errors arrive and the automatic retry loop
   stops entirely — recovery requires manual `ctrl+r` (which always works).
3. **`state.toml` field name lies**: `last_offset_deg` actually stores the
   last *entered true azimuth* (the arm prompt value), not the computed
   offset. Behavior is correct (prefill = last entered azimuth) but the name
   invites a wrong reimplementation.
4. **README vs code on the MQTT password**: README says the password "never
   [comes from] the config file", but `config.MQTTConfig` falls back to the
   `[mqtt] password` TOML key when the env var is empty (a documented
   accommodation for a double-clicked Windows exe with no environment). A
   reimplementation must decide which contract to keep.
5. **`-port sim` does not exist.** The `simhead` package doc mentions "`go
   test` and `-port sim`", but `cmd/pelcobridge2/main.go` supports only real
   serial ports and `tcp:` — there is no in-process simulator mode in the
   shipped binary.
6. **rotctld `set_pos` replies `RPRT 0` before the head has moved** — the
   ladder is asynchronous. A naive client that treats `RPRT 0` as
   "positioned" is misled; convergence is only observable via `/state` or
   the TUI. This is a protocol-shape constraint of rotctld, but it must be
   documented to clients.
7. **Hold-to-move depends on terminal auto-repeat.** On a terminal that
   does not auto-repeat arrow-key holds, each jog stops after
   `jog_hold_ms` (250 ms). Windows Terminal behavior is explicitly
   bench-unverified (recorded in CLAUDE.md "bench-dependent defaults");
   the design fallback is a toggle-jog, which is NOT implemented.
8. **`JogSpeedIntent` has no source gate in the engine** (unlike
   Arm/Disarm/SelfTest). Harmless today — only the TUI constructs it — but
   the engine's "twice enforced" discipline is not uniform.
9. **The `/meta` expose min/max values are hardcoded** (0–360 / 0–90) and
   do not track `[control] min_az/max_az/…`, which feed only rotctld
   `\dump_state`. A config with non-default limits produces inconsistent
   advertised limits between the two surfaces.
10. **Config `[control] min_az/max_az/min_el/max_el` are advertised but not
    enforced** — a `set_pos 400 95` from a client is not range-checked
    against them; the pan target wraps to 40° and the tilt clamps to 90° at
    the frame layer instead.
11. **Event loss by design**: engine→TUI events are dropped when the TUI is
    slow (latest snapshot matters), and the engine's sink drops events when
    saturated. Log lines can be lost under load; a reimplementation must not
    promise a complete wire log.
12. **The mock TCP transport in `main.go` serves one client at a time**
    (like the real RS-485 line); the rotctld TCP server has no such limit
    and funnels all clients through the one engine queue — a misbehaving
    client can consume the 16-slot pending queue and get everyone else
    "engine busy" (the e-stop still cuts through).

---

## 10. Re-implementation notes

**Must be preserved verbatim (behavior contract):**

- All Pelco-D frame bytes, the additive checksum, opcode values, speed-byte
  placement (d1 pan / d2 tilt), degrees×100 big-endian word encoding, and the
  head's bench quirks (quiet-line sets, garbage readback while moving). The
  component speaks Pelco-D only; Pelco-P framing was removed 2026-08-29 (§2.3).
- The RX assembler's two rules (noise-before-frame ordering; partial frames
  bounded by a receive gap) — they exist because of observed bench failures
  (fabricated checksum-valid positions).
- The exact rotctld wire responses, byte-for-byte: `p` success has NO RPRT
  line; error codes -1/-4/-6/-9/-11 in the exact situations of §4.1;
  `\dump_state` v1 body; `+` extended prefix semantics; comment/empty-line
  silence; `q/Q` closes.
- The full arming model: TUI-only construction path + engine source gate +
  never-persisted state + fresh-readback requirement (10 s) + stationary-head
  requirement + true-azimuth entry with confirm-prefill; and the
  arm-gates-rotctld-only scoping (TUI jog/goto-0 disarmed, all-stop
  everywhere).
- The stop-only MQTT surface, the exact topic strings, payload field
  names/types/rounding (0.01° degrees, 0.1 s age), the dedup key excluding
  `ts` and `readback_age_s`, republish-last-state-on-reconnect, LWT
  online/offline retained, and the /meta body shape (consumers parse these).
- All timing constants: frame gap formula (1.5 frame times, 20 ms min),
  400 ms reply wait, 2000 ms settle, 3 ladder attempts, 0.3° tolerance,
  10 s arm freshness, 250 ms jog hold, 2 s caller timeout, 2 s reopen
  cooldown, 5 s MQTT retry, 16 pending cap.
- The set ladder phase machine, the cancel-on-stop/jog/new-set semantics,
  queue cancellation on all-stop, and the write-failure unwind.
- Self-check disable (preset set 105) at start; self-test (preset call 125)
  guarded as in §8.4.
- Config key names and defaults (operators' config.toml files carry them),
  seed-once deploy, explicit-path-missing-is-fatal, password handling
  (env preferred, flag never).
- Deployment shape: interactive Windows console app on the shack PC, no
  service — this is inseparable from the safety model (a human must be at
  the keyboard to arm).

**Free to change (implementation detail):**

- The Go/Bubble Tea technology stack, channel architecture, goroutine
  layout, and the specific value-receiver patterns of the TUI — provided the
  single-serializer engine invariant and the no-timer-polling rule survive
  in equivalent form.
- The 2 s blocking-Call for the TUI e-stop could be any mechanism that
  guarantees the stop is actually dequeued, not merely submitted.
- The hold-to-move auto-repeat mechanism (any key-release detection that
  reliably stops the head within `jog_hold_ms` of release is acceptable; a
  real key-release event would be *better* than the timer).
- Log rendering, help text, colors, and the 50-column/2000-line log limits
  (cosmetic; the *content* of wire-log lines is operator-facing but not
  machine-consumed).
- The mock-head binary — anything that reproduces the three quirks for
  testing suffices.
- The dedup-by-JSON-marshal trick — any change-detection that excludes
  timestamp and readback age and suppresses identical republishes works.
- The once-per-process vs once-per-reconnect self-check disable (§9.1) — a
  reimplementation SHOULD send it after every successful (re)open, which
  would *improve* on the current behavior; confirm against head firmware
  first.