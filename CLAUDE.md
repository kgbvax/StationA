# CLAUDE.md — stationa monorepo

This is a single git repository holding the whole Mühle station-automation ecosystem:
shared documentation **and** all component projects as subdirectories. The Go
components are a [Go workspace](https://go.dev/ref/mod#workspaces) tied together by a
root `go.work`; each component keeps its own `go.mod` (per-module, independently
`go build`/`go test`-able). A shared module, `codeberg.org/kgbvax/stationa/shared`,
holds cross-cutting plumbing (`shared/mqtt`, `shared/schema`, later `shared/config`);
every Go component imports it via a `replace … => ../shared` so each stays self-building
without the workspace. Bridges import `shared/` but never another bridge's `internal/`
— enforced by Go's `internal/` visibility rule across separate modules, not just
convention. Non-Go components (`waveshare_relay-antswitch-bridge` = ESPHome YAML,
`m5stamp-hf-ctrl` and `m5dial-hf-rotctrl` = PlatformIO firmware) live alongside as plain
subdirectories and are not in `go.work`.

The projects below were previously standalone git repos nested here and gitignored;
they have been folded into this repo with history (`git subtree`). There are no longer
separate per-component remotes to push to.

---

## Projects and locations

| Project | Path | What it does |
|---------|------|-------------|
| flexbridge | `flexbridge/` | FLEX-8400 radio → MQTT bridge |
| ultrabridge | `ultrabridge/` | Ultrabeam RCU-06 controller (tunes one antenna) |
| acom1200s-pa-bridge | `acom1200s-pa-bridge/` | ACOM 1200S PA bridge |
| wrc-rotator-bridge | `wrc-rotator-bridge/` | HF rotator bridge (Yaesu G-450DC via AF6SA WRC, websocket) |
| atr1k-tuner-bridge | `atr1k-tuner-bridge/` | ATR-1000 ATU bridge (in-line/bypass + tune, binary WebSocket) |
| waveshare_relay-antswitch-bridge | `waveshare_relay-antswitch-bridge/` | 1:6 antenna switch bridge (ESPHome, WaveShare relay-board family) |
| shelly-power-bridge | `shelly-power-bridge/` | Shelly smart-plug bridge → `power/master` + `power/psu-13v8` (supply layer) |
| m5stamp-hf-ctrl | `m5stamp-hf-ctrl/` | M5 Stamp PLC #1 firmware → `hf/pa-arm` + `hf/switch` (PA/TRX remote-on + arm) |
| powerseq | `powerseq/` | Startup/shutdown sequencer → `hf/power-seq` (ordered, delay + liveness confirmations) |
| antennaselect | `antennaselect/` | Antenna-selection reconciler (core implemented) |
| hadiscovery | `hadiscovery/` | Home Assistant discovery consumer (reads `/meta` `expose`, renders HA discovery) |
| pelcobridge2 | `pelcobridge2/` | UHF rotator TUI + rotctld server (Pelco-D/P pan/tilt head over RS-485) |
| m5dial-hf-rotctrl | `m5dial-hf-rotctrl/` | M5Stack Dial firmware — HF rotator control head (analog meter face + knob; not a slot; consumer + /cmd stimulator) |
| testui | `testui/` | MQTT relay + schema-aware browser UI for the bus (not a slot; passive consumer + /cmd stimulator) |
| mqtt-broker | `mqtt-broker/` | Shack-local Mosquitto broker on shari, bridged to the HA broker (infra — not a slot, not Go) |

Each project has its own `CLAUDE.md` and is independently buildable (`go build`/`go test`
from its own directory works without the workspace, via the `replace … => ../shared`).
Open a project by navigating into its directory. From the repo root, `go build ./...`
and `go work sync` operate over the whole workspace at once.

---

## Slot assignments

| Slot address | Component | Physical device |
|-------------|-----------|-----------------|
| `muehle/power/master` | shelly-power-bridge | Shelly plug — station master mains, wifi |
| `muehle/power/psu-13v8` | shelly-power-bridge | Shelly plug — 13.8 V PSU (site-level; feeds HF+UHF), wifi |
| `muehle/hf/radio` | flexbridge | FLEX-8400, ethernet |
| `muehle/hf/ant-ctrl` | ultrabridge | Ultrabeam RCU-06, USB-serial via FTDI |
| `muehle/hf/ant-switch` | waveshare_relay-antswitch-bridge | 1:6 antenna switch, wifi (ESPHome) |
| `muehle/hf/switch` | m5stamp-hf-ctrl | M5 Stamp PLC #1 — PA/TRX remote-on relays (relays 3 & 4), wifi |
| `muehle/hf/pa-arm` | m5stamp-hf-ctrl | M5 Stamp PLC #1 — PA arm relay (relay 1), wifi |
| `muehle/hf/antenna-select` | antennaselect | logic slot — no device (runs on shari) |
| `muehle/hf/pa` | acom1200s-pa-bridge | ACOM 1200S, serial (`set_power`/RTS removed; `power` is telemetry only) |
| `muehle/hf/rotator` | wrc-rotator-bridge | Yaesu G-450DC via AF6SA WRC, websocket |
| `muehle/hf/tuner` | atr1k-tuner-bridge | ATR-1000 ATU, wifi (binary WebSocket) |
| `muehle/hf/power-seq` | powerseq | logic slot — no device (runs on shari); startup/shutdown sequencer |
| `muehle/uhf/pol-ctrl` | m5stamp-hf-ctrl (PLC #2) | M5 Stamp PLC #2 — X-Quad polarization, wifi |
| `muehle/uhf/rotator` | pelcobridge2 | PTS-303Z/3050DZ pan/tilt head, RS-485 — interactive TUI on shack-pc (arming is manual, never remote) |
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

The MQTT broker is the **shack-local Mosquitto on shari** (`127.0.0.1:1883`
for shari-local services, `192.168.1.139:1883` from the LAN), bridged to the
Home Assistant broker at `192.168.1.50:1883`. See `mqtt-broker/` and
`docs/conventions/mqtt-topology.md`.

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
| MQTT broker topology | `docs/conventions/mqtt-topology.md` |
| MQTT schema template | `docs/templates/mqtt-schema.md` |
| Station-wide known issues / decisions register | `docs/known-issues.md` |
| Runtime-library constraints (shared/ contracts) | `docs/conventions/runtime-library.md` |
| Logging convention (slog, stderr, component/slot attrs) | `docs/conventions/logging.md` |

All components live as subdirectories of this one repo, so the shared docs path
relative to any component is `../docs/` (e.g. `../docs/station-integration-model.md`
from `flexbridge/`). Cross-cutting Go code, not docs, lives in the `shared/` module
(see the intro); components reference shared docs as `../docs/…` and shared code as
`codeberg.org/kgbvax/stationa/shared/…`.

---

## MQTT broker access

The station runs a **shack-local Mosquitto broker on shari** (`mqtt-broker/`),
authoritative for the `muehle/#` namespace. A mosquitto `bridge` connection
replicates it to the Home Assistant broker at `192.168.1.50:1883` (HA's own
Mosquitto add-on), which stays untouched — it still serves HA's other MQTT
devices. On-shari Go services talk to `127.0.0.1:1883`; remote clients (Shelly
plugs, M5 PLC, ant-switch ESP, the console tablet, workstations) use
`192.168.1.139:1883`. See `docs/conventions/mqtt-topology.md` for the full
topology, topic-direction table, and ACLs.

Credentials: the `hf` (station services), `bridge` (HA bridge connection), and
`console` (tablet) accounts are seeded once on shari in `/etc/mosquitto/passwd`
(0600, owned by the mosquitto user). Do not pass credentials on the command
line or in shell history.

To inspect the bus from a workstation (the shack broker):

```bash
# Subscribe to all topics under muehle/ (shack broker on shari)
mosquitto_sub -h 192.168.1.139 -u hf -P "$MQTT_PASSWORD" -t 'muehle/#' -v

# Watch a single slot
mosquitto_sub -h 192.168.1.139 -u hf -P "$MQTT_PASSWORD" -t 'muehle/hf/radio/#' -v
```

The HA broker at `192.168.1.50:1883` receives the same `muehle/#` traffic via
the bridge; HA's MQTT integration reads it there.

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
9. **Shared module for cross-cutting plumbing** — the paho connect / background
   job-queue (`shared/mqtt`) and topic helpers + `/cmd` `value`-key convention
   (`shared/schema`) live in `codeberg.org/kgbvax/stationa/shared`, not duplicated
   per bridge. A bridge imports `shared/` but never another bridge's `internal/`
   (Go's `internal/` rule enforces this). Each module's `go.mod` carries both the
   `require` and a `replace … => ../shared` so it builds without the workspace;
   the root `go.work` ties the modules together for whole-repo `go build ./...`.
10. **Logging** — `log/slog` text handler to stderr with a constant `component` attr
    and per-slot child loggers; real `Warn`/`Error` levels so `journalctl -p warning`
    filters errors; no per-service log files (journald is the consolidator)
    (see `docs/conventions/logging.md`)
