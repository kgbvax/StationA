# pelcobridge2

UHF rotator console for the stationa monorepo — a **TUI application** (not a
daemon) that drives the PTS-303Z/3050DZ Pelco-D/P pan/tilt head over RS-485,
and serves it to hamlib clients as a **rotctld server** (`-m 901`).

Design evolved from `../pelcotest/ptest` (bench TUI) — its serial/assembler
invariants and TUI disciplines are kept; the daemon-shaped predecessor
`pelcobridge` was deleted.

## Safety model (read before use)

- **Disarmed at every start.** Armed state is never persisted.
- **Arming is manual and TUI-only.** There is *no code path* from rotctld or
  MQTT to the arm intent. Before arming you must enter the **true azimuth**
  the head is physically pointing at; this sets the offset. The last entered
  value is kept in `state.toml` as a *prefill only* — you confirm or correct
  it each run.
- **Motion intents are refused while disarmed.** The all-stop (`SPACE`/`ESC`)
  always works, from every source, in every state.
- **Self-test (preset call 125) re-homes the head and can rip cables** if it
  is pointed wrong. It is refused while armed and requires a two-stage
  confirmation: `y`, then typing `RIPCABLES`.
- **No timer polling.** The engine is purely event-driven; the only timers are
  one-shots that *release gates* (frame gap, reply wait, settle window after
  an absolute set). The single deliberate deviation is the TUI's hold-to-move
  stop timer (below).
- **MQTT is stop-only.** `/cmd` accepts exactly `{"action":"stop"}`; nothing
  else is even parsed.

## Keys

| Key | Action |
|---|---|
| `←→↑↓` / `hjkl` | hold-to-move jog at the jog speed (auto-repeat refreshes; release → stop) |
| `SPACE` / `ESC` | **global e-stop** — all-stop frame, cancels prompts, always allowed |
| `a` / `e` | query azimuth / elevation (event-driven; no polling) |
| `A` | arm flow: enter true azimuth (prefilled from `state.toml`) |
| `0` | goto **physical** zero (offset never applied) |
| `+` / `-` | jog speed ±1 (clamped 0x00–0x3F, default `0x12`) |
| `s` | self-test — disarmed only, two-stage `RIPCABLES` confirm |
| `d` | disarm |
| `ctrl+r` | reopen serial port (USB re-enumeration heal) |
| `ctrl+l` / `tab` | clear / scroll the wire log |
| `ctrl+c` / `ctrl+q` | quit — best-effort all-stop first |

**Hold-to-move deviation:** Bubble Tea has no key-release events. Each
keypress arms a one-shot `jog_hold_ms` timer (default 250); terminal auto-repeat
refreshes it; an expiring tick without a fresh keypress sends one stop. This
one-shot is a safety net, not polling — it is the recorded deviation from the
no-timer rule.

## Absolute positioning without polling

`set_pos` from rotctld runs a **verification ladder** on the engine: send the
absolute set, keep the line quiet for `settle_ms` (default 2000), query once,
compare within `set_tolerance_deg` (default 0.3°) — converged; otherwise
re-send up to `set_attempts` (default 3), else report failure. Any stop, jog,
or new set cancels the ladder (the human wins). No polling loop anywhere; the
only engine timers are one-shot gate releases.

## Run

```sh
# which serial ports exist? (with USB identity: vid:pid, product, serial no.)
pelcobridge2 -list-ports

# bench, no hardware: mock head over TCP (works on Windows too)
go run ./cmd/pelcobridge2-mock -listen :4001
go run ./cmd/pelcobridge2 -port tcp:127.0.0.1:4001

# macOS/Linux loopback with a real pty
socat -d -d pty,raw,echo=0 pty,raw,echo=0
go run ./cmd/pelcobridge2-mock -pty /dev/ttys013
go run ./cmd/pelcobridge2 -port /dev/ttys014

# real head
go run ./cmd/pelcobridge2 -port /dev/serial/by-id/... -config config.toml
```

Then: `A` → confirm the true azimuth → `ARMED`. From another terminal:

```sh
rotctl -m 901 -r localhost:4533
# disarmed  →  set_pos answers RPRT -9
# armed     →  set_pos answers RPRT 0 and the head moves
```

`gpredict` (`-m 901`) works against `localhost:4533`; it sends `\dump_state`
at open, which this server answers in protocol v1.

## MQTT — slot `muehle/uhf/rotator`

Four planes per the station integration model (`../docs/station-integration-model.md`):

- `/meta` (retained): device identity + `expose` — fields `az, el, target_az,
  target_el, moving, armed, device_online`; exactly one action, `stop`.
- `/state` (retained, change-deduped JSON): `ts, az, el, phys_az, phys_el,
  readback_valid, readback_age_s, armed, az_offset_deg, moving, target_az/el,
  set_status, jog_speed, protocol, rotctld_clients, device_online, link, error`.
- `/status`: component LWT (`online`/`offline` retained) — distinct from
  `/state.device_online`, which is the head's serial link.
- `/cmd`: **only** `{"action":"stop"}` is accepted; everything else is logged
  and ignored. There is no code path from MQTT to arming or motion.

MQTT is optional and never fatal; the TUI shows the broker state. The password
comes from the environment (`PELCOBRIDGE2_MQTT_PASSWORD`), never a flag and
never the config file.

## Configuration

Seed-once TOML, 0600 (see `../docs/conventions/config-and-secrets.md`). Path
resolution: `-config` flag > `PELCOBRIDGE2_CONFIG` > `config.toml` next to the
executable (Windows double-click friendly) > `./config.toml`. Missing file →
built-in defaults. See `config.example.toml` for every key with defaults.

`state.toml` (next to the config, 0600) stores the last entered azimuth
offset as an arm-prompt prefill. Nothing else is persisted; armed state never
survives a restart.

## Build & deploy

```sh
./build.sh                 # dist/pelcobridge2-{windows-amd64.exe,linux-amd64,darwin-arm64}
TARGETS=all ./build.sh     # + linux/arm64, darwin/amd64
./deploy.sh                # build windows-amd64 + scp to the shack PC
```

`deploy.sh` copies the binary to `iotte@192.168.1.197:C:/Users/iotte/pelcobridge2/`,
seeds `config.toml` once (never overwrites), and prints the manual start
command. The Windows instance runs interactively — no auto-start, no service
(deliberate deviation from the deployment convention, recorded there).