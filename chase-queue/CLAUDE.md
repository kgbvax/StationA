# CLAUDE.md — chase-queue

chase-queue is the **chase queue** logic slot `muehle/hf/chase`. It reads the CQ stations bmrec hears
(`muehle/hf/cqfinder/state`) and a DX cluster, knows the operator's history, and publishes one ordered
list of what is needed. bmrec's HF CQ page (where the operator qualifies candidates) and chaseq-ui show that
list; other consumers (hf_console, Home Assistant) can read the same retained state. It has no device and
runs on scmino. Contract: `docs/chase-queue-mqtt-api.md`. **Target architecture of the domain** (roles,
award model, the separate `station-log`, gaps to the code): `../docs/chasing-stations-architecture.md`.
v1 requirements: `../docs/plans/2026-11-01-2252-feat-chase-queue-v1-plan.md`.

## What it decides

**Awards and goals** (`internal/award`, 2026-11-03; roadmap item 3). What makes a station needed is no longer
two hard-coded modes but the enabled **goals**: each is an award (definition: key + slice + rule) that says
which slot the station would fill and whether the log has it. DXCC per band is the built-in award (below);
a field award such as WAS comes from a TOML file in `<data_dir>/awards`; the running contest is the award
`contest` (wrapping `internal/contest`). A row is needed when any enabled goal's slot is open; `needs[]` on the
wire says which. Ranking in the needed tier: goal priority, number of awards, then NewSlots / Most Wanted /
first seen. Slots that cannot be told stay `unknown` (own tier), never "not needed". The old `activity` is
derived (`contest` while an instance runs); skip/handled keep their contest-vs-DX semantics keyed on that.
Reference awards (SOTA/WWFF/POTA) are deferred and rejected. Format and rules: `docs/chase-queue-mqtt-api.md`.
Behaviour change worth knowing: in a contest DXCC stays a goal (priority below the contest), so a station can be
`need` (not `mult`); `[[goal]] award = "dxcc" enabled = false` gives the old contest-only ranking.

- **Contest** award (instance running): ranking from bmrec's former contest engine — new multipliers first
  (more new slots, then Club Log Most Wanted rank, then district/zone), then unknown, unworked, no-call
  worked-on-this-band (dupes), no-call candidates. History: DXLog's UDP feed. Tag `mult` on new
  multipliers only.
- **DXCC per band** (built-in award, default goal): needed when the DXCC entity is **not confirmed on the band**; an entity
  confirmed on no band ranks before one new on this band, rarest first. History: Club Log confirmations
  (token API), Log4OM's ADIF UDP QSOs. Tag `need` (`mult` only when the contest award needs it). Until Club Log has loaded, nothing is tagged. A station
  already worked on the row's band is **listed, not hidden**, `needed:false`, no tag, in its own tier
  after the unworked ones.
- Both: with `[[cluster]]` entries set (1..n; the old single `[cluster]` table loads as one named `cluster`),
  chase-queue reads DX clusters itself (telnet, one reconnecting client each). Their spots hide the heard
  stations they name, and **needed** spotted stations (and buddies) join `ranked` as `source:"cluster"` rows
  (negative `row_id`, `cluster_name`, `spot` block with `rbn` for skimmer spots, ≤ 40 needed), ordered with the
  heard rows by the same rules. The same call+band on several clusters is one row, the freshest spot wins.
  Bands: 160-10 m, 6 m, 2 m, 70 cm (`internal/band`); VHF/UHF modes are unknown unless the comment says.
- Both: `[bmrec] gate` (`off` | `trusted` default | `qualified`) decides which bmrec rows are stations; the rest
  are counted in `/state.bmrec_unqualified`. `qualified` needs bmrec's optional `qualified` row field (spec in
  the API doc, section 5); without it nothing passes and `bmrec sends no qualified flag` is a problem. `trusted`
  hides call-less and only-inferred rows (set `off` for the old list).
- Both: `[[buddy]]` (call, note, boost): `buddy:true` on the row, listed even when not needed (own tier just
  below the needed block), first among needed rows of equal goal priority.
- Both: each listed call carries `info` (QRZ.com callbook: name, QTH, locator, picture) and
  `worked_before` (QSO count, bands, modes, `this_band`, last QSO from the station log and stored contest
  logs; so far only QSOs since deployment, until `station-log`). Enrichment never changes the ranking.
- Both: **every row is self-contained**: `freq_hz`, `mode` (derived: bmrec rows get the sideband rule, LSB
  below 10 MHz except 60 m; cluster rows the comment keyword, else the IARU R1 band plan, else absent;
  `internal/mode`) and `source` (`bmrec` | `cluster`), so a consumer never reads bmrec's `cqfinder`.
- Both: a heard station spotted on chase-queue's own cluster login (same station, same band, 30 min) is replaced
  by its cluster row; a spot only bmrec relays hides it only when it is not needed. A station handled or
  skipped is **not listed** as a heard row; one worked on the band is listed (tier above). Only needed
  cluster stations are listed, so a worked cluster station is not. Not-needed stations stay below the needed ones.

## Layout

| Path | Role |
|---|---|
| `cmd/chase-queue` | main; `-check`, `-clublog-probe`, `-qrz-lookup`, `-import-award-xml` |
| `internal/award` | award definitions (TOML, catalog, built-in DXCC), slot evaluator, contest wrapper, Log4OM XML importer |
| `internal/engine` | pure ranking + Skip/handled/Undo state; one mutex, no I/O |
| `internal/app` | service: jobs worker, publisher, MQTT, local HTTP, contest manager |
| `internal/bus` | wire types (the contract with bmrec) |
| `internal/contest`, `cty`, `dxlog`, `clublog` | ported from bmrec (definition/prefill/value/queue, cty.dat+csv, DXLog XML, Most Wanted) plus the Club Log token client |
| `internal/log4om`, `adif` | Log4OM ADIF_MESSAGE UDP listener (off when `[station_log]` is set) and tolerant ADIF parser |
| `internal/logview` | station-log client: worked/confirmed view from `/v1/worked` + `<slot>/event` |
| `internal/cluster` | DX-cluster telnet client (login, `DX de` parser, reconnect) |
| `internal/qrz`, `callbook` | QRZ.com XML client; background lookup worker with memory + SQLite cache |
| `internal/store` | SQLite (modernc, pure Go): contests, QSOs, log, confirmations, DX skips, callbook cache |

## Rules that matter

- **Handlers never publish.** MQTT callbacks only `Enqueue`; `shared/mqtt.RunJobs` applies work, one
  publisher goroutine writes the retained `/state` (coalesced, 30 s heartbeat).
- **Clean session, `/cmd` at QoS 0.** The candidates are retained: a restarted chase-queue gets bmrec's rows
  *and* the last 30 minutes of spots back at once (no replay protocol).
- **A stale or repeated `seq` never moves the ranking back.** `/state.seq` echoes the candidates' `seq`.
- **An action on a row no longer listed is ignored** (`ErrNotListed`): two windows cannot act twice.
- **DX QSY does not hide a station**; DX Skip lasts until 00:00 UTC; Undo does not cross activities.
- **Secrets are env-only** (`CHASE_QUEUE_MQTT_PASSWORD`, `CHASE_QUEUE_CLUBLOG_TOKEN`,
  `CHASE_QUEUE_QRZ_PASSWORD`), never in the TOML,
  logs, errors, `/state`, `/meta` or HTTP (tested). Callsigns and logs are personal data: the database stays
  under `/var/lib/chase-queue`, never in git.
- **Club Log's matrix status values are undocumented** but were checked against the web chart (2026-11-02,
  Belgium/Germany/Åland rows): **1 = Confirmed, 2 = Worked, 3 = Verified**, so `clublog.confirmed_status`
  defaults to `[1,3]`. `chase-queue -clublog-probe` prints the counts per value if Club Log ever changes them.
  Band keys are metres, except `70`/`23`/`13` = cm. The committed fixture is synthetic.
- **Worked** = with `[station_log]`: a row of station-log's worked view on that call (normalized: `/P` etc.
  dropped) and band, any mode, ever. Without it: a Log4OM QSO or a stored Club Log log QSO. **The Club
  Log log download is not implemented yet** (only the confirmation matrix); `store.ReplaceLogQSOs` is the seam.
- **Log view (`internal/logview`)** = chase-queue's derived copy of station-log (`muehle/hf/station-log`, roadmap
  item 2). Rows are station-log's `GET /v1/worked?own=<own_call>` (one per call+band+mode_class). Loaded at
  start, on every MQTT (re)connect (events are not retained, the session is clean), and every
  `reload_minutes`; kept current from `muehle/hf/station-log/event` (QoS 1, applied in the paho handler: memory
  only, never a publish). `added`/`merged` fold into the row (latest `at`/mode, confirmed OR-ed, dxcc);
  events dedupe by `id`+`updated`; events that arrive during a load are replayed on the new snapshot; events
  of another `own_call` are ignored. `deleted` triggers a full reload (a row aggregates other QSOs). The
  engine is fed (`SetDXWorked`, `SetLogEntries`: call, entity, state, band, mode class, confirmed) by `syncLogView`, coalesced, never on the publish path.
  Station-log down: the last good view stays in use, `problems` gets `station-log not reachable: ...`, one Warn
  per outage (`shared/logging.Streak`); before the first load the local log is the interim history.
  `worked_before.qsos` from the view counts call+band+mode_class entries, not raw QSOs. Contest QSOs stored
  from DXLog are no longer part of `worked_before`/DX-worked while the view is on (the contest tier itself
  still uses the contest log); they reach station-log only by ADIF import.
- **Confirmed (DXCC per band)** = Club Log's matrix (entity level) OR the log view's confirmed rows with a
  `dxcc`. Either source loaded is enough to value DX stations; a band confirmed in either counts.
  **Decision (2026-11-03): the Club Log client stays in chase-queue** — its matrix is entity-level, not a QSO
  list, so it is not a station-log feeder. It becomes one only if the Club Log log download is built.
- **Cutover to station-log** (roadmap item 2): 1) deploy station-log on scmino and import the Log4OM ADIF
  export (`station-log -import-adif`); 2) point Log4OM's UDP ADIF_MESSAGE at station-log's `[log4om] listen`
  (2341) instead of chase-queue's (2340); 3) add `[station_log]` to `/etc/chase-queue/config.toml` and restart —
  `[log4om]` is then ignored with a Warn (remove it); `/meta.capabilities.history` reads `station-log`;
  4) rollback = delete the `[station_log]` section and point Log4OM back at 2340.
- **Field awards take the state from the log first** (the call's own QSO in the station-log view), else the
  callbook (`info.state`); the engine asks the callbook only for a call a field award covers (entity in the
  award's `dxcc`) and the log has no state for, so DXCC-only setups cause no extra QRZ lookups.
- **Award definitions reload without a restart**: at start (a bad file or a goal naming an unknown award stops the start), then within 30 s of a change in `<data_dir>/awards` (poll of file names + mtimes + sizes) or on `SIGHUP` (handler registered at the start of `Run`; `systemctl kill -s HUP chase-queue`). A reload that fails keeps the previous catalog (a parse or validation failure is not re-read until a file changes; an I/O failure is retried at the next poll), logs one Warn per distinct failure and shows in `/state.problems`; a good one is swapped into the engine (`Engine.ReplaceAwards`, file I/O before the lock) and a state is published. A goal whose award vanished stays configured, matches nothing and gets a problem line. The Log4OM award XML schema is not
  documented; `internal/award/importxml.go` looks fields up by name (AwardType, QsoField, AllowedDxcc,
  AwardItems/AwardItem/RC, ConfirmationMethod, AwardConfigs/AwardConfig with Bands, Modes, EmissionType) and is
  tested on a real WAS export (`testdata/award_WAS_log4om.xml`, 50 states, 133 configs). A real DXCC export (`QsoField Dxcc`, 402 entities, no configs)
  is recognised as the built-in award: `-import-award-xml` prints that and exits 4 without output. A config named like its award is the award itself; `WAS-10-CW` keeps its name.
- **Callbook lookups never run on the publish path.** `callbook.Book.Info` answers from memory and queues a
  lookup; one worker asks QRZ (a portable form not found is retried under its home call) and pokes the
  publisher. A refused login backs off an hour and shows in `problems`. The QRZ XML login goes in the request
  URL (QRZ's documented GET), so `qrz` strips `*url.Error` before an error leaves the package (tested).
  `chase-queue -qrz-lookup CALL` checks a login by hand.
- **Country files refresh themselves.** At start and daily chase-queue fetches AD1C's `bigcty/cty.dat` +
  `cty.csv` into `data_dir`; a download that does not parse or does not resolve DL1ABC to 230 never replaces
  the old files. New files apply at once in DX mode; a running contest keeps its model until re-activated.
  Without them every station is "unknown" and nothing is listed as needed (first deploy, 2026-11-02).
- **Ports:** UDP 12060 is taken by station-log's working-station feed on scmino, so DXLog needs a second broadcast target on
  `contest.listen` (default 12061); Log4OM needs its own outbound ADIF_MESSAGE connection to `log4om.listen`
  (2340), Log4OM ≥ 2.42 (only without `[station_log]`; station-log's own listener is 2341).

## Commands

```bash
go test ./... -race
go run ./cmd/chase-queue -config config.example.toml -check
./deploy.sh        # build linux/arm64, ship to scmino, seed config once, (re)start — only when the operator says so
ssh io@192.168.1.178 'journalctl -u chase-queue -f'
```

Config: `/etc/chase-queue/config.toml` (0600, seed-once; see `config.example.toml`). Local end-to-end check:
run a mosquitto on another port, point `[mqtt] broker` at it, publish a `cqfinder/state` and watch `chase/state`.

## Debugging a missing station

`curl -s 'http://127.0.0.1:8095/debug/rows?call=XX'` on scmino says, per candidate row, whether it is listed or
which rule kept it off (gate, hidden, not listed) and shows the gate header (`qualified_flag_seen`, `gate_kept_off`).
No broker credentials needed. Contract: `docs/chase-queue-mqtt-api.md`, "Debugging".
