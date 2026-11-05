# CLAUDE.md — chaseq-ui

chaseq-ui is the operator's window onto chase-queue (`muehle/hf/chase`): a native window on the shack
PC bwpc showing the ranked list, with Enter/click = tune the FLEX and put the callsign into Log4OM.
It is **not a slot** (consumer + `/cmd` sender, like testui). It owns no ranking: chase-queue decides
what is needed and in which order; chaseq-ui adds local view filters only. Plan:
`../docs/plans/2026-11-02-1908-feat-chaseq-ui-plan.md`. Domain roles and the target (self-contained rows, configurable loggers): `../docs/chasing-stations-architecture.md`.

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
a config with `[mqtt] broker = "tcp://127.0.0.1:18883"`, then `GOWORK=off go run ./cmd/chaseq-ui -config <file> -ui headless` (or `-ui browser`)
and publish retained `muehle/hf/chase/state`, and `…/chase/status`.

## Layout

| Path | Role |
|---|---|
| `internal/bus` | wire types copied from chase-queue (its `internal/bus` cannot be imported), decoders, MQTT session |
| `internal/model` | pure list model: rows as sent (freq, mode, worked marks), filters (persisted JSON), highlight by `row_id`, staleness (75 s) |
| `internal/action` | Enter/Skip/Undo runner; one action at a time; every part reports its own result |
| `internal/loggers` | Log4OM Remote Control `SetCallsign` (UDP, reply on port+1); `Sender` is the seam for further loggers; `Build` (registry.go) maps config `kind` to a constructor, unknown kind = start-up error |
| `internal/web` | loopback pages, SSE (`/events`), write routes guarded by same-origin + `X-Chaseq` header |
| `internal/desktop` | glaze window (the only package importing glaze) |

## Rules that matter

- **One tuning writer per keypress:** flexbridge `set_freq` (slice A while split, else TX slice), then `set_mode`
  with the row's `mode` (only `cw`/`usb`/`lsb`: flexbridge accepts no others, so data/am/fm/unknown send none;
  skipped in split because `set_mode` targets the TX slice). Never also
  Log4OM `SetTxFrequeny`.
- **Callsigns go in after the radio state reaches the target** (or 2 s): a logger that clears its entry on QSY
  would erase it. Radio and chase-queue parts are fire-and-forget on the bus, so they are confirmed from the
  next `radio/state` / `chase/state` ("not confirmed" otherwise).
- **Contest only:** Enter also sends `handled` (as bmrec does; `how` defaults to `qsy` in chase-queue). DX rows stay listed.
- **Frequency and mode** are the row's own `freq_hz` / `mode` (chase-queue derives both; no `cqfinder` subscription).
  A row without `freq_hz` shows "no freq" and cannot be tuned; against an older chase-queue only cluster rows
  (`spot.freq_hz`) are tunable, and no mode is set at all (the Radio part says "mode unchanged"): deploy
  chase-queue first. The Radio part also says "mode unchanged (data)" etc. for a mode flexbridge cannot set.
- **Worked marks:** `worked_before.this_band` shows "worked <band>" (boxed; chase-queue sets `this_band` from the same verdict that lists the row last, so such rows
  arrive with `needed` false and the needed-only filter hides them); other bands show as "wkd 40m 15m"; never worked shows nothing.
- **Logger senders are config:** `[[loggers.sender]]` entries `{kind, addr, timeout, enabled}` (only `kind = "log4om"` exists);
  with none, the legacy `log4om_addr`/`log4om_timeout` stand for one Log4OM. A new logger = a `Sender` plus one entry in the
  `constructors` map.
- **Row enrichments, all optional:** `info` (QRZ: name, qth, state, country, grid shown as one line; zones decoded, not shown),
  `needs[]` `{award, key, band, mode, status}` (tag "award key", class by status; status is `unworked` or `worked-unconfirmed`),
  `buddy` + `buddy_note` (badge, note as hover), and on cluster rows `cluster_name` (compact label; "cluster" without it) and
  `spot.rbn` (RBN marker). The wire types mirror chase-queue's `internal/bus/bus.go` field by field (`worked_before.last` is a
  timestamp). Absent = nothing rendered; everything goes through html/template, never raw.
- **Mode filter (local, `filters.json`):** mode CLASSES `cw` (cw), `phone` (usb, lsb, am, fm), `data` (data); chips toggle like the
  band chips, several at once. **Default = phone only** (nothing persisted, fresh start, and "clear"); all three chips on shows every
  mode. Switching off the last chip shows all three (never an empty list). A row with an empty or unknown mode is never hidden
  (it cannot be classified). `nil` `modes` in `filters.json` = default; an explicit list is saved. Hidden rows count into
  "N hidden by filter". `POST /filters?mode=<cw|phone|data>` toggles. The highlight copes as with the band filter.
- **Filters (local, `filters.json`):** bands, needed only, **award** (chips for the award codes among the needs of the
  unfiltered list, first seen = highest priority; a row passes with a need for any selected code; an active code that left the
  list stays as a chip so it can be switched off) and **source** (`cluster` / `bmrec`, click again for both). All count into
  "N hidden by filter"; none changes chase-queue's ranking. `POST /filters?award=<code>&source=<cluster|bmrec>` toggle.
- **Band lock (`lock_band` in `filters.json`, chip "follow radio band"):** while on, the band filter is the single band the radio reports
  (`radio/state` `band`, or `rx_band` while split; empty/`gen`/`unknown`/`device_online` false = unknown = keep the last filter, never
  all) and follows every radio band change, whoever made it. Manual band chips are `aria-disabled` and `POST /filters?band=` is a 409.
  Off keeps the band it had. `clear` keeps the lock. The follow re-renders and saves only when the band filter really changed
  (`Model.SetRadioBand`). `POST /filters?lock=1|0`. Old files without it = off.
- **Filter toolbar = four groups, one question each:** Band (with the "follow radio band" lock), Mode, Needed (needed only + award chips),
  Source (cluster / bmrec); `clear` and "N hidden by filter" sit at the right end. Group and chip classes must not reuse a row-cell
  class (`.mode`, `.need`, `.band`, `.src` style the list rows): groups are `g-band`, `g-mode`, `g-need`, `g-source`, `g-end`, mode chips `modeclass`.
- **App icon:** `assets/icon.svg` (ranked list, amber top entry, signal arcs) is the source; `assets/icon.ico`/`icon.png` and
  `internal/desktop/icon.png` (embedded) and `internal/web/static/icon.svg` (favicon) are renderings of it. On Windows the window gets
  it at runtime (`icon_windows.go`, WM_SETICON from the embedded PNG). The exe file itself has no icon resource (needs `go-winres`
  to make a `.syso`; not run here).
- **Chip kinds live in one list:** `KINDS` in `static/live.js` (focus restore after every re-render, so a stray Enter never reaches
  `/act`) and `toggle()` in `keys.js` must cover every `data-*` on a chip button in `list.html`; `TestEveryChipKindRestoresFocusAndToggles` enforces it.
- **Enter/`s`/`u` act from anywhere except a filter chip** (a focused chip swallows them); arrows only ever move the highlight. A held key is ignored (`e.repeat`). The page fragment is only swapped when it changed, so focus and scroll survive.
- **Secrets:** MQTT password in `config.toml` `[mqtt] password` (no env vars, like qslotter);
  never logged (`Config.String` redacts).
- Log4OM answers on its inbound port + 1 as an IPv4 broadcast: the reply socket binds `udp4` on all interfaces;
  Windows Defender Firewall may need to allow it, else the Log4OM part says "sent, no reply".
- bmrec reads chase-queue's `undo` as an object while chase-queue publishes a string (`bus.go`): chaseq-ui follows
  chase-queue; the bmrec side is its own fix.
