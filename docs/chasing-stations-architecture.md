# Chasing Stations — domain architecture

Status: **target architecture, 2026-11-03** (operator-directed). It supersedes the role and scope statements
of `plans/2026-11-01-2252-feat-chase-queue-v1-plan.md` and `plans/2026-11-02-1908-feat-chaseq-ui-plan.md`
where they conflict. Those plans stay the record of what v1 built. §5 lists the gaps between this target and
the code as of `main` 3226c43.

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
- Slot address and role name are to be fixed when it is planned (proposal: `muehle/hf/station-log`, role
  `log-store`).

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

1. **Self-contained rows.** chase-queue publishes `freq_hz`, `mode` and `source` on every row (backend done:
   `source` is `bmrec` | `cluster`, mode derived in `chase-queue/internal/mode`). Decision with it: a station
   worked on the row's band is no longer hidden, in DX and contest mode; it is listed after the unworked ones
   with `needed:false`, no tag and `worked_before.this_band`. chaseq-ui dropped its `cqfinder` join, and QSY uses
   the row's mode instead of the frequency rule.
2. **station-log v1.** New module: store, ADIF import + duplicate merge, `-import-adif FILE`, upload API,
   MQTT `/state` + events, read API. chase-queue builds its log view from it. The Log4OM UDP listener and the
   Club Log client move to it as feeders.
3. **Generic award model.** Evaluator + definition file + Log4OM XML importer. DXCC-per-band and contest
   become definitions. Goals. `needs[]` on the wire. chaseq-ui shows the award(s) per row.
4. **bmrec qualification gate.** chase-queue takes only qualified (or high-confidence) rows. This needs a
   qualification field in bmrec's `cqfinder` rows (bmrec change, outside this repo).
5. **Buddies** as a ranking input.
6. **Clusters 1..n.** `[[cluster]]` list with a name per source. RBN per source. A band table beyond HF 160–10 m.
7. **chaseq-ui:** configurable logger senders (today Log4OM only; the DXLog script was dropped 2026-11-03). Show `info` / `worked_before`.

## 5. Gaps: as-is (main 3226c43) vs this target

| Target | Status | As-is |
|---|---|---|
| 1..n clusters, RBN optional | partial | One `[cluster]` struct (`chase-queue/internal/config/config.go`), one telnet login. RBN = `skimmer` on/off. HF 160–10 m only (`internal/band/band.go`). The feed stops while bwpc sleeps (powerseq standby). |
| Cluster spots = available stations | differs | Cluster spots mainly hide heard rows. Only needed cluster spots become rows, at most 40 (`engine.go` `Compute`). |
| Only qualified bmrec rows | differs | Every open bmrec row is listed, also untrusted and call-less ones. `call_trusted` is used only for contest skips; `call_source` is not read. |
| station-log | missing | History is in chase-queue: Log4OM UDP since start + Club Log DXCC×band matrix. No import, no backfill, `STATION_CALLSIGN` ignored. |
| Award definitions, Log4OM XML | missing | One hard-coded rule (DXCC not confirmed on the band, any mode). Contest is a separate, exclusive evaluator. |
| Contest = one award type | differs | Contest is an "activity" with its own mode switch (`set_mode`, `switch_instance`). |
| Buddies | missing | — |
| Needed for which award | missing | Wire has `needed` + `tag` only. The engine computes per-slot valuations and drops them. |
| QRZ.com enrichment | matches | Display-only, cached. chaseq-ui does not show it yet. |
| Self-contained rows / UI only displays | matches | Every row carries `freq_hz`, `mode` and `source`; chase-queue derives the mode (sideband rule for bmrec rows; comment keyword, then IARU R1 band plan for cluster rows). Worked-on-band rows are listed last-but-candidates, not needed. chaseq-ui reads `freq_hz`/`mode`/`worked_before` from the row and no longer subscribes to `cqfinder`. |
| Local filters | matches | Band (160–6 m, fixed) and needed-only. |
| Activate = QSY + logger | partial | FLEX via flexbridge `set_freq`/`set_mode`. Logger fixed to Log4OM (the DXLog script was dropped 2026-11-03). |
| skip/undo from the UI | matches | `handled` is sent in contest only. |

## 6. Open questions

- **Snapshot import.** A full Log4OM ADIF file carries deletes. Merge-only import does not remove a QSO that
  was deleted in the logger. Does station-log also need a per-source snapshot replace?
- **Scoring across awards.** How several `needs` on one row add up in the ranking.
- **station-log slot address and role name.**
