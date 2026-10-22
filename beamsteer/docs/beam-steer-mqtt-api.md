# beamsteer — MQTT and PstRotator API

Logic slot `muehle/hf/beam-steer` (role `steering`, runs on scmino). Input is the
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
  The band comes from ant-ctrl's `/state.band` (fallback: the radio's).
- **Travel** is linear `|target − az|`. The G-450 cannot turn through its end
  stop. `steer.max_az = 450` allows targets in the 360..450 overlap. The default
  is 360 until the overlap readback is verified.
- If the rotator is moving, the decision uses `target_az`, not the mid-travel
  position.
- **Direction changes are held** while ant-ctrl reports `moving`, the radio
  transmits (`hf/radio/state.tx == "tx"`, trusted only when the radio is live),
  or an earlier flip is still in flight. The held change goes out when all
  clear. A newer request replaces it. A held 180° is dropped if the band is
  6m by the time it would go out.
- **Commands in flight.** ultrabridge republishes `/state` only on a 2 s poll.
  Until a sibling confirms a command (ant-ctrl at rest in the new direction;
  rotator arrived, or retargeted by someone else), decisions use what was
  commanded, not the stale `/state`. A flip not confirmed within 15 s, or a
  `set_az` the rotator has not acknowledged within 10 s (120 s once it has),
  falls back to the reported state.
- **6m** is judged from ant-ctrl's own `band` when it is live, else the
  radio's.
- Rotator not live (`/status` offline or `device_online` false) or no position
  yet → the request is ignored. The reason goes to `/state.last.reason`, and a
  Warn goes to the journal.

### Antenna-mode lock (`hf/ant-mode`)

beamsteer reads the retained `hf/ant-mode/state` (slot `steer.ant_mode_slot`,
default `ant-mode`; contract `../../antmode/docs/ant-mode-mqtt-api.md`) through
`shared/antmode`. While `ant_mode` is `storm` or `maintenance` the rotator and
the Ultrabeam are parked by their own bridges, and beamsteer stands down:

- Every rotate request (logger `Goto`, console `aim`) is ignored, toggle on
  **or off** — no plain passthrough either. `/state.last.reason` reads
  `antenna mode storm: ignored`; the journal gets one Warn per refusal reason
  (then Info with a `repeat` count). A new reason — hold → storm, storm →
  maintenance — warns again.
- A held direction change is dropped when the lock lands.
- Nothing is sent to `hf/ant-ctrl` (no `direction`) or `hf/rotator` (no
  `set_az`).
- The logger's `STOP` is **not** forwarded: a contest logger must not cancel
  the park. The console and the M5 dial send STOP straight to the rotator
  bridge, which never refuses a stop.
- `AZ?` readback keeps working (it reports the parked heading).
- `/state.lock` carries the mode name.

On unlock beamsteer resumes with the next request. Nothing dropped while
locked is replayed.

Reading rules: no retained state (or an empty payload) = normal, so beamsteer
works before antmode exists. A valid locked state is enforced even while
`hf/ant-mode/status` is offline (beamsteer does not read it). A malformed
payload keeps the last known state and logs an Error. `ts` is not checked.

**Connect hold:** after every broker connect — the first and each reconnect —
requests and the logger STOP are ignored (`antenna mode not yet known:
ignored`) until the retained ant-mode state of this connection lands, or for
at most 3 s when there is none. A request cannot slip in ahead of a storm lock,
including one set while beamsteer was cut off from the broker. A lock known
from before the outage stays enforced during the hold. When the hold lapses
with no state, the last known lock stands (normal if none was ever seen).
`steer.ant_mode_slot = ""` disables the lock and the hold.

The paho handler only parses and records the state under a mutex; the jobs
worker applies it. Every path that could move the mast applies the recorded
state first, so a dropped jobs-queue entry cannot hide a lock.

## 2. PstRotator UDP (default port 12050)

Configure the logger's PstRotator output as `scmino:12050`. The
wrc-rotator-bridge listener on :12040 still drives the rotator directly and
bypasses smart rotation.

| Datagram | Effect |
|---|---|
| `<PST><AZIMUTH>b</AZIMUTH></PST>` | rotate request (above). `ELEVATION` is ignored |
| `<PST><STOP>1</STOP></PST>` | rotator `stop`, whether the toggle is on or off; beats other tags. **Not forwarded while the antenna is locked** (§1) |
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
| `/state` | yes | toggle + last decision + antenna-mode lock |
| `/status` | yes | LWT `online`/`offline` |
| `/cmd` | enable/disable yes, aim no | `{"action":"enable"}` / `{"action":"disable"}` / `{"action":"aim","value":265}` |

- **enable / disable** are published retained by the console, so the steady
  state survives restarts. The default with no retained cmd is **disabled**.
  A toggle clears `last`.
- **aim** is the console's tap-to-aim while AUTO is on. It is decided exactly
  like a logger request. It must be published **unretained**, so the retained
  enable/disable on the same topic stays intact. `value` is a bearing
  (number or numeric string, −360..720).

beamsteer connects with a **clean session**. A persistent session let the
broker queue QoS-1 `/state` while beamsteer was down. Those messages then
arrived before the subscriptions existed and were never acknowledged, and
after 20 of them mosquitto delivered nothing more: beamsteer was blind, and
stayed blind across restarts (live 2026-10-01). Everything it needs is
retained, so a clean session loses nothing.

### `/state`

```json
{
  "ts": "2026-09-26T12:00:00Z",
  "enabled": true,
  "inputs": { "rotator": true, "ant_ctrl": true, "radio": true },
  "lock": "storm",
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

- `inputs` is the sibling liveness: `rotator` (live and a position known),
  `ant_ctrl`, `radio`. With `inputs.rotator` false, every request is ignored.
  The console shows that as an alert on the AUTO line.
- `lock` is the antenna-mode lock (`storm` | `maintenance`), present only while
  locked. Republished on every change.
- `pending` is present only while a direction change is held (never while
  locked).
- `last.rotate_to` is present only when the rotator was commanded.
- `last.direction` is present only when a direction change was decided.

### `/meta`

```json
{
  "schema": "1.0", "role": "steering", "link": "none",
  "location": "bauwagen", "host": "scmino",
  "capabilities": {
    "controls": ["rotator", "ant-ctrl"], "input": "pstrotator-udp",
    "pstrotator_port": 12050, "lobe_deg": 30, "bidir_lobe_deg": 45, "max_az": 360
  },
  "expose": {
    "device": { "name": "Beam steering" },
    "fields": [
      { "key": "enabled", "name": "Smart rotation", "type": "boolean" },
      { "key": "lock", "name": "Antenna Mode Lock", "type": "string" }
    ]
  }
}
```

## 4. Emitted commands

| Target | Payload | Retained |
|---|---|---|
| `hf/rotator/cmd` | `{"action":"set_az","az":85}` / `{"action":"stop"}` | no |
| `hf/ant-ctrl/cmd` | `{"action":"direction","value":"reverse","ts":"…"}` | yes (ultrabridge clears it; 30 s `ts` gate) |
