# CLAUDE.md — stationa meta-repo

This repo contains shared documentation for the Mühle station automation ecosystem.
No Go code lives here at the top level. Each component project is a separately-tracked
git repo nested as a subdirectory of this one (and gitignored here).

---

## Projects and locations

| Project | Path | What it does |
|---------|------|-------------|
| flexbridge | `flexbridge/` | FLEX-8400 radio → MQTT bridge |
| ultrabridge | `ultrabridge/` | Ultrabeam RCU-06 controller (tunes one antenna) |
| acom1200s-pa-bridge | `acom1200s-pa-bridge/` | ACOM 1200S PA bridge |
| wrcrotorbridge | `wrcrotorbridge/` | HF rotator bridge (Yaesu G-450DC via AF6SA WRC, websocket) |
| pelcobridge | `pelcobridge/` | Pelco-D rotator controller (UHF sat rotator) |
| atr1k-tuner-bridge | `atr1k-tuner-bridge/` | ATR-1000 ATU bridge (in-line/bypass + tune, binary WebSocket) |
| antswitchbridge | `antswitchbridge/` | 1:6 antenna switch bridge (ESPHome) |
| antennaselect | `antennaselect/` | Antenna-selection reconciler (core implemented) |
| hadiscovery | `hadiscovery/` | Home Assistant discovery consumer (reads `/meta` `expose`, renders HA discovery) |

Each project has its own `CLAUDE.md` and is independently buildable. Open a project
by navigating into its directory.

---

## Slot assignments

| Slot address | Component | Physical device |
|-------------|-----------|-----------------|
| `muehle/hf/radio` | flexbridge | FLEX-8400, ethernet |
| `muehle/hf/ant-ctrl` | ultrabridge | Ultrabeam RCU-06, USB-serial via FTDI |
| `muehle/hf/ant-switch` | antswitchbridge | 1:6 antenna switch, wifi (ESPHome) |
| `muehle/hf/antenna-select` | antennaselect | logic slot — no device (runs on shari) |
| `muehle/hf/pa` | acom1200s-pa-bridge | ACOM 1200S, serial |
| `muehle/hf/rotator` | wrcrotorbridge | Yaesu G-450DC via AF6SA WRC, websocket |
| `muehle/hf/tuner` | atr1k-tuner-bridge | ATR-1000 ATU, wifi (binary WebSocket) |
| `muehle/uhf/rotator` | pelcobridge | UHF sat rotator, Pelco-D, serial |
| `muehle/hf/discovery` | hadiscovery | logic slot — no device (runs on shari); passive consumer of `/meta` |

**Antennas are not slots.** `ant-ctrl` is the *controller* that tunes the Ultrabeam
(canonical role `ant-ctrl`; the device name Ultrabeam RCU-06 lives in `/meta.device`,
never in the address), not "the antenna." The physical antennas are **passive
resources** — `ant/ultrabeam` (port 3), `ant/fan-dipole` 80/40 (port 6),
`ant/dummy-load` (port 1) — with no MQTT presence; they exist only in the
`antennaselect` wiring map. Routing among them is `ant-switch` (actuator) driven by
`antenna-select` (policy). See the integration model §2–§4, §7.1.

---

## shari — the deployment target

All services run on shari, a Raspberry Pi at `192.168.1.139`.

```bash
# SSH in
ssh io@192.168.1.139

# Check service status
sudo systemctl status flexbridge ultrabridge

# Follow logs
journalctl -u flexbridge -f
journalctl -u ultrabridge -f

# Config files (0600, owned by service user — contain MQTT credentials)
sudo cat /etc/flexbridge/config.toml    # MQTT password via EnvironmentFile, not here
sudo cat /etc/flexbridge/flexbridge.env # FLEXBRIDGE_MQTT_PASSWORD
sudo cat /etc/ultrabridge/config.toml        # contains password directly

# Restart a service
sudo systemctl restart flexbridge
sudo systemctl restart ultrabridge
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
| Bridge-naming convention | `docs/conventions/naming.md` |
| Canonical band/mode reference | `docs/conventions/band-mode-reference.md` |
| MQTT schema template | `docs/templates/mqtt-schema.md` |

The component repos are nested inside this one, so the shared docs path relative to
any component is `../docs/` (e.g. `../docs/station-integration-model.md` from
`flexbridge/`).

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
8. **Bridge naming** — device bridges are `<devtag>-<function>-bridge` where
   `<devtag>` is the device family/control interface (e.g. `atr1k-tuner-bridge`)
   (see `docs/conventions/naming.md`)
