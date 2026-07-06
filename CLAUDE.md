# CLAUDE.md — stationa meta-repo

This repo contains shared documentation for the Mühle station automation ecosystem.
No Go code lives here. Each component project is a separate repo in the adjacent
directories.

---

## Projects and locations

| Project | Path | What it does |
|---------|------|-------------|
| flex2mqtt | `../flexbridge/` | FLEX-8400 radio → MQTT bridge |
| ubctrl | `../ubctrl/` | Ultrabeam RCU-06 antenna controller |
| acombridge | `../acombridge/` | ACOM 1200S PA bridge |
| pelcobridge | `../pelcobridge/` | Pelco-D rotator controller |

Each project has its own `CLAUDE.md` and is independently buildable. Open a project
by navigating into its directory.

---

## Slot assignments

| Slot address | Component | Physical device |
|-------------|-----------|-----------------|
| `muehle/hf/radio` | flex2mqtt | FLEX-8400, ethernet |
| `muehle/hf/antenna` | ubctrl | Ultrabeam RCU-06, USB-serial via FTDI |
| `muehle/hf/pa` | acombridge | ACOM 1200S, serial |
| `muehle/hf/rotator` | pelcobridge | Rotator, serial |

---

## shari — the deployment target

All services run on shari, a Raspberry Pi at `192.168.1.139`.

```bash
# SSH in
ssh io@192.168.1.139

# Check service status
sudo systemctl status flex2mqtt ubctrl

# Follow logs
journalctl -u flex2mqtt -f
journalctl -u ubctrl -f

# Config files (0600, owned by service user — contain MQTT credentials)
sudo cat /etc/flex2mqtt/config.toml    # MQTT password via EnvironmentFile, not here
sudo cat /etc/flex2mqtt/flex2mqtt.env  # FLEX2MQTT_MQTT_PASSWORD
sudo cat /etc/ubctrl/config.toml       # contains password directly

# Restart a service
sudo systemctl restart flex2mqtt
sudo systemctl restart ubctrl
```

The MQTT broker runs separately at `192.168.1.50:1883`.

---

## Shared documentation

All shared docs are in `docs/` in this repo:

| Document | Path |
|----------|------|
| Station integration model | `docs/station-integration-model.md` |
| Config and secrets convention | `docs/conventions/config-and-secrets.md` |
| Deployment convention | `docs/conventions/deployment.md` |
| Canonical band/mode reference | `docs/conventions/band-mode-reference.md` |
| MQTT schema template | `docs/templates/mqtt-schema.md` |

Each component's `CLAUDE.md` references these as `../../docs/` (relative to the
component repo adjacent to this one). The shared docs path relative to any component
is `../stationa/docs/` (or just `../docs/` if `stationa/` is used as the repo name).

---

## MQTT broker access

The MQTT broker is at `192.168.1.50:1883`. Credentials for the `hf` user are stored
on shari in the service config files. Do not pass credentials on the command line
or in shell history.

To inspect the bus from a workstation:

```bash
# Subscribe to all topics under muehle/
mosquitto_sub -h 192.168.1.50 -u hf -P "$MQTT_PASSWORD" -t 'muehle/#' -v

# Watch a single slot
mosquitto_sub -h 192.168.1.50 -u hf -P "$MQTT_PASSWORD" -t 'muehle/hf/radio/#' -v
```

---

## Conventions

All components follow these shared conventions:

1. **Three-plane MQTT schema** — `meta`, `state`, `status`, `cmd` per slot
   (see `docs/station-integration-model.md`)
2. **Single retained JSON state snapshot** — not per-field topics
3. **Config in 0600 TOML file** — never in ExecStart or command line
   (see `docs/conventions/config-and-secrets.md`)
4. **Seed-once deploy** — `deploy.sh` seeds the config once; device owns settings
5. **Hardened systemd unit** — NoNewPrivileges, ProtectSystem, PrivateTmp
   (see `docs/conventions/deployment.md`)
6. **freq_hz in Hz as integer** — never kHz or MHz on the bus
7. **Canonical mode names** — `cw`, `usb`, `lsb`, `am`, `fm`, `data`
   (see `docs/conventions/band-mode-reference.md`)
