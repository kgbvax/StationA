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

**Position without a locator**: Log4OM's CALLSIGN broadcast is the bare call,
so the bridge optionally resolves it itself via the QRZ.com XML API —
`[qrz]` in the config, needs a paid QRZ XML subscription, password via
`LOGGER_SPOT_BRIDGE_QRZ_PASSWORD`. Logger-provided positions (DXLog
azimuth+distance, N1MM grid) always win; the lookup only fills the gap, from
a disk cache so QRZ is hit at most once per call per month.

```
DXLog ──UDP lookupinfo──┐
Log4OM ──UDP callsign───┤  logger-spot-bridge (shack PC)
                        │   └─ QRZ lookup for bare calls (optional)
                        └──► muehle/hf/spots/state (retained, MQTT)
                                   │
                                   ▼
                        hf_console compass + Mercator map
                        (pin + callsign + bearing/distance chip)
```

Run `./deploy.sh` (Windows shack PC) — see the script header. Config and
secrets are host state and survive every update:

- `config.toml` and `start-bridge.cmd` (the Windows env-file holding the MQTT
  password) are **seed-once**: an existing file is never overwritten.
- The schtasks logon task runs `start-bridge.cmd`, never the bare exe — the
  wrapper carries the password; `/create /f` on every deploy repairs stale
  registrations that pointed at the exe (the 2026-09-16 outage: bridge up,
  but no password and a stale broker after a redeploy).
- Every deploy ends with `logger-spot-bridge.exe -check` on the shack PC: the
  effective config is printed redacted and the broker DNS + password presence
  are verified *before* the task restarts. `-check` also runs standalone.

Wire contract: `docs/logger-spot-bridge-mqtt-api.md`. Engineering notes:
`CLAUDE.md`.
