# logger-spot-bridge

Fronts the shack logging software (**DXLog**, **Log4OM**) as the canonical
`muehle/hf/spots` slot: the bridge listens on UDP next to the loggers for the
operator-entered-callsign broadcasts and publishes the *selected station* —
callsign, frequency/band/mode, map position, beam bearing and distance — as
the retained `/state` snapshot. The hf_console maps render it, so the operator
can judge whether (and where) to turn the beam before keying up.

Not a spot feed: a call typed into the logger by hand behaves exactly like one
picked from the spot list — the signal is the **entry-window selection**
(N1MM-family `lookupinfo`, Log4OM outbound CALLSIGN), not the cluster stream.

```
DXLog ──UDP lookupinfo──┐
Log4OM ──UDP callsign───┤  logger-spot-bridge (shack PC)
                        └──► muehle/hf/spots/state (retained, MQTT)
                                   │
                                   ▼
                        hf_console compass + Mercator map
                        (pin + callsign + bearing/distance chip)
```

Run `./deploy.sh` (Windows shack PC, seed-once config) — see the script header.
Wire contract: `docs/logger-spot-bridge-mqtt-api.md`. Engineering notes:
`CLAUDE.md`.
