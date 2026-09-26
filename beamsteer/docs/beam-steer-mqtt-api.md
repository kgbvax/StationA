# beamsteer — MQTT and PstRotator API

Logic slot `muehle/hf/beam-steer` (role `steering`, runs on shari). Input is the
contest logger's rotator output, received by emulating PstRotator's UDP
interface. Outputs are commands to `hf/rotator` (wrc-rotator-bridge) and
`hf/ant-ctrl` (ultrabridge).

## 1. Behaviour

For each logger rotate request with bearing `b`:

| Toggle | Ultrabeam direction | Station at `b` | Action |
|---|---|---|---|
| off | any | any | rotator `set_az b` (plain passthrough) |
| on | forward / reverse | within ±30° of boom heading | `forward` (flip only if reversed), no rotation |
| on | forward / reverse | within ±30° of boom heading + 180 | `reverse` (180°), no rotation |
| on | forward / reverse | outside both | rotate to `b` forward **or** to `b+180` reversed — whichever needs less rotator travel (tie → forward) |
| on | bidirectional | within ±45° of front or back | nothing |
| on | bidirectional | outside both | rotate to `b` or `b+180`, whichever needs less travel; never changes direction |
| on | unknown (ant-ctrl offline) | any | rotator `set_az b` |

- **6m**: the Ultrabeam has no reverse on 6m. Only forward candidates are used.
- **Travel** is linear `|target − az|`. The G-450 cannot turn through its end
  stop. `steer.max_az = 450` allows targets in the 360..450 overlap. The default
  is 360 until the overlap readback is verified.
- If the rotator is moving, the decision uses `target_az`, not the mid-travel
  position.
- **Direction changes are held** while ant-ctrl reports `moving` or the radio
  transmits (`hf/radio/state.tx == "tx"`, trusted only when the radio is live).
  The held change goes out when both clear. A newer logger request replaces it.
- Rotator not live (`/status` offline or `device_online` false) or no position
  yet → the request is ignored. The reason goes to `/state.last.reason`, and a
  Warn goes to the journal.

## 2. PstRotator UDP (default port 12050)

Configure the logger's PstRotator output as `shari:12050`. The
wrc-rotator-bridge listener on :12040 still drives the rotator directly and
bypasses smart rotation.

| Datagram | Effect |
|---|---|
| `<PST><AZIMUTH>b</AZIMUTH></PST>` | rotate request (above). `ELEVATION` is ignored |
| `<PST><STOP>1</STOP></PST>` | rotator `stop`, whether the toggle is on or off; beats other tags |
| `<PST><PARK>1</PARK></PST>` | ignored (logged) |
| `<PST>AZ?</PST>` | reply to sender IP, port+1: the **effective beam heading** (boom az, +180 when reversed) |

The reply shape is set by `pstrotator.reply`:
- `manual` gives `AZ:xxx.x\r`.
- `xml` gives `<PST><AZIMUTH>nnn</AZIMUTH></PST>`.

With no valid rotator position there is no reply. The grammar lives in
`shared/pstrotator`.

## 3. Planes

| Suffix | Retained | Purpose |
|---|---|---|
| `/meta` | yes | birth certificate |
| `/state` | yes | toggle + last decision |
| `/status` | yes | LWT `online`/`offline` |
| `/cmd` | yes | `{"action":"enable"}` / `{"action":"disable"}` |

`/cmd` is published retained by the console. The enable/disable steady state
therefore survives restarts. The default with no retained cmd is **disabled**.

### `/state`

```json
{
  "ts": "2026-09-26T12:00:00Z",
  "enabled": true,
  "pending": "reverse",
  "last": {
    "bearing": 265,
    "rotate_to": 85,
    "direction": "reverse",
    "reason": "rotate to 85 180°",
    "ts": "2026-09-26T12:00:00Z"
  }
}
```

- `pending` is present only while a direction change is held.
- `last.rotate_to` is present only when the rotator was commanded.
- `last.direction` is present only when a direction change was decided.

### `/meta`

```json
{
  "schema": "1.0", "role": "steering", "link": "none",
  "location": "bauwagen", "host": "shari",
  "capabilities": {
    "controls": ["rotator", "ant-ctrl"], "input": "pstrotator-udp",
    "pstrotator_port": 12050, "lobe_deg": 30, "bidir_lobe_deg": 45, "max_az": 360
  },
  "expose": {
    "device": { "name": "Beam steering" },
    "fields": [ { "key": "enabled", "name": "Smart rotation", "type": "boolean" } ]
  }
}
```

## 4. Emitted commands

| Target | Payload | Retained |
|---|---|---|
| `hf/rotator/cmd` | `{"action":"set_az","az":85}` / `{"action":"stop"}` | no |
| `hf/ant-ctrl/cmd` | `{"action":"direction","value":"reverse","ts":"…"}` | yes (ultrabridge clears it; 30 s `ts` gate) |
