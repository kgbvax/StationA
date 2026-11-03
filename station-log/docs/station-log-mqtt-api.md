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

`capabilities`: `{imports:["adif"], feeders:["log4om"]|[], event:"muehle/hf/station-log/event",
events:["added","merged","deleted"], dup_window:"5m", http_api:"127.0.0.1:8096", api:[…]}`. There is no `/cmd`.

## 3. `/state` (retained)

```json
{"ts":"2026-11-03T20:41:07Z","qso_count":5121,"own_calls":["DL9ET","DK0XYZ"],
 "last_import":{"at":"2026-11-03T20:40:55Z","source":"log4om-file","added":12,"merged":3},
 "problems":[]}
```

- `last_import` is `null` before the first import; it is the last import that held records (a UDP datagram
  counts). `merged` counts matches that changed the stored QSO.
- `problems` (worst first, `[]` when none): empty log; "N QSO events were not published (broker offline)";
  a failed Log4OM listener.
- Republished when it changes (ignoring `ts`), on every (re)connect, and re-evaluated every 60 s (so a CLI
  import shows up).

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
