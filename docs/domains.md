# Domains: StationA and opsotter

Two groups of software surround the Mühle station. This page says which group a
component belongs to and how the two talk to each other.

- **StationA** is the machine: the hardware and the control logic that keeps it
  safe and coordinated. Question it answers: *what is the equipment doing, and
  can it do this safely?*
- **opsotter** is the operator's activity: who was worked, who to work next, what
  is on record, and what was heard. Question it answers: *what am I doing on the
  air, and what is the record of it?*

opsotter is a name for the group, not a repository. Its members (`qslotter`,
`bmrec`, and the spot/DX tools) stay independent projects.

## Ham-radio domains

| Domain | Contents | Belongs to |
|---|---|---|
| Station | radios, PA, tuner, antennas, rotators, power, interlocks, sequencing | StationA |
| Operating | logging, spotting and DX cluster, band map, chase queue, contesting, satellite passes | opsotter |
| Records and confirmation | the log, QSL, Clublog, LoTW, eQSL, awards (DXCC, WAS) | opsotter |
| Monitoring and capture | recording, transcribing and decoding traffic that is heard | opsotter |
| Propagation and environment | space weather, VOACAP, satellite tracking, wind and storm | advisory data; opsotter publishes, StationA may consume |

Inside opsotter, keep one subdirectory or module per domain so that Operating and
Records can be split later without a rewrite.

## Rules

1. **StationA runs without opsotter.** Powering up, tuning, rotating and keying
   must work with every opsotter component stopped. Test: *would this exist if no
   QSO were made?* If yes, it is StationA.
2. **Dependencies point one way.** opsotter may depend on the station's bus
   contract (`station-integration-model.md`, the `/meta` `/state` `/status` `/cmd`
   planes). StationA never imports, calls or waits for an opsotter component.
3. **Intent crosses the bus, not code.** opsotter hands the station an intent by
   publishing to a topic. The station decides what to do with it. opsotter does
   not reach into station internals, and bridges still never import another
   component's `internal/`.
4. **Deploy cadence differs.** StationA components are safety-relevant (PA arm,
   power-down) and change deliberately. opsotter components (LLM analysis, UI,
   Clublog sync) may change often and fail without hurting hardware.

## Pattern for opsotter to influence the station: `stationhold`

`bmrec` is a slot on the bus (`muehle/hf/recorder`, role `recorder`). While it
records it publishes a retained `/state` with `station_hold`, `hold_until`,
`holder` and `reason`. `antennaselect` and `powerseq` read that hold and skip
their walk-away actions (grounding the antenna, standby, shutdown).

The hold is safe by construction: it carries its own expiry (`hold_until`), is
republished every heartbeat, is released by the last will if bmrec crashes, and
never causes transmission. A hung or dead opsotter component therefore cannot
keep the station up forever. New opsotter-to-station influence should follow the
same shape: a retained, self-expiring statement of fact or intent that the
station is free to ignore.

## Component map

### StationA

| Group | Components |
|---|---|
| Radio and RF chain | `flexbridge`, `acom1200s-pa-bridge`, `tgxl-tuner-bridge`, `ultrabridge`, `waveshare_relay-antswitch-bridge`, `icom9700-radio-bridge` |
| Rotators | `wrc-rotator-bridge`, `spid-ercm-rotator-bridge` |
| Power and interlocks | `shelly-power-bridge`, `winpc-power-bridge`, `m5stamp-hf-ctrl`, `m5stamp-pol-ctrl`, `powerseq`, `antennaselect`, `antmode` |
| Station firmware heads | `m5dial-hf-rotctrl`, `m5dualkey-hf-antctrl` |
| Logic that acts on operator intent | `beamsteer` (consumes the working station from `station-log/working`, decides the moves) |
| Infrastructure | `mqtt-broker`, `shared`, `hadiscovery`, `stationportal`, `testui`, `hf-mqtt-capture` |
| Station camera | `vhfcam-restream` |
| Station UI | `hf_console` (a client of both domains; stays here for now) |
| Process and docs | `docs`, `requirements`, `sas` |

### opsotter

Components that live in this repository today are marked (here).

| Domain | Components |
|---|---|
| Records | `station-log` (here; the QSO record, slot `hf/station-log`), `qslotter` (QSL cards, Clublog, QRZ) |
| Operating | `chase-queue` (here; slot `hf/chase`), `chaseq-ui` (here; consumer, outside `go.work`), `wkdb4`, `dxlens`, `horstreporter` |
| Monitoring and capture | `bmrec` (recording, transcription, CQ finder; feeds qualified rows to chase-queue) |
| Propagation and environment | `oscarwatch-sattrack-bridge` (here; slot `uhf/sat-track`, read-only) |

`logger-spot-bridge` no longer exists as a directory: `station-log` absorbed it
(the working-station feed is now `muehle/hf/station-log/working`). The chase
domain is described in `chasing-stations-architecture.md`; this page only places
it on the map.

Four opsotter components still live in this repository. They are already
decoupled by the bus contract, so moving them is possible but not urgent. See
below for what a move costs.

## Cost of separate repositories

Every Go module here uses `replace … => ../shared` and the root `go.work`. A
component in another repository needs `shared` published with version tags, or
its own copy of the MQTT and schema plumbing. `chaseq-ui` already builds with
`GOWORK=off` because it needs a newer Go, so it shows what that looks like. The
bus contract also becomes a cross-repository interface that needs versioning.
Until that cost is justified, the boundary above is documentation, not a
repository split.
