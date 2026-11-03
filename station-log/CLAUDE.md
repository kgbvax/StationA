# CLAUDE.md — station-log

station-log is the station's **QSO record** (logic slot `muehle/hf/station-log`, role `log-store`): a local
SQLite log that takes QSOs in by **ADIF import with automatic duplicate merge** and answers "worked /
confirmed" for every consumer (first: chase-queue, which builds its log view from it). It has no device and
runs on scmino. Target architecture and where it sits: `../docs/chasing-stations-architecture.md` (§2.2).
Contract: `docs/station-log-mqtt-api.md`. How it is kept in sync with the loggers is **out of scope**; it is
fed by files, `POST /import` and the optional Log4OM UDP feeder.

## What it decides

- **One QSO = own call + call + band + mode class within +- `dup_window` (default 5 min).** The mode class
  (`cw` | `phone` | `data`) is derived from ADIF `MODE`/`SUBMODE`; SSB, USB and LSB are one class. A match
  **merges**, it never skips: later confirmations (`QSL_RCVD`, `LOTW_QSL_RCVD`, `EQSL_QSL_RCVD`, Y or V) and
  award references (`STATE`, `SOTA_REF`, `WWFF_REF`, `SIG`, `SIG_INFO`, `DXCC`, zones, grid) update the stored
  QSO. Earliest time kept, flags only turn true, an empty value never overwrites, a same-source re-import only fills gaps (another source's value corrects; the longer grid wins), mode/frequency are only
  filled in, the source stays the first one's. `store.QSO.Merge` is the single place for these rules.
- **Idempotent.** Importing the same file twice reports `added 0 merged 0` and publishes no event (checked with a real 4562-record Log4OM export).
- **Own call** = `STATION_CALLSIGN`, else `OPERATOR`, else `default_own_call`; a record without one is invalid.
- **Portable calls stay as logged** (`DL1ABC/P` and `DL1ABC` are two calls). Matching them up is the
  consumer's job (chase-queue has `calls.SameStation`).
- **Merge-only import does not remove QSOs deleted in the logger.** `POST /import?replace_source=NAME` (CLI:
  `-source NAME -replace-source`) deletes the QSOs source NAME contributed to (stored or merged into; table `qso_sources`) that the file
  does not mention, unless another source contributed too (then the row stays); off by default; refused for a
  file without a valid record, a truncated or partly invalid file (422) and a delete of more than half of the
  source (`force=1` / `-force` overrides the last).
- Mode classes and band edges are copies of the ideas in chase-queue (`internal/adif` is a copy of its tolerant
  parser; `internal/band` is its own table, with 30 m, 6 m and VHF/UHF). The two copies are independent on
  purpose (separate modules).

## Layout

| Path | Role |
|---|---|
| `cmd/station-log` | main; `-config`, `-check`, `-import-adif FILE [-source NAME] [-replace-source]` (offline, no events) |
| `internal/adif` | tolerant ADIF parser |
| `internal/band` | frequency -> ADIF band name |
| `internal/store` | SQLite (modernc): `qsos`, settings; `Tx.Upsert` (find duplicate + merge), worked view, stats |
| `internal/importer` | ADIF record -> QSO, one transaction per import, `Report`, replace_source, events to a `Sink` |
| `internal/httpapi` | `POST /import`, `GET /healthz /v1/stats /v1/worked /v1/qsos`, `allow_from` guard |
| `internal/log4om` | ADIF_MESSAGE UDP listener (datagram text -> importer) |
| `internal/app` | MQTT slot, jobs worker for events, state publisher, wiring |
| `internal/config` | TOML + env (`STATION_LOG_MQTT_PASSWORD`) |

## Rules that matter

- **Handlers never publish.** Events are queued on a jobs channel and published by one
  `shared/mqtt.RunJobs` worker after the import's transaction committed (a rolled-back import emits nothing);
  the retained `/state` comes from one publisher goroutine (change-detected, 60 s heartbeat).
- **Events are QoS 1, not retained.** A publish that fails (broker down) is counted and shown in
  `/state.problems`; a consumer reloads from `GET /v1/worked` on (re)connect, the events are only the delta.
- **The CLI import is offline**: it opens the database directly and publishes nothing. A running service shows
  the new count in `/state` within a minute; consumers must reload from the API.
- **POST /import has no auth.** Loopback by default; a non-loopback `http.listen` requires `allow_from`
  (validated). Browser requests (`Origin` / `Sec-Fetch-Site`) are refused. One connection: an import holds the
  database, reads wait for it.
- **Personal data.** The database lives under `/var/lib/station-log`, never in git. Test fixtures are
  synthetic (`internal/importer/testdata`); never commit a real log or a Log4OM export.
- **Secrets env-only** (`STATION_LOG_MQTT_PASSWORD`), never in the TOML, logs, `/state` or `/meta`.
- **Not done (roadmap item 2, later):** the Club Log confirmation matrix as a feeder (the `club_log` flag exists
  and is set from `CLUBLOG_QSO_DOWNLOAD_STATUS=Y` in ADIF only), Log4OM full-ADIF file / ADIF POST feeder,
  chase-queue's log view reading `/v1/worked` + events, stationportal inventory entry (add
  `stationportal/internal/inventory/inventory.toml` + `hf_console` host map when deployed).

## Commands

```bash
go test ./... -race                                   # from this directory; GOWORK=off also works
go run ./cmd/station-log -config config.example.toml -check
go run ./cmd/station-log -config my.toml -import-adif log.adi -source log4om-file
./deploy.sh        # build linux/arm64, ship to scmino, seed config once, (re)start — only when the operator says so
ssh io@192.168.1.178 'journalctl -u station-log -f'
```

Config: `/etc/station-log/config.toml` (0600, seed-once; see `config.example.toml`). Port 8096 (HTTP) and, if
enabled, UDP 2341 (Log4OM; chase-queue's own listener uses 2340).
