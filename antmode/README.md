# antmode

Station antenna mode for the HF Ultrabeam 3 el 6–20 m and its G-450DC rotator: logic slot
`muehle/hf/ant-mode`.

| Mode | What happens |
|---|---|
| `normal` | Nothing is locked. |
| `storm` | Antenna grounded, elements retracted, boom parked broadside to the DWD wind (once). Motion refused. |
| `maintenance` | Same, parked at a preset azimuth for lowering the mast; console nudges allowed. |

- **Storm entry:** the operator (console or Home Assistant select) can enter storm. A
  DWD wind warning (BBK NINA, level 1 "Windböen" and up, active now) can also enter it:
  - station unmanned and `auto_enter` on: storm is entered automatically;
  - otherwise: storm is only suggested (`storm_suggested`).
- **Storm exit:** an auto-entered storm exits on its own 60 min after the last warning.
  Everything else exits only manually.
- **Enforcement:** antmode only publishes the lock. wrc-rotator-bridge, ultrabridge,
  antennaselect, beamsteer and powerseq read it through `shared/antmode` and act on it.

- Contract: [`docs/ant-mode-mqtt-api.md`](docs/ant-mode-mqtt-api.md)
- Configuration: [`config.example.toml`](config.example.toml)

## Data sources

- **Warnings:** BBK NINA API, `https://warnung.bund.de/api31`
  - `/dashboard/{ARS}.json`
  - `/warnings/{id}.json`
  - DWD II event codes: DWD "Warnungen (WW/WU)" code table (`warning_codes_pdf.pdf`).
- **Wind:** DWD app backend, `https://app-prod-ws.warnwetter.de/v30/stationOverviewExtended?stationIds=<id>`
  - uses the `days[]` entry for today;
  - `windDirection` and `windGust` are in tenths.

`internal/nina/testdata` and `internal/dwd/testdata` contain real responses fetched on
2026-10-22.

## Build, test, deploy

```bash
go test -race ./...
NINA_ARS=<12-digit ARS> DWD_STATION=<station id> ./deploy.sh   # scmino, seed-once config
```

Ship it observe-only (`auto_enter = false`, `[power] borrow = false`). Watch
`storm_suggested` against WarnWetter for some days before you enable either option.
