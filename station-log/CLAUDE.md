# CLAUDE.md — station-log

station-log is the station's **QSO record** (logic slot `muehle/hf/station-log`, role `log-store`): a local
SQLite log that takes QSOs in by **ADIF import with automatic duplicate merge** and answers "worked /
confirmed" for every consumer (first: chase-queue, which builds its log view from it). It has no device and
runs on scmino. Target architecture and where it sits: `../docs/chasing-stations-architecture.md` (§2.2).
Contract: `docs/station-log-mqtt-api.md`. How it is kept in sync with the loggers is **out of scope**; an optional two-way Wavelog sync exists (below); it is
fed by files, `POST /import` and the optional Log4OM UDP feeder. It also publishes the **currently working
station** (the call keyed in DXLog/Log4OM, resolved to a map position and beam bearing) on the retained sibling
topic `muehle/hf/station-log/working`; this absorbed logger-spot-bridge (slot `muehle/hf/spots`).

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
| `internal/watch` | ADIF drop-directory feeder (`[watch]`): lists `*.adi`/`*.adif`, imports unseen files (name+size+mtime in settings `watch_imported`) through the importer, `/state.watch` |
| `internal/log4om` | ADIF_MESSAGE UDP listener (datagram text -> importer) |
| `internal/working` | working-station feed: `Feed` (liveness, jobs-worker publishing), `Resolver`, UDP `Listener`, `CanonicalMode`/`NormalizeBand`; subpackages `n1mm` (DXLog lookupinfo), `callsign` (Log4OM CALLSIGN), `geo`, `qrz` (+ disk cache), copied from logger-spot-bridge |
| `internal/wavelog` | Wavelog two-way sync (`[wavelog]`, off by default): `Client` (REST API v2, Bearer token header), `Syncer` (pull cursor, push ledger, backoff, Streak), `BuildADIF` |
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
- **The drop directory** (`[watch] dir`, off by default) never deletes or moves a file, skips one whose mtime is
  younger than 5 s, merges (never `replace_source`) and keeps going after a bad file (one Warn per outage via
  `shared/logging.Streak`, shown in `/state.watch.error` and `problems`). The first scan waits for the MQTT
  link (events go through the same queue as POST /import). A failed file is not retried until its
  name+size+mtime changes (`failed` counts files, not retries); its error stays in `watch.error` while it is
  present, and a good file in the same pass does not mask it. The service user must be able to read
  the directory (`ProtectSystem`/`ReadWritePaths` do not matter for reading; check `ProtectHome`). A missing
  directory is a Warn, not fatal. Deploy only seeds `config.example.toml` once; an existing config needs the
  section added by hand.
- **Working-station feed** (`[working]`, off without a `[[working.listener]]`; contract `docs/station-log-mqtt-api.md`
  §3a). Behaviour kept from logger-spot-bridge: every datagram republishes with a fresh ts (no dedup on the
  selection path); an empty `lookupinfo` call clears, `contactinfo` is not a clear; a QRZ failure or stall
  never blocks publication (bounded to 3x the request timeout, runs on the feed's own worker); DXLog az+dist beats
  a QRZ position; (0,0) is the no-coordinates sentinel; `device_online` is false until the first datagram and
  drops after `stale_after`. Handlers (UDP, paho) only decode and enqueue; the feed has its OWN queue + `RunJobs` worker (`wjobs`), separate from the QSO event worker, so a big import or a QRZ stall cannot starve the other. The
  document lives on its own topic so keystrokes never churn `/state`; `/state.working` has no call in it. A
  listener that cannot bind or read is a `/state` problem and not fatal; it retries with capped backoff (1 s..30 s, one Warn per outage) and the problem clears on success; datagrams from hosts outside `allow_from` are Warned once per source and counted in `/state.working.rejected`; `allow_from` is mandatory. Default ports
  keep the old loggers' settings valid (DXLog 12060, Log4OM CALLSIGN 2249); stop logger-spot-bridge first
  (EADDRINUSE). A bare call is placed from `qsos.grid` of that call before QRZ is asked. Secret:
  `STATION_LOG_QRZ_PASSWORD` (env file, never TOML/log; it rides in the QRZ request URL, so the qrz package unwraps `*url.Error` from every error); cache `<data_dir>/qrz-cache.json`. No Log4OM clear
  signal exists: the console ages the marker out by `selected.ts`. Band uses `internal/band` (so 30 m/6 m/VHF
  have names, 60 m is 5.06-5.45 MHz) plus `gen` for the HF gap; modes are the console's canonical six, not
  `mode_class`.
- **Wavelog sync** (`[wavelog] direction = off|pull|push|both`; contract `docs/station-log-mqtt-api.md` §3b). Speaks the
  documented **Wavelog REST API v2** (<https://docs.wavelog.org/developer/api-v2/>, needs Wavelog >= 3.1.0; the
  legacy v1 API is not used and v1 keys are refused). Auth is `Authorization: Bearer <token>` only. Token scopes:
  `qso:read` (pull), `qso:write` (push), `station:read` (only for `-wavelog-check`). **Verified against the real
  instance (`https://log.dclnext.darc.de`) only up to the unauthenticated probe** (`GET /api/v2/status` ->
  `{"data":{"name":"Wavelog API","status":"ok"},...}`; an invalid token -> HTTP 401
  `{"error":{"code":"invalid_token",...}}`); everything behind a token (the QSO shapes, the `/token` and `/station`
  field names, scope behaviour) is encoded from the docs in `internal/wavelog/{fake,sync}_test.go` (httptest fakes,
  doc sections cited) and unverified until the operator provides a token. Run a pull-only first. Rules: one
  goroutine, never inside the importer lock or an HTTP handler (a Wavelog outage cannot block imports); first cycle
  waits for the MQTT link. **Pull** per `[[wavelog.profile]]` (`own_call` -> integer `station_profile_id`, also the
  default own call for ADIF without `STATION_CALLSIGN`): `GET /api/v2/qso?format=adif&since_id=<cursor>&station_id=<id>&per_page=<pull_limit>`;
  rows ascend by id, `data.adif` (null when nothing new) goes through the importer with source `wavelog` (never
  `replace_source`; the ADIF frequency is MHz), `data.lastfetchedid` is the cursor (settings `wavelog_cursor_<profile>`),
  written only after the page was imported, paging continues while `meta.has_more`. A full pull (`since_id=0`) runs at
  first enable and every `full_resync_hours` (Wavelog's own LoTW/QRZ sync confirms old rows). **Push** takes candidates
  from a store query (not events; a Kick after logger events only wakes the loop, debounced 10 s): own call has a
  profile, QSO time >= `push_since` (default = first-enable time, kept in settings `wavelog_push_since`, so history is
  not bulk-pushed), a source matches `push_sources`, **no `wavelog` row in `qso_sources`** (structural loop guard:
  pulled or merged-with-Wavelog QSOs are never pushed), no `wavelog_push` row. **A QSO is pushed once**: Wavelog
  skips duplicates on import and has its own LoTW/QRZ sync, so there is no re-push for gained confirmations. Batches
  of `batch_size` records go as `POST /api/v2/qso` `{"import_type":"adif","station_profile_id":N,"adif":"..."}`;
  the answer `data{parsed,imported,skipped,messages}` is a success when imported + skipped cover every record
  (`skipped` = Wavelog already has it). A batch that is refused (400 `validation_error`) or stored fewer records is
  bisected down to single records; a record that still fails counts an attempt (up to 5, then `push_failed` until its
  fingerprint changes), the rest goes through. A record that fails alone counts an attempt only when a sibling was
  stored (same batch or earlier in the cycle's run for that profile; a single-record batch also counts when the
  profile pushed before). If nothing is stored (or three identical single-record refusals precede any success) the
  refusal is systemic: the cycle aborts with an endpoint error carrying Wavelog's message, **no attempt is counted**,
  the backlog stays pending. Requests per batch are capped at `3*len+10`. Wavelog never gets deletes from us (and we none from it).
  `qso_raw` keeps the first non-Wavelog ADIF record of a QSO so a push keeps RST/name/comment; a QSO imported before
  v3 has none and goes out struct-only (re-import the original file to fill it). `store.WeakSources` (`wavelog`): its
  DXCC/zones/state/references only fill gaps, flags still OR in. **Errors** (v2 envelope `{"error":{"code","message","details"}}`):
  401 (`invalid_token`, `token_expired`) is a dead token (15 min backoff at once); 403 `insufficient_scope` names the missing scope (`details.required_scope`) in `/state.wavelog.error` and `/state.problems`,
  and only the affected direction stops (a missing `qso:write` does not block the pull and vice versa); every other 403 (`forbidden` = station id not
  the token owner's, `insufficient_club_permission`, `club_access_revoked`) is **per profile**: that profile is skipped
  with an entry in `/state.wavelog.problems` (also in `/state.problems`) naming own call and station id, the other
  profiles carry on, no backoff; the entry clears once the profile works again. Any endpoint-level error (429, 5xx,
  transport, timeout, HTML, nonsense body, a pull page with `has_more` but a `lastfetchedid` that does not advance)
  aborts the rest of the cycle at once; the backoff uses the largest `Retry-After` among the joined errors. 429 honours
  `Retry-After` (seconds or HTTP date, else `details.retry_after`); 404/405/5xx/HTML/unreadable 2xx are endpoint errors:
  backoff, no attempt counted against a QSO. Backoff 30 s doubling to 15 min with jitter, one Warn per outage
  (`logging.Streak`), a logger kick is skipped while backing off, pull and push run per profile (scope errors and
  profile refusals do not stop the other profiles/direction; endpoint errors do). QSOs at 5 attempts get new ones after 24 h (`wavelog_attempts_reset_at`) or when the profile
  map changes (`wavelog_profiles_hash`). `wavelog_url_hash` ties cursors, full-resync marks and the push ledger to the
  instance URL: a changed URL resets them with a Warn and `/state.wavelog.note`. An unreadable `wavelog_push_since`
  is re-initialised to now with a Warn, never to zero. Secret `STATION_LOG_WAVELOG_KEY` (env only, must start with
  `wl2_`; it rides in the header only, never in a URL, body, log line, error or `/state`; response text echoed into
  errors is truncated and token-redacted); https required except for loopback/private hosts; redirects not followed.
  **`station-log -wavelog-check -config X`** (token from the env; needs only `url`, not direction or profiles) calls
  `GET /api/v2/token` and `GET /api/v2/station` and prints the token's scopes (flagging missing ones) and the station
  locations (id, callsign, name) so the operator can fill `[[wavelog.profile]]`; it never prints the token. The
  `/token` and `/station` field names are not in the saved docs, so they are read leniently (several usual names).
- **Not done (roadmap item 2, later):** the Club Log confirmation matrix as a feeder (the `club_log` flag exists
  and is set from `CLUBLOG_QSO_DOWNLOAD_STATUS=Y` in ADIF only), an ADIF POST feeder from Log4OM itself,
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
enabled, UDP 2341 (Log4OM; chase-queue's own listener uses 2340) and the `[working]` listeners (12060 DXLog, 2249
Log4OM CALLSIGN; chase-queue's contest feed needs DXLog's second target on 12061).
