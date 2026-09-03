# Salvage: pelcobridge2.md
> Extracted from PRD/03-components/pelcobridge2.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [protocol] Pelco-D frame format and opcode table (device wire, not MQTT)

A 7-byte frame: `FF addr cmd1 cmd2 d1 d2 sum`, where:

- `FF` (hex) is the fixed start byte.
- `addr` is the head's DIP-switch address (**default 1**, configurable). The
  operator sets it by hand.
- `cmd1` is always `0x00` for every function this head documents — the action
  lives in `cmd2`.
- `d1`, `d2` are data bytes (speed byte, or halves of a 16-bit position word).
- `sum` = `(addr + cmd1 + cmd2 + d1 + d2) & 0xFF` — an 8-bit additive checksum.

Serial: 8N1, default baud **2400** (vendor documents the head family at
1200–9600; the bench link runs 2400). The configured port may also be the
string `tcp:host:port` (connects to the mock head over TCP; enables bench
testing on Windows, which has no pseudo-terminal facility).

Opcodes (cmd2 values) — normative table:

| cmd2 | Name | Meaning | Data bytes |
|---|---|---|---|
| `0x00` | stop | Stop all movement. | 0, 0. |
| `0x02` | jog right | Pan jog. | Speed byte in `d1`. |
| `0x04` | jog left | Pan jog. | Speed byte in `d1`. |
| `0x08` | jog up | Tilt jog: raises the native tilt. The tilt word runs opposite to elevation, so this action lowers elevation. | Speed byte in `d2`. |
| `0x10` | jog down | Tilt jog: lowers the native tilt. The tilt word runs opposite to elevation, so this action raises elevation. | Speed byte in `d2`. |
| `0x4B` | set pan | Absolute pan set. It lands only on a quiet line. | Target word. |
| `0x4D` | set tilt | Absolute tilt set. Same quiet-line rule. | Target word. |
| `0x51` | query pan | Pan angle request. | None. |
| `0x53` | query tilt | Tilt angle request. | None. |
| `0x59` | pan response | Pan position reply. | Position word. |
| `0x5B` | tilt response | Tilt position reply. | Position word. |
| `0x03` | preset set | "Set preset N" / extended selector. | N in `d2`. |
| `0x07` | preset call | "Call preset N" / extended selector. | N in `d2`. |

- **Speed byte**: jog speed lives in `d1` for pan-axis operations and in `d2`
  for tilt-axis operations. Documented range `0x00`–`0x3F` (0–63). Default jog
  speed **`0x12`** (18).
- **Position encoding**: `d1` is the high byte and `d2` the low byte of a
  big-endian 16-bit word. **degrees = word / 100** (hundredths of a degree).
  Word range 0–655.35. Confirmed on the bench 2026-08-28 for the pan reply
  (`0x59`) and 2026-08-30 for the tilt reply (`0x5B`).
- **Tilt word runs opposite to elevation** (bench 2026-08-30): the word speaks
  the head's *native* tilt. Native 0 = zenith (mechanical home), native 90 =
  horizon; `elevation = 90 − native tilt`. Jog opcodes speak the native scale
  too. The component converts at the wire boundary; user-facing surfaces report
  true elevation. (Inversion confirmed in code: `pelco.TiltToEl`/`ElToTilt`,
  engine `dirOpcode` swaps the tilt pair.)
- **Target shaping**: a pan set wraps its argument into [0, 360). A tilt set
  **clamps** its argument to [0, 90]. Overshooting tilt travel is the dangerous
  direction. Every entry path must reject NaN and infinity targets before
  encoding.
- Two preset selectors are load-bearing: **preset set 105 (`0x69`)** disables
  the head's periodic self-check (sent once at start and after every successful
  reopen); **preset call 125 (`0x7D`)** is the factory self-test — it restores
  defaults and re-homes the head (the head swings physically; can rip cables).

## [protocol] RX assembler rules and the frame-gap bound

The receive path is a byte-level resynchronizer. Two load-bearing rules:

1. **Sync on the start byte**: `0xFF` → expect a 7-byte D frame. `0xA0` →
   expect an 8-byte P frame. Any other byte collects into a noise run. The
   assembler validates the matching checksum; on failure it drops the leading
   byte and resumes scanning one byte later. Noise runs are emitted to the wire
   log **before** the frame that follows them, in wire order, capped at 256
   bytes per single feed (a wrong-baud flood stays readable).
2. **A time bound stops incomplete frames.** The reference has no independent
   receive-gap timer; the reply-wait expiry (default 400 ms) flushes a held
   partial frame as a "partial" noise event. Without this bound, a truncated
   reply merges with the next reply and the merge can produce a checksum-valid
   frame with a **fabricated position word**. The bench observed this failure;
   it is the reason this rule exists.

The bound for any standalone decoder: **1.5 frame times at the configured
baud** (frame bits = 7 bytes × 10 bits), minimum 20 ms — ≈43.75 ms at 2400
baud. This is the transmit frame-gap formula (`IdleGap(baud)`); the ptest
research marked it as contract for any protocol decoder for this head.

Wire-log rendering per receive event: a raw hex line, a decoded-fields line
(`raw: addr=.. cmd1=.. cmd2=.. d1=.. d2=..`), a checksum line
(`word=XXXX (n)  chk=XX ok|BAD (want XX)`), and for position frames a
`pan %.2f°` / `tilt %.2f°` line. Each line is truncated at 50 columns; the log
pane does not wrap, so long lines lose their tail silently (accepted cosmetic).

## [protocol] Measured head behavior — link-level facts (bench, normative)

1. **The head is silent on undecodable or wrong-address frames.** It gives no
   error reply. Timeouts are the only failure signal.
2. **There is no connection handshake.** The link is a raw serial line. The
   engine's "connect" ritual is a single disable-self-check frame (preset set
   105) at start and after every successful reopen.
3. **At most one outstanding query.** The engine tracks the expected response
   opcode (`0x59` or `0x5B`) for the single in-flight query and matches
   incoming readbacks against it. A query with no usable answer within the
   reply wait (default **400 ms**) fails with "no valid readback"; the
   assembler then flushes any stalled partial bytes and the engine resumes.
4. **Absolute sets (`0x4B`/`0x4D`) work only on a quiet line.** The head
   ignores an absolute set when another frame is sent too close to it.
   (Resolution of the earlier ptest "head ignores absolute sets" finding,
   bench 2026-08-30: the earlier observation was an artifact of the missing
   quiet-line discipline; the working set ladder confirms it.)
5. **Readback is garbage while a motor runs.** Mid-motion the head returns
   checksum-valid words unrelated to position; no checksum test can filter
   them. The engine refuses user position queries while the head moves; the
   set ladder checks only after the settle window.
6. **The head itself is protocol-adaptive on reception**: measured on the
   bench, it answers in whichever envelope a frame arrives in. The component
   speaks only Pelco-D, so every answer comes back in the Pelco-D envelope.
7. Writes are single complete frames, atomic from the engine's perspective; a
   short write is an error (`short write: N of M bytes`).
8. **Opening the link at startup is fatal on failure.** The process prints the
   error and holds the console open with a "press Enter to exit…" prompt (so a
   double-clicked Windows executable's error is readable), then exits. It must
   not present a UI over a dead link.

## [protocol] Pelco-P framing removed — do not re-add

The component speaks exactly one wire envelope: Pelco-D. It must not send
Pelco-P frames, and its receive assembler must not accept them (an 8-byte P
frame has no `FF` start byte, so it assembles as a noise run). The team
removed P framing on 2026-08-29. Two reasons:

1. The P envelope gave no benefit on this one-head link.
2. The head's Pelco-P addressing is an **unverified assumption**: strict
   Pelco-P gear is zero-indexed (address byte n = unit n+1). A P frame with a
   wrong address goes to a different unit; the head then silently ignores it
   and the operator sees "does not answer Pelco-P".

Do not add P framing again until a bench session checks P addressing on this
head. The device fact that the head answers in the envelope of the received
frame stays true; it has no effect on a D-only component.

## [defect] Auto-reopen gives up permanently after a failed reopen

PRD evidence (§4, §16.5): on a read error the reader dies and the engine
auto-reopens (throttled to one try per 2 s). If the reopen try itself fails,
no reader then runs, so no further read errors arrive to retry on — the
automatic retry stops entirely. Recovery then needs the manual `ctrl+r`
reopen, which always works. Documented as a known deviation; a continuous
throttled retry would be an improvement, not a regression.

Code-check verdict: **still open** — `pelcobridge2/internal/control/engine.go`
`onReadErr` (~line 234): on `reopen()` error it records `"reopen: …"` and
returns without starting a reader, so nothing retriggers it.

## [defect] `state.toml` key name lies

The state file holds exactly one key, `last_offset_deg`. Despite its name it
stores the last **entered true azimuth** (the value the operator typed at the
arm prompt), not the computed offset. The engine uses it only to pre-fill the
next arm prompt; armed state never survives a restart. Behavior is correct;
the name invites a wrong re-implementation. A re-implementation should rename
the key and keep the meaning.

Code-check verdict: **still open** — `pelcobridge2/internal/config/state.go:15`
`LastOffsetDeg float64 \`toml:"last_offset_deg"\``, consumed only as the arm
prefill (`cmd/pelcobridge2/main.go:197-199`).

## [defect] `/meta` min/max hardcoded; rotctld limits unenforced

The `/meta` expose min/max values are **fixed 0–360 (az) and 0–90 (el)** and
do not track the configured `[control] min_az/max_az/min_el/max_el`. Those
config keys feed only the rotctld `\dump_state` advertisement. The engine
enforces neither set on set arguments: a `set_pos 400 95` wraps pan to 40° and
clamps tilt to 90° at the frame layer instead of a range rejection (elevation
outside 0..90 is refused at the intent boundary, but the wrap/clamp is what
the frame layer does). Either enforce the configured limits consistently on
both surfaces or document the wrap/clamp behavior to clients.

Code-check verdict: **still open** — `pelcobridge2/internal/mqtt/slot.go:267`
hardcodes `azMin, azMax := 0.0, 360.0` for the `/meta` expose.

## [defect] `JogSpeed` intent has no engine-level source gate

Unlike Arm/Disarm/SelfTest/SelfCheck, the `JogSpeed` intent has no
engine-level source check (TUI is the only caller in practice). Harmless
today, but the "twice enforced" discipline is not uniform. Adding the gate for
symmetry is an improvement.

Code-check verdict: **still open** — `pelcobridge2/internal/control/engine.go:417`
`case JogSpeedIntent:` clamps and publishes with no `SrcTUI` check.

## [defect] `-port sim` documented but not supported

A package doc in the reference code mentions an in-process simulator mode
(`-port sim`). The shipped binary supports only real serial ports and `tcp:`.
Documentation drift, not behavior.

Code-check verdict: **still open** — `pelcobridge2/internal/simhead/head.go:2`
mentions `-port sim`; `cmd/pelcobridge2/main.go` accepts only real ports and
`tcp:`.

## [decision] MQTT password source — README vs code disagreement

The README says the MQTT password never comes from the config file. The code
does use the `[mqtt] password` key as a fallback when the environment variable
`PELCOBRIDGE2_MQTT_PASSWORD` is empty — a documented accommodation for a
double-clicked Windows executable with no environment. Never a command-line
flag. A re-implementation must pick one contract and document it.

Code-check verdict: **still present** — `pelcobridge2/internal/config/config.go`
`MQTTConfig` (lines ~161-162) falls back to the TOML password when the env
value is empty.

## [decision] Self-check disable cadence — open firmware questions

The engine sends preset set 105 (disable periodic self-check) at process start
and again after every successful port reopen. The model reads `"unknown"`
until the head answers a frame after the preset went out. Two firmware
questions stay open and need bench confirmation:

1. Does the disable survive head power loss?
2. Is re-sending the preset idempotent on the head?

Until the bench answers, the TUI key `c` lets the operator send the disable
frame again at any time without a process restart.

## [decision] rotctld per-client fairness is an open design decision

The rotctld server accepts unlimited concurrent clients. All funnel into the
one 16-slot engine queue. A flood from one client can get everyone else
"engine busy" (the e-stop still cuts through). Per-client fairness/throttling
is an open design decision.

Code-check verdict: **still open** — `pelcobridge2/internal/rotctld/server.go`
accept loop (line ~79) spawns a goroutine per connection with no connection
limit and no per-client throttling.

## [decision] UHF-side MQTT capture gap

The station's MQTT traffic recorder defaults to the HF subtree only. It does
not record the `muehle/uhf/rotator` slot's bus traffic by default. This is an
operational note for debugging, not a component decision.

## [decision] Deployment shape — deliberate deviation from station convention

The component is an interactive TUI, so there is **no service, no auto-start,
no service-manager unit, and no watchdog** (REQ-ROLE-1). A human starts it by
hand whenever the station needs the UHF rotator; arming is a further manual
step inside it. This deployment shape is inseparable from the safety model —
a human must be at the keyboard to arm.

- Target host: shack-pc, a Windows machine at `192.168.1.197` (SSH user
  `iotte`), destination `C:/Users/iotte/pelcobridge2/`.
- Environment variables `PELCO2_HOST` / `PELCO2_USER` / `PELCO2_DEST` override
  host/user/destination in the deploy script.
- The fatal-error path holds the console open with "press Enter to exit…" so
  the operator can read a double-clicked executable's error.

## [unique] Mock head contract (REQ-ROLE-3)

The component ships with a companion mock-head binary (`pelcobridge2-mock`)
that simulates the head over a TCP socket (or pseudo-terminal) and reproduces
the hardware quirks: silence-required absolute sets, garbage readback while
moving, each quirk toggleable. The mock must serve **exactly one client
connection at a time**, like the real RS-485 line: a new connection can take
over only after the previous one closes. A concurrent mock lets two clients
interleave frames, which breaks the bench-testing contract.

## [unique] rotctld edge facts

- **RPRT code meanings** (Hamlib reply convention): `RPRT 0` = success;
  `-9` = rejected/command not allowed (disarmed); `-11` = no usable position
  readback (a moving head counts — readback is garbage mid-motion);
  `-1` = rejected/invalid parameter; `-4` = unknown command;
  `-6` = operation timed out.
- **`RPRT 0` on `set_pos` does not mean "positioned".** It means the engine
  accepted the request. Convergence and failure are observable only through
  `/state.set_status` or the TUI. This is a shape constraint of the rotctld
  protocol, not a defect.
- The parser accepts a decimal comma (`45,5` → 45.5) per Hamlib convention.
- `get_info` returns `"pelcobridge2 · <device name>"` (default
  `pelcobridge2 · UHF Rotator`); Hamlib model 901 = NET_ROTCTL.
- `\dump_state` is the protocol v1 block; gpredict sends it at connection
  open. Min/max come from the configured rotctld limits, `rot_model` fixed at
  901. (Byte-exact body pinned by `internal/rotctld/server_test.go`.)

## [unique] Config zero-sentinel quirk for speed bytes

`[control] jog_speed` values outside 0x00–0x3F get the default (0x12), not a
clamp. A configured value of exactly 0 also gets the default through
engine-level defaulting, because 0 is the TOML zero value and the engine
cannot tell it from unset — the operator cannot configure a jog speed of 0.
(Same sentinel rule for `crawl_speed` 0 → 0x04.)

## [unique] Design lineage

The component evolved from a one-time bench tool (`pelcotest`/`ptest`); the
team used it to reverse-engineer the head's serial behavior, folded the
knowledge into the wire-protocol facts above, and deleted the tool from the
repository on 2026-08-29. It does not need reconstruction; its serial and
assembler invariants live on in `internal/pelco` and the engine. The mock
head's quirks model the bench-observed behavior, not the vendor manual's
idealized behavior.