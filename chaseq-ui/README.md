# chaseq-ui

The chase-queue list in a window on the shack PC. Arrow keys move the highlight (nothing is tuned);
**Enter or a click** tunes the FLEX to the station and puts its callsign into Log4OM; `s` Skip,
`u` Undo; `f` focuses the band / mode / award / source / needed-only filters (Left/Right, Space, Esc back to the list).
Each row shows frequency, mode and a worked mark: a boxed "worked 20m" = already worked on this band (listed, not
needed), "wkd 40m 15m" = worked on other bands only, nothing = never worked. Frequency and mode come from chase-queue's row.
A second line under the row shows the QRZ entry chase-queue attached (name, QTH, country, grid; nothing when it has none). Once chase-queue
publishes award data, small tags ("DXCC", "WAS TX"; coloured by unworked / worked-unconfirmed / confirmed) and a "buddy" badge appear too. Cluster rows show the cluster's name and an RBN marker for skimmer spots.
Award chips (one per award code among the listed needs) and cluster / bmrec chips narrow the list locally, saved with the band filter.
"follow radio band" locks the band filter to the band the radio reports and follows its band changes (also from SmartSDR); band chips are inactive meanwhile, an unknown band keeps the last filter, off keeps the current band.
Mode chips (cw / phone / data) filter by mode class: a click selects that class alone, shift-click adds or removes one, "all" shows everything; the default is phone only (usb, lsb, am, fm) and "clear" returns to it. Rows with an empty or unknown mode are never hidden.

Build/test/layout: `CLAUDE.md`. Config: `config.example.toml`. Deploy: `./deploy.sh` (bwpc, on request).

## Setup on bwpc

1. Copy `config.example.toml` to `C:\Users\iotte\chaseq-ui\config.toml`; set `[mqtt] password`.
2. **Log4OM:** Settings → Connections → Remote Control Interface: note the inbound UDP port and put it in
   `[[loggers.sender]] addr` (`kind = "log4om"`; more loggers are more entries, `enabled = false` switches one off;
   the older `log4om_addr`/`log4om_timeout` keys still work when there is no `[[loggers.sender]]`). Log4OM ignores `SetCallsign` when you have already typed a call by hand.
3. **flexbridge** must run the build with `set_freq` (deploy it on scmino). If DXLog runs alongside, turn its
   **Auto Switch TX slice** off on all CAT ports and avoid SO2V (DXLog puts back its last VFO B frequency).
