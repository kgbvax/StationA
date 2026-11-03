# chaseq-ui

The chase-queue list in a window on the shack PC. Arrow keys move the highlight (nothing is tuned);
**Enter or a click** tunes the FLEX to the station and puts its callsign into Log4OM and DXLog; `s` Skip,
`u` Undo; `f` focuses the band / needed-only filters (Left/Right, Space, Esc back to the list).

Build/test/layout: `CLAUDE.md`. Config: `config.example.toml`. Deploy: `./deploy.sh` (bwpc, on request).

## Setup on bwpc

1. Copy `config.example.toml` to `C:\Users\iotte\chaseq-ui\config.toml`; set `[mqtt] password`.
2. **Log4OM:** Settings → Connections → Remote Control Interface: note the inbound UDP port and put it in
   `[loggers] log4om_addr`. Log4OM ignores `SetCallsign` when you have already typed a call by hand.
3. **DXLog.net:** see below.
4. **flexbridge** must run the build with `set_freq` (deploy it on scmino). In DXLog turn **Auto Switch TX slice**
   off on all CAT ports, and avoid SO2V (DXLog puts back its last VFO B frequency).

## DXLog script

DXLog.net has no documented way for another program to set the callsign, so chaseq-ui ships a script,
`dxlog/ChaseqCallsign.cs`: it listens on `127.0.0.1:9889` for `CALL <callsign>`, sets the entry field on
DXLog's UI thread and answers `OK`. Copy it into DXLog's scripts folder and enable it. **It has not been run in
DXLog yet.** First live check: press Enter on a row with DXLog open. If the callsign does not appear, the
script's `Initialize` signature or the `txtCallSign` lookup needs adjusting to DXLog's own example scripts; if
setting `Text` does not reach DXLog's entry line, use the `SendKeys` fallback noted in `SetCall`. With DXLog
(or the script) not running, the DXLog part reports "no answer" after 1 s and Log4OM still gets the call.
