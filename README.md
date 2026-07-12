# stationa

Documentation and conventions for the Mühle amateur radio station automation ecosystem.
Each component is a separate Go service; this repo contains only shared documentation.

---

## Components

| Project | Role | Slot address | Host | Interface |
|---------|------|-------------|------|-----------|
| [flexbridge](flexbridge/) | FLEX-8400 radio bridge | `muehle/hf/radio` | shari | Ethernet (SmartSDR API) |
| [ultrabridge](ultrabridge/) | Antenna controller (Ultrabeam RCU-06) | `muehle/hf/ant-ctrl` | shari | USB-serial (FTDI) |
| [antswitchbridge](antswitchbridge/) | 1:6 antenna switch bridge | `muehle/hf/ant-switch` | shari | wifi (contract-first) |
| [antennaselect](antennaselect/) | Antenna-selection reconciler | `muehle/hf/antenna-select` | shari | logic slot (core implemented) |
| [acombridge](acombridge/) | ACOM 1200S PA bridge | `muehle/hf/pa` | shari | Serial |
| [wrcrotorbridge](wrcrotorbridge/) | HF rotator bridge (Yaesu G-450DC via AF6SA WRC) | `muehle/hf/rotator` | shari | WebSocket (AF6SA WRC) |
| [pelcobridge](pelcobridge/) | Pelco-D rotator controller (UHF sat rotator) | `muehle/uhf/rotator` | shari | Serial |
| [atr1k-tuner-bridge](atr1k-tuner-bridge/) | ATR-1000 ATU bridge | `muehle/hf/tuner` | shari | wifi (binary WebSocket) |
| [hadiscovery](hadiscovery/) | Home Assistant discovery consumer | `muehle/hf/discovery` | shari | logic slot — reads `/meta`, renders HA discovery |

All components publish to a shared MQTT broker (`tcp://192.168.1.50:1883`) using the
three-plane station integration model. `hadiscovery` is a passive consumer: it reads each
slot's consumer-neutral `expose` block from `/meta` and renders Home Assistant discovery
(integration model §3.1, §9). The bridges carry `expose` and contain no HA knowledge.

---

## MQTT bus layout

```
muehle/
  hf/
    radio/           ← flexbridge       (FLEX-8400)
    ant-ctrl/        ← ultrabridge           (Ultrabeam RCU-06 — tunes one antenna)
    ant-switch/      ← antswitchbridge  (1:6 antenna switch, dumb actuator)
    antenna-select/  ← antennaselect    (reconciler — picks the antenna)
    pa/              ← acombridge       (ACOM 1200S)
    rotator/         ← wrcrotorbridge   (Yaesu G-450DC via AF6SA WRC, websocket)
    tuner/           ← atr1k-tuner-bridge (ATR-1000 ATU, wifi)
    discovery/       ← hadiscovery      (HA discovery consumer — reads /meta.expose)
  uhf/
    rotator/         ← pelcobridge      (UHF sat rotator, Pelco-D)
  host/
    shari/           ← shari RPi liveness
```

Antennas themselves are **passive resources** (no MQTT presence): `ant/ultrabeam` (port 3),
`ant/fan-dipole` 80/40 (port 2), `ant/dummy-load` (port 1). They live only in the
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
