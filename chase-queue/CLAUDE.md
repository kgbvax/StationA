# CLAUDE.md — chase-queue

chase-queue is the **chase queue** logic slot `muehle/hf/chase`. It reads the CQ stations bmrec hears
(`muehle/hf/cqfinder/state`), knows the operator's history, and publishes one ordered list of what is
needed. bmrec shows that list; other consumers (hf_console, horstreporter, Home Assistant) can read the
same retained state. It has no device and runs on scmino. Contract: `docs/chase-queue-mqtt-api.md`.
Requirements: `../docs/plans/2026-11-01-2252-feat-chase-queue-v1-plan.md`.

## What it decides

- **Contest** (instance running): ranking from bmrec's former contest engine — new multipliers first
  (more new slots, then Club Log Most Wanted rank, then district/zone), then unknown, unworked, no-call
  candidates. History: DXLog's UDP feed. Tag `mult` on new multipliers only.
- **DX chasing** (no instance): needed when the DXCC entity is **not confirmed on the band**; an entity
  confirmed on no band ranks before one new on this band, rarest first. History: Club Log confirmations
  (token API), Log4OM's ADIF UDP QSOs. Tag `need`. Until Club Log has loaded, nothing is tagged.
- Both: each listed call carries `info` (QRZ.com callbook: name, QTH, locator, picture) and
  `worked_before` (QSO count, bands, modes, last QSO from the station log and stored contest logs).
  Enrichment never changes the ranking.
- Both: a station spotted on the cluster (same station, same band, 30 min), worked, handled or skipped is
  **not listed**. Not-needed stations stay below the needed ones.

## Layout

| Path | Role |
|---|---|
| `cmd/chase-queue` | main; `-check`, `-clublog-probe`, `-qrz-lookup` |
| `internal/engine` | pure ranking + Skip/handled/Undo state; one mutex, no I/O |
| `internal/app` | service: jobs worker, publisher, MQTT, local HTTP, contest manager |
| `internal/bus` | wire types (the contract with bmrec) |
| `internal/contest`, `cty`, `dxlog`, `clublog` | ported from bmrec (definition/prefill/value/queue, cty.dat+csv, DXLog XML, Most Wanted) plus the Club Log token client |
| `internal/log4om`, `adif` | Log4OM ADIF_MESSAGE UDP listener and tolerant ADIF parser |
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
- **Club Log's matrix status values are undocumented.** `clublog.confirmed_status` (default `[1,3]`) is a
  guess: run `chase-queue -clublog-probe` with a real token and compare with the web chart before trusting
  "needed". The committed fixture is synthetic.
- **Worked** = a Log4OM QSO or a stored Club Log log QSO on that call and band, any mode, ever. **The Club
  Log log download is not implemented yet** (only the confirmation matrix); `store.ReplaceLogQSOs` is the seam.
- **Callbook lookups never run on the publish path.** `callbook.Book.Info` answers from memory and queues a
  lookup; one worker asks QRZ (a portable form not found is retried under its home call) and pokes the
  publisher. A refused login backs off an hour and shows in `problems`. The QRZ XML login goes in the request
  URL (QRZ's documented GET), so `qrz` strips `*url.Error` before an error leaves the package (tested).
  `chase-queue -qrz-lookup CALL` checks a login by hand.
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
