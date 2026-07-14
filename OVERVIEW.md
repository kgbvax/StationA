# stationa — System Overview

stationa automates the Mühle HF amateur-radio station. It is a set of small,
independent Go services ("bridges") that each front one piece of hardware — or
one logic role — and mirror it onto a shared MQTT bus. **The bus is the system:**
every component publishes what it is and what is currently true about it to a
well-known address, and any consumer (Home Assistant, a dashboard, a historian)
subscribes and reads. There is no central server; components integrate by
subscribing to each other's state, never by calling one another.

## Key capabilities

- **Live self-description** — each slot publishes a retained birth certificate
  (`/meta`) naming its role, device, link, capabilities, and a consumer-neutral
  `expose` field surface. The station inventory is a live read, not a document
  that rots.
- **Three-plane bus** — per slot: `meta` (identity/capability), `state`
  (retained — what's true now), `status` (online/offline LWT), `cmd` (transient
  intent). Consumers couple to **state**, never to the fact that a command was
  sent (fire-and-observe).
- **Hardware control over MQTT** — frequency, mode, antenna selection, PA
  operate/band, rotator, and antenna-switch port are all settable via `/cmd` and
  confirmed via `/state`.
- **Automatic antenna selection** — a reconciler watches radio / switch /
  operator state and drives the antenna switch by a fixed priority ladder plus
  band policy, with cold-switch (no TX-during-switch) safety.
- **Zero-config Home Assistant** — a passive discovery consumer reads every
  slot's `expose` block and renders HA entities. Bridges carry **no HA
  knowledge**; HA is one renderer of the same neutral surface a Grafana or
  Prometheus consumer could read.
- **Hardened, headless deployment** — services run as locked-down systemd units
  on a Raspberry Pi; config in 0600 TOML, secrets via EnvironmentFile,
  seed-once deploys.

## Architecture in one breath

Addressing is `site/station/slot` (e.g. `muehle/hf/radio`). The slot segment is a
**canonical role** (`radio`, `pa`, `ant-ctrl`…) — never a product name — so the
device is an attribute of the slot and swapping the box behind an address changes
nothing downstream. Antennas are **passive resources** (no MQTT presence);
routing among them is `ant-switch` (actuator) driven by `antenna-select` (policy).

## Components

| Slot | Component | Role | Interface |
|---|---|---|---|
| `hf/radio` | flexbridge | FLEX-8400 radio bridge (read-only) | Ethernet (SmartSDR) |
| `hf/ant-ctrl` | ultrabridge | Ultrabeam RCU-06 antenna controller | USB-serial |
| `hf/ant-switch` | waveshare_relay-antswitch-bridge | 1:6 antenna switch — dumb actuator | Wi-Fi (contract-first) |
| `hf/antenna-select` | antennaselect | antenna-selection reconciler (logic) | — |
| `hf/pa` | acombridge | ACOM 1200S PA bridge | Serial |
| `hf/rotator` | wrc-rotator-bridge | HF rotator (Yaesu G-450DC via AF6SA WRC) | WebSocket |
| `hf/tuner` | atr1k-tuner-bridge | ATR-1000 ATU bridge (in-line / bypass, tune) | Wi-Fi (binary WebSocket) |
| `hf/discovery` | hadiscovery | HA discovery consumer (logic) | reads `/meta.expose` |
| `uhf/rotator` | pelcobridge | Pelco-D rotator controller (UHF sat rotator) | Serial |

All run on **shari** (Raspberry Pi, `192.168.1.139`) against the MQTT broker at
`192.168.1.50:1883`, under the `muehle/…` address tree.

> **Bridge naming:** device-specific bridges follow `<devtag>-<function>-bridge`
> (e.g. `atr1k-tuner-bridge`); the `<devtag>` is the device *family / control
> interface*, not the model number, so swapping a box within a family changes
> nothing. Existing bridges predate the convention; their rename is a tracked
> TODO. See `docs/conventions/naming.md`.

## Shared conventions

Three-plane schema · single retained JSON state snapshot · 0600 TOML config
(never on the command line) · seed-once deploy · hardened systemd
(NoNewPrivileges, ProtectSystem, PrivateTmp) · `freq_hz` in Hz as integer ·
canonical modes (`cw` / `usb` / `lsb` / `am` / `fm` / `data`).

## Where to look next

- **Integration model** — the grammar of the bus: `docs/station-integration-model.md`
- **Conventions** — `docs/conventions/{deployment,config-and-secrets,band-mode-reference}.md`
- **Per component** — each component directory's `CLAUDE.md` and `docs/`