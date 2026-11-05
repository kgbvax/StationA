# MQTT and HTTP schema — station-log

station-log implements the `hf/station-log` logic slot (role `log-store`) of the station integration model
(`../../docs/station-integration-model.md`). It subscribes to nothing. The event follows the `…/log/event`
idea of `../../docs/logging-integration-model.md` §4/§6.1 (non-retained, QoS 1, stable `id`), with its own
envelope. Derived from `internal/app`, `internal/store` and `internal/httpapi`.

## 1. Connection

| Property | Value |
|---|---|
| Protocol | MQTT 3.1.1, `tcp://127.0.0.1:1883` (the station broker on scmino) |
| Authentication | `[mqtt] user` + `STATION_LOG_MQTT_PASSWORD` |
| Clean session | Yes. Nothing is subscribed |
| Last will | `<slot>/status` = `offline`, retained |
| Client ID | `muehle-hf-station-log` (`mqtt.client_id`) |

## 2. Topics

`<site>/<station>/<slot>` = `muehle/hf/station-log`.

| Topic | Retained | QoS | Payload |
|---|---|---|---|
| `muehle/hf/station-log/status` | yes | 1 | `online` \| `offline` |
| `muehle/hf/station-log/meta` | yes | 1 | `{schema:"1.0", role:"log-store", host, device:{name:"Station log"}, capabilities}` |
| `muehle/hf/station-log/state` | yes | 1 | §3 |
| `muehle/hf/station-log/event` | **no** | 1 | §4 |
| `muehle/hf/station-log/working` | yes | 1 | §3a (only with `[working]` listeners) |

`capabilities`: `{imports:["adif"], feeders:["log4om"]|[], event:"muehle/hf/station-log/event",
events:["added","merged","deleted"], dup_window:"5m", http_api:"127.0.0.1:8096", api:[…]}`. With the working
feed on, `feeders` also lists `logger-udp` and `capabilities.working` is `"muehle/hf/station-log/working"`.
There is no `/cmd`.

## 3. `/state` (retained)

```json
{"ts":"2026-11-03T20:41:07Z","qso_count":5121,"own_calls":["DL9ET","DK0XYZ"],
 "last_import":{"at":"2026-11-03T20:40:55Z","source":"log4om-file","added":12,"merged":3},
 "watch":{"dir":"/var/lib/station-log/drop","last_file":"log.adi","last_at":"2026-11-03T20:40:55Z","files":1,"failed":0,"added":12,"merged":3},
 "problems":[]}
```

- `last_import` is `null` before the first import; it is the last import that held records (a UDP datagram
  counts). `merged` counts matches that changed the stored QSO.
- `watch` is present only when the ADIF drop directory (`[watch] dir`) is configured: `dir`, `last_file` and
  `last_at` (the last file tried), `files` / `failed` (file imports since start; a failing file counts once until it changes), `added` / `merged` (QSOs since
  start), `error` (a failing file, invalid records or a missing directory; stays while the cause is present, omitted when none). Meta `feeders` then lists
  `adif-drop`.
- `working` is present only when the working feed is on: `{listening:["dxlog","log4om"], online, qrz, rejected}`
  (`listening` = listeners that bound, `online` = a logger was heard within `stale_after`, `qrz` = QRZ lookup
  configured, `rejected` = datagrams from hosts outside `allow_from`, omitted when 0; each source is logged once). A listener that failed to bind retries with backoff and returns to `listening` once bound. It never carries the keyed call, so typing does not republish `/state`.
- `problems` (worst first, `[]` when none): empty log; "N QSO events were not published (broker offline)";
  a failed Log4OM listener; an ADIF drop directory error; a working-station listener that could not bind;
  QRZ rejecting the credentials.
- Republished when it changes (ignoring `ts`), on every (re)connect, and re-evaluated every 60 s (so a CLI
  import shows up).

## 3a. Working station `muehle/hf/station-log/working` (retained, QoS 1)

The station the operator has keyed in the shack logger, resolved for the console's map and beam. It replaces
the `/state` of the retired logger-spot-bridge slot `muehle/hf/spots`; the document is the same.

```json
{"ts":"2026-11-05T14:03:12Z","device_online":true,
 "selected":{"call":"VK9XY","freq_hz":14023000,"band":"20m","mode":"cw","locator":"QH93","lat":-11.53,"lng":151.19,
  "azimuth":62.4,"distance_km":15420.3,"country_prefix":"VK9","wpx_prefix":"VK9","name":"Fred Nerk",
  "country":"Australia","qth":"Cairns","source":"dxlog","ts":"2026-11-05T14:03:12Z"}}
```

- `selected` is omitted when the entry was cleared (or nothing was keyed since start). Fields other than
  `call`, `source` and `ts` are omitted when unknown: `freq_hz`, `band` (`20m`, `gen` for the HF gap, none
  outside), `mode` (canonical `cw|usb|lsb|am|fm|data`, `USB-D`/`LSB-D`/`FM-D` keep their carrier), `locator`,
  `lat`/`lng` (0,0 means none), `azimuth`/`distance_km`, `country_prefix`/`wpx_prefix`, `name`/`country`/`qth`.
  `source` is the listener name; `selected.ts` is when the datagram arrived.
- **Every datagram republishes** with a fresh `ts` and `selected.ts` (no dedup: the console re-brightens its
  marker off it). An empty `lookupinfo` call clears; `contactinfo` and `RadioInfo` are not a clear and not a
  selection. Log4OM's CALLSIGN carries no clear signal; the console ages the marker out by `selected.ts`.
- `device_online` is the logger layer of the two-layer liveness: a datagram was heard within `stale_after`
  (false from start until the first one). The bridge layer is station-log's `<slot>/status` (the LWT);
  consumers AND both. A flip republishes the document with the selection kept.
- Position order: the logger's locator; else DXLog azimuth+distance placed from `station_locator`; else
  coordinates give azimuth and distance. A bare call (Log4OM) is placed from the grid of a QSO with that call in
  station-log's own store, then from QRZ. QRZ also fills `name`/`country`/`qth` for every call; it never moves a
  logger-given position. A QRZ failure never blocks publication (call and RF context only).
- Published by the working feed's own worker only (separate from the QSO event worker). On every (re)connect the last document is replayed; with nothing seen yet
  `{ts, device_online:false}` replaces a stale retained one of the previous run.
- Feeders (config `[working]`): UDP `kind="n1mm"` (DXLog/N1MM `lookupinfo`, default port 12060) and
  `kind="log4om"` (Log4OM outbound CALLSIGN = the bare call, default port 2249). `allow_from` is mandatory.

## 3b. Wavelog sync (`/state.wavelog`, `/meta`)

Optional (`[wavelog] direction` pull | push | both); absent from `/state` while off. All Wavelog API details
are from memory and unverified against a real instance (see `CLAUDE.md`).

`/state.wavelog`: `direction`, `url_host` (host only), `last_pull_at`, `last_push_at`, `cursor` (profile id -> last
pulled Wavelog row id), `pulled_added`, `pulled_merged`, `pushed` (these three since start), `push_pending`,
`push_failed` (QSOs given up after 5 attempts until they change), `skipped_no_profile` (pushable QSOs whose own
call has no `[[wavelog.profile]]`), `last_full_resync_at`, `backoff_seconds`, `error`, `note` (one-off remark, e.g. cursors and push ledger reset after an instance URL change). While `error` is set,
`/state.problems` carries `Wavelog: <reason>`; `push_failed > 0` adds a problem line.

`/meta.capabilities`: `feeders` gains `wavelog-pull` (pull on); `sync: ["wavelog"]` appears when push is on.
No new topic or `/cmd`. Pulled QSOs publish `added`/`merged` events like any import (source `wavelog`).

## 4. Event `muehle/hf/station-log/event` (not retained, QoS 1)

One message per QSO that an import added, changed or deleted. Imports that change nothing publish nothing.

```json
{"id":"4711","action":"merged","qso":{"id":"4711","own_call":"DL9ET","call":"K1ABC","band":"20m","mode":"SSB",
 "mode_class":"phone","at":"2026-11-01T10:13:00Z","freq_hz":14205000,"dxcc":291,"state":"MA","cq_zone":5,
 "itu_zone":8,"grid":"FN42AB","lotw_rcvd":true,"qsl_rcvd":false,"eqsl_rcvd":false,"club_log":false,
 "confirmed":true,"source":"log4om-file","created":"2026-11-01T10:20:00Z","updated":"2026-11-03T20:40:55Z"}}
```

- `id` is the stable row id as a string; `qso.id` is the same. Dedupe on `id` + `action` + `qso.updated`
  (QoS 1 is at-least-once). `action`: `added` | `merged` (the stored QSO changed) | `deleted` (`replace_source`;
  `qso` is the QSO as it was).
- `qso` fields: `id, own_call, call, band` (`20m`, `70cm`), `mode` (as logged, upper case), `submode?`,
  `mode_class` (`cw`|`phone`|`data`), `at` (RFC 3339 UTC), `freq_hz?`, `dxcc?` (ADIF number), `state?`,
  `cq_zone?`, `itu_zone?`, `grid?`, `sota_ref?`, `wwff_ref?`, `sig?`, `sig_info?`, `qsl_rcvd`, `lotw_rcvd`,
  `eqsl_rcvd`, `club_log`, `confirmed` (any of the four), `source`, `created`, `updated`. `?` = omitted when
  empty/0.
- Events are the delta only. A consumer loads its view from `GET /v1/worked` at start and on every MQTT
  reconnect (events missed while offline are not queued: the session is clean).
- Events are published after the import's transaction committed, by one worker; a big import (tens of
  thousands of QSOs) publishes one message per QSO.

## 5. HTTP API

`http.listen` (default `127.0.0.1:8096`). Non-loopback needs `http.allow_from` (IPs/CIDRs; other clients get
403). No auth. All responses JSON.

| Request | Response |
|---|---|
| `GET /healthz` | `{"ok":true}` |
| `GET /v1/stats` | `{qso_count, confirmed_count, own_calls[], sources:{name:n}, first_at?, last_at?, dup_window, last_import?}` |
| `GET /v1/worked?own=CALL[&since=RFC3339]` | `{own, count, worked:[{call, band, mode_class, mode, at, confirmed, dxcc?, state?, sota_ref?, wwff_ref?}]}` |
| `GET /v1/qsos?call=X[&own=CALL]` | `{call, count, qsos:[<full qso, §4>]}` oldest first; exact call match (upper-cased) |
| `POST /import[?source=NAME][&replace_source=NAME]` | the import report (§6) |

- `worked`: one row per call + band + mode class over the whole log (or one own call; without `own` all own
  calls are folded together). `at`/`mode` are the latest QSO's, `confirmed` is OR-ed, `dxcc`/`state`/refs the
  latest non-empty values. `since` returns only rows with a QSO added or changed at or after `since`
  (aggregated over the whole row), for a delta refresh. A bad `since` is 400.
- `POST /import`: body = ADIF text (a file, or one record), up to 96 MiB. `source` tags new QSOs (default
  `adif`, max 64 characters). Requests with an `Origin` or `Sec-Fetch-Site` header are 403. `replace_source`
  implies `source` (both given must be equal, else 400); a file without a valid record, a truncated or partly invalid file, or a replace that would delete more than
  half of the source (override: `force=1`) is 422 and deletes nothing.

## 6. Import semantics

Report (HTTP body and `-import-adif` output):

```json
{"records":6,"added":4,"merged":0,"unchanged":0,"skipped_invalid":2,"deleted":0,
 "errors":["record 5: no MODE","record 6: no CALL"]}
```

`records` = parsed records; `added` = new QSOs; `merged` = matches that **changed** the stored QSO;
`unchanged` = matches that changed nothing; `skipped_invalid` = records unusable (no `CALL`, `MODE`,
`QSO_DATE`/`TIME_ON`, a band from `BAND` or `FREQ`, or an own call); `deleted` = `replace_source` only;
`errors` = at most 20 messages `record N: reason`. Importing the same file twice gives
`added 0, merged 0`. An error that aborts the import (database failure) rolls the whole import back.

**Record to QSO.** Own call: `STATION_CALLSIGN`, else `OPERATOR`, else `default_own_call`. `CALL` upper-cased
and trimmed (portable forms kept). Band: `BAND`, else derived from `FREQ` (MHz). Time: `QSO_DATE` +
`TIME_ON` (`HHMM` or `HHMMSS`), UTC. `mode_class`: `CW` -> `cw`; `SSB USB LSB AM FM DIGITALVOICE C4FM DSTAR
FREEDV DMR M17` (or a `USB`/`LSB` submode) -> `phone`; everything else -> `data`. Confirmation flags: ADIF
`Y` or `V` in `QSL_RCVD`, `LOTW_QSL_RCVD`, `EQSL_QSL_RCVD`, `CLUBLOG_QSO_DOWNLOAD_STATUS`.

**Duplicate key and merge.** Key: own call + call + band + mode class, and a stored QSO within +- `dup_window`
(inclusive; the closest one if several). A match merges:

| Field | Rule |
|---|---|
| time | the earlier one is kept |
| `qsl_rcvd`, `lotw_rcvd`, `eqsl_rcvd`, `club_log`, `confirmed` | only ever turn true |
| `state`, `sota_ref`, `wwff_ref`, `sig`, `sig_info`, `dxcc`, `cq_zone`, `itu_zone` | a non-empty value from **another source** overwrites (a correction); from the same source it only fills an empty field, so re-importing a file never churns (two log entries of one QSO may disagree). Fix a wrong value in the same source with `replace_source` |
| `grid` | the more precise (longer) locator wins, whatever the source |
| `mode`, `submode`, `freq_hz` | filled in only when the stored one is empty |
| `source`, `created`, `own_call`, `call`, `band`, `mode_class` | unchanged |

`updated` moves only when something changed.

**Open question: deletes.** A merge-only import cannot remove a QSO that was deleted in the logger. Option
`replace_source=NAME` (or CLI `-source NAME -replace-source`) deletes the QSOs source NAME contributed to (stored
or merged into) that the file does not mention, in the same transaction, and publishes a `deleted` event for each. It is off by
default, and a QSO that another source also contributed to (for example LoTW confirmations) loses only NAME's claim
and stays. It refuses a truncated or partly invalid file and a delete of more than half of the source unless
`force=1` (CLI `-force`) is given. Use it only with a full export of that logger. Whether a snapshot replace is enough
for the logger sync or the loggers should feed events instead is still open (architecture §6).
