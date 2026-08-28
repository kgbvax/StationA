# ptest — manual Pelco-D/Pelco-P rotor test TUI

A standalone, deliberately-minimal tool for re-engineering / verifying the
303Z/3050DZ PTZ rotor's serial behaviour. It is intentionally separate from
`pelcobridge/` and shares no code with it.

The **TUI** is strictly manual: nothing is sent on a timer, nothing polls,
nothing acts automatically — every transmitted frame is a keypress. (It has one
receive-side timer, which transmits nothing: see *Receive gaps* below.) The one
automated sequence, the `-sweep` recorder, is a separate non-TUI mode.

The command spec lives in
`../docs/更新云台说明书串口指令.20240327154146056-2.xls`.

## Trust model

**Raw hex first.** Every received frame is printed as a full hex dump plus a
raw field breakdown (addr / cmd1 / cmd2 / data bytes / 16-bit word / checksum).
Only then does an interpretation line appear, and each one is attributed:
`doc:` is the vendor manual's claim (untrusted), `hyp:` is a map *you* supplied
with `-tilt-cal` and is always marked `UNVERIFIED`. ptest never presents a
decode as measured fact.

### The tilt readback: what is known is only what it is NOT

The manual claims position responses carry degrees×100. For **pan** (cmd2
`0x59`) that is true and ptest treats it as such. For **tilt** (cmd2 `0x5B`) the
honest state of knowledge is:

- ❌ **Not hundredths of a degree.** Every real reading renders as an angle far
  outside the head's 0–90° travel (e.g. `word=57B8` → "224.56°").
- ❌ **Not a linear raw-encoder-count map of elevation either.** A model
  `raw = raw_at_0 + 355.878·elev` was fitted to bench observations and looked
  convincing, but it was **re-checked on the bench 2026-08-27 and contradicted:
  elevation does not appear in the tilt word.** Whatever `0x5B` carries, it is
  not a function of elevation alone.
- ❓ **Meaning unknown.** ptest therefore asserts nothing.

Note this is separate from, and worse than, the known garbage-while-moving
behaviour below: even at rest the word has no established meaning.

So a tilt reply is presented as raw bytes plus the manual's claim, explicitly
flagged:

```
12:01:11.201 RX FF 01 00 5B 57 B8 6B
   raw: addr=01 cmd1=00 cmd2=5B d1=57 d2=B8
   word=57B8 (22456)  d1=87 d2=184  chk=6B ok
   doc: tilt resp, word/100 = 224.56°
        └ impossible: head travels 0..90°
        meaning of this word is UNKNOWN
   Δ prev tilt: +3558 counts
```

The **`Δ prev tilt`** line is the useful part, and it is pure observation with
no model attached. It is how you find out what the word actually tracks:

- `UNCHANGED` across a real tilt move ⇒ the word is not a position readback.
- A delta that does not scale with the size of the move ⇒ no linear map exists.
- A delta that tracks something else (time, pan, motor run duration) ⇒ chase that.

To *test* a map you suspect, pass `-tilt-cal raw_at_0,raw_at_90`. It is **off by
default**, and when set the log labels the reading `hyp:` and `UNVERIFIED` so it
can never be read back as a measurement:

```
   hyp: raw 38470 → 45.00° (-tilt-cal)
        UNVERIFIED: 355.878 counts/°
```

**Two things that will confound a tilt experiment:**

- The head **ignores** the absolute set-position opcodes `0x4B`/`0x4D`
  (confirmed live: zero movement for minutes after each set, and every
  third-party controller of this rotor family reports the same). So
  set-then-read-back cannot be used to probe the encoding. Jog, stop, then query.
- While the tilt motor is **running**, the readback is a continuous stream of
  **valid-checksum garbage**. It is at best trustworthy only once the motor has
  halted. No checksum test can filter this; `stop` first.

## Usage

```bash
ptest -list                 # list serial ports
ptest -port /dev/tty.usbserial-XXXX -addr 1
ptest -port COM3            # Windows
ptest -port /dev/tty.usbserial-XXXX -baud 9600
ptest -port /dev/tty.usbserial-XXXX -p          # start in Pelco-P TX mode
ptest -port /dev/tty.usbserial-XXXX -tilt-cal 22456,54485   # test a hypothesis
```

8N1. `-baud` defaults to 2400; the 303Z/3050DZ family is documented for
**1200–9600** and third-party controllers of the same head default to 9600, so
if the RX pane shows only unframed bytes, try another rate. `-addr` is the
Pelco-D address (default 1; the doc's DIP range is 0–64).

## Sweep recorder — `-sweep`

The one automated sequence, deliberately outside the TUI so the interactive tool
stays strictly manual. It exists because the meaning of the tilt word is unknown:
finding out what it tracks needs a recorded series of known mechanical moves
paired with the raw bytes that came back.

```bash
ptest -port /dev/tty.usbserial-XXXX -sweep up   -sweep-out tilt-up.csv
ptest -port /dev/tty.usbserial-XXXX -sweep down -sweep-out tilt-down.csv
```

One step of the loop:

```
TX jog <dir> at -sweep-speed   → wait -sweep-post-tx
   (motor runs so total on-time == -sweep-move)
TX stop                        → wait -sweep-post-tx
wait -sweep-settle             (readback is only trustworthy once halted)
TX tilt query                  → wait -sweep-post-tx
read the reply, append the raw frame to the CSV
```

It repeats until the tilt word has been **identical for `-sweep-stable`
consecutive readings** — the head has hit a mechanical stop, or the word does not
track the move at all. Both are results worth having.

| Flag | Default | Meaning |
|---|---|---|
| `-sweep` | — | `up` or `down`; enables sweep mode |
| `-sweep-out` | `tilt-sweep.csv` | CSV path |
| `-sweep-move` | `1s` | motor-on time per step |
| `-sweep-settle` | `200ms` | wait after halting before querying |
| `-sweep-post-tx` | `50ms` | pause after **every** transmitted frame |
| `-sweep-reply-wait` | `2s` | per-step reply timeout |
| `-sweep-stable` | `3` | consecutive identical readings that end the sweep |
| `-sweep-max-steps` | `200` | safety cap |
| `-sweep-speed` | `0x20` | tilt jog speed byte (`00`–`3F`) |

The CSV is raw data with no decode applied:

```
step,iso_time,elapsed_ms,dir,motor_on_ms,reply_ms,tx_hex,rx_hex,chk_ok,
word_dec,word_hex,d1_dec,d2_dec,delta_counts,note
1,2026-08-27T23:35:17.695+02:00,0,up,301,51,FF 01 00 53 00 00 54,
  FF 01 00 5B 57 B8 6B,true,22456,57B8,87,184,,
```

`motor_on_ms` is measured, not assumed, so you can correlate the word against
actual motor run time. A step with no reply leaves the value columns **empty**
rather than writing a misleading `0`, and records why in `note`; unframed bytes
and frames with other opcodes are noted there too, so nothing seen on the wire
is dropped from the record. The file is flushed after every step, so a sweep you
interrupt keeps its data.

**Safety:** a `stop` is always transmitted before the process exits, including on
Ctrl+C (which also flushes and closes the CSV). The rotator is never left jogging.

## Pelco-D vs Pelco-P

The manual calls the unit "Pelco-D/Pelco-P adaptive": it detects the protocol
per received frame (start byte `0xFF` vs `0xA0`) and answers in the protocol
the frame arrived in. The command set is identical — the same opcodes
(`0x53` tilt query, `0x51` pan query, `0x4B`/`0x4D` set, `0x59`/`0x5B`
responses) ride in both envelopes:

| | Pelco-D (default) | Pelco-P |
|---|---|---|
| frame | `FF addr c1 c2 d1 d2 sum` (7 B) | `A0 addr c1 c2 d1 d2 AF xor` (8 B) |
| checksum | additive sum of bytes 2–6 | XOR of bytes 1–7 |
| address byte | the DIP address code | **assumed the same code — UNVERIFIED** |

`p` toggles the TX envelope (from the menu or log pane; it is ordinary text in
the input line). RX is always adaptive: both envelopes are decoded no matter
the TX mode.

⚠️ The Pelco-P address convention is an **assumption**, not a measurement.
Strict Pelco-P gear is zero-indexed (CommFront's worked example addresses
camera 1 as `0x00`); this unit is assumed not to be. If it *is* zero-indexed,
every P frame ptest sends is addressed to unit *n+1* and is silently ignored —
which would look exactly like "the unit does not answer Pelco-P". Test it with
raw hex entry: send the same query with the address byte and with address−1.

Raw hex entry accepts 7 bytes (D) or 8 bytes (P) and is sent exactly as
typed — never re-framed. Each byte must be two hex digits.

## Receive gaps

The assembler holds an incomplete frame only until the next receive gap
(~1.5 frame times at the configured baud), then reports the stalled bytes as
`?? partial frame (gap)`. Without that bound a truncated reply stayed buffered
and merged with the next reply — whose `0xFF` start byte lands exactly where
the lost checksum byte was. Whenever the lost byte happened to be 0xFF the
merged window passes the additive checksum, so ptest emitted a
**checksum-valid frame carrying a fabricated position word** and reported it as
`chk ok` while the genuine reply was discarded. Rejected bytes are likewise never withheld; a corrupted
reply is reported as `?? unframed` rather than vanishing.

### Keys

| Key | Action |
|-----|--------|
| ↑/↓ or j/k | select command in the left menu |
| Enter | send (parameterless commands) / focus input line |
| Tab | cycle menu → input → log (abandons a pending parameter) |
| Esc | cancel pending parameter |
| p | toggle TX framing Pelco-D ↔ Pelco-P (menu/log pane only) |
| g/G or home/end | first / last entry, or top / bottom of the log |
| pgup/pgdown, ↑/↓ | scroll log (when log pane focused) |
| Ctrl+R | reopen the serial port after a read error |
| Ctrl+L | clear log |
| Ctrl+C / Ctrl+Q | quit |

Commands 42/44 (angle set) ask for degrees — use a decimal point, not a comma;
trailing junk is rejected rather than silently truncated. "raw frame" lets you
send 7 (or 8) arbitrary hex bytes.

**Destructive commands ask y/n first**, and the check is on the *built frame*,
not the menu label: every "NN call" entry in the doc sheet is really
preset-call NN, so `preset call 125` is byte-identical to
"40 defaults+selftest" (restore defaults + self-test — a self-test re-homes the
head, moving whatever zero the readback is referenced to) and `preset call 120`
clears all presets.
Both prompt.

If the USB-serial adapter drops and re-enumerates, the header turns red with
`RX DEAD`; **Ctrl+R** reopens the port and restarts the reader.

## Build

```bash
go build ./...        # host
GOOS=windows go build -o ptest.exe .          # cross-compile for Windows
GOOS=windows go build -o ptest-mock.exe ./cmd/ptest-mock
```

## Loopback testing without hardware

`cmd/ptest-mock` is a canned rotor. Its tilt reply is a **raw 16-bit value with
no model attached** — it cannot model an elevation nobody can decode — while pan
really is hundredths and is modelled:

```bash
ptest-mock -port /dev/ttys00A                     # tilt word 8E90, observed live
ptest-mock -port /dev/ttys00A -tilt-word 22456    # any value you want to see
ptest-mock -port /dev/ttys00A -pan-deg 299.99
ptest-mock -port /dev/ttys00A -doc-mode           # the manual's example frames
```

`-doc-mode` reproduces the manual's `FF 01 00 5B 1F 3F BA` / `FF 01 00 59 75 2F FE`
examples. It is not the default because the manual's tilt value is one the real
head is not observed to emit.

macOS/Linux:

```bash
socat -d -d pty,raw,echo=0 pty,raw,echo=0    # prints the two pty names
go run ./cmd/ptest-mock -port /dev/ttys00A
go run . -port /dev/ttys00B
```

Windows: pair two ports with com0com and use those.
