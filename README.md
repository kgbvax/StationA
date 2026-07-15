# stationa

The Mühle amateur radio station automation ecosystem — one git repo holding the shared
documentation/conventions **and** all component projects as subdirectories. The Go
components form a single [Go workspace](https://go.dev/ref/mod#workspaces) (`go.work`);
each keeps its own `go.mod` and imports shared plumbing from a
`codeberg.org/kgbvax/stationa/shared` module (`shared/mqtt`, `shared/schema`).
Non-Go components (ESPHome YAML, PlatformIO firmware) live alongside as plain
subdirectories. `go build ./...` at the repo root builds every Go component.

---

## Components

| Project | Role | Slot address | Host | Interface |
|---------|------|-------------|------|-----------|
| [flexbridge](flexbridge/) | FLEX-8400 radio bridge | `muehle/hf/radio` | shari | Ethernet (SmartSDR API) |
| [ultrabridge](ultrabridge/) | Antenna controller (Ultrabeam RCU-06) | `muehle/hf/ant-ctrl` | shari | USB-serial (FTDI) |
| [waveshare_relay-antswitch-bridge](waveshare_relay-antswitch-bridge/) | 1:6 antenna switch bridge | `muehle/hf/ant-switch` | embedded (ESPHome) | wifi (contract-first) |
| [antennaselect](antennaselect/) | Antenna-selection reconciler | `muehle/hf/antenna-select` | shari | logic slot (core implemented) |
| [acom1200s-pa-bridge](acom1200s-pa-bridge/) | ACOM 1200S PA bridge | `muehle/hf/pa` | shari | Serial (power telemetry only; PA power-on via `hf/switch`) |
| [wrc-rotator-bridge](wrc-rotator-bridge/) | HF rotator bridge (Yaesu G-450DC via AF6SA WRC) | `muehle/hf/rotator` | shari | WebSocket (AF6SA WRC) |
| [pelcobridge](pelcobridge/) | Pelco-D rotator controller (UHF sat rotator) | `muehle/uhf/rotator` | shari | Serial |
| [atr1k-tuner-bridge](atr1k-tuner-bridge/) | ATR-1000 ATU bridge | `muehle/hf/tuner` | shari | wifi (binary WebSocket) |
| [shelly-power-bridge](shelly-power-bridge/) | Shelly smart-plug bridge (station master + 13.8 V PSU) | `muehle/power/master`, `muehle/power/psu-13v8` | shari | wifi (Gen2+ MQTT) |
| [m5stamp-hf-ctrl](m5stamp-hf-ctrl/) | M5 Stamp PLC #1 firmware (PA/TRX remote-on + arm relay) | `muehle/hf/switch`, `muehle/hf/pa-arm` | embedded (M5 Stamp PLC) | wifi (compound node, embedded arm logic) |
| [powerseq](powerseq/) | Station startup/shutdown sequencer | `muehle/hf/power-seq` | shari | logic slot (no device) |
| [hadiscovery](hadiscovery/) | Home Assistant discovery consumer | `muehle/hf/discovery` | shari | logic slot — reads `/meta`, renders HA discovery |

All components publish to a shared MQTT broker (`tcp://192.168.1.50:1883`) using the
three-plane station integration model. `hadiscovery` is a passive consumer: it reads each
slot's consumer-neutral `expose` block from `/meta` and renders Home Assistant discovery
(integration model §3.1, §9). The bridges carry `expose` and contain no HA knowledge.

The **power-distribution layer** (`shelly-power-bridge` at site-level
`muehle/power/*`) owns station mains and the 13.8 V DC rail; the **M5 Stamp PLC**
(`m5stamp-hf-ctrl`) owns the PA/TRX remote-on relays (`hf/switch`) and the
fail-safe-open PA arm relay (`hf/pa-arm`, arm logic embedded); and `powerseq`
drives the ordered startup/shutdown of the whole chain. The ACOM PA bridge is
now a pure observer — its `set_power`/RTS wake-line is retired; PA power-on
comes from `hf/switch` (integration model §7.1).

---

## MQTT bus layout

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
  uhf/
    rotator/         ← pelcobridge      (UHF sat rotator, Pelco-D)
  host/
    shari/           ← shari RPi liveness
```

Antennas themselves are **passive resources** (no MQTT presence): `ant/ultrabeam` (port 3),
`ant/fan-dipole` 80/40 (port 6), `ant/dummy-load` (port 1). They live only in the
`antenna-select` wiring map; `ant-switch` routes to them and `antenna-select` decides which.

Each slot publishes four topics:

```
<site>/<station>/<slot>/meta      retained  birth certificate
<site>/<station>/<slot>/state     retained  live JSON snapshot
<site>/<station>/<slot>/status    retained  online | offline (LWT)
<site>/<station>/<slot>/cmd       varies    desired state / command
```

---

## Shared documentation

| Document | Description |
|----------|-------------|
| [Station integration model](docs/station-integration-model.md) | Three-plane MQTT contract, slot template, site instantiation |
| [Config and secrets](docs/conventions/config-and-secrets.md) | 0600 TOML file, seed-once deploy, EnvironmentFile pattern |
| [Deployment](docs/conventions/deployment.md) | Cross-compile, systemd hardening, udev rules, shari service management |
| [Band/mode reference](docs/conventions/band-mode-reference.md) | Canonical Hz ranges and mode names |
| [Bridge naming](docs/conventions/naming.md) | `<devtag>-<function>-bridge` for device bridges |
| [MQTT schema template](docs/templates/mqtt-schema.md) | Template for per-component MQTT API docs |

---

## Infrastructure

**MQTT broker:** `tcp://192.168.1.50:1883` (Mosquitto, persistent store)

**shari** — Raspberry Pi, `192.168.1.139`, user `io`

```bash
ssh io@192.168.1.139

# Logs
journalctl -u flexbridge -f
journalctl -u ultrabridge -f

# Status
sudo systemctl status flexbridge ultrabridge
```

---

## Adding a new component

1. Create the service as a new Go repo nested in this one (own `.git`, add the
   directory to this repo's `.gitignore`).
2. Implement the `internal/config` package pattern — see `docs/conventions/config-and-secrets.md`.
3. Copy `deploy.sh` from an existing service and update the variables.
4. Write `CLAUDE.md` using the station model shared-conventions section.
5. Write `docs/<component>-mqtt-api.md` using `docs/templates/mqtt-schema.md`.
6. Add the component to the table and bus diagram above.
7. Add a slot assignment to `CLAUDE.md` in this repo.
