# Chasing Stations — domain architecture

Status: **target architecture, 2026-11-03, implemented and live since 2026-11-04** (operator-directed; items 1-7
of the roadmap are built, merged to `main` and deployed; the Log4OM cutover to station-log is done; bmrec sends
`qualified` and chase-queue's gate is `qualified`). It supersedes the role and scope statements
of `plans/2026-11-01-2252-feat-chase-queue-v1-plan.md` and `plans/2026-11-02-1908-feat-chaseq-ui-plan.md`
where they conflict. Those plans stay the record of what v1 built. §5 lists the gaps between this target and
the code as built; the as-is column now matches the live system except where it says *left*.

## 1. Purpose

Show the operator, at any moment, which stations on the air are worth working, why, and in what order, and
let the operator go to one with one action. The domain stops at the QSO: logging the QSO, and keeping the
station's loggers in sync, are outside it.

## 2. Components and roles

```
 DX clusters (1..n)  ──spots──┐
 bmrec (CQ finder)   ──qualified rows──┐
                                ▼      ▼
 station-log ──log view──►  chase-queue  ──ranked list──►  chaseq-ui ──QSY──► radio (flexbridge)
   ▲   (QSOs, confirmations)   ▲   ▲                          │      └─call─► logger(s)
   │                           │   └── award definitions      └─skip/undo──► chase-queue
 ADIF import, feeders          └────── buddies, QRZ.com (enrichment)
```

| Component | Role in the domain | Owns |
|---|---|---|
| **DX clusters** (1..n) | Sources of human spots. RBN skimmer spots are optional per cluster. **Now: one cluster, Log4OM NG's internal cluster** (telnet relay on bwpc `192.168.1.197:23`). | — |
| **bmrec** (outside this repo) | Detects stations calling CQ and lets the operator **qualify** candidates on its HF CQ page. Only qualified rows (or rows with high confidence) are available stations. bmrec's page stays: it is the qualification tool, not a second chase view. | detection, qualification |
| **Loggers** (Log4OM primary; DXLog, N1MM, WSJT-X, …) | Where QSOs are logged. They are sources for `station-log` and targets of the call that chaseq-ui sends. No domain component depends on one specific logger. | the operator's logs |
| **station-log** (new component) | The station's QSO record for this domain: a local log database. It takes QSOs in by **ADIF import with automatic duplicate merge** (v1), and later by feeders. It answers "worked / confirmed" for every consumer. How it is kept in sync with the loggers is **out of scope of this domain**. | QSOs, confirmations, own callsigns |
| **chase-queue** (`muehle/hf/chase`) | Takes in available stations (cluster spots + qualified bmrec rows), the log view from station-log, award definitions, buddies and QRZ.com data. Decides which stations are needed **for which award**, ranks them, and publishes one self-contained ranked list. It keeps the skip/undo state. | award evaluation, ranking, skip/undo |
| **chaseq-ui** (bwpc) | Shows chase-queue's ranked list. Filters locally (band, needed-only, …). **Activate** a row = QSY the radio + put the call into the logger(s). Sends skip/undo to chase-queue. It does no joining, valuing or ranking of its own. | view filters |

### 2.1 Awards

- An **award** is a definition (key + slice + confirmation rule), not Go code. **DXCC per band is the first
  definition.** The current hard-coded rule (`chase-queue/internal/engine/engine.go` `dxValueLocked`) becomes
  one call of a generic evaluator.
- **A contest is one type of award.** Its multiplier slots (DXCC/CQ zone/ITU zone/WPX/custom, from a DXLog
  definition) are the award's slots, and its history is the contest log. "Activity dx/contest" stops being a
  separate mode switch. It becomes "which awards (goals) are active".
- **Log4OM award XML is the starting point** for definitions. It is imported into a small definition file
  that chase-queue owns. It is not read natively at runtime.
- **Goals** select the active award configurations (e.g. DXCC, WAS-20-CW, a running contest), each with a
  priority.
- horstreporter's award engine is **removed**. It is not an alternative or a source.

The detailed options are in the configurable-award ideation of 2026-11-02 (`docs/ideation/2026-11-02-
configurable-award-definitions-ideation.html`, branch `claude/configurable-award-definitions-e4d9db`, not yet
on main): generic award-slot evaluator (#1), own definition file with Log4OM XML as import (#3), catalog
refresh (#4), `needs[]` on the wire (#5), goals (#6). Its idea #2 (Log4OM full ADIF + ADIF POST as the history
source) applies to **station-log**, not to chase-queue.

### 2.2 station-log

- Its own component and Go module. SQLite on scmino. Personal data: the database is never in git.
- **ADIF import with duplicate merge** (v1). Duplicate key: own call (`STATION_CALLSIGN`, else `OPERATOR`) +
  worked call + band + mode + `QSO_DATE`/`TIME_ON` within a small window. A match **merges** (it does not skip):
  confirmation fields that arrive later (`QSL_RCVD`, `LOTW_QSL_RCVD`, `EQSL_QSL_RCVD`) and award references
  (`STATE`, `SOTA_REF`, `WWFF_REF`, `SIG_INFO`) update the stored QSO.
- **Own callsigns** come from the records. Award evaluation filters on the active callsign.
- **Interfaces.** MQTT per the station model: `/meta`, `/status`, a retained `/state` summary (QSO count,
  own calls, last import, problems) and a non-retained per-QSO event (added / merged / deleted). The event
  follows the `…/log/event` shape in `logging-integration-model.md` §4/§6.1. Plus a read HTTP API for the
  initial load and for queries: MQTT is no query interface, and the whole log does not fit in a retained
  message.
- **Feeders** (later, outside the domain's decision logic): Log4OM `ADIF_MESSAGE` UDP and the Club Log
  confirmation matrix move here from chase-queue, and Log4OM full ADIF file / ADIF POST (ideation #2) can follow.
  Each feeder is an import path into the same store.
- Slot `muehle/hf/station-log`, role `log-store` (fixed 2026-11-03). HTTP on `127.0.0.1:8096`; contract in
  `station-log/docs/station-log-mqtt-api.md`.

chase-queue keeps a **derived log view** (worked/confirmed per call, entity, band, mode). It loads the view
from the station-log API at start and keeps it current from the events, so ranking never waits on a remote
call.

### 2.3 The ranked list (wire target)

Each ranked row is **self-contained**: `freq_hz`, `mode` (when known), `source` (`bmrec` | `cluster:<name>`
| `rbn`), call, band, and `needs: [{award, key, band, mode, status}]`. `needed` and `tag` stay as a derived
summary until all consumers have moved. Enrichment (`info` from QRZ.com, `worked_before` from the log view)
stays display-only. A consumer never needs bmrec's `cqfinder` topic to tune a row.

### 2.4 Scope

- **In:** clusters (1..n, RBN optional), bmrec qualified rows, award definitions incl. contests, goals,
  buddies, QRZ.com enrichment, station-log via ADIF import, activation (QSY + logger call), skip/undo.
- **Deferred (may come later):** POTA, SOTA and WWFF (operator decision 2026-11-03). They are reference
  awards that can be matched only through cluster comments (ideation #7) and come back together.
  station-log still stores their ADIF reference fields, so no history is lost meanwhile.
- **Out:** keeping the loggers in sync with each other or with station-log, logging the QSO, spotting
  (posting to a cluster).

## 3. Interaction rules

- bmrec decides **what is a candidate**, and the operator qualifies it there. chase-queue decides **what is
  needed and in what order**. chaseq-ui decides only **what this window shows**.
- chase-queue is the only writer of the ranked list. bmrec and chaseq-ui both send skip/undo to it.
- station-log is the only writer of the QSO record. chase-queue reads, it never writes QSOs.
- Spot sources are kept apart by `source`. A spot hides a heard row only when the row that stands for it is
  listed.

## 4. Roadmap (each item gets its own plan)

Status 2026-11-03: items 1 to 7 are built, merged to main and deployed (station-log, chase-queue, chaseq-ui).
**Cutover done 2026-11-04:** Log4OM's UDP ADIF_MESSAGE goes to station-log (2341), chase-queue reads its history
from station-log (`[station_log]`, own_call = DL9ET; its own `[log4om]` listener is off), the bmrec gate is `off`.
The gap between the first export (2026-10-25) and the cutover was closed by importing a fresh export on
2026-11-04 (202 QSOs added, 54 merged; `curl --data-binary @export.adi
'http://127.0.0.1:8096/import?source=log4om-file'` on scmino; the duplicate merge absorbs the overlap). Rollback steps: `chase-queue/CLAUDE.md`.
Deploy order: **station-log, then chase-queue, then chaseq-ui** (an older chase-queue leaves rows untunable and
sets no mode; chase-queue without station-log keeps its local log). Cutover steps: `known-issues.md` "Chasing
Stations" and `chase-queue/CLAUDE.md`.

1. **Self-contained rows.** Built and merged on main. chase-queue publishes `freq_hz`, `mode` and `source` on
   every row (mode derived in `chase-queue/internal/mode`); a station worked on the row's band is listed after
   the unworked ones with `needed:false`, no tag and `worked_before.this_band`. chaseq-ui has no `cqfinder`
   join; QSY uses the row's mode.
2. **station-log v1.** Built: new module `station-log/` (SQLite store, ADIF import with duplicate merge,
   `-import-adif FILE`, `POST /import` with optional `replace_source`, MQTT `/meta` `/state` and
   `…/event`, read API `GET /v1/worked /v1/qsos /v1/stats`, optional Log4OM `ADIF_MESSAGE` UDP feeder on 2341).
   chase-queue builds its log view from it (`[station_log]`, `internal/logview`: load at start, on reconnect and
   periodically; kept current from the events). **Left:** the Club Log confirmation matrix stays in
   chase-queue (decided: it is entity-level, not a QSO list, so it is no feeder unless a Club Log log download
   is built); the Log4OM full-ADIF file feeder is built
   as the ADIF drop directory (`[watch]` in station-log, off until configured and Log4OM exports into it); an ADIF
   POST feeder from Log4OM itself is not (ideation #2).
3. **Generic award model.** Built: `chase-queue/internal/award` (definition file in TOML, slot evaluator,
   built-in DXCC per band, field awards such as WAS, contest wrapper, Log4OM XML importer
   `-import-award-xml`), `[[goal]]` with priority, `needs[]` on the wire, chaseq-ui shows the award(s) per row
   and has an award filter. The XML importer is checked against the real `award_WAS.xml` (2026-11-04; two bugs found
   and fixed: item keys are in `RC`, the CW/PHONE/DIGITAL split is in `EmissionType`); the real `dxcc.xml` is
   recognised as the built-in DXCC award (exit code 4, nothing imported). A need carries `atno: true` when the key (for DXCC the entity) is
   worked in no slice at all, so the window can tell an all-time new one from a new band slot; chaseq-ui shows an
   ATNO chip. **Left:** Definitions reload within 30 s of a file change or on SIGHUP (a bad reload keeps the old catalog and shows in `problems`). No catalog refresh (ideation #4). Only field awards (a `QsoField` with allowed values) and DXCC are
   covered; POTA, SOTA and WWFF are deferred and rejected.
4. **bmrec qualification gate.** Built and live (2026-11-04): `[bmrec] gate` = `off` | `trusted` (default) |
   `qualified`, rows kept out counted in `/state.bmrec_unqualified`. bmrec (commit 27fd301, on its `master`
   since e3fdca8) sends `qualified` (the operator's own action on the HF CQ page) and `confident` on every
   `cqfinder` row (spec: `chase-queue/docs/chase-queue-mqtt-api.md` section 5). chase-queue on scmino runs with
   `gate = "qualified"`, so only rows the operator qualified, plus needed cluster spots, are listed.
5. **Buddies.** Built: `[[buddy]]` (call, note, boost); `buddy:true` on the row, own tier below the needed
   block, first among needed rows of equal goal priority; chaseq-ui shows a badge and the note.
6. **Clusters 1..n.** Built: `[[cluster]]` list with a name per source (`cluster_name` on the row), RBN
   per entry (`skimmer`), the same call and band on several clusters is one row; band table extended by 6 m,
   2 m and 70 cm. **Left:** a second cluster is not configured live, and the Log4OM cluster on bwpc still
   stops while bwpc sleeps.
7. **chaseq-ui.** Built: configurable logger senders (`[[loggers.sender]]`, `Sender` seam; only `log4om`
   exists), `info` / `worked_before` / `needs` / `buddy` shown, filters for band, needed only and award.
   **Left:** a second logger kind; filters beyond HF bands are not exposed (rows on 6 m and VHF/UHF arrive,
   the UI band list is fixed).

## 5. Gaps: as-is (implementation branch) vs this target

| Target | Status | As-is |
|---|---|---|
| 1..n clusters, RBN optional | matches | `[[cluster]]` entries with a name each, one reconnecting telnet client per entry, RBN per entry, merge by call+band. Bands 160-10 m, 6 m, 2 m, 70 cm. Left: no second cluster configured live; the Log4OM cluster stops while bwpc sleeps (powerseq standby). |
| Cluster spots = available stations | matches | Needed cluster spots (and buddies) become rows with `source:"cluster"` and `cluster_name`, at most 40; a spot also hides the heard row it names. |
| Only qualified bmrec rows | matches | The gate (`off` / `trusted` / `qualified`) is live on `qualified`; bmrec sends `qualified` and `confident` (27fd301). First bmrec-sourced row after qualification not yet observed live (band conditions). |
| station-log | matches (live) | Own module and slot with ADIF import, duplicate merge, `replace_source`, events and read API. chase-queue's log view reads it. Left: Club Log matrix feeder (stays in chase-queue by decision), ADIF POST feeder; the ADIF drop directory (`[watch]`) is built but not yet enabled live. |
| Award definitions, Log4OM XML | matches | Definition files + built-in DXCC + XML importer, checked against real Log4OM WAS and DXCC exports. Definitions load at start only. |
| Contest = one award type | matches | The contest is the award `contest` (priority 100) while an instance runs; `set_mode` / `switch_instance` remain as instance control, `activity` is derived. DXCC stays a goal in a contest unless a `[[goal]]` turns it off. |
| Buddies | matches | `[[buddy]]`, see item 5. |
| Needed for which award | matches | `needs[]` `{award, key, band, mode, status, atno?}` on every needed row; `needed` + `tag` are the derived summary. |
| QRZ.com enrichment | matches | Display-only, cached; chaseq-ui shows `info` as one line. |
| Self-contained rows / UI only displays | matches | `freq_hz`, `mode`, `source` on every row; chaseq-ui has no `cqfinder` subscription. |
| Local filters | matches | Band, needed only, award. |
| Activate = QSY + logger | partial | FLEX via flexbridge `set_freq` / `set_mode`; loggers are config (`[[loggers.sender]]`), only Log4OM exists. |
| skip/undo from the UI | matches | `handled` is sent in contest only. |

## 6. Open questions and defaults chosen

Defaults taken on 2026-11-03 so the implementation could proceed; change them here and in code together.

- **Snapshot import.** Decided: optional. `POST /import?replace_source=NAME` (CLI `-source NAME
  -replace-source`) deletes the QSOs that source NAME contributed to and the file does not mention, unless
  another source contributed too. Off by default; refused for an empty, truncated or partly invalid file and
  for a delete of more than half of the source (`force` overrides the last). Plain imports stay merge-only.
- **Scoring across awards.** Decided: goal priority of the first award that needs the station, then the number
  of satisfied-by-this-station awards (more first), then the existing keys of the old order (NewSlots, Most
  Wanted, first seen). With one goal this is the old order. Defaults: contest 100, DXCC 10, other awards 50.
- **station-log slot address and role name.** Decided: `muehle/hf/station-log`, role `log-store`.
- **bmrec gate default.** `trusted`. `qualified` is the target once bmrec sends the field.
- **bmrec `qualified` field (to do in bmrec).** Each `cqfinder/state.rows` entry gets `"qualified": true|false`
  on every row (no `omitempty`): true when the operator qualified the row on the HF CQ page or the detection is
  confident, false otherwise. Full spec: `chase-queue/docs/chase-queue-mqtt-api.md` section 5. Without the field,
  `gate = "qualified"` lists nothing and reports `bmrec sends no qualified flag`.
- **Club Log matrix.** Stays in chase-queue: it is an entity-level matrix, not a QSO list.
- **Still open.** Whether the simplified IARU R1 band plan gives the right mode on the air (the first live rows
  looked right: KH8A 28033 CW, 21270 USB, 18101.9 data); whether a second cluster (RBN) is wanted live.
