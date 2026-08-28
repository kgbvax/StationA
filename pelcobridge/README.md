# pelcots

A terminal tool and headless daemon for driving and observing a **Pelco-D /
Pelco-P** PTZ / rotator. It can talk to the unit over a directly attached serial port, a
TCP serial bridge, or a built-in **simulator** (no hardware), and it can act as
a **network rotator controller** that external tracking software drives over
the **Hamlib rotctld** protocol.
Optional **cable-wrap protection** guards infinite-azimuth rotators against
over-winding.

## Status
WIP, mostly untested 

## Build

```sh
go build -o pelcots .
```

Requires Go (see `go.mod` for the version).

## Run modes

### Interactive TUI (default)

```sh
./pelcots
```

A Text UI showing live azimuth/elevation readback, a TX/RX trace, and the
connection / server / cable-wrap state. **Local motion is hold-to-move**:
the unit moves only while a key is held and stops as soon as you release it, so
the TUI never leaves the rotator moving unattended.

Keys:

| Key | Action |
| --- | --- |
| `tab` / `shift+tab` | move between editable fields |
| type + hold `enter` | go to the typed azimuth/elevation |
| hold `g` | go to target · hold `h` | go home (0,0) |
| hold arrows / `k` `j` | jog pan/tilt |
| `t` | toggle turbo jog speed |
| `space` | stop all motion |
| `m` | toggle transport (serial ⇄ tcp) · `r` | (re)connect |
| `o` | toggle the rotctld inbound server |
| `w` | toggle cable-wrap protection · `z` | re-zero the wind accumulator |
| `a` | zero azimuth (current direction reads as 0°) |
| `q` / `ctrl+c` | quit (settings are saved) |

> Note on hold-to-move: terminals don't report key-release events, so a held
> key is detected via the OS key-repeat stream. Set your keyboard's repeat rate
> reasonably fast for smooth holds; a brief tap moves for ~300 ms then stops.

### Daemon (headless network controller)

```sh
./pelcots -d
```

No TUI, no console required. Inbound control servers drive the rotator directly
(motion is not gated by key presses — it is the explicit, opt-in external
control path). At least one control server must be enabled in the config.
`SIGINT`/`SIGTERM` stops motion and persists the cable-wind state.

## Connection

- **Serial** (default): a directly attached device, 8N1 at the configured baud.
- **TCP bridge**: a serial-to-TCP bridge (ser2net / esp-link / USR-TCP232) that
  exposes the raw Pelco-D byte stream. The bridge owns the real serial
  parameters; `baud` is informational in TCP mode.
- **Simulator** (`sim`): an in-memory emulator — nothing is opened on the host.
  Absolute moves snap to the target, jogs step the position by `sim.jog_step`
  per frame (so the cable-wrap unwrap path still accumulates travel), and
  position queries are answered from that in-memory state. Use this to drive
  the inbound rotctld control server and exercise the sat-tracking
  integration **without a rotator attached**:

  ```sh
  ./pelcots -d -transport sim      # headless; enable a control server in pelcots.yaml
  ```

Switch live in the TUI with `m` (transport) and `r` (reconnect), or set it at
launch with `-transport` / `-tcp` / `-transport sim`.

### PTZ self-check (disable)

Some PTZs (e.g. the 303Z/3050DZ) run a self-check sweep on power-up (and after a
factory reset) that rotates the unit through its range. With
`self_check.disable: true` (the default), pelcots sends **set preset 105**
(`FF <addr> 00 03 00 69 <chk>`) once per successful connect to turn the
self-check *function* off, so it does not run on subsequent power-ups. The
setting is persistent on the unit; re-sending on each (re)connect is idempotent.
Set `self_check.disable: false` to opt out. In sim mode the command is a
harmless no-op (the emulator ignores presets).

## Self-healing link

The link is **self-healing**: if the device is unplugged, the bridge drops, or a
write stalls, the engine tears the connection down and automatically retries
every 200 ms until it reconnects — in both the TUI (status shows
`reconnecting…`) and the headless daemon. No manual `r` is needed; it also
applies before the first successful connect, so starting with the device absent
just waits for it to appear. Repeated failures are logged once (not every
retry) to keep the trace clean. The `warn`-level interplay with `loglevel` is
unchanged. On every (re)connect the engine issues an **all-stop** — Pelco-D jog
motion has no auto-stop, so a dropped link leaves the unit moving; the stop
halts any in-flight motion (and abandons a cable-wrap unwrap that was in
progress, rather than resuming it against a stale wind accumulator).

## Inbound control protocols

The server is **disabled by default** and **binds to `127.0.0.1`** (set
`control.bind` to expose it on a LAN). Azimuth maps to pan, elevation to tilt.

**Hamlib rotctld** (default port 4533, newline-delimited): `p` (get position),
`P <az> <el>` (set position), `S` (stop), `_` (get info), `q` (quit).

```sh
rotctl -m 2 -r 127.0.0.1:4533 P 123 45   # move to az=123 el=45
rotctl -m 2 -r 127.0.0.1:4533 p          # read position
```

## Cable-wrap protection

For rotators with infinite azimuth rotation, pelcots tracks a signed
**wind accumulator** integrated from the position readback (each move assumed to
take the shortest path). When an absolute move would push the accumulator beyond
the configured `± limit`, pelcots drives the **long way round** under
closed-loop control (a full-rotation unwrap) instead of over-winding; if no
route stays within the limit the move is refused.

Example: a `± 270°` limit permits a half-turn plus 90° of over-rotation in each
direction. The wind state is **persisted** across runs (the cable stays wound
when the app is closed); press `z` (or zero it in config) after manually
centering the cable.

## Configuration (`pelcots.yaml`)

Settings live in `./pelcots.yaml` (override the path with `-config`). The file
is created/updated automatically (auto-saved on quit; periodically in daemon
mode). Defaults are used when it is absent.

```yaml
transport: serial            # serial | tcp | sim
serial:
  port: /dev/tty.usbmodemXXXX
  baud: 2400
tcp:
  address: 192.168.1.50:4001
sim:                         # in-memory emulator (transport: sim), no hardware
  start_pan: 0                # initial azimuth, degrees
  start_tilt: 0               # initial elevation, degrees
  jog_step: 5                 # degrees of travel per jog frame
self_check:                  # PTZ self-check (power-up sweep) gating
  disable: true              # send set-preset-105 on connect so it does not run (303Z/3050DZ)
addr: 1                      # Pelco-D camera address (1-255)
protocol: d                  # wire protocol SENT: d (Pelco-D, default) | p (Pelco-P);
                             # the same address byte is used in both — do not subtract one.
                             # Receiving is always adaptive (either protocol accepted).
log: pelcots.log             # TX/RX trace file ("" disables)
log_level: info              # error | warn | info | debug | trace
az_offset: 0                 # azimuth zero offset: physical azimuth that reads as 0° (degrees)
tilt_invert: false           # invert elevation for an upside-down mount (logical = 90 - physical)
control:
  bind: 127.0.0.1            # listen address for inbound servers
  rotctld:
    enabled: false
    port: 4533
wrap:
  enabled: false
  limit: 270                 # ± degrees of permitted wind
  accumulated: 0             # signed wind state, persisted across runs
```

### Logging

`log_level` (or `-loglevel`) controls how much detail is recorded, both in the
TUI trace panel and in the `log` file / daemon stderr. Each level includes the
ones above it:

| Level | Adds |
| --- | --- |
| `error` | failures that abort an operation (send/connect/server-start errors) |
| `warn` | recoverable problems (read errors, TX while disconnected, blocked over-wrap) |
| `info` | operational milestones (connect, server start/stop, cable unwind) *(default)* |
| `debug` | per-frame TX and decoded RX position readback |
| `trace` | raw bytes and unrecognized frames |

`info` is the default — operational events only. Use `debug` or `trace` for
live diagnostics of the TX/RX traffic.

## Flags

Flags override the corresponding config values for that run:

| Flag | Meaning |
| --- | --- |
| `-config <path>` | settings file (default `pelcots.yaml`) |
| `-transport serial\|tcp\|sim` | outbound transport (`sim` = in-memory emulator, no hardware) |
| `-port <dev>` | serial device path |
| `-baud <n>` | serial baud rate |
| `-tcp <host:port>` | TCP bridge address (implies `-transport tcp`) |
| `-addr <1-255>` | Pelco-D camera address |
| `-protocol d\|p` | wire protocol sent to the unit: Pelco-D (default) or Pelco-P; RX is always adaptive |
| `-log <path>` | TX/RX trace file |
| `-loglevel <level>` | log verbosity: `error`\|`warn`\|`info`\|`debug`\|`trace` |
| `-d` | run headless as a network controller |

## Safety

Inbound control servers are off by default and bound to localhost. They move the
rotator on remote request — only expose them on a trusted network, and review
the cable-wrap limit before enabling unattended/daemon operation.

## License

GPLv3 — see [`LICENSE`](LICENSE). `SPDX-License-Identifier: GPL-3.0-or-later`.
