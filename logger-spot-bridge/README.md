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

Run `./deploy.sh` — deploys to **shari** (192.168.1.139) as a hardened systemd
service, the standard stationa pattern. The bridge only LISTENS on UDP, and
logger broadcasts travel the whole LAN, so it does not need to run on the
logging PC. Config and secrets are host state and survive every update:

- `/etc/logger-spot-bridge/config.toml` (0600) and
  `/etc/logger-spot-bridge/logger-spot-bridge.env` (0600, holds
  `LOGGER_SPOT_BRIDGE_MQTT_PASSWORD` and `LOGGER_SPOT_BRIDGE_QRZ_PASSWORD`)
  are **seed-once**: existing files are never overwritten.
- Every deploy ends with `logger-spot-bridge -check` on the Pi — the effective
  config is printed redacted and broker DNS + password presence are verified
  *before* the service restarts. `-check` also runs standalone.
- Log4OM's outbound CALLSIGN service must point at shari:2249; DXLog's
  broadcast (subnet broadcast, port 12060) needs no target.
- History: the bridge ran on the Windows shack PC as a schtasks logon task
  until 2026-09-16 (a recorded mis-fit: no auto-restart, secrets in a wrapper
  cmd, one redeploy left it broken — the outage that prompted the move). The
  task there is disabled; the files remain as fallback.

Wire contract: `docs/logger-spot-bridge-mqtt-api.md`. Engineering notes:
`CLAUDE.md`.
