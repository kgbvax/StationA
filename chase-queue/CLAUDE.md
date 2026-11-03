# CLAUDE.md — chase-queue

chase-queue is the **chase queue** logic slot `muehle/hf/chase`. It reads the CQ stations bmrec hears
(`muehle/hf/cqfinder/state`) and a DX cluster, knows the operator's history, and publishes one ordered
list of what is needed. bmrec's HF CQ page (where the operator qualifies candidates) and chaseq-ui show that
list; other consumers (hf_console, Home Assistant) can read the same retained state. It has no device and
runs on scmino. Contract: `docs/chase-queue-mqtt-api.md`. **Target architecture of the domain** (roles,
award model, the separate `station-log`, gaps to the code): `../docs/chasing-stations-architecture.md`.
v1 requirements: `../docs/plans/2026-11-01-2252-feat-chase-queue-v1-plan.md`.

## What it decides

- **Contest** (instance running): ranking from bmrec's former contest engine — new multipliers first
  (more new slots, then Club Log Most Wanted rank, then district/zone), then unknown, unworked, no-call
  worked-on-this-band (dupes), no-call candidates. History: DXLog's UDP feed. Tag `mult` on new
  multipliers only.
- **DX chasing** (no instance): needed when the DXCC entity is **not confirmed on the band**; an entity
  confirmed on no band ranks before one new on this band, rarest first. History: Club Log confirmations
  (token API), Log4OM's ADIF UDP QSOs. Tag `need`. Until Club Log has loaded, nothing is tagged. A station
  already worked on the row's band is **listed, not hidden**, `needed:false`, no tag, in its own tier
  after the unworked ones.
- Both: with `[cluster]` set, chase-queue reads a DX cluster itself (telnet). Its spots hide the heard
  stations they name, and **needed** spotted stations join `ranked` as `source:"cluster"` rows (negative
  `row_id`, `spot` block, ≤ 40), ordered with the heard rows by the same rules.
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
| `cmd/chase-queue` | main; `-check`, `-clublog-probe`, `-qrz-lookup` |
| `internal/engine` | pure ranking + Skip/handled/Undo state; one mutex, no I/O |
| `internal/app` | service: jobs worker, publisher, MQTT, local HTTP, contest manager |
| `internal/bus` | wire types (the contract with bmrec) |
| `internal/contest`, `cty`, `dxlog`, `clublog` | ported from bmrec (definition/prefill/value/queue, cty.dat+csv, DXLog XML, Most Wanted) plus the Club Log token client |
| `internal/log4om`, `adif` | Log4OM ADIF_MESSAGE UDP listener and tolerant ADIF parser |
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
- **Worked** = a Log4OM QSO or a stored Club Log log QSO on that call and band, any mode, ever. **The Club
  Log log download is not implemented yet** (only the confirmation matrix); `store.ReplaceLogQSOs` is the seam.
- **Callbook lookups never run on the publish path.** `callbook.Book.Info` answers from memory and queues a
  lookup; one worker asks QRZ (a portable form not found is retried under its home call) and pokes the
  publisher. A refused login backs off an hour and shows in `problems`. The QRZ XML login goes in the request
  URL (QRZ's documented GET), so `qrz` strips `*url.Error` before an error leaves the package (tested).
  `chase-queue -qrz-lookup CALL` checks a login by hand.
- **Country files refresh themselves.** At start and daily chase-queue fetches AD1C's `bigcty/cty.dat` +
  `cty.csv` into `data_dir`; a download that does not parse or does not resolve DL1ABC to 230 never replaces
  the old files. New files apply at once in DX mode; a running contest keeps its model until re-activated.
  Without them every station is "unknown" and nothing is listed as needed (first deploy, 2026-11-02).
- **Ports:** UDP 12060 is taken by logger-spot-bridge on scmino, so DXLog needs a second broadcast target on
  `contest.listen` (default 12061); Log4OM needs its own outbound ADIF_MESSAGE connection to `log4om.listen`
  (2340), Log4OM ≥ 2.42.

## Commands

```bash
go test ./... -race
go run ./cmd/chase-queue -config config.example.toml -check
./deploy.sh        # build linux/arm64, ship to scmino, seed config once, (re)start — only when the operator says so
ssh io@192.168.1.178 'journalctl -u chase-queue -f'
```

Config: `/etc/chase-queue/config.toml` (0600, seed-once; see `config.example.toml`). Local end-to-end check:
run a mosquitto on another port, point `[mqtt] broker` at it, publish a `cqfinder/state` and watch `chase/state`.
