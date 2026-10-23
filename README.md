# stationa

**stationa** is an experiment in **amateur-radio station integration**: the protocols
and topologies that are meant to let a pile of unrelated, vendor-locked boxes behave as one
coherent station. The main artifact is the [station integration model](docs/station-integration-model.md):
a strict, versioned grammar for how a station's components describe themselves and
coordinate. The software in this repo is a working instance of that model, the **Mühle**
station (DL9ET's shack). It exists to check the model against real hardware and to show
what a conformant adapter looks like.

## The problem

A modern ham station is often a collection of devices that were not designed to talk to
each other: a radio on one TCP API, an amplifier on a serial line, a rotator on a websocket, an
antenna switch on wifi, a smart-plug on MQTT, a PLC relay board on some vendor firmware.
Each speaks its own protocol, each is updated on its own schedule, and the "integration"
is often pairwise glue that breaks easily: a script per device, a dashboard
hard-coded to today's IP addresses, a mental model that lives in one operator's head. It
tends to go stale as the devices change, and it transfers poorly: rebuild the station, or
hand it to another operator, and most of the glue is of little use.

## The approach

The [integration model](docs/station-integration-model.md) treats every component
(device bridge, logic service, or passive consumer) as a small control loop with
**three separate planes that never share a path**:

- **Identity & capability** (`/meta`) - who I am, what I can do. Static, published on connect.
- **State** (`/state`) - what is currently true about me. Live, retained.
- **Command / intent** (`/cmd`) - what someone would like to be true. Transient.

The principles below follow from these planes (the model has the full argument):

- **The live configuration is the documentation.** A component, once connected, describes
  itself to a well-known address; anyone who wants to understand the station subscribes
  and reads. The inventory is always a live read; it is never kept as a separate document
  that can fall out of date. Things are referenced by canonical role and name, never by IP
  address or UDP port.
- **Change is constant; avoid tight coupling.** Components publish state to their own
  address and receive intent at their own address. Consumers react to **state**, never to
  the fact that a command was sent. Consumers only know the address, so the box behind it
  can be swapped without changes downstream.
- **Integration can be cheap when the core is strict.** The canonical vocabulary and slot
  template are versioned and changed with care; per-vendor adapters are thin and
  easy to replace. A GenAI-written adapter has a strict target to translate into, which
  helps keep many cheap adapters consistent with each other.

The model is transport-bound to MQTT and address-structured as `<site>/<station>/<slot>`
(e.g. `muehle/hf/radio`), with site-wide infrastructure at `<site>/power/<slot>` (e.g.
`muehle/power/psu-13v8`). Antennas are passive resources and do not get a slot;
routing among them is done by an actuator (`ant-switch`) driven by a policy reconciler
(`antenna-select`).

```
<site>/<station>/<slot>/meta      retained  birth certificate
<site>/<station>/<slot>/state     retained  live JSON snapshot
<site>/<station>/<slot>/status    retained  online | offline (LWT)
<site>/<station>/<slot>/cmd       varies    desired state / command
```

## The example station: Mühle Bauwagen (DL9ET)

The components in this repo are the Mühle station: an HF + UHF shack run from two
Raspberry Pis, `scmino` (a CM5 that hosts the station Mosquitto and almost every service)
and `shari` (the sat-ops rotator bridge and the bus recorder), plus the Windows shack PC
(`bwpc`, a sleep agent) and several embedded nodes (ESPHome, M5 Stamp PLCs, M5Stack control
heads). They are **examples** of conformant adapters, each a thin bridge between one
vendor's protocol and the canonical model. Together they show, for one station, that the model
can be built and run day to day.

| Project | Role | Slot address | Interface |
|---------|------|-------------|-----------|
| [flexbridge](flexbridge/) | FLEX-8/6xx radio bridge | `muehle/hf/radio` | Ethernet (SmartSDR API) |
| [ultrabridge](ultrabridge/) | Antenna controller (Ultrabeam RCU-06) | `muehle/hf/ant-ctrl` | USB-serial (FTDI) |
| [waveshare_relay-antswitch-bridge](waveshare_relay-antswitch-bridge/) | 1:6 antenna switch bridge | `muehle/hf/ant-switch` | wifi (ESPHome) |
| [antennaselect](antennaselect/) | Antenna-selection reconciler | `muehle/hf/antenna-select` | logic slot |
| [acom1200s-pa-bridge](acom1200s-pa-bridge/) | ACOM 1200S PA bridge (operate/standby, band; power state read-only) | `muehle/hf/pa` | Serial (USB, Prolific) |
| [wrc-rotator-bridge](wrc-rotator-bridge/) | HF rotator bridge (Yaesu G-450DC via AF6SA WRC) | `muehle/hf/rotator` | WebSocket (AF6SA WRC) |
| [atr1k-tuner-bridge](atr1k-tuner-bridge/) | ATR-1000 ATU bridge | `muehle/hf/tuner` | wifi (binary WebSocket) |
| [shelly-power-bridge](shelly-power-bridge/) | Shelly smart-plug bridge (13.8 V PSU, antenna power, shack PC mains) | `muehle/power/psu-13v8`, `muehle/power/antennas`, `muehle/power/shack-pc-mains` | wifi (Shelly MQTT, Gen1 + Gen2+) |
| [m5stamp-hf-ctrl](m5stamp-hf-ctrl/) | M5 Stamp PLC firmware (PA/TRX remote-on + arm relay) | `muehle/hf/switch`, `muehle/hf/pa-arm` | wifi (embedded) |
| [powerseq](powerseq/) | Station startup/shutdown sequencer | `muehle/hf/power-seq` | logic slot |
| [hadiscovery](hadiscovery/) | Home Assistant discovery consumer | `muehle/hf/discovery` | logic slot (reads `/meta`) |
| [oscarwatch-sattrack-bridge](oscarwatch-sattrack-bridge/) | Satellite-tracking source (OscarWatch Satellite link) | `muehle/uhf/sat-track` | WebSocket (OscarWatch, read-only) |
| [winpc-power-bridge](winpc-power-bridge/) | Shack-PC sleep agent (Windows, runs on `bwpc`; `sleep` is refused while the operator uses the PC) | `muehle/power/shack-pc` | ethernet (offline = asleep) |
| [logger-spot-bridge](logger-spot-bridge/) | Shack-logger bridge (DXLog/Log4OM): the operator-keyed station (call, position, beam bearing) | `muehle/hf/spots` | LAN UDP broadcasts |
| [beamsteer](beamsteer/) | Smart rotation: emulates PstRotator for the contest logger; flips the Ultrabeam 180° or rotates to the cheaper lobe | `muehle/hf/beam-steer` | logic slot (PstRotator UDP :12050) |
| [antmode](antmode/) | Antenna-mode lock (normal/storm/maintenance) for the Ultrabeam and the HF rotator; polls NINA/DWD | `muehle/hf/ant-mode` | logic slot |
| [icom9700-radio-bridge](icom9700-radio-bridge/) | IC-9700 UHF radio bridge (demand-driven RX audio capture; `park_radio` retune via rigctld-ic9700; read-only serial CI-V telemetry supported but disabled on scmino; no remote TX) | `muehle/uhf/radio` | LAN (RS-BA1, audio); CI-V via rigctld-ic9700 (park only) |
| [spid-ercm-rotator-bridge](spid-ercm-rotator-bridge/) | Sat-ops az/el rotator bridge (SPID azimuth, GS-500 elevation via ERC-M); rotctld :4534, PstRotator UDP :12041, GS-232B TCP :4533 | `muehle/uhf/az-rotator`, `muehle/uhf/el-rotator` | Serial (Rot1Prog, GS-232B) |
| [m5stamp-pol-ctrl](m5stamp-pol-ctrl/) | M5 Stamp PLC #2 firmware (X-Quad polarization; not yet flashed per `docs/known-issues.md`) | `muehle/uhf/pol-ctrl` | wifi (ESPHome) |
| [vhfcam-restream](vhfcam-restream/) | VHF camera (UniFi Protect RTSPS) restreamer for YouTube Live and a LAN preview, with the operational-data overlay | `muehle/hf/vhfcam` | minimal `/status` + `/state` (accessory; not in the console's `expectedSlots`) |

`hadiscovery` is a passive consumer: it reads each slot's consumer-neutral `expose` block
from `/meta` and renders Home Assistant discovery (model §3.1, §9). The bridges carry
`expose`. The legacy embedded HA discovery left in a few of them is off by default
(`publish_ha_discovery = false`) and due for removal (model §9). Because consumers only
read what the bridges publish, a consumer can be swapped without touching any
bridge, which is one reason for keeping the planes separate.

Consumers, control heads and infrastructure that are not slots:

| Project | Role | Runs on |
|---------|------|---------|
| [hf_console](hf_console/) | Flutter operator console (Android tablet, iPhone, web via the `hf-console-web` bridge on :8091) | tablet / phone; web bridge on scmino |
| [testui](testui/) | MQTT relay + schema-aware browser UI for the bus (:8090) | scmino |
| [stationportal](stationportal/) | Station landing page (:80): probed service links, live slot/hardware inventory from `/meta` | scmino |
| [m5dial-hf-rotctrl](m5dial-hf-rotctrl/) | M5Stack Dial firmware: analog-meter control head for the HF rotator | embedded |
| [m5dualkey-hf-antctrl](m5dualkey-hf-antctrl/) | M5Stack Chain DualKey firmware: Ultrabeam direction keys (a chained Chain Key toggles DVK memory 2 on `hf/radio`) | embedded |
| [mqtt-broker](mqtt-broker/) | Station Mosquitto config and deploy, bridged to the HA broker (see the broker note below) | scmino |
| [hf-mqtt-capture](hf-mqtt-capture/) | Passive recorder of the HF bus (`muehle/hf/#`) to hourly log files | shari |

The **power-distribution layer** shows how the model's slots fit together: `shelly-power-bridge`
(site-level `muehle/power/psu-13v8`, `…/antennas`, `…/shack-pc-mains`) owns the 13.8 V DC
rail, the antenna power plug and the shack-PC mains plug; `winpc-power-bridge`
(`muehle/power/shack-pc`) puts the shack PC to sleep; the site master mains is a Home
Assistant-owned plug outside StationA; the M5 Stamp PLC owns the PA/TRX remote-on relays
(`hf/switch`) and the fail-safe-open PA arm relay (`hf/pa-arm`); and `powerseq` runs the
startup/shutdown of the whole chain in order, with delays and confirmations. Optionally
`powerseq` also runs idle standby: antenna power off and the shack PC asleep while the
station is inactive, then Wake-on-LAN and antenna power on at wake. It can also run a full
idle shutdown a configurable time after the last activity (`[idle] shutdown_minutes`, 120
in the example config). The ACOM PA bridge only observes the amplifier's power state: its
`set_power`/RTS wake-line is retired, and PA power-on comes from `hf/switch` (model §7.1).

### Bus topology

```
muehle/
  power/                            (site-level supplies)
    psu-13v8/        ← shelly-power-bridge  (13.8 V PSU, Shelly - feeds HF + UHF)
    antennas/        ← shelly-power-bridge  (antenna power: HF + UHF rotators, Ultrabeam RCU-06)
    shack-pc-mains/  ← shelly-power-bridge  (shack PC mains, Shelly)
    shack-pc/        ← winpc-power-bridge   (shack PC sleep agent on bwpc; offline = asleep)
  hf/
    radio/           ← flexbridge       (FLEX-8400)
    ant-ctrl/        ← ultrabridge           (Ultrabeam RCU-06 - tunes one antenna)
    ant-switch/      ← waveshare_relay-antswitch-bridge  (1:6 antenna switch, dumb actuator)
    antenna-select/  ← antennaselect    (reconciler - picks the antenna)
    switch/          ← m5stamp-hf-ctrl  (M5 Stamp PLC #1 - PA + TRX remote-on relays)
    pa-arm/          ← m5stamp-hf-ctrl  (M5 Stamp PLC #1 - PA arm relay, fail-safe-open)
    power-seq/       ← powerseq          (startup/shutdown sequencer)
    pa/              ← acom1200s-pa-bridge (ACOM 1200S - operate/standby + band; power read-only, powered via switch)
    rotator/         ← wrc-rotator-bridge   (Yaesu G-450DC via AF6SA WRC, websocket)
    tuner/           ← atr1k-tuner-bridge (ATR-1000 ATU, wifi)
    discovery/       ← hadiscovery      (HA discovery consumer - reads /meta.expose)
    spots/           ← logger-spot-bridge (DXLog/Log4OM - the operator-keyed station)
    beam-steer/      ← beamsteer        (smart rotation - PstRotator UDP emulator)
    ant-mode/        ← antmode          (antenna-mode lock: normal/storm/maintenance)
    vhfcam/          ← vhfcam-restream  (VHF cam restreamer - minimal /status + /state)
  uhf/
    radio/           ← icom9700-radio-bridge (IC-9700 - RX audio; serial CI-V telemetry off on scmino)
    sat-track/       ← oscarwatch-sattrack-bridge (OscarWatch satellite link, read-only)
    az-rotator/      ← spid-ercm-rotator-bridge (SPID azimuth, serial)
    el-rotator/      ← spid-ercm-rotator-bridge (GS-500 elevation via ERC-M, serial)
    pol-ctrl/        ← m5stamp-pol-ctrl (M5 Stamp PLC #2 - X-Quad polarization; not yet flashed)
```

Antennas themselves are **passive resources** (no MQTT presence): `ant/ultrabeam` (port 4
as seeded; on-site confirmation open, see `docs/known-issues.md`), `ant/fan-dipole` 80/40
(port 6), `ant/dummy-load` (port 1). They live only in the `antennaselect` wiring map;
`ant-switch` routes to them and `antenna-select` decides which.

## Documentation

| Document | Description |
|----------|-------------|
| [Station integration model](docs/station-integration-model.md) | The main artifact: three-plane MQTT contract, slot template, site instantiation |
| [Config and secrets](docs/conventions/config-and-secrets.md) | 0600 TOML file, seed-once deploy, EnvironmentFile pattern |
| [Deployment](docs/conventions/deployment.md) | Cross-compile, systemd hardening, udev rules, service management |
| [Band/mode reference](docs/conventions/band-mode-reference.md) | Canonical Hz ranges and mode names |
| [Bridge naming](docs/conventions/naming.md) | `<devtag>-<function>-bridge` for device bridges |
| [MQTT schema template](docs/templates/mqtt-schema.md) | Template for per-component MQTT API docs |
| [MQTT broker topology](docs/conventions/mqtt-topology.md) | Broker layout, bridge to the HA broker, topic directions and ACLs |
| [Known issues / decisions](docs/known-issues.md) | Station-wide register of open issues, safety gaps and decisions |
| [Runtime library](docs/conventions/runtime-library.md) | Constraints on the shared `shared/` module |
| [Logging](docs/conventions/logging.md) | `log/slog` to stderr with `component`/`slot` attrs; journald priorities |
| [Logging integration model](docs/logging-integration-model.md) | Draft companion to the model: the logging and operating layer |
| [System overview](OVERVIEW.md) | Short prose overview of the bus and its capabilities |

---

## Technical details

Everything below is implementation detail about how the example station is packaged.

This is one git repo. The Go components form a single
[Go workspace](https://go.dev/ref/mod#workspaces) (`go.work`); each keeps its own `go.mod`
and imports shared code from a `codeberg.org/kgbvax/stationa/shared` module
(`shared/mqtt`: context-aware paho connect + background job queue; `shared/schema`:
topic helpers + the `/cmd` `value`-key convention; `shared/logging`: the slog/journald
root handler; `shared/pstrotator`: the PstRotator UDP listener; `shared/antmode`: the
`hf/ant-mode` lock parser). Each module's `go.mod` carries a `replace … => ../shared` so it
builds standalone; `go build $(go list -m -f '{{.Dir}}/...')` at the repo root builds them
all (a plain `./...` does not work there, because the root is not a module). Non-Go
components (ESPHome YAML, PlatformIO firmware, the Flutter console `hf_console` with its own
small Go web bridge, and the Mosquitto config in `mqtt-broker/`) live alongside as plain
subdirectories and are not in `go.work`. Bridges import `shared/` but never another
bridge's `internal/` (Go's `internal/` rule enforces this across separate modules).

**MQTT broker:** the station Mosquitto runs on scmino (`192.168.1.178:1883`, persistent
store; `mqtt-broker/`), bridged to the Home Assistant broker at `192.168.1.50:1883` as a
transitional `muehle/#` mirror. Clients move to it one at a time. Per `mqtt-broker/CLAUDE.md`
the M5 Stamp PLC #1 and shelly-power-bridge (with its Shelly plugs) use it, and
most of the others still connect to `192.168.1.50` (root `CLAUDE.md`, 2026-10-18); each service's seed-once
on-device config sets its own broker, so check there. The original 2026-08 design put this
broker on shari; it was never deployed there. `docs/conventions/mqtt-topology.md` describes the
target end state, with the scmino broker authoritative for `muehle/#`.

**scmino** (Raspberry Pi CM5, `192.168.1.178`, user `io`): the station broker and almost
every Go service, as hardened systemd units. **shari** (Raspberry Pi, `192.168.1.139`,
same user): `spid-ercm-rotator-bridge` and `hf-mqtt-capture`. `winpc-power-bridge` runs on
the Windows shack PC `bwpc` (`192.168.1.197`).

```bash
ssh io@192.168.1.178                 # scmino
journalctl -u flexbridge -f          # follow a service's logs
sudo systemctl status flexbridge ultrabridge
```

### Adding a new component

1. Add the service as a new subdirectory with its own `go.mod` (and a
   `replace codeberg.org/kgbvax/stationa/shared => ../shared`), then list it in `go.work`.
2. Implement the `internal/config` pattern; see
   [config and secrets](docs/conventions/config-and-secrets.md).
3. Copy `deploy.sh` from an existing service and update the variables.
4. Write `CLAUDE.md` using the station-model shared-conventions section.
5. Write `docs/<component>-mqtt-api.md` using the
   [MQTT schema template](docs/templates/mqtt-schema.md).
6. Set up the root logger per [logging](docs/conventions/logging.md) §1.
7. Add the slot to the model's site instantiation (`docs/station-integration-model.md` §7,
   including the §7.3 host table) and to the table and bus diagram above; add the project
   and slot rows to the root `CLAUDE.md`.
8. Add the slot (`[[slot]]`) and component (`[[software]]`) to
   `stationportal/internal/inventory/inventory.toml` and redeploy the portal; add the slot
   to `hf_console/lib/store/station_hosts.dart` (and, if the console uses it, `wiring.dart`
   `expectedSlots`/`cmdRetain`). `go test` / `flutter test` fail on a missing or stale
   entry.

## License

Copyright © 2026 Ingomar Otter.

Licensed under the GNU Affero General Public License v3.0 or later
(SPDX: `AGPL-3.0-or-later`) - see [LICENSE](LICENSE).
