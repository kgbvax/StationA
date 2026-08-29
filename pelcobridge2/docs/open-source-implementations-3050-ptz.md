# Open-source implementations for PTS-303Z / 3050(DZ) PTZ control & readback

Research, 2026-08-27. The Mühle UHF sat rotator is a PTSCCTV 303Z/3050DZ-class
outdoor pan/tilt head driven over RS-485 with Pelco-D (manual claims Pelco-D/P
adaptive). This document surveys open-source projects that control or read the
same hardware family, and what each does differently from `pelcobridge`/`ptest`.

## Directly targeting the same hardware

### [belovictor/pelco_d_rotator](https://github.com/belovictor/pelco_d_rotator) — Python, exact 3050(DZ) match
- rotctld-compatible TCP server (port 4533, `p`/`P`/`dump_state` subset) bridging
  gpredict/rotctl to a **3050(DZ) RS-485 head**; the author uses the same
  AliExpress unit. Default baud **9600**, address configurable.
- **Control approach: no absolute positioning.** Because "most PELCO-D devices
  are not able to respond to commands while executing this position command",
  the bridge continuously polls current position and issues *relative jog*
  commands computed from the delta to target — a closed loop built on jog.
  This is the same conclusion `pelcobridge` reached for the 303Z (the head
  ignores `0x4B`/`0x4D` SetPan/SetTilt; goto is closed-loop jog).
- Pelco-D framing based on the [jn0 gist](https://gist.github.com/jn0/cc5c78f4a0f447a6fb2e45a5d9efa13d)
  (full command set incl. `0x51/0x53` queries and `0x59/0x5B` responses;
  Python-2-era code, has known bugs).
- Hardware mods for antenna use: remove one pan stop bracket → full 360° az;
  flip both tilt stop brackets outside → full 90° el.
- No license file visible.

### [AaronJackson/pts303z-rotator](https://github.com/AaronJackson/pts303z-rotator) — Node.js, exact PTS-303Z match
- "Antenna azimuth/elevation rotator using the PTS-303Z off AliExpress."
  `PelcoD.js` + `hamlib.js` (rotctld-style) + 3D-printed mounts. MIT, 2024.
- Its `PelcoD.js` implements jog (`0x04/0x02/0x08/0x10`), stop, `queryPan`
  (`0x51`) and `queryTilt` (`0x53` — note: the spec-standard tilt response
  opcode is `0x5B`; our unit does answer `0x53` queries with `0x5B` responses).
  Readback mapping constants: `FULL_RIGHT = 35000`, `FULL_UP = 9222`, with
  hand-tuned linear rescales for a ~35°-swing pan center and "up max 23,
  down max 29" tilt — i.e. **the author also had to calibrate readback words
  to mechanical reality per unit**, consistent with our raw-encoder tilt
  calibration (raw = raw_at_0 + 355.878·elev).
- Checksum in that file is sum-modulo-**100** (nonstandard; spec is mod 256) —
  a bug to be aware of if copying from it.

## Pelco-D head, different readback strategy

### [genepool99/Peltrack](https://github.com/genepool99/Peltrack) — Python, MIT, 2025
- Web UI (Flask/Socket.IO, port 5000) + **rotctld/EasyComm II TCP server on
  4533** for Gpredict. Drives a Pelco-D head over RS-485 (CH340/FTDI).
- **Fully open-loop, dead-reckoned**: "most Pelco-D heads don't report absolute
  position" — no `0x51/0x53` queries at all. Position is integrated from
  calibrated °/s with a `TIME_SAFETY_FACTOR` (0.985), stiction/breakaway
  pulses near stops, near-stop speed reduction, overshoot-and-settle at zenith,
  and a calibration CLI that writes measured speeds to `config.json`.
- Motion serialized with a global lock, axis-staggered, cancel-aware via
  chunked sleeps; soft limits in `limits.json`.
- Interesting contrast: we get real readback from the 303Z (one query per
  tick); Peltrack trusts dead reckoning instead — a fallback worth remembering
  if readback becomes unreliable.
 

## Protocol libraries / generic controllers

- [jn0's `pelco_d.py` gist](https://gist.github.com/jn0/cc5c78f4a0f447a6fb2e45a5d9efa13d) —
  the canonical open-source Pelco-D command table (spec TF-0002 v2.1): jog,
  presets, aux, patterns, `0x4B/0x4D/0x4F` set position, `0x51/0x53/0x55`
  queries with `0x59/0x5B/0x5D` responses, general (4-byte) and extended
  (7-byte) response parsers. Python 2, buggy as-is, but the opcode reference
  is the value. Several projects above derive from it.
- [longxianlei/PTZCameraController](https://github.com/longxianlei/PTZCameraController) —
  C++ RS-485 Pelco-D jog driver (on/off/pan/tilt demo), no position readback.
- Generic Pelco-D/P libraries exist for most languages; none handle the
  303Z's tilt raw-encoder readback — that quirk is unit-firmware-specific.

## Ecosystem context
- Adjacent DIY antenna-rotator projects (not Pelco, same role):
  [deltanu/dnu-AntCtrl](https://github.com/deltanu/dnu-AntCtrl) (ESP32 + absolute
  encoders, GPL-3.0), [evanwuren/Rot32](https://github.com/evanwurden/Rot32)
  (ESP32-S3 + LSM303, rotctld over Wi-Fi), [F4DIW/F4DIW-rotator](https://github.com/F4DIW/F4DIW-rotator),
  [ricovangenugten/rotator-uc](https://github.com/ricovangenugten/rotator-uc)
  (EasyComm II + encoders). Useful for encoder/offset/calibration patterns.

## Takeaways relevant to our elevation-readback work

1. **Everyone hits the same wall**: the head can't answer queries while (or
   right after) moving, and absolute set commands are ignored by this class of
   firmware. belovictor solves it exactly like `pelcobridge` (poll ↔ jog
   closed loop); Peltrack abandons readback entirely and dead-reckons.
2. **Per-unit readback calibration is normal.** pts303z-rotator hand-tunes
   `FULL_RIGHT`/`FULL_UP` and swing offsets in code; we do it with
   `tilt_cal` (raw_at_0 + scale). Nobody else documents the raw-encoder-count
   tilt encoding — our finding that tilt readback is `raw_at_0 + 355.878·elev`
   (not hundredths) appears to be novel documentation.
3. **The rotctld port-4533 surface is the ecosystem standard**; gs232 and
   PstRotator UDP are secondary. `pelcobridge` already implements all three.
4. No open-source implementation speaks **Pelco-P** to this family despite the
   manual's "D/P adaptive" claim — the TX-protocol option added to
   `pelcobridge`/`ptest` has no known prior art to compare against; trace-level
   verification against the real head is the only check available.

## Sources

- https://github.com/belovictor/pelco_d_rotator
- https://github.com/AaronJackson/pts303z-rotator
- https://github.com/genepool99/Peltrack
- https://github.com/awatchar/satfinder-pass-simulator
- https://gist.github.com/jn0/cc5c78f4a0f447a6fb2e45a5d9efa13d
- https://github.com/longxianlei/PTZCameraController
- https://github.com/Hamlib/Hamlib/wiki/Supported-Rotators
- http://www.ptscctv.com/en/prodetails/142/393.html (PTS-3050DZ-Y product page:
  Pelco-D/P, RS-485, 1200–9600 baud, addr 0–64, 0–15°/s az, 0–2°/s el)