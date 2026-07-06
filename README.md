# stationa

Documentation and conventions for the Mühle amateur radio station automation ecosystem.
Each component is a separate Go service; this repo contains only shared documentation.

---

## Components

| Project | Role | Slot address | Host | Interface |
|---------|------|-------------|------|-----------|
| [flex2mqtt](flexbridge/) | FLEX-8400 radio bridge | `muehle/hf/radio` | shari | Ethernet (SmartSDR API) |
| [ubctrl](ubctrl/) | Ultrabeam RCU-06 controller | `muehle/hf/antenna` | shari | USB-serial (FTDI) |
| [acombridge](acombridge/) | ACOM 1200S PA bridge | `muehle/hf/pa` | shari | Serial |
| [pelcobridge](pelcobridge/) | Pelco-D rotator controller | `muehle/hf/rotator` | shari | Serial |

All components publish to a shared MQTT broker (`tcp://192.168.1.50:1883`) using the
three-plane station integration model.

---

## MQTT bus layout

```
muehle/
  hf/
    radio/      ← flex2mqtt    (FLEX-8400)
    antenna/    ← ubctrl       (Ultrabeam RCU-06)
    pa/         ← acombridge   (ACOM 1200S)
    rotator/    ← pelcobridge  (rotator)
  host/
    shari/      ← shari RPi liveness
```

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
| [MQTT schema template](docs/templates/mqtt-schema.md) | Template for per-component MQTT API docs |

---

## Infrastructure

**MQTT broker:** `tcp://192.168.1.50:1883` (Mosquitto, persistent store)

**shari** — Raspberry Pi, `192.168.1.139`, user `io`

```bash
ssh io@192.168.1.139

# Logs
journalctl -u flex2mqtt -f
journalctl -u ubctrl -f

# Status
sudo systemctl status flex2mqtt ubctrl
```

---

## Adding a new component

1. Create the service as a new Go repo adjacent to this one.
2. Implement the `internal/config` package pattern — see `docs/conventions/config-and-secrets.md`.
3. Copy `deploy.sh` from an existing service and update the variables.
4. Write `CLAUDE.md` using the station model shared-conventions section.
5. Write `docs/<component>-mqtt-api.md` using `docs/templates/mqtt-schema.md`.
6. Add the component to the table and bus diagram above.
7. Add a slot assignment to `CLAUDE.md` in this repo.
