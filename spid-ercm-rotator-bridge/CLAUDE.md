# CLAUDE.md — spid-ercm-rotator-bridge

spid-ercm-rotator-bridge fronts the station's **satellite-tracking az/el rotator
mount** as two canonical `rotator` slots: `muehle/uhf/az-rotator` (SPID
azimuth rotor, Rot1Prog binary protocol over serial, 1200 baud) and
`muehle/uhf/el-rotator` (GS-500 elevation via the **ERC-M** controller,
GS-232B dialect over serial, 9600 baud). It is a **compound bridge** (one
process, two slots, two MQTT clients — the shelly-power-bridge `[[slot]]`
shape): each slot gets its own paho client and LWT so a dead serial port
degrades only its own slot while a process death takes both offline with no
stale-online gap.

Beyond the bus surface it runs two protocol listeners fed from the same
dispatch core: a **rotctld**-compatible TCP server (:4534, the
pelcobridge2-proven hamlib dialect, for gpredict and hamlib clients) and a
**PstRotator** native UDP listener (:12041). Motion is **free with no arming
gate** — the reviewed no-auth station posture (plan KTD4); the exposure
register records the accepted vectors.

Its name follows the stationa bridge-naming convention
`<devtag>-<function>-bridge` (see `../docs/conventions/naming.md`) — the
compound `spid-ercm` devtag covers both fronted device families.

**Status (implemented):** the full stack is landed and unit-tested — SPID
Rot1Prog driver (U2), ERC-M GS-232B driver (U3), mount dispatch façade (U4),
two-slot MQTT surface (U5), rotctld TCP server (U6), PstRotator UDP listener
(U7) — see `../docs/plans/2026-09-12-001-feat-sat-ops-rotators-plan.md`. An
**empty configured serial port selects the in-process mock device per axis**
(KTD7), so the whole stack — slots, both listeners, dispatch, self-heal — runs
bench- and CI-side without hardware; the shipped `config.example.toml` defaults
to mock until the bench bring-up pins the real by-id adapter identities. The
pre-deploy exposure review (KTD4, U9) is recorded in
`../docs/known-issues.md` ("Sat-ops rotators: pre-deploy exposure review") —
that was the deploy gate; what remains before the first live shari deploy is
the bench bring-up of the two serial adapters.

---

## Commands

```bash
go build ./cmd/spid-ercm-rotator-bridge                 # local build
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o dist/spid-ercm-rotator-bridge-linux-arm64 ./cmd/spid-ercm-rotator-bridge   # Pi cross-compile
go test ./...                                     # unit tests (no network/hardware)
go test ./... -race                               # tests with race detector
go vet ./...                                      # vet
gofmt -s -w .                                     # fmt
./deploy.sh                                       # cross-compile, ship, install as hardened systemd service
```

Run a single test package:
```bash
go test ./internal/config/...
```

CLI flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `/etc/spid-ercm-rotator-bridge/config.toml` | Path to TOML config file |
| `-log.level` | (from config) | `debug` \| `info` \| `warn` \| `error`; overrides config |

There is deliberately **no password flag** — the MQTT secret never rides a
command line, and a `password` key in the TOML is a hard parse error.

---

## Architecture

**Data flow:** SPID serial + ERC-M serial → `internal/spid` +
`internal/ercm` (drivers; empty port ⇒ in-process mock) → `internal/mount`
(dispatch façade — the only cross-axis-semantics owner) → `internal/mqttslot`
(two-slot MQTT surface); the protocol listeners `internal/rotctld`
(TCP :4534) and `internal/pstrotator` (UDP :12041) also consume only the
façade, so protocol-driven motion surfaces in `/state` exactly like bus-driven
motion (R4).

1. `cmd/spid-ercm-rotator-bridge/main.go` — flags, config load, signal ctx,
   root slog logger with the constant `component` attr, one child logger per
   slot; wires drivers → façade → both slots → both listeners. Initial MQTT
   connect failure and a failed listener bind are both fatal (exit non-zero,
   systemd restarts — §8.1 item 10).
2. `internal/config` — TOML config (one `[[slot]]` per axis), flags,
   `SPID_ERCM_ROTATOR_BRIDGE_*` env overrides.
3. `internal/spid` (Rot1Prog driver, 13-byte frames at 1200 baud 8N1, az-only),
   `internal/ercm` (GS-232B driver: `W` goto, `C2`/`B` readback, `S`/`E` stop,
   `rFMW` firmware; polls at `control.poll_interval`), `internal/mount`
   (per-axis Controller + mount façade: latest-wins coalescing with one
   in-flight per axis, bounded stop epoch that halts BOTH axes and cancels
   pending targets, two-axis refusal aggregation into the single client reply,
   park as an atomic mount-level intent), `internal/mqttslot` (two paho
   clients, one per slot), `internal/rotctld` (`p`/`P`/`S`/`_`/`\dump_state`/`q`,
   `RPRT 0/-1/-4/-6/-9/-11`), `internal/pstrotator` (`<PST>` datagrams, `AZ?`/
   `EL?` replies to the source IP at listen-port+1, `<STOP>`, `<PARK>`).
4. `internal/mount` self-heal: each driver re-resolves its stable
   `/dev/serial/by-id/` path and retries its reopen indefinitely after a
   serial error (`control.reopen_cooldown`); a `Down` transition clears that
   axis's cached-readback validity and `moving`, so the deadband can never
   no-op against a pre-outage stale position.

**Key design pins (plan KTDs):**
- **KTD2** one process, two slots, two MQTT clients (per-slot LWT).
- **KTD7** serial self-heal: re-resolve the stable `/dev/serial/by-id/` path,
  retry indefinitely, re-init after reopen; an **empty configured port selects
  the in-process mock device** so the whole stack runs bench- and CI-side
  without hardware. A raw `/dev/ttyUSB*` port is rejected at validation.
- **KTD13** rotator `/cmd` is one-shot: non-retained, QoS-0 subscription,
  cleared after execute-or-reject.
- **KTD14** `/state` cadence: poll tick, dedup, change/edge republish, always
  fresh `ts`.

---

## MQTT topics

spid-ercm-rotator-bridge publishes the station integration model topics
per axis (`<slot>` = `az-rotator` | `el-rotator`):

```
muehle/uhf/<slot>/meta     retained  birth certificate (role `rotator`, capabilities: axes + limits {min,max,park} + deadband)
muehle/uhf/<slot>/state    retained  live rotator state JSON snapshot (see below)
muehle/uhf/<slot>/status   retained  online | offline (LWT — the bridge, not the controller)
muehle/uhf/<slot>/cmd      one-shot  goto | stop intent (bus → bridge), value-keyed args, non-retained
```

`/state` is a single retained JSON document —
`{ts, az|el (position °, omitted while readback invalid), target?, moving, link,
device_online, error?}` — where `moving` is inferred
(`|target − readback| > deadband`, false while readback validity is unknown),
never wire-reported. `device_online` is always an explicit boolean (two-layer
liveness: `/status` is the bridge process, `/state.device_online` is **this
slot's own serial link** — a dead elevation port takes only `el-rotator`
offline). `/cmd` payloads: `{"action":"goto","value":"45.0"}` /
`{"action":"stop"}` — published non-retained, subscribed at QoS 0, cleared with
an empty retained publish after execute-or-reject, `ts`-gated when stamped
(KTD13; unstamped producers tolerated). The ERC-M's `rFMW` firmware string
folds into `/meta.device.firmware` once the first link open reads it; the SPID
Rot1Prog has none and omits the key. Rotator slots publish **read-only**
`expose` blocks (state fields only — no writable setpoints, no actions) —
hadiscovery renders state but no HA motion widgets (KTD4).

---

## Configuration and secrets

Config is TOML (`/etc/spid-ercm-rotator-bridge/config.toml` by default, or
`-config <path>`). It holds one `[[slot]]` per axis (`axis` = `az`|`el`, slot
name, `device_model`, `link`, a `[slot.serial]` port/baud table — empty port ⇒
mock mode), the `[rotctld]` and `[pstrotator]` listener endpoints, and
`[control]` (per-axis travel limits, deadband ~1°, park positions, poll
interval, reopen cooldown). The MQTT password is **not** in the TOML — it is
loaded from an `EnvironmentFile` so it never appears in the unit file or
process command line:

```bash
# /etc/spid-ercm-rotator-bridge/spid-ercm-rotator-bridge.env  (0600, owned by spid-ercm-rotator-bridge user)
SPID_ERCM_ROTATOR_BRIDGE_MQTT_PASSWORD=<password>
```

The systemd unit contains
`EnvironmentFile=/etc/spid-ercm-rotator-bridge/spid-ercm-rotator-bridge.env`.
Env overrides: `SPID_ERCM_ROTATOR_BRIDGE_MQTT_BROKER`/`_CLIENT_ID`/`_USER`/
`_PASSWORD`/`_SITE`/`_STATION`. Per-slot values (axis, slot name, device
identity, serial) and `[control]` come from the TOML only. The env prefix is
the dir name uppercased with hyphens → underscores (naming convention).

See `../docs/conventions/config-and-secrets.md` for the full convention and
`config.example.toml` for the annotated shape.

---

## Deployment

Target: Raspberry Pi (`shari`, `192.168.1.139`, user `io`).

```bash
./deploy.sh              # cross-compile, ship, install as a hardened systemd service
# Before first deploy, set the real serial identities at the bench bring-up:
AZ_PORT=/dev/serial/by-id/usb-... EL_PORT=/dev/serial/by-id/usb-... ./deploy.sh
# Two USB-serial adapters may be two vendors — list every vendor for the udev rules:
SERIAL_USB_VENDORS="0403 067b" ./deploy.sh
```

The unit uses a **new hardening combination** (plan KTD3): serial access
forbids `PrivateDevices`, so it takes ultrabridge's `SupplementaryGroups=dialout`
+ `DeviceAllow=char-tty{USB,ACM} rw` + udev rules, atr1k's remaining hardening
with `MemoryMax=256M`/`TasksMax=64`, and wrc's
`RestrictAddressFamilies=AF_INET AF_INET6` for the inbound TCP/UDP listeners.
`deploy.sh` generates **one udev rule per USB vendor** from
`SERIAL_USB_VENDORS` (space-separated; the single-vendor template would miss a
second adapter family).

**Deploy gate:** the shari deploy was gated on the Tier-1 exposure review being
recorded (U9) — it now is (`../docs/known-issues.md`, "Sat-ops rotators:
pre-deploy exposure review"; five rotator vectors + the phase-controller
sixth). What still precedes the first live deploy is the bench bring-up:
pinning the real `/dev/serial/by-id/` adapter identities (mock-mode ports must
not reach a live deploy) and confirming the shack SPID controller is a
Rot1Prog (not an MD-0x in ROT1 mode, plan KTD5). See
`../docs/conventions/deployment.md` (serial addendum) for the underlying
hardening requirements.

---

## Station model and shared conventions

Shared documentation lives in the `../docs/` (this component is a subdirectory of the stationa monorepo).

| Document | Path |
|---|---|
| Station integration model (three-plane MQTT contract) | `../docs/station-integration-model.md` |
| Feature plan (KTDs, requirements, units) | `../docs/plans/2026-09-12-001-feat-sat-ops-rotators-plan.md` |
| Config and secrets convention | `../docs/conventions/config-and-secrets.md` |
| Deployment convention (serial addendum) | `../docs/conventions/deployment.md` |
| Logging convention (slog, component/slot attrs) | `../docs/conventions/logging.md` |
| Bridge-naming convention | `../docs/conventions/naming.md` |

This project implements the `muehle/uhf/az-rotator` and `muehle/uhf/el-rotator`
slots and must conform to those conventions.