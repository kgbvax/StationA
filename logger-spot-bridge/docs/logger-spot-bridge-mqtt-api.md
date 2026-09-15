# logger-spot-bridge — on-the-wire contract

The slot is `muehle/hf/spots` (site `muehle`, station `hf`, slot `spots`),
following the logging-integration draft `../logging-integration-model.md` §3
(role `bandmap`). One operator seat, collapsed addressing (§3 "collapsed").

## Planes

| Topic | Retained | Content |
|---|---|---|
| `muehle/hf/spots/meta` | yes | birth certificate (below) |
| `muehle/hf/spots/state` | yes | single JSON snapshot: `{ts, device_online, selected?}` |
| `muehle/hf/spots/status` | yes | `online` / `offline` — the bridge LWT |
| `muehle/hf/spots/cmd` | — | **not subscribed** (read-only slot this pass) |

## `/state`

```json
{
  "ts": "2026-09-15T14:03:12Z",
  "device_online": true,
  "selected": {
    "call": "VK9XY",
    "freq_hz": 14023000,
    "band": "20m",
    "mode": "cw",
    "lat": -11.53,
    "lng": 151.19,
    "azimuth": 62.4,
    "distance_km": 15420.3,
    "country_prefix": "VK9",
    "source": "dxlog",
    "ts": "2026-09-15T14:03:12Z"
  }
}
```

Field notes:

- `selected` is **omitted when nothing is keyed** — the operator cleared the
  entry window (DXLog `lookupinfo` with an empty call) is the clear signal.
  It re-appears (fresh `ts`) on the next datagram, even for the same call:
  re-keying republishes, which is how the console re-brightens the marker.
- `freq_hz` is integer Hz. N1MM-family datagrams carry 10 Hz units; the
  conversion happens here, once.
- `band` is the canonical label from `docs/conventions/band-mode-reference.md`
  (`gen` for HF general-coverage gaps; omitted outside). A canonical-looking
  logger label ("20m") is trusted; DXLog's MHz form ("14.0") is re-derived
  from the frequency.
- `mode` is canonical (`cw|usb|lsb|am|fm|data`); `data` collapses the digital
  family. Omitted when the logger sent nothing recognizable.
- Position: `lat`/`lng` come from a logger-provided locator (N1MM `gridsquare`,
  Log4OM locator) or — DXLog path — the great-circle direct problem from the
  bridge's `station_locator` using the logger's `azimuth`+`distance` (DXLog
  computes those from its CTY database relative to the logging PC). Both
  omitted when nothing derivable.
- `azimuth` (degrees true) and `distance_km`: logger-provided, or the inverse
  problem from `station_locator` when only coordinates are known. Either may
  be omitted alone.
- `locator` (the raw logger locator) is carried when that was the position
  source.
- `source` is the listener `name` config ("dxlog", "log4om", …).
- `device_online` is the **logger feed** liveness (two-layer liveness model):
  true while any UDP datagram arrived within `stale_after` (default 10 min).
  `device_online: false` says "no logger has keyed anything recently" — NOT
  "the bridge is down" (that is `/status`). It is `false` from bridge start
  until the first datagram. Loggers are quiet by nature; this flag is a hint,
  not an alarm.

## Publisher discipline

- `/state` is republished on every change; the on-connect birth sequence
  republishes the last snapshot verbatim (broker-restart recovery, REQ-RT-5).
  There is deliberately **no heartbeat**: the snapshot is retained, and the
  console ages the marker from `selected.ts` (dim > 5 min, hide > 15 min).
- No `/cmd`: the slot is observe-only this pass (logging-integration draft §2).

## Decoders

- `n1mm` — DXLog/N1MM `lookupinfo` XML (DXLog wiki "Additional Information";
  N1MM+ external-UDP appendix). `contactinfo`/`RadioInfo`/`spot` roots are
  recognized and ignored (radio context lives on `muehle/hf/radio`; the spot
  stream has no consumer yet — `…/spots/event` is a future add).
- `log4om` — outbound CALLSIGN service. **The datagram is the bare callsign
  as plain ASCII text** (pinned by live capture 2026-09-15 — the guides name
  the service but publish no format). A Log4OM selection therefore carries
  only `call` + `source`; bearing/distance are unavailable from this source.
  An XML probe remains as a robustness fallback; unmatched datagrams log
  their raw content at debug.
