# stationa

**stationa** is an experiment in **amateur-radio station integration** — the protocols
and topologies that let a pile of unrelated, vendor-locked boxes behave as one coherent
station. The artifact that matters is the [station integration model](docs/station-integration-model.md):
a strict, versioned grammar for how a station's components describe themselves and
coordinate. The software in this repo is a working instance of that model — the **Mühle**
station, DL9ET's shack — not the point itself. It exists to validate the model against
real hardware and to show what a conformant adapter looks like.

## The problem

A modern ham station is a federation of devices that were never designed to talk to each
other: a radio on one TCP API, an amplifier on a serial line, a rotator on a websocket, an
antenna switch on wifi, a smart-plug on MQTT, a PLC relay board on some vendor firmware.
Each speaks its own protocol, each is updated on its own schedule, and the "integration"
is usually a sprawl of brittle, pairwise glue — a script per device, a dashboard
hard-coded to today's IP addresses, a mental model that lives in one operator's head. It
rots fast, and it does not transfer: rebuild the station, or hand it to another operator,
and the glue is worth nothing.

## The approach

The [integration model](docs/station-integration-model.md) collapses the goals into one
architecture by treating every component — device bridge, logic service, or passive
consumer — as a small control loop with **three separate planes that never share a path**:

- **Identity & capability** (`/meta`) — who I am, what I can do. Static, published on connect.
- **State** (`/state`) — what is currently true about me. Live, retained.
- **Command / intent** (`/cmd`) — what someone would like to be true. Transient.

Three principles follow (the model has the full argument):

- **The live configuration is the documentation.** A component, once connected, describes
  itself to a well-known address; anyone who wants to understand the station subscribes
  and reads. The inventory is a live read, never a separate document that rots. Things are
  referenced by canonical role and name — never by IP address or UDP port.
- **Change is constant; avoid tight coupling.** Components publish state to their own
  address and receive intent at their own address. Consumers react to **state**, never to
  the fact that a command was sent. Swap the box behind an address and nothing downstream
  changes.
- **Integration is cheap because the core is strict.** The canonical vocabulary and slot
  template are precious and versioned; per-vendor adapters are thin and disposable. A
  GenAI-written adapter has a strict target to translate into — and that strictness is
  exactly what keeps a fleet of cheap adapters coherent.

The model is transport-bound to MQTT and address-structured as `<site>/<station>/<slot>`
(e.g. `muehle/hf/radio`). Antennas are passive resources, not slots; routing among them is
an actuator (`ant-switch`) driven by a policy reconciler (`antenna-select`).

```
<site>/<station>/<slot>/meta      retained  birth certificate
<site>/<station>/<slot>/state     retained  live JSON snapshot
<site>/<station>/<slot>/status    retained  online | offline (LWT)
<site>/<station>/<slot>/cmd       varies    desired state / command
```

## The example station — Mühle Bauwagen (DL9ET)

The components in this repo are the Mühle station: an HF + UHF shack run from a Raspberry
Pi (`shari`) plus a couple of embedded nodes (ESPHome, M5 Stamp PLC). They are **examples**
of conformant adapters — each a thin bridge between one vendor's protocol and the canonical
model. None of them is the model; together they prove the model is buildable and livable.

| Project | Role | Slot address | Interface |
|---------|------|-------------|-----------|
| [flexbridge](flexbridge/) | FLEX-8/6xx radio bridge | `muehle/hf/radio` | Ethernet (SmartSDR API) |
| [ultrabridge](ultrabridge/) | Antenna controller (Ultrabeam RCU-06) | `muehle/hf/ant-ctrl` | USB-serial (FTDI) |
| [waveshare_relay-antswitch-bridge](waveshare_relay-antswitch-bridge/) | 1:6 antenna switch bridge | `muehle/hf/ant-switch` | wifi (ESPHome) |
| [antennaselect](antennaselect/) | Antenna-selection reconciler | `muehle/hf/antenna-select` | logic slot |
| [acom1200s-pa-bridge](acom1200s-pa-bridge/) | ACOM 1200S PA bridge | `muehle/hf/pa` | Serial (telemetry only) |
| [wrc-rotator-bridge](wrc-rotator-bridge/) | HF rotator bridge (Yaesu G-450DC via AF6SA WRC) | `muehle/hf/rotator` | WebSocket (AF6SA WRC) |
| [atr1k-tuner-bridge](atr1k-tuner-bridge/) | ATR-1000 ATU bridge | `muehle/hf/tuner` | wifi (binary WebSocket) |
| [shelly-power-bridge](shelly-power-bridge/) | Shelly smart-plug bridge (master mains + 13.8 V PSU) | `muehle/power/master`, `muehle/power/psu-13v8` | wifi (Gen2+ MQTT) |
| [m5stamp-hf-ctrl](m5stamp-hf-ctrl/) | M5 Stamp PLC firmware (PA/TRX remote-on + arm relay) | `muehle/hf/switch`, `muehle/hf/pa-arm` | wifi (embedded) |
| [powerseq](powerseq/) | Station startup/shutdown sequencer | `muehle/hf/power-seq` | logic slot |
| [hadiscovery](hadiscovery/) | Home Assistant discovery consumer | `muehle/hf/discovery` | logic slot — reads `/meta` |

`hadiscovery` is a passive consumer: it reads each slot's consumer-neutral `expose` block
from `/meta` and renders Home Assistant discovery (model §3.1, §9). The bridges carry
`expose` and contain no HA knowledge — a consumer can be swapped without touching any
bridge, which is the point of the plane discipline.

The **power-distribution layer** illustrates how the model composes: `shelly-power-bridge`
(site-level `muehle/power/*`) owns station mains and the 13.8 V DC rail; the M5 Stamp PLC
owns the PA/TRX remote-on relays (`hf/switch`) and the fail-safe-open PA arm relay
(`hf/pa-arm`); and `powerseq` drives the ordered, delay-and-confirmation startup/shutdown
of the whole chain. The ACOM PA bridge is a pure observer — its `set_power`/RTS wake-line
is retired; PA power-on comes from `hf/switch` (model §7.1).

### Bus topology

```
muehle/
  power/                          ← shelly-power-bridge   (site-level supplies)
    master/                          station master mains (Shelly)
    psu-13v8/                         13.8 V PSU (Shelly) — feeds HF + UHF
  hf/
    radio/           ← flexbridge       (FLEX-8400)
    ant-ctrl/        ← ultrabridge           (Ultrabeam RCU-06 — tunes one antenna)
    ant-switch/      ← waveshare_relay-antswitch-bridge  (1:6 antenna switch, dumb actuator)
    antenna-select/  ← antennaselect    (reconciler — picks the antenna)
    switch/          ← m5stamp-hf-ctrl  (M5 Stamp PLC #1 — PA + TRX remote-on relays)
    pa-arm/          ← m5stamp-hf-ctrl  (M5 Stamp PLC #1 — PA arm relay, fail-safe-open)
    power-seq/       ← powerseq          (startup/shutdown sequencer)
    pa/              ← acom1200s-pa-bridge (ACOM 1200S — telemetry only; powered via switch)
    rotator/         ← wrc-rotator-bridge   (Yaesu G-450DC via AF6SA WRC, websocket)
    tuner/           ← atr1k-tuner-bridge (ATR-1000 ATU, wifi)
    discovery/       ← hadiscovery      (HA discovery consumer — reads /meta.expose)
  host/
    shari/           ← shari RPi liveness
```

Antennas themselves are **passive resources** (no MQTT presence): `ant/ultrabeam` (port 3),
`ant/fan-dipole` 80/40 (port 6), `ant/dummy-load` (port 1). They live only in the
`antennaselect` wiring map; `ant-switch` routes to them and `antenna-select` decides which.

## Documentation

| Document | Description |
|----------|-------------|
| [Station integration model](docs/station-integration-model.md) | The artifact — three-plane MQTT contract, slot template, site instantiation |
| [Config and secrets](docs/conventions/config-and-secrets.md) | 0600 TOML file, seed-once deploy, EnvironmentFile pattern |
| [Deployment](docs/conventions/deployment.md) | Cross-compile, systemd hardening, udev rules, service management |
| [Band/mode reference](docs/conventions/band-mode-reference.md) | Canonical Hz ranges and mode names |
| [Bridge naming](docs/conventions/naming.md) | `<devtag>-<function>-bridge` for device bridges |
| [MQTT schema template](docs/templates/mqtt-schema.md) | Template for per-component MQTT API docs |

---

## Technical details

Everything below is implementation plumbing — how the example station is packaged, not what
it is. The model and the prose above are what matters.

This is one git repo. The Go components form a single
[Go workspace](https://go.dev/ref/mod#workspaces) (`go.work`); each keeps its own `go.mod`
and imports shared plumbing from a `codeberg.org/kgbvax/stationa/shared` module
(`shared/mqtt` — context-aware paho connect + background job queue; `shared/schema` —
topic helpers + the `/cmd` `value`-key convention). Each module's `go.mod` carries a
`replace … => ../shared` so it builds standalone; `go build ./...` at the repo root builds
them all. Non-Go components (ESPHome YAML, PlatformIO firmware) live alongside as plain
subdirectories and are not in `go.work`. Bridges import `shared/` but never another
bridge's `internal/` (Go's `internal/` rule enforces this across separate modules).

**MQTT broker:** a shack-local Mosquitto on shari (`127.0.0.1:1883` for shari
services, `192.168.1.139:1883` from the LAN), bridged to the HA broker at
`192.168.1.50:1883` (see `docs/conventions/mqtt-topology.md`). Persistent store.

**shari** — Raspberry Pi, `192.168.1.139`, user `io`; all Go services run here as hardened
systemd units.

```bash
ssh io@192.168.1.139
journalctl -u flexbridge -f          # follow a service's logs
sudo systemctl status flexbridge ultrabridge
```

### Adding a new component

1. Add the service as a new subdirectory with its own `go.mod` (and a
   `replace codeberg.org/kgbvax/stationa/shared => ../shared`), then list it in `go.work`.
2. Implement the `internal/config` pattern — see
   [config and secrets](docs/conventions/config-and-secrets.md).
3. Copy `deploy.sh` from an existing service and update the variables.
4. Write `CLAUDE.md` using the station-model shared-conventions section.
5. Write `docs/<component>-mqtt-api.md` using the
   [MQTT schema template](docs/templates/mqtt-schema.md).
6. Add the component to the table and bus diagram above, and a slot assignment to
   `CLAUDE.md`.

## License

Copyright © 2026 Ingomar Otter.

Licensed under the GNU Affero General Public License v3.0 or later
(SPDX: `AGPL-3.0-or-later`) — see [LICENSE](LICENSE).
