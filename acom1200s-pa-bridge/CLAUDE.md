# CLAUDE.md — acom1200s-pa-bridge

acom1200s-pa-bridge bridges an **ACOM 600S/1200S linear amplifier** to MQTT using the
station integration model (slot `muehle/hf/pa`). It reads the amplifier's
proprietary serial protocol over a USB-serial adapter (Prolific, 9600 8N1),
publishes a canonical PA state snapshot, and dispatches `/cmd` intent
(`set_mode`, `set_band`) back to the amp. It is a **pure observer of the amp's
power state**: `pa.power` is read-only telemetry, and powering the PA on/off is
owned by the power-distribution layer (the `hf/switch` slot's remote-on relays).
The bridge drives no host control lines and issues no power commands.

---

## Commands

```bash
go build ./cmd/acom1200s-pa-bridge                 # local build
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o dist/acom1200s-pa-bridge-linux-arm64 ./cmd/acom1200s-pa-bridge   # Pi cross-compile
go test ./...                             # unit tests (no network/hardware)
go test ./... -race                       # tests with race detector
go vet ./...                              # vet
gofmt -s -w .                             # fmt
./deploy.sh                               # cross-compile, ship, install as hardened systemd service
```

Run a single test package:
```bash
go test ./internal/acom/...
go test ./internal/bridge/... -run TestState
```

CLI flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `/etc/acom1200s-pa-bridge/config.toml` | Path to TOML config file |
| `-log.level` | (from config) | `debug` \| `info` \| `warn` \| `error`; overrides config |
| `-debug` | `false` | Hex-dump all serial I/O to stderr |

---

## Architecture

**Data flow:** serial run loop → `internal/acom` → `internal/bridge` → MQTT.

1. `cmd/acom1200s-pa-bridge/main.go` — flags, config load, signal ctx, MQTT connect+LWT,
   `/cmd` subscription, serial restart loop with exponential backoff.
2. `internal/acom` — serial protocol: open port, frame scan + checksum, ACK,
   enable-telemetry, 72-byte telemetry parser, forward-power averager, band
   navigation (next/prev walk), mode/band commands. Owns protocol-level state
   (band index for navigation).
3. `internal/bridge` — canonical PA state model + MQTT publishing: `/meta`
   (capabilities + `expose`), retained `/state` snapshot, `/cmd` dispatch via a
   `Commander` interface, gated legacy HA discovery.
4. `internal/config` — TOML config, flags, `ACOM1200S_PA_BRIDGE_*` env overrides.
5. `internal/ha` — legacy embedded HA discovery payload builders (gated off by
   default; slated for deletion once `hadiscovery` is proven, model §9).

**Liveness and re-arm** (`acom.Device.Run`): keyed on **decoded telemetry
frames**, never raw bytes. Enable-telemetry is re-sent every 3 s while no
frame has decoded for 3 s, or while the amp reports OFF/RESET/INIT (a booting
amp needs an enable *after* its boot; one sent while it is off is lost). No
frame for 30 s is an outage: the link-down handler publishes
`device_online:false` once (Warn once per outage), band/mode are forgotten,
and **the port stays open** with the re-arm running — so a remote-on gets the
amp streaming within seconds. `/status` stays `online` throughout; only
`/state.device_online` flips, and only the first decoded frame raises it
(opening the port never does). Discarded bytes and non-telemetry frames are
counted and logged with the re-arm/outage lines; the first frame logs
`telemetry streaming: mode=…`. 2026-10-18 history: a byte-keyed 5 s re-send
plus close + 60 s backoff left a freshly powered amp unarmed for minutes and
powerseq's `wait-pa-power-on` timed out. The bridge never drives the amp's
power state; re-arming telemetry is not a power command.

**Restart loop** (`serialLoop`): opens the port, publishes meta, runs the
telemetry loop. A run ends only when the port is unrecoverable (faults with no
successful read for 30 s) or on ctx cancel; then the loop backs off
exponentially and re-opens. There is no `power_default` and no parking.

**Serial self-heal** (`acom.Device.reopen`): a transient port fault — the
live failure mode is EIO after the USB-serial adapter drops and the kernel
re-enumerates it under a new tty — is healed *in place* before the restart
loop is entered: the stale handle is closed, the (by-id) port path is
re-resolved to the fresh tty, and telemetry is re-armed, without `Run`
returning and without `/state.device_online` flipping. Because the EIO
surfaces *before* udev has recreated the by-id path, a reopen that fails
with ENOENT is retried in place (spaced by `reopenMinInterval`, 2 s) until
it lands — the adapter is coming back, and this is exactly the fault the
in-place heal exists for. A link that *opens* but faults again inside the
window is treated as persistently broken and falls through to the serialLoop
restart path and its backoff. The 30 s port watchdog covers the fault path
and the reopen-retry path and is refreshed only by a successful read, never
by a reopen, so a flapping adapter ends the run however often it reopens.
`device_online` is independent of this: it follows decoded frames, so a dead
link cannot hold it true (two-layer liveness stays honest). The
serialLoop backoff resets to the initial value after a run that lasted
≥ 1 min (measured on run duration, applied before the wait), so escalation
belongs to the episode that broke the link rather than to history.
(Pattern ported from ultrabridge's transport self-heal, including its
retry-next-tick half.)

**Concurrency:** `acom.Device.mu` guards the serial port and all writes (ACK,
enable, mode/band commands) — fixes the original single-file's ACK-without-lock
race. `acom.Device.stateMu` guards mode/band/online. `bridge.Bridge.mu` guards
the canonical state and the discovery-once flag. The paho `/cmd` handler runs
in paho's goroutine and reaches the device only through the `Commander`
interface (mutex-guarded).

---

## MQTT topics

acom1200s-pa-bridge publishes to the station integration model topics:

```
muehle/hf/pa/meta      retained  birth certificate (capabilities + expose)
muehle/hf/pa/state     retained  live PA state JSON snapshot
muehle/hf/pa/status    retained  online | offline (LWT — the bridge, not the amp)
muehle/hf/pa/cmd       not retained  set_mode | set_band intent (bus → bridge)
```

`/cmd` is intent and must never be retained: the bridge drops retained `/cmd` deliveries
(logged at Warn) and ignores empty payloads, so a stale retained command is not replayed on
restart/reconnect (`cmdMessageHandler` in `cmd/acom1200s-pa-bridge/main.go`).

`/state` is a single retained JSON document. Canonical fields: `mode`
(`operate`/`standby`), `band`, `keyed` (`rx`/`tx`/`inhibited`), `fwd_power_w`,
`rfl_power_w`, `temp_c`, `swr`, `fault` (`none`/`swr`/`temp`/`reflected`/`other`),
`power` (`on`/`off`, **read-only actual** — `off` only when `pa_state == OFF` or
the port is lost), plus the raw diagnostic `pa_state` (firmware mode string) and
`device_online`/`error`. The raw firmware mode is **only** in `pa_state`/`error`,
never in the canonical `mode`/`keyed`/`fault` fields. `power` is **telemetry
only** — there is no `set_power` command and no `power_default` config; powering
the PA is the job of the `hf/switch` slot's remote-on relays. See
`docs/pa-mqtt-api.md` for the full on-the-wire contract and the
firmware→canonical mapping.

---

## Configuration and secrets

Config is TOML (`/etc/acom1200s-pa-bridge/config.toml` by default, or `-config <path>`).
The MQTT password is **not** in the TOML — it is loaded from an
`EnvironmentFile` so it never appears in the unit file or process command line:

```bash
# /etc/acom1200s-pa-bridge/acom1200s-pa-bridge.env  (0600, owned by acom1200s-pa-bridge user)
ACOM1200S_PA_BRIDGE_MQTT_PASSWORD=<password>
```

The systemd unit contains `EnvironmentFile=/etc/acom1200s-pa-bridge/acom1200s-pa-bridge.env`.
Env overrides: `ACOM1200S_PA_BRIDGE_MQTT_BROKER`/`_CLIENT_ID`/`_USER`/`_PASSWORD`/
`_SITE`/`_STATION`/`_SLOT`, `ACOM1200S_PA_BRIDGE_SERIAL_PORT`.

See `../docs/conventions/config-and-secrets.md` for the full convention.

---

## Deployment

Target: Raspberry Pi **scmino** (`192.168.1.178`, user `io`) since 2026-10-12 — the ACOM's Prolific
USB-serial adapter is plugged into scmino's USB (it was on shari, `192.168.1.139`). The shari
copy is disabled, not removed (rollback). Never run two instances: they share the MQTT
client_id `muehle-hf-pa` and kick each other off the broker. Live broker is the HA broker
`192.168.1.50` (`127.0.0.1` on scmino is the unused mirror).

```bash
./deploy.sh              # cross-compile, ship, install as a hardened systemd service
```

**Migrating from the legacy `acombridge` install** (shari only; already done): run the one-time
`./migrate-from-acombridge.sh` **before** the first `./deploy.sh`. It stops +
disables the old `acombridge` service, copies `/etc/acombridge/{config.toml,acombridge.env}`
to `/etc/acom1200s-pa-bridge/` (rewriting `ACOMBRIDGE_` → `ACOM1200S_PA_BRIDGE_` in the
env file **on the device** so the MQTT password never leaves the Pi), then removes the
old unit, `/opt/acombridge`, `/etc/acombridge`, the old udev rule, and (best-effort) the
old user. Idempotent — safe to re-run. Ordering matters: migration first, then
`./deploy.sh` whose seed-once keeps the migrated real config.

The unit is hardened like flexbridge **minus `PrivateDevices`** (the bridge
must open `/dev/ttyUSB*`). It keeps `SupplementaryGroups=dialout`, the three
`DeviceAllow=char-tty{USB,ACM} rw`, and the Prolific (`067b`) udev rule that
pins the adapter's tty to the dialout group. See `../docs/conventions/deployment.md`
(serial addendum) for the serial-specific hardening requirements.

---

## Protocol reference

`docs/ACOM A600S A1200S Serial Protocol V1.3.pdf` — the authoritative source
for frame layout and byte offsets used in `internal/acom/parser.go`.

---

## Station model and shared conventions

Shared documentation lives in `../docs/` (this component is a subdirectory of the stationa monorepo).

| Document | Path |
|---|---|
| Station integration model (three-plane MQTT contract, §7.1 PA slot) | `../docs/station-integration-model.md` |
| Config and secrets convention | `../docs/conventions/config-and-secrets.md` |
| Deployment convention (serial addendum) | `../docs/conventions/deployment.md` |
| Canonical band/mode vocabulary | `../docs/conventions/band-mode-reference.md` |

This project implements the `muehle/hf/pa` slot and must conform to those
conventions. Component-specific schema lives in `docs/pa-mqtt-api.md`.