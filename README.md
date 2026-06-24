# pelcots

A terminal tool and headless daemon for driving and observing a **Pelco-D**
PTZ / rotator. It can talk to the unit over a directly attached serial port or a
TCP serial bridge, and it can act as a **network rotator controller** that
external tracking software drives over the **Yaesu GS-232** and **Hamlib
rotctld** protocols. Optional **cable-wrap protection** guards infinite-azimuth
rotators against over-winding. 

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
| `y` / `o` | toggle the GS-232 / rotctld inbound server |
| `w` | toggle cable-wrap protection · `z` | re-zero the wind accumulator |
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

Switch live in the TUI with `m` (transport) and `r` (reconnect), or set it at
launch with `-transport` / `-tcp`.

## Inbound control protocols

Both servers are **disabled by default** and **bind to `127.0.0.1`** (set
`control.bind` to expose them on a LAN). Azimuth maps to pan, elevation to tilt.

**Hamlib rotctld** (default port 4533): `p` (get position), `P <az> <el>` (set
position), `S` (stop).

```sh
rotctl -m 2 -r 127.0.0.1:4533 P 123 45   # move to az=123 el=45
rotctl -m 2 -r 127.0.0.1:4533 p          # read position
```

**Yaesu GS-232A** (default port 4000, CR-terminated): `C` / `B` / `C2`
(query azimuth / elevation / both), `W aaa eee` and `M aaa` (move), `S`/`A`/`E`
(stop), `R`/`L`/`U`/`D` (continuous jog).

```sh
printf 'W123 045\r' | nc 127.0.0.1 4000  # move to az=123 el=45
printf 'C2\r'       | nc 127.0.0.1 4000  # -> AZ=123 EL=045
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
transport: serial            # serial | tcp
serial:
  port: /dev/tty.usbmodemXXXX
  baud: 2400
tcp:
  address: 192.168.1.50:4001
addr: 1                      # Pelco-D camera address (1-255)
log: pelcots.log             # TX/RX trace file ("" disables)
log_level: info              # error | warn | info | debug | trace
control:
  bind: 127.0.0.1            # listen address for inbound servers
  gs232:
    enabled: false
    port: 4000
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
| `-transport serial\|tcp` | outbound transport |
| `-port <dev>` | serial device path |
| `-baud <n>` | serial baud rate |
| `-tcp <host:port>` | TCP bridge address (implies `-transport tcp`) |
| `-addr <1-255>` | Pelco-D camera address |
| `-log <path>` | TX/RX trace file |
| `-loglevel <level>` | log verbosity: `error`\|`warn`\|`info`\|`debug`\|`trace` |
| `-d` | run headless as a network controller |

## Safety

Inbound control servers are off by default and bound to localhost. They move the
rotator on remote request — only expose them on a trusted network, and review
the cable-wrap limit before enabling unattended/daemon operation.

## License

GPLv3 — see [`LICENSE`](LICENSE). `SPDX-License-Identifier: GPL-3.0-or-later`.
