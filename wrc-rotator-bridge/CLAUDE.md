# CLAUDE.md — wrc-rotator-bridge

wrc-rotator-bridge bridges the **HF antenna rotator** (Yaesu G-450DC steered via an
AF6SA WRC controller) to MQTT using the station integration model (slot
`muehle/hf/rotator`). It dials the WRC's WebSocket status stream, publishes a
canonical rotator state snapshot, dispatches `/cmd` intent (`set_az`, `stop`,
`fwd`, `rev`) back to the rotator, and optionally runs two parallel legacy
control paths: a GS-232B TCP server on port 7373 and a PSTRotator-compatible
UDP listener on port 12040. Either path lets rotator-control software drive the
rotator directly; resulting motion still surfaces in `/state`. It is
**read-write**. The UHF sat rotators are separate slots (`muehle/uhf/az-rotator`,
`muehle/uhf/el-rotator`, spid-ercm-rotator-bridge) — no collision.

The bridge **enforces the station antenna mode** (`muehle/hf/ant-mode`, component
`antmode`): in `storm`/`maintenance` it refuses motion on all three inbound
paths, never refuses stop, and executes the park (`lock.park_az`) itself — see
"Antenna-mode lock" below.

---

## Commands

```bash
go build ./cmd/wrc-rotator-bridge                 # local build
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o dist/wrc-rotator-bridge-linux-arm64 ./cmd/wrc-rotator-bridge   # Pi cross-compile
go test ./...                                 # unit tests (no network/hardware)
go test ./... -race                           # tests with race detector
go vet ./...                                  # vet
gofmt -s -w .                                 # fmt
./deploy.sh                                   # cross-compile, ship, install as hardened systemd service
```

Run a single test package:
```bash
go test ./internal/bridge/... -run TestHandleCommand
go test ./internal/gs232/...
go test ./internal/pstrotator/...
go test ./internal/lock/...
```

CLI flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `/etc/wrc-rotator-bridge/config.toml` | Path to TOML config file |
| `-log.level` | (from config) | `debug` \| `info` \| `warn` \| `error`; overrides config |
| `-debug` | `false` | Log WRC WebSocket I/O |

---

## Architecture

**Data flow:** WRC WebSocket → `internal/rotor` → `internal/bridge` → MQTT,
with `internal/gs232` and `internal/pstrotator` as parallel inbound control
paths.

1. `cmd/wrc-rotator-bridge/main.go` — flags, config load, signal ctx, MQTT
   connect+LWT, ant-mode subscription (before `/cmd`), `/cmd` subscription +
   bounded worker, the lock loop (1 s ticker + kicks: park step + `/state.lock`),
   WRC WebSocket restart loop with exponential backoff, GS-232 and PSTRotator
   server start.
2. `internal/rotor` — WRC WebSocket device: dial (ctx-aware `DialContext`),
   read loop parsing `RotorStatus` into canonical `State`, mutex-guarded
   command writes (`SetAz`/`Stop`/`Jog`) bounded by a 3 s per-write deadline
   (a wedged write tears the connection down so the restart loop redials —
   STOP must never wedge), thread-safe `Snapshot`/`CurrentAz`
   for the GS-232 server.
3. `internal/bridge` — canonical rotator state model + MQTT publishing:
   `/meta` (role `rotator`, `axes [az]`, `expose`), retained `/state` snapshot
   with change-dedup (the `lock` label is part of it), `/cmd` dispatch via a
   `Commander` (a `set_az` with `override` goes to `SetAzOverride` when the
   commander implements `OverrideCommander`).
4. `internal/gs232` — optional GS-232B TCP server: `C`/`C2` position query,
   `Mxxx`/`Wxxx` move, `S` stop. Drives the same lock gate the bridge
   does; the resulting motion surfaces in `/state`.
5. `internal/pstrotator` — optional PSTRotator-compatible UDP listener on port
   12040 (configurable). Accepts XML datagrams (`AZIMUTH`, `STOP`, `PARK`,
   `AZ?` query) and drives the same lock gate; the resulting motion
   surfaces in `/state`. `Serve` takes a pre-bound socket (tests bind first).
6. `internal/lock` — the antenna-mode `Gate`: a `rotor.Commander` wrapping the
   `rotor.Device`, handed to `/cmd`, GS-232 and PSTRotator in place of the
   device. Parses `hf/ant-mode/state` with `shared/antmode`, refuses motion
   with `rotor.ErrLocked`, passes stop, accepts the maintenance nudge, and
   runs the park state machine (`Observe` from telemetry, `Step` from the
   lock loop).
7. `internal/config` — TOML config, flags, `WRC_ROTATOR_BRIDGE_*` env overrides.

**Restart loop** (`wsLoop`): dials the WRC, publishes meta, runs the read loop
until the WebSocket errors or ctx cancels, then marks the device offline and
backoffs-and-retries. `/status` stays `online` while the bridge retries — only
`/state.device_online` flips. The initial paho `Connect()` is made ctx-aware
(bridging `tok.Wait()` through a goroutine + `select` on `ctx.Done()`) so a
SIGTERM while the broker is unreachable can interrupt the connect — otherwise
systemd must SIGKILL after `TimeoutStopSec`.

**Concurrency:** `rotor.Device.writeMu` guards the WebSocket and all writes
(commands from `/cmd` and from GS-232 handlers serialize on it). `rotor.Device.
stateMu` guards the last state snapshot. `bridge.Bridge.mu` guards the
canonical state and the dedup snapshot; `bridge.Bridge.pubMu` serializes
"snapshot + publish" across the WRC read loop, wsLoop and the lock loop, so a
stale `/state` is never left retained. The paho `/cmd` handler runs in paho's
goroutine and only enqueues onto a bounded channel — it never blocks (a
`set_az` writes to the WRC). GS-232 handlers run in their own accept goroutines
and reach the device only through the mutex-guarded `Controller` interface.
The paho ant-mode handler only parses under `lock.Gate.mu` and kicks the lock
loop; the loop does the park write and the `/state.lock` publish. `Gate.mu` is
never held across a device write (a WRC write can take up to 3 s).

---

## Antenna-mode lock

Contract: `../antmode/docs/ant-mode-mqtt-api.md`; decision record:
`../docs/known-issues.md` ("HF antenna mode"). Config `[mqtt] ant_mode_slot`
(default `ant-mode`; `""` disables the lock and the startup hold) and
`[mqtt] nudge_max_deg` (default 10, must be > 0; the maintenance nudge bound).

- No retained ant-mode state = normal (rollout-safe). A valid locked snapshot
  is enforced even while `hf/ant-mode/status` is offline. Malformed = keep the
  last known lock + Error.
- Startup hold: until the first ant-mode state arrives, motion is refused
  while MQTT connects and for 3 s after it is up (`Gate.StartHold` from
  OnConnect, before the ant-mode subscription), so a command cannot beat the
  retained lock even behind a slow broker.
- Locked: `set_az`/`fwd`/`rev` (MQTT), `M`/`W` (GS-232, still acked) and
  `<AZIMUTH>` (PSTRotator) are refused with `rotor.ErrLocked` — one Warn per
  refusal streak (`shared/logging.Streak`), callers log nothing extra. `stop`,
  `S` and `<STOP>` always pass; `C`/`AZ?` always answer.
- Maintenance with `lock.nudge`: a `/cmd` `set_az` tagged
  `"override":"maintenance"` passes (console intent tagging, not security)
  when its target is within `nudge_max_deg` of the current readback — linear
  in rotor coordinates (0..450° with the overlap), never wrapped mod 360. A
  target farther away, outside 0..450°, or any nudge while the WRC is offline (no readback to
  bound it by), is refused with `rotor.ErrLocked` (its own Warn streak, reset
  by an accepted nudge or a lock change) and ends nothing. The console sends
  readback ± 5°; the bound stops any other client carrying the tag (or an old
  APK with tap-to-aim) from slewing the brakeless G-450 a full sweep.
- Park: once per `lock.seq`, after the WRC is online and the readback held
  within 1° for 3 s with `moving:false`; arrival = within 3°. No re-aim for
  drift. Re-sent only if it never arrived and the WRC reconnected (or the
  write failed). An operator stop during the park cancels it until a new seq;
  a stop racing the park write is re-sent after it. An accepted maintenance
  nudge ends the park for its seq too, also a nudge before `park_az` was
  published (`Gate.moveMu` makes a park write in flight land before the
  nudge); antmode sees the nudge as a new `target_az` and reports `parked`
  with the offset as `drift_deg`. Wind creep restarts the 3 s window (the park waits for a lull, by
  design); antmode's `rotate_timeout_s` is the only deadline.
- Restart: a finished park (arrived/cancelled/nudged) is recorded in
  `<state_dir>/park.json` (config `state_dir`, default the systemd
  StateDirectory `/var/lib/wrc-rotator-bridge`); the same mode+seq+park_az is
  not parked again after a restart (a nudge record: mode+seq, any park_az).
  A park underway at the restart is re-sent.
- `/state.lock` = `storm` | `maintenance`, omitted when unlocked.

---

## MQTT topics

wrc-rotator-bridge publishes to the station integration model topics:

```
muehle/hf/rotator/meta     retained  birth certificate (capabilities + expose)
muehle/hf/rotator/state    retained  live rotator state JSON snapshot
muehle/hf/rotator/status   retained  online | offline (LWT — the bridge, not the WRC)
muehle/hf/rotator/cmd      not retained  set_az | stop | fwd | rev intent (bus → bridge)
muehle/hf/ant-mode/state   retained      antenna-mode lock (read; owned by antmode)
```

`/state` is a single retained JSON document. Canonical fields: `az`
(degrees), `target_az` (commanded target, omitempty), `moving` (bool),
`rotor_state` (raw WRC state string, diagnostic), `device_online`/`error`,
`lock` (antenna-mode lock label, omitted when unlocked). A
rotator carries **no** `freq_hz`/`band`/`mode` — those are radio concerns.
`/cmd` is **not retained**: a rotator move is a one-shot command; re-applying a
stale target on restart could spin the rotator unexpectedly. See
`docs/wrc-rotator-bridge-mqtt-api.md` for the full on-the-wire contract.

---

## Configuration and secrets

Config is TOML (`/etc/wrc-rotator-bridge/config.toml` by default, or
`-config <path>`). The MQTT password is **not** in the TOML — it is loaded from
an `EnvironmentFile` so it never appears in the unit file or process command line:

```bash
# /etc/wrc-rotator-bridge/wrc-rotator-bridge.env  (0600, owned by wrc-rotator-bridge user)
WRC_ROTATOR_BRIDGE_MQTT_PASSWORD=<password>
```

The systemd unit contains `EnvironmentFile=/etc/wrc-rotator-bridge/wrc-rotator-bridge.env`.
Env overrides: `WRC_ROTATOR_BRIDGE_MQTT_BROKER`/`_CLIENT_ID`/`_USER`/`_PASSWORD`/
`_SITE`/`_STATION`/`_SLOT`, `WRC_ROTATOR_BRIDGE_ROTOR_URL`. The GS-232 and
PSTRotator listeners are configured via `[gs232]` / `[pstrotator]` in TOML
(`deploy.sh` seeds them on first deploy).

See `../docs/conventions/config-and-secrets.md` for the full convention.

---

## Deployment

Target: Raspberry Pi `scmino` (`192.168.1.178`, user `io`) since 2026-10-12; was `shari`.
The GS-232 server (:7373 TCP) and PSTRotator listener (:12040 UDP) moved with it: rotator-control
software must point at scmino, not shari.

```bash
./deploy.sh              # cross-compile, ship, install as a hardened systemd service
```

**Migrating from the legacy `rotint` rotor bridge:** shari's prior rotator service
was the **upstream AF6SA `rotint` project** (`rotor-bridge.service`, binary
`/home/io/rotint/rotor-bridge`), not a stationa bridge — it ran as user `io`, passed
the MQTT password on the command line, had no TOML config, and published the
`rotor2mqtt/…` topic tree plus embedded HA discovery under `homeassistant/…`. This
bridge replaces it with the stationa three-plane schema (`muehle/hf/rotator/*` +
`/meta.expose` consumed by `hadiscovery`).

Run the one-time `./migrate-from-rotint.sh` **before** the first `./deploy.sh`. It
creates the new service user + config dir, extracts the MQTT password from the old
unit's `-password="…"` command line and writes it to
`/etc/wrc-rotator-bridge/wrc-rotator-bridge.env` (0600, owner the new user) **on the
device** so the password never leaves the Pi (this also fixes rotint's pre-existing
command-line secret exposure), then stops + disables `rotor-bridge.service` and
removes its unit. It deliberately leaves `/home/io/rotint` and user `io` in place
(the user's files / the deploy user). Idempotent — safe to re-run. Ordering
matters: migration first, then `./deploy.sh`, whose seed-once seeds `config.toml`
with defaults that match rotint's hardcoded values (WRC `ws://192.168.1.108/wsrotor`,
broker `tcp://192.168.1.139:1883`, user `hf`, GS-232 `0.0.0.0:7373`) and leaves the
migrated env file (the real password) untouched. wrc-rotator-bridge is network-only,
so the migration creates no udev rule and adds no serial group.

The unit is hardened like flexbridge (network-only): `ProtectSystem=strict`,
`PrivateDevices=true` (no serial devices), `RestrictAddressFamilies=AF_INET
AF_INET6` (covers the outbound WRC+MQTT and the inbound GS-232 TCP plus
PSTRotator UDP listens), `MemoryMax=256M`, `TasksMax=64`. There is no udev
rule and no `DeviceAllow` — the bridge makes outbound TCP connections plus two
inbound listen sockets. See `../docs/conventions/deployment.md`.

---

## Station model and shared conventions

Shared documentation lives in `../docs/` (this component is a subdirectory of the stationa monorepo).

| Document | Path |
|---|---|
| Station integration model (three-plane MQTT contract, §7.1 rotator slot) | `../docs/station-integration-model.md` |
| Config and secrets convention | `../docs/conventions/config-and-secrets.md` |
| Deployment convention | `../docs/conventions/deployment.md` |
| Canonical band/mode vocabulary (not used by a rotator) | `../docs/conventions/band-mode-reference.md` |

This project implements the `muehle/hf/rotator` slot and must conform to those
conventions. Component-specific schema lives in `docs/wrc-rotator-bridge-mqtt-api.md`.