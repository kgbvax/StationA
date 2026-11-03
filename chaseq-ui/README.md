# chaseq-ui

The chase-queue list in a window on the shack PC. Arrow keys move the highlight (nothing is tuned);
**Enter or a click** tunes the FLEX to the station and puts its callsign into Log4OM; `s` Skip,
`u` Undo; `f` focuses the band / needed-only filters (Left/Right, Space, Esc back to the list).
Each row shows frequency, mode and a worked mark: a boxed "worked 20m" = already worked on this band (listed, not
needed), "wkd 40m 15m" = worked on other bands only, nothing = never worked. Frequency and mode come from chase-queue's row.

Build/test/layout: `CLAUDE.md`. Config: `config.example.toml`. Deploy: `./deploy.sh` (bwpc, on request).

## Setup on bwpc

1. Copy `config.example.toml` to `C:\Users\iotte\chaseq-ui\config.toml`; set `[mqtt] password`.
2. **Log4OM:** Settings → Connections → Remote Control Interface: note the inbound UDP port and put it in
   `[loggers] log4om_addr`. Log4OM ignores `SetCallsign` when you have already typed a call by hand.
3. **flexbridge** must run the build with `set_freq` (deploy it on scmino). If DXLog runs alongside, turn its
   **Auto Switch TX slice** off on all CAT ports and avoid SO2V (DXLog puts back its last VFO B frequency).
