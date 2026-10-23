# CLAUDE.md — stationa monorepo

This is a single git repository holding the whole Mühle station-automation ecosystem:
shared documentation **and** all component projects as subdirectories. The Go
components are a [Go workspace](https://go.dev/ref/mod#workspaces) tied together by a
root `go.work`; each component keeps its own `go.mod` (per-module, independently
`go build`/`go test`-able). A shared module, `codeberg.org/kgbvax/stationa/shared`,
holds cross-cutting plumbing (`shared/mqtt`, `shared/schema`, `shared/logging`, `shared/pstrotator`, `shared/antmode`, later `shared/config`);
every Go component imports it via a `replace … => ../shared` so each stays self-building
without the workspace. Bridges import `shared/` but never another bridge's `internal/`
— enforced by Go's `internal/` visibility rule across separate modules, not just
convention. Non-Go components (`waveshare_relay-antswitch-bridge` and
`m5stamp-pol-ctrl` = ESPHome YAML, `m5stamp-hf-ctrl`, `m5dial-hf-rotctrl` and `m5dualkey-hf-antctrl` =
PlatformIO firmware, `hf_console` = Flutter app with its own stand-alone Go `webbridge/` module,
`mqtt-broker` = Mosquitto config + deploy script) live alongside as plain subdirectories and are not in
`go.work`.

The original components were standalone git repos nested here and gitignored;
they were folded into this repo with history (`git subtree`). Later components were
created in-repo. There are no separate per-component remotes to push to.

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
| shelly-power-bridge | `shelly-power-bridge/` | Shelly smart-plug bridge → `power/psu-13v8` + `power/antennas` + `power/shack-pc-mains` (supply layer; the site master mains is out of scope — HA-owned Athom plug that also powers the Pis/network) |
| m5stamp-hf-ctrl | `m5stamp-hf-ctrl/` | M5 Stamp PLC #1 firmware → `hf/pa-arm` + `hf/switch` (PA/TRX remote-on + arm) |
| m5stamp-pol-ctrl | `m5stamp-pol-ctrl/` | M5 Stamp PLC #2 firmware → `uhf/pol-ctrl` (X-Quad polarization, ESPHome) |
| powerseq | `powerseq/` | Startup/shutdown sequencer → `hf/power-seq` (ordered, delay + liveness confirmations); idle standby: antenna power off + bwpc sleep when antenna-select reports inactive, WOL + antenna power on at wake; idle shutdown: full power-down 2 h after the last activity |
| winpc-power-bridge | `winpc-power-bridge/` | Windows agent on bwpc → `power/shack-pc` (`sleep` cmd from powerseq standby, refused while the operator uses the PC; runs at logon via Task Scheduler) |
| antennaselect | `antennaselect/` | Antenna-selection reconciler (live on scmino since 2026-10-12, unit `antenna-select`) |
| antmode | `antmode/` | Antenna-mode logic slot → `hf/ant-mode` (`normal`/`storm`/`maintenance`). In storm/maintenance it publishes a retained lock (ground → retract → park_az): antennaselect grounds the switch, ultrabridge retracts, wrc-rotator-bridge parks; beamsteer stands down; powerseq holds its idle logic (`power_hold`). Storm suggestion from DWD warnings via NINA, storm heading from DWD wind; `auto_enter` off by default (observe only). Runs on scmino, unit `antmode` (since 2026-10-22) |
| beamsteer | `beamsteer/` | Smart rotation → `hf/beam-steer`: PstRotator UDP emulator (:12050) for the contest logger; station behind → Ultrabeam 180° flip, outside both lobes → rotate to the cheaper lobe (±30°, bi-dir ±45° no flips); toggle = hf_console Ultrabeam `AUTO` |
| hadiscovery | `hadiscovery/` | Home Assistant discovery consumer (reads `/meta` `expose`, renders HA discovery) |
| spid-ercm-rotator-bridge | `spid-ercm-rotator-bridge/` | Sat-ops az/el rotator bridge → `uhf/az-rotator` (SPID) + `uhf/el-rotator` (GS-500 via ERC-M); rotctld :4534 + PstRotator UDP :12041 + GS-232B TCP :4533 listeners (free motion, no arming gate) |
| icom9700-radio-bridge | `icom9700-radio-bridge/` | IC-9700 UHF radio bridge → `muehle/uhf/radio` (RS-BA1 LAN session for demand-driven RX audio capture; `park_radio` retune via the `rigctld-ic9700` unit on scmino; read-only serial CI-V telemetry supported but disabled on scmino (`serial.device` unset by design: the only CI-V adapter on scmino belongs to `rigctld-ic9700`); no remote TX) |
| m5dial-hf-rotctrl | `m5dial-hf-rotctrl/` | M5Stack Dial firmware — HF rotator control head (analog meter face + knob; not a slot; consumer + /cmd stimulator) |
| m5dualkey-hf-antctrl | `m5dualkey-hf-antctrl/` | M5Stack Chain DualKey firmware — Ultrabeam direction keys (A forward / B reverse / A+B bidirectional, LED shows `hf/ant-ctrl` direction; chained M5 Chain Key toggles DVK memory 2 on `hf/radio`; not a slot; consumer + /cmd stimulator) |
| logger-spot-bridge | `logger-spot-bridge/` | Shack-logger bridge (DXLog/Log4OM) → `hf/spots` — the operator-keyed station (call, position, beam bearing) |
| oscarwatch-sattrack-bridge | `oscarwatch-sattrack-bridge/` | OscarWatch Satellite-link WebSocket bridge → `uhf/sat-track` — the satellite being tracked (name, NORAD, transponder, az/el, range, range rate, sunlit, derived sub-satellite point, radio-corrected up/downlink); read-only, runs on scmino |
| testui | `testui/` | MQTT relay + schema-aware browser UI for the bus (not a slot; passive consumer + /cmd stimulator) |
| hf_console | `hf_console/` | Flutter station console: Android APK on the shack tablet, iPhone IPA, and web channel `hf-console-web` on scmino :8091 (Go `webbridge/` WebSocket→MQTT TCP relay). Not a slot; consumer + /cmd stimulator |
| vhfcam-restream | `vhfcam-restream/` | VHF cam (UniFi Protect RTSPS) multi-sink restreamer — YouTube Live + LAN web preview (:8083) with operational-data overlay (AZ/EL/freq/TX drawtext from the bus) and IC-9700 RX audio (demand-driven via icom9700-radio-bridge); web radio controls; Go supervisor around ffmpeg; MQTT consumer + minimal `/status`/`/state` planes (`muehle/hf/vhfcam`). **Runs on scmino** (`192.168.1.178`), not shari |
| stationportal | `stationportal/` | Station landing page on scmino :80 (`http://scmino/`) — links to every key service (probed), live slot/hardware inventory from `/meta` + two-layer liveness, software/host/resource inventory (not a slot; passive consumer) |
| hf-mqtt-capture | `hf-mqtt-capture/` | Passive MQTT recorder: `muehle/hf/#` to hourly log files in `/var/log/hf-mqtt-capture/` (72 h retention); runs on shari (not a slot) |
| mqtt-broker | `mqtt-broker/` | Station Mosquitto broker on **scmino** (`192.168.1.178:1883`), bridged to the HA broker `.50` as a transitional `muehle/#` mirror — **clients: the M5 Stamp PLC #1 (2026-10-03) and shelly-power-bridge + its Shelly plugs (2026-10-18); everything else still connects to `.50` per the 2026-10-18 live-state note (per-service configs not verified live)** (infra — not a slot, not Go) |

Non-component directories: `sas/` (hf_console design artefacts and the mobile-console design
handoff), `requirements/` (file-based requirements queue for the /loop workflow, see
`requirements/README.md`), `docs/` (shared docs), `shared/` (shared Go module).

Most projects have their own `CLAUDE.md` (`m5dualkey-hf-antctrl` and `hf-mqtt-capture` have a
README.md instead). Each Go module is independently buildable (`go build`/`go test` from its own
directory works without the workspace, via the `replace … => ../shared`).
Open a project by navigating into its directory. From the repo root,
`go build $(go list -m -f '{{.Dir}}/...')` builds every workspace module (a plain
`go build ./...` fails there because the root is not a module); `go work sync` syncs the
workspace.

---

## Slot assignments

| Slot address | Component | Physical device |
|-------------|-----------|-----------------|
| `muehle/power/psu-13v8` | shelly-power-bridge | Shelly plug — 13.8 V TRX PSU (site-level; feeds HF+UHF), wifi |
| `muehle/power/antennas` | shelly-power-bridge | Shelly plug — antenna power: HF + UHF rotators and the Ultrabeam RCU-06, wifi |
| `muehle/power/shack-pc-mains` | shelly-power-bridge | Shelly plug — shack PC (bwpc) mains; on at cold start, never off by powerseq, wifi |
| `muehle/power/shack-pc` | winpc-power-bridge | Shack PC bwpc (`192.168.1.197`) — sleep/wake (offline = asleep), ethernet |
| `muehle/hf/radio` | flexbridge | FLEX-8400, ethernet |
| `muehle/hf/ant-ctrl` | ultrabridge | Ultrabeam RCU-06, USB-serial via FTDI — on scmino since 2026-10-09 (HamClock took :8080 on shari) |
| `muehle/hf/ant-switch` | waveshare_relay-antswitch-bridge | 1:6 antenna switch, wifi (ESPHome) |
| `muehle/hf/switch` | m5stamp-hf-ctrl | M5 Stamp PLC #1 — PA/TRX remote-on relays (relays 3 & 4), wifi |
| `muehle/hf/pa-arm` | m5stamp-hf-ctrl | M5 Stamp PLC #1 — PA arm relay (relay 1), wifi |
| `muehle/hf/antenna-select` | antennaselect | logic slot — no device (runs on scmino) |
| `muehle/hf/pa` | acom1200s-pa-bridge | ACOM 1200S, serial (`set_power`/RTS removed; `power` is telemetry only) — on scmino since 2026-10-12 (Prolific adapter re-plugged there) |
| `muehle/hf/rotator` | wrc-rotator-bridge | Yaesu G-450DC via AF6SA WRC, websocket — on scmino since 2026-10-12 (GS-232 :7373 / PstRotator :12040 moved with it) |
| `muehle/hf/tuner` | atr1k-tuner-bridge | ATR-1000 ATU, wifi (binary WebSocket) — bridge on scmino since 2026-10-12 |
| `muehle/hf/spots` | logger-spot-bridge | DXLog/Log4OM broadcasts (LAN-wide UDP) → operator-keyed station feed (role `bandmap`; systemd service on scmino since 2026-10-18) |
| `muehle/hf/beam-steer` | beamsteer | logic slot — no device (runs on scmino); logger PstRotator UDP :12050 (logger must target scmino) → `hf/rotator` + `hf/ant-ctrl` direction |
| `muehle/hf/power-seq` | powerseq | logic slot — no device (runs on scmino since 2026-10-18); startup/shutdown sequencer |
| `muehle/hf/ant-mode` | antmode | logic slot — no device (runs on scmino since 2026-10-22); antenna mode normal/storm/maintenance, retained lock enforced by antennaselect (ground), ultrabridge (retract) and wrc-rotator-bridge (park_az) |
| `muehle/hf/vhfcam` | vhfcam-restream | camera restreamer bookkeeping — minimal `/status` + `/state`, no device, an accessory, not in hf_console `expectedSlots` (runs on scmino) |
| `muehle/uhf/pol-ctrl` | m5stamp-pol-ctrl | M5 Stamp PLC #2 — X-Quad polarization (ESPHome), wifi; not yet flashed (USB flash pending, `docs/known-issues.md`) |
| `muehle/uhf/radio` | icom9700-radio-bridge | Icom IC-9700 — RS-BA1 LAN session carries RX audio capture (demand-driven); park retune via rigctld-ic9700 (scmino, loopback :4532); serial CI-V monitor off on scmino (its CI-V adapter belongs to rigctld-ic9700); no remote TX (2026-09 pivot) |
| `muehle/uhf/sat-track` | oscarwatch-sattrack-bridge | software source — OscarWatch on BWPC (`ws://192.168.1.197:7373/`); bridge on scmino (192.168.1.178), read-only |
| `muehle/uhf/az-rotator` | spid-ercm-rotator-bridge | SPID azimuth rotator, serial (Rot1Prog) — runs on shari with rotctld :4534 + PstRotator UDP :12041 + GS-232B TCP :4533; free motion, no arming gate |
| `muehle/uhf/el-rotator` | spid-ercm-rotator-bridge | GS-500 elevation via ERC-M controller, serial (GS-232B) — same compound bridge as az-rotator (one process, two slots, per-axis device_online) |
| `muehle/hf/discovery` | hadiscovery | logic slot — no device (runs on scmino); passive consumer of `/meta` |

**Antennas are not slots.** `ant-ctrl` is the *controller* that tunes the Ultrabeam
(canonical role `ant-ctrl`; the device name Ultrabeam RCU-06 lives in `/meta.device`,
never in the address), not "the antenna." The physical antennas are **passive
resources** — `ant/ultrabeam` (port 4 as seeded: antennaselect `config.example.toml`/`deploy.sh`,
PLC #1 `ULTRABEAM_PORT`, hf_console `antennaMap`; on-site confirmation open, see
`docs/known-issues.md` "Ultrabeam antenna-switch port: 3 or 4"; the live
`/etc/antenna-select/config.toml` on scmino is authoritative; the antennaselect reconcile
test fixtures and the portal `[[resource]]` Ultrabeam note still say port 3), `ant/fan-dipole`
80/40 (port 6), `ant/dummy-load` (port 1) — with no MQTT presence; they are defined in the
`antennaselect` wiring map (also hard-coded in hf_console `antennaMap`). Routing among them is `ant-switch` (actuator) driven by
`antenna-select` (policy). See the integration model §2–§4, §7.1.

---

## Hosts — scmino and shari

Most services run on **scmino** (`192.168.1.178`, Raspberry Pi CM5, `ssh io@`): flexbridge,
ultrabridge (:8080, RCU-06 FTDI cable on scmino's USB), acom1200s-pa-bridge,
wrc-rotator-bridge, atr1k-tuner-bridge, antenna-select, beamsteer, antmode, hadiscovery,
powerseq, shelly-power-bridge, logger-spot-bridge, icom9700-radio-bridge + rigctld-ic9700,
oscarwatch-sattrack-bridge, vhfcam-restream (:8083), the hf_console web channel
`hf-console-web` (:8091), testui (:8090), the landing page stationportal (:80) and the
station Mosquitto mirror. **shari** (`192.168.1.139`, Raspberry Pi 4) runs only
spid-ercm-rotator-bridge and hf-mqtt-capture (plus HamClock and SvxLink, which are not repo
components). winpc-power-bridge runs on the Windows shack PC bwpc (`192.168.1.197`).

```bash
# SSH in
ssh io@192.168.1.178   # scmino: flexbridge, ultrabridge, acom1200s-pa-bridge, …
ssh io@192.168.1.139   # shari: spid-ercm-rotator-bridge, hf-mqtt-capture

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

The station broker is Mosquitto on scmino (`192.168.1.178:1883`, `mqtt-broker/`), bridged
both ways to the Home Assistant broker `192.168.1.50:1883` as a `muehle/#` mirror. PLC #1
and shelly-power-bridge (+ its Shelly plugs) use the scmino broker; per the 2026-10-18
live-state note below, the other services still connect to `.50` (per-service configs not
verified live). shari runs no broker. See `mqtt-broker/` and
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
| Logging/operating-layer integration model (draft companion to the integration model; `bandmap`/spots layer) | `docs/logging-integration-model.md` |
| Station architecture diagram | `docs/architecture.svg` (source `docs/architecture.d2`) |
| Feature plans | `docs/plans/` |

All components live as subdirectories of this one repo, so the shared docs path
relative to any component is `../docs/` (e.g. `../docs/station-integration-model.md`
from `flexbridge/`). Cross-cutting Go code, not docs, lives in the `shared/` module
(see the intro); components reference shared docs as `../docs/…` and shared code as
`codeberg.org/kgbvax/stationa/shared/…`.

---

## MQTT broker access

> **Live state (2026-10-18):** every station client except the M5 Stamp PLC #1
> (`hf/switch` + `hf/pa-arm`) and shelly-power-bridge + its Shelly plugs
> (`power/*`) still uses the HA broker `192.168.1.50:1883`.
> shari runs no mosquitto. A new station broker runs on **scmino**
> (`192.168.1.178:1883`) as a bridged `muehle/#` mirror of `.50`; the PLC
> moved there first — see `mqtt-broker/CLAUDE.md`. The broker was
> originally planned for shari; it now runs on scmino.

Target design: the station Mosquitto on scmino (`mqtt-broker/`) becomes authoritative for
`muehle/#` and bridges to the HA broker `192.168.1.50:1883` (HA's Mosquitto add-on,
untouched; it keeps serving HA's other MQTT devices). Clients move to
`192.168.1.178:1883` one at a time; once all have moved, the bridge narrows to the split
mapping in `mqtt-broker/mosquitto.conf.example`. See `docs/conventions/mqtt-topology.md`
for the full topology, topic-direction table, and ACLs.

Credentials: the scmino station broker has one local account, `hf` (in
`/etc/mosquitto/passwd` on scmino, 0600, owned by the mosquitto user); per
`mqtt-broker/CLAUDE.md` its bridge logs in to `.50` as `hf` (the 2026-10-01 deploy ran with
`HA_REMOTE_USER=hf`, per `mqtt-broker/CLAUDE.md` and the README current-state note); the
`stationa-bridge` account named in `mosquitto.conf.example`, the `deploy.sh` default and the
README HA-side setup is the target design. The `bridge`/`console`/`dial` accounts in `mqtt-broker/acl.conf.example` belong to
the target split topology and are not in use yet. Do not pass credentials on the command
line or in shell history.

To inspect the bus from a workstation (either broker shows all of `muehle/#`):

```bash
# Subscribe to all topics under muehle/ (HA broker .50; station broker: -h 192.168.1.178)
mosquitto_sub -h 192.168.1.50 -u hf -P "$MQTT_PASSWORD" -t 'muehle/#' -v

# Watch a single slot
mosquitto_sub -h 192.168.1.50 -u hf -P "$MQTT_PASSWORD" -t 'muehle/hf/radio/#' -v
```

Most services still connect to `.50` (live-state note, 2026-10-18); the scmino station
broker mirrors `muehle/#` to and from it, and HA's MQTT integration reads it on `.50`.

---

## Conventions

All components follow these shared conventions:

1. **Three-plane MQTT schema** — `meta`, `state`, `status`, `cmd` per slot
   (see `docs/station-integration-model.md`)
2. **Single retained JSON state snapshot** — not per-field topics
3. **Config in 0600 TOML file** — never in ExecStart or command line
   (see `docs/conventions/config-and-secrets.md`)
4. **Seed-once deploy** — `deploy.sh` seeds the config once; device owns settings
   (exception: powerseq's deploy runs `migrate-config.sh` to add new sections in place, with
   backup + validation by the new binary)
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
   (`shared/schema`), the slog root handler + `Streak` (`shared/logging`), the PstRotator
   grammar (`shared/pstrotator`) and the ant-mode lock parser (`shared/antmode`) live in
   `codeberg.org/kgbvax/stationa/shared`, not duplicated
   per bridge. A bridge imports `shared/` but never another bridge's `internal/`
   (Go's `internal/` rule enforces this). Each module's `go.mod` carries both the
   `require` and a `replace … => ../shared` so it builds without the workspace;
   the root `go.work` ties the modules together for a whole-repo build
   (`go build $(go list -m -f '{{.Dir}}/...')` at the root; a plain `./...` does not work there).
10. **Logging** — `log/slog` text handler to stderr with a constant `component` attr
    and per-slot child loggers; real `Warn`/`Error` levels so `journalctl -p warning`
    filters errors; no per-service log files (journald is the consolidator)
    (see `docs/conventions/logging.md`)
11. **Landing page inventory** — `http://scmino/` (`stationportal/`) lists every
    service, host, slot, device and software component. **Whenever the
    infrastructure changes** — a device or host is added/replaced/removed, an IP
    address, hostname or port changes, a service moves host, a component is
    added or retired — update `stationportal/internal/inventory/inventory.toml`
    in the same change and redeploy (`cd stationportal && ./deploy.sh`). Live
    device facts (model/serial/firmware from `/meta`) need no edit; everything
    hand-written there (links, IPs, hosts, slot→component map, software list)
    does. `go test` fails on a misplaced key or a slot whose component is
    missing from `[[software]]`. When a bridge moves host, also update the
    hf_console station schematic's host map
    (`hf_console/lib/store/station_hosts.dart`); its test cross-checks the
    inventory and fails on a stale entry.
