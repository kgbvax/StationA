# CLAUDE.md — chaseq-ui

chaseq-ui is the operator's window onto chase-queue (`muehle/hf/chase`): a native window on the shack
PC bwpc showing the ranked list, with Enter/click = tune the FLEX and put the callsign into Log4OM and
DXLog. It is **not a slot** (consumer + `/cmd` sender, like testui). It owns no ranking: chase-queue decides
what is needed and in which order; chaseq-ui adds local view filters only. Plan:
`../docs/plans/2026-11-02-1908-feat-chaseq-ui-plan.md`.

## Build and test

This module is **outside `go.work`** (glaze needs Go 1.27.1, the workspace is on 1.26.5), so every `go`
command here needs `GOWORK=off`:

```bash
GOWORK=off go test ./... -race
GOWORK=off go vet ./...
GOWORK=off CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-s -w -H=windowsgui" ./cmd/chaseq-ui
./deploy.sh        # bwpc: only when the operator says so
```

Local run against a scratch broker: `mosquitto -c` on another port, then
`CHASEQ_UI_MQTT_BROKER=tcp://127.0.0.1:18883 GOWORK=off go run ./cmd/chaseq-ui -ui headless` (or `-ui browser`)
and publish retained `muehle/hf/chase/state`, `…/chase/status` and `…/cqfinder/state`.

## Layout

| Path | Role |
|---|---|
| `internal/bus` | wire types copied from chase-queue (its `internal/bus` cannot be imported), decoders, MQTT session |
| `internal/model` | pure list model: cqfinder frequency join, filters (persisted JSON), highlight by `row_id`, staleness (75 s) |
| `internal/action` | Enter/Skip/Undo runner; one action at a time; every part reports its own result |
| `internal/loggers` | Log4OM Remote Control `SetCallsign` (UDP, reply on port+1) and the DXLog script sender |
| `internal/web` | loopback pages, SSE (`/events`), write routes guarded by same-origin + `X-Chaseq` header |
| `internal/desktop` | glaze window (the only package importing glaze) |
| `dxlog/ChaseqCallsign.cs` | DXLog.net script: loopback UDP `CALL <call>` → entry field (**untested in DXLog**, see README) |

## Rules that matter

- **One tuning writer per keypress:** flexbridge `set_freq` (slice A while split, else TX slice), then `set_mode`
  (LSB below 10 MHz except 60 m; skipped in split because `set_mode` targets the TX slice). Never also
  Log4OM `SetTxFrequeny`.
- **Callsigns go in after the radio state reaches the target** (or 2 s): a logger that clears its entry on QSY
  would erase it. Radio and chase-queue parts are fire-and-forget on the bus, so they are confirmed from the
  next `radio/state` / `chase/state` ("not confirmed" otherwise).
- **Contest only:** Enter also sends `handled` (as bmrec does; `how` defaults to `qsy` in chase-queue). DX rows stay listed.
- **Frequency:** cluster rows carry `spot.freq_hz`; heard rows get it from bmrec's `cqfinder/state` by `row_id`.
  A heard row bmrec no longer has cannot be tuned.
- **Enter/`s`/`u` act from anywhere except a filter chip** (a focused chip swallows them); arrows only ever move the highlight. A held key is ignored (`e.repeat`). The page fragment is only swapped when it changed, so focus and scroll survive.
- **Secrets:** `CHASEQ_UI_MQTT_PASSWORD` (user env var on bwpc); TOML `[mqtt] password` is only a fallback;
  never logged (`Config.String` redacts).
- Log4OM answers on its inbound port + 1 as an IPv4 broadcast: the reply socket binds `udp4` on all interfaces;
  Windows Defender Firewall may need to allow it, else the Log4OM part says "sent, no reply".
- bmrec reads chase-queue's `undo` as an object while chase-queue publishes a string (`bus.go`): chaseq-ui follows
  chase-queue; the bmrec side is its own fix.
