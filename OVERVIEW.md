# stationa — System Overview

stationa automates the Mühle amateur-radio station: the HF station, the UHF/VHF
satellite station and the site power layer. It is a set of small, independent
services ("bridges": Go services on Linux and Windows, plus ESPHome and
PlatformIO firmware on embedded nodes) that each front one piece of hardware —
or one logic role — and mirror it onto a shared MQTT bus. **The bus is the system:**
every component publishes what it is and what is currently true about it to a
well-known address, and any consumer (Home Assistant, a dashboard, a historian)
subscribes and reads. There is no central server; components integrate by
subscribing to each other's state, never by calling one another.

## Key capabilities

- **Live self-description** — each device or logic slot publishes a retained
  birth certificate (`/meta`) naming its role, device, link, location, host,
  capabilities and, where present, a consumer-neutral `expose` field surface.
  (vhfcam-restream publishes only `/status` and `/state`.) The station
  inventory is a live read, not a document that rots.
- **Three-plane bus** — per slot: `meta` (identity/capability), `state`
  (retained — what's true now), `status` (online/offline LWT), `cmd` (transient
  intent). Consumers couple to **state**, never to the fact that a command was
  sent (fire-and-observe).
- **Hardware control over MQTT** — radio band, mode (`cw`/`usb`/`lsb`), DVK and
  mic profile; Ultrabeam frequency, band, direction and retract; PA
  operate/standby and band; tuner in-line/bypass and tune; HF rotator azimuth
  and UHF az/el rotators (goto, stop, park); antenna-switch port and
  antenna-selection hold; power plugs, PA arm and TRX/PA remote-on;
  polarization; antenna mode, power-sequence start/stop and shack-PC sleep —
  all settable via `/cmd` and confirmed via `/state`. The radio frequency
  itself is read-only.
- **Automatic antenna selection** — a reconciler watches radio, switch,
  antenna-mode and operator state and drives the antenna switch by a fixed
  priority ladder (lock > manual stand-down > idle > operator > auto) plus band
  policy, with cold-switch (no TX-during-switch) safety. It also makes the
  Ultrabeam, PA band and tuner in-line state follow the radio.
- **Antenna mode** — the `hf/ant-mode` slot holds a storm/maintenance lock: it
  makes antennaselect ground the switch, ultrabridge retract the Ultrabeam and
  wrc-rotator-bridge park the HF rotator; beamsteer stands down while locked.
- **Beam steering** — beamsteer takes the contest logger's PstRotator bearing
  and picks rotation or an Ultrabeam 180° flip.
- **Power sequencing** — powerseq runs the ordered cold start and shutdown,
  idle standby (antenna power off, shack PC asleep, WOL on wake) and full idle
  shutdown 2 h after the last activity, and keeps its phase across a restart.
- **PA safety and remote-on** — the M5 Stamp PLC (m5stamp-hf-ctrl) drives the
  PA/TRX remote-on relays and the fail-safe-open PA arm relay, which closes only
  while every interlock gate is clear.
- **Logger spot feed** — logger-spot-bridge publishes the operator-keyed
  station from DXLog/Log4OM on `hf/spots`.
- **UHF satellite operation** — az/el rotators (rotctld/PstRotator), the
  OscarWatch tracking feed, IC-9700 RX audio (serial CI-V telemetry supported, disabled on scmino), X-Quad
  polarization.
- **Operator surfaces** — hf_console (tablet + web), testui, the landing page
  `http://scmino/`, the VHF cam restream.
- **Zero-config Home Assistant** — a passive discovery consumer reads each
  slot's `/meta` `expose` block (and renders a liveness diagnostic for slots
  without one) and renders HA entities. Bridges publish **no HA discovery by
  default**; a legacy embedded path (`publish_ha_discovery = false`) remains in
  flexbridge, ultrabridge and acom1200s-pa-bridge (wrc-rotator-bridge only
  still accepts the key). HA is
  one renderer of the same neutral surface a Grafana or Prometheus consumer
  could read.
- **Hardened, headless deployment** — Linux services run as locked-down systemd
  units on two Raspberry Pis (scmino, shari). The shack-PC agent runs as a
  Windows Task Scheduler task, and the embedded nodes run their own firmware.
  Config lives in 0600 TOML, with the MQTT password in that file or in a 0600
  EnvironmentFile. Deploys are seed-once.

## Architecture in one breath

Addressing is `site/station/slot` (e.g. `muehle/hf/radio`); site-wide
infrastructure sits outside the station path at `site/power/<name>` (e.g.
`muehle/power/psu-13v8`); `site/host/<name>` is reserved for compute nodes but
nothing publishes it yet (host placement is `/meta.host`). The slot segment is a
**canonical role** (`radio`, `pa`, `ant-ctrl`…) — never a product name — so the
device is an attribute of the slot and swapping the box behind an address changes
nothing downstream. Antennas are **passive resources** (no MQTT presence);
routing among them is `ant-switch` (actuator) driven by `antenna-select` (policy).

## Components

| Slot | Component | Role | Interface |
|---|---|---|---|
| `power/psu-13v8` | shelly-power-bridge | 13.8 V TRX PSU plug (site-level; feeds HF + UHF) | Wi-Fi (Shelly) |
| `power/antennas` | shelly-power-bridge | antenna-power plug (HF + UHF rotators, Ultrabeam RCU-06) | Wi-Fi (Shelly) |
| `power/shack-pc-mains` | shelly-power-bridge | shack PC (bwpc) mains plug; on at cold start, never off by powerseq | Wi-Fi (Shelly) |
| `power/shack-pc` | winpc-power-bridge | shack PC (bwpc) sleep agent, Windows (offline = asleep) | Ethernet |
| `hf/radio` | flexbridge | FLEX-8400 radio bridge (tuning state read-only; `/cmd`: `set_band`, `set_mode` cw/usb/lsb, DVK play/stop, `set_mic_profile`) | Ethernet (SmartSDR) |
| `hf/ant-ctrl` | ultrabridge | Ultrabeam RCU-06 antenna controller | USB-serial (FTDI) |
| `hf/ant-switch` | waveshare_relay-antswitch-bridge | 1:6 antenna switch — dumb actuator (ESPHome firmware, no Go bridge) | Wi-Fi (ESPHome) |
| `hf/switch` | m5stamp-hf-ctrl | M5 Stamp PLC #1 — PA / TRX remote-on relays | Wi-Fi |
| `hf/pa-arm` | m5stamp-hf-ctrl | M5 Stamp PLC #1 — PA arm relay (fail-safe open, interlock gates) | Wi-Fi |
| `hf/antenna-select` | antennaselect | antenna-selection reconciler (logic) | — |
| `hf/pa` | acom1200s-pa-bridge | ACOM 1200S PA bridge (operate/standby, band; `power` is telemetry only) | Serial (USB) |
| `hf/rotator` | wrc-rotator-bridge | HF rotator (Yaesu G-450DC via AF6SA WRC) | WebSocket |
| `hf/tuner` | atr1k-tuner-bridge | ATR-1000 ATU bridge (in-line / bypass, tune) | Wi-Fi (binary WebSocket) |
| `hf/spots` | logger-spot-bridge | operator-keyed station feed from the shack logger (call, position, beam bearing) | LAN UDP (DXLog / Log4OM) |
| `hf/beam-steer` | beamsteer | smart rotation: logger PstRotator UDP emulator → rotator + Ultrabeam direction (logic) | UDP :12050 |
| `hf/ant-mode` | antmode | antenna mode: storm / maintenance lock for the Ultrabeam and HF rotator (logic) | — |
| `hf/power-seq` | powerseq | startup / shutdown sequencer, idle standby and shutdown (logic) | — |
| `hf/discovery` | hadiscovery | HA discovery consumer (logic) | reads `/meta.expose` |
| `hf/vhfcam` | vhfcam-restream | VHF cam restreamer (YouTube Live + LAN preview); publishes `/status` and `/state` only | RTSPS (UniFi Protect) |
| `uhf/pol-ctrl` | m5stamp-pol-ctrl | M5 Stamp PLC #2 — X-Quad polarization; not yet flashed (USB flash pending, `docs/known-issues.md`) | Wi-Fi (ESPHome) |
| `uhf/radio` | icom9700-radio-bridge | IC-9700 radio bridge (demand-driven RX audio capture; `park_radio` retune via `rigctld-ic9700` on scmino; read-only serial CI-V telemetry supported but disabled on scmino; no remote TX) | LAN (RS-BA1) |
| `uhf/sat-track` | oscarwatch-sattrack-bridge | OscarWatch satellite-tracking feed (read-only) | WebSocket |
| `uhf/az-rotator` | spid-ercm-rotator-bridge | SPID azimuth rotator (rotctld :4534, PstRotator UDP :12041, GS-232B TCP :4533) | Serial (Rot1Prog) |
| `uhf/el-rotator` | spid-ercm-rotator-bridge | GS-500 elevation via ERC-M (same process as `uhf/az-rotator`) | Serial (GS-232B) |

Not slots (consumers and infrastructure): `hf_console` (Flutter operator
console, tablet APK + web on scmino:8091), `testui` (bus browser, :8090),
`stationportal` (landing page `http://scmino/`), `mqtt-broker` (station
Mosquitto on scmino), `m5dial-hf-rotctrl` and `m5dualkey-hf-antctrl` (M5Stack
control heads), `hf-mqtt-capture` (bus recorder on shari).

The services run on two Raspberry Pis. **scmino** (`192.168.1.178`, CM5) runs
every HF Go bridge and logic slot, `shelly-power-bridge`, the UHF radio and
sat-track bridges, vhfcam-restream, the station Mosquitto, the console web
channel (:8091), testui (:8090) and the landing page (:80). **shari**
(`192.168.1.139`) runs `spid-ercm-rotator-bridge` (UHF az/el) and
`hf-mqtt-capture`. `winpc-power-bridge` runs on the shack PC bwpc
(`192.168.1.197`). The ant-switch, the M5 Stamp PLCs and the control heads are
embedded firmware that speak MQTT directly. Everything lives under the
`muehle/…` address tree. Most clients still connect to the Home Assistant Mosquitto at
`192.168.1.50:1883` (root `CLAUDE.md`, as of 2026-10-18;
per-service broker settings are seed-once configs, so the repo does not settle
it per service). A station Mosquitto on scmino (`192.168.1.178:1883`) mirrors
`muehle/#` in both directions and serves M5 Stamp PLC #1 and
shelly-power-bridge with its Shelly plugs; see `mqtt-broker/CLAUDE.md`. The
shack-local broker originally planned for shari was never deployed (shari runs
no mosquitto); `docs/conventions/mqtt-topology.md` describes the target end
state, in which the scmino station broker is authoritative for `muehle/#`. (The former `uhf/rotator` slot —
pelcobridge2, an interactive Pelco-D/P pan/tilt TUI on the shack PC — was
retired 2026-10-10, not in use; UHF az/el is `spid-ercm-rotator-bridge`.)

> **Bridge naming:** device-specific bridges follow `<devtag>-<function>-bridge`
> (e.g. `atr1k-tuner-bridge`); the `<devtag>` is the device *family / control
> interface*, not the model number, so swapping a box within a family changes
> nothing. Two bridges, `flexbridge` and `ultrabridge`, predate the convention;
> their rename (to `flex-radio-bridge` / `ultrabeam-ant-ctrl-bridge`) is a
> tracked TODO. The other legacy names are already renamed. See
> `docs/conventions/naming.md`.

## Shared conventions

Three-plane schema · single retained JSON state snapshot · 0600 TOML config
(never on the command line) · seed-once deploy · hardened systemd
(NoNewPrivileges, ProtectSystem, PrivateTmp) · `freq_hz` in Hz as integer ·
canonical modes (`cw` / `usb` / `lsb` / `am` / `fm` / `data`) · two-layer
liveness (`/status` LWT AND `/state.device_online`) ·
`<devtag>-<function>-bridge` naming · cross-cutting plumbing in the `shared/` Go
module · `log/slog` to stderr per `docs/conventions/logging.md` · update
`stationportal/internal/inventory/inventory.toml` with every infrastructure
change.

## Where to look next

- **Integration model** — the grammar of the bus: `docs/station-integration-model.md`
- **Conventions** — `docs/conventions/{deployment,config-and-secrets,band-mode-reference,naming,mqtt-topology,logging,runtime-library}.md`
- **Decisions and known issues** — `docs/known-issues.md`
- **Full project and slot tables** — root `CLAUDE.md` and
  `stationportal/internal/inventory/inventory.toml`; diagram `docs/architecture.svg`
- **Per component** — each component directory's `CLAUDE.md` (or `README.md`)
  and, where present, its `docs/` and `*-mqtt-api.md` slot contract