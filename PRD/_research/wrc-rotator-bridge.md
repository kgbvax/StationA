# Research spec — wrc-rotator-bridge (HF antenna rotator → MQTT bridge)

Source of truth: code at `/Users/ingomar.otter/dev/stationa/wrc-rotator-bridge` (Go). All facts
below were read from the code, cross-checked against the project CLAUDE.md and
`docs/wrc-rotator-bridge-mqtt-api.md`. Where docs and code disagree, the code wins and the
disagreement is called out.

**Jargon primer (plain-language definitions, used throughout):**
- **Amateur radio (ham) station**: a licensed private radio installation. This one is called
  "Mühle" (site `muehle`).
- **Rotator**: an electric motor mount that physically turns a directional antenna so it points
  at a chosen compass heading.
- **Azimuth (`az`)**: compass bearing in degrees. 0 = north, 90 = east, 180 = south, 270 = west.
- **CW / CCW**: clockwise / counter-clockwise rotation direction (seen from above; CW increases
  azimuth). Nothing to do with Morse "CW" mode.
- **MQTT**: a lightweight publish/subscribe message protocol. Clients publish messages to
  *topics* (hierarchical strings) and subscribe to topic filters.
- **Retained message**: an MQTT message the broker stores and re-delivers to every future
  subscriber of that topic, until replaced. Acts like "the last known value".
- **QoS**: MQTT delivery guarantee level. QoS 0 = at-most-once; QoS 1 = at-least-once.
- **LWT (Last Will and Testament)**: a message the broker publishes on a client's behalf if that
  client disconnects uncleanly (crash, network loss). Used here as the component's liveness flag.
- **WRC**: "Web Remote Control", a third-party controller (by radio amateur AF6SA) wired to the
  rotator; it exposes a WebSocket server for monitoring and steering. It is the *upstream device*
  this bridge talks to.
- **GS-232B**: a classic Yaesu ASCII command protocol for rotator controllers (commands like
  `C`, `M180`, `S`), spoken by most legacy rotator-control software.
- **PSTRotator**: a popular Windows rotator-control program (by YO3DMU); it can steer rotators
  by sending short XML datagrams over UDP.
- **Slot**: this station models every device/adapter as an MQTT address
  `<site>/<station>/<slot>` with four "planes": `/meta`, `/state`, `/status`, `/cmd`.
- **shari**: the Raspberry Pi (192.168.1.139) that runs all station services.

---

## 1. Purpose & role

wrc-rotator-bridge is a small always-on daemon that connects the station's HF-band antenna
rotator — a Yaesu G-450DC motor driven through an AF6SA WRC controller — to the station's MQTT
message bus, filling the slot `muehle/hf/rotator`. It does four things:

1. Dials the WRC's WebSocket (default `ws://192.168.1.108/wsrotor`), which continuously streams
   the rotator's status as JSON, and republishes that status as a single retained JSON document
   on `muehle/hf/rotator/state`.
2. Accepts commands on `muehle/hf/rotator/cmd` (`set_az`, `stop`, `fwd`, `rev`) and forwards
   them to the WRC over the same WebSocket. It is a **read-write** slot.
3. Optionally (on by default) runs a **GS-232B TCP server** on port 7373 and a
   **PSTRotator UDP listener** on port 12040 so legacy desktop rotator-control software can
   drive the same rotator directly; motion resulting from either path still surfaces in
   `/state` because all paths funnel into the same device object.
4. Publishes a retained `/meta` "birth certificate" describing its identity and capabilities,
   and a retained `/status` online/offline liveness flag backed by an MQTT Last Will.

It replaced an older third-party bridge ("rotint") that used a different topic tree
(`rotor2mqtt/…`) and carried its MQTT password on the command line. A separate slot
`muehle/uhf/rotator` (a different program, pelcobridge2) handles the UHF antenna positioner —
no collision.

## 2. Upstream interface — the AF6SA WRC WebSocket

**Transport**: plain (unencrypted, unauthenticated) WebSocket to a configured URL
(`rotor.url`, default `ws://192.168.1.108/wsrotor`). No HTTP headers are sent on the upgrade
request (empty `http.Header{}`); no WebSocket subprotocol; no authentication; the client never
sends anything before the server's first status frame.

**Connection parameters (behavior contract)**:
- WebSocket handshake timeout: **10 seconds** (dialer `HandshakeTimeout`).
- The dial is context-aware: a shutdown signal during the handshake aborts it.
- There is **no application-level handshake**: once the socket is open, the WRC simply pushes
  JSON text frames; the bridge may at any time push JSON command frames.
- There is **no read timeout / liveness watchdog** on the stream (see Known defects).

**Downstream frames (WRC → bridge)**: JSON text messages, one status document per frame:

```json
{
  "state": "rotating",        // controller state string, e.g. "stopped", "rotating", "idle"
  "name":   "…",              // rotor label (optional; parsed, unused)
  "az":     123.5,            // current azimuth, degrees (number)
  "lim1":   0.0,              // CCW rotation limit (optional; parsed, unused)
  "lim2":   450.0,            // CW rotation limit (optional; parsed, unused)
  "tdeg":   180.0,            // commanded target azimuth (optional)
  "fmsg":   "…"               // fault message (optional)
}
```

Only `state` and `az` are semantically consumed; `tdeg` becomes `target_az`, `fmsg` becomes
`error`. The WRC is observed to stream these frames continuously ("frequently") — the exact
cadence is firmware-determined and not controlled or documented by this bridge; a stationary
rotator still gets frames (they are deduplicated before publishing; see §3).

**Upstream frames (bridge → WRC)**: JSON, always a single object with one key `az` whose value
is **either a string**:

| Command | Wire frame | Meaning |
|---|---|---|
| Rotate to azimuth | `{"az":"180"}` | Rotate to 180°. **The number MUST be sent as a quoted string** (Go: `strconv.FormatFloat(az,'f',0,64)` — no decimal point, no exponent). The controller firmware ignores numeric JSON values; a quoted string is required. |
| Stop | `{"az":"stop"}` | Halt motion immediately. |
| Jog clockwise | `{"az":"fwd"}` | Continuous rotation CW until stopped. |
| Jog counter-clockwise | `{"az":"rev"}` | Continuous rotation CCW until stopped. |

Writes are serialized behind a mutex; commands from MQTT, GS-232 and PSTRotator paths never
interleave on the wire.

**Connection-loss detection**: only via WebSocket read errors (read loop returns on any
`ReadMessage` error) or process shutdown closing the socket. No ping/pong, no idle timeout.
On loss, the bridge marks the device offline (see §5) and retries with backoff.

**Canonicalization rules (behavior contract)**:
- `moving` is computed from the raw `state` string: lower-cased, `true` iff it contains the
  substring `rotat` OR `moving`. Empty string → `false`. (Deliberately fuzzy so renamed
  firmware strings still map; observed values are `rotating` while turning, `stopped`/`idle`
  at rest.)
- `target_az` is set from `tdeg` **only when `tdeg != 0`** — a legitimate target of 0° is
  indistinguishable from "absent" (see Known defects).
- The raw `state` string is preserved verbatim in `rotor_state` for diagnostics.
- The last known `az` is kept across a reconnect (the device object never zeroes it while the
  process lives).

## 3. MQTT presence

MQTT 3.1.1 over plain TCP to `mqtt.broker` (default `tcp://192.168.1.50:1883`), username `hf`
(only if `mqtt.user` is non-empty), password from the `WRC_ROTATOR_BRIDGE_MQTT_PASSWORD`
environment variable. Client ID defaults to `<site>-<station>-<slot>` = `muehle-hf-rotator`
(configurable). paho auto-reconnect is on with connect-retry every **5 seconds**; the initial
connect is made interruptible by the shutdown signal (a Go workaround for a paho library
limitation — implementation detail, but the *behavior* "SIGTERM during broker outage resolves
promptly" is contract).

### 3.1 Topics (exact strings, defaults)

| Topic | Direction | Retained | QoS | Cadence |
|---|---|---|---|---|
| `muehle/hf/rotator/meta` | publish | yes | 1 | on every MQTT (re)connect |
| `muehle/hf/rotator/state` | publish | yes | 1 | on every *change* of a published field (dedup) |
| `muehle/hf/rotator/status` | publish (LWT + on connect) | yes | 1 | `online` on every (re)connect; `offline` is the broker-delivered Last Will |
| `muehle/hf/rotator/cmd` | subscribe | no | 1 | resubscribed on every (re)connect |

Topic-addressing rule: `<site>/<station>/<slot>/<plane>` built from config; the literal
defaults above come from `site=muehle`, `station=hf`, `slot=rotator`. Publishing QoS rule:
retained topics go at QoS 1, non-retained at QoS 0 (only `/cmd` inbound is QoS 1).

`/cmd` is deliberately **not** retained by publishers: a rotator move is one-shot intent, and
replaying a stale azimuth after a restart could spin the antenna unexpectedly. There is no
desired-state reconciliation loop; consumers observe the result in `/state`.

### 3.2 `/meta` payload (retained birth certificate)

Published identically on every MQTT connect. Exact JSON (values shown with defaults; `host`,
`location`, `device.model`, `link` come from config):

```json
{
  "schema": "1.0",
  "role": "rotator",
  "device": { "model": "Yaesu G-450DC" },
  "link": "ethernet",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": { "axes": ["az"] },
  "expose": {
    "device": { "name": "HF Rotator", "model": "Yaesu G-450DC", "manufacturer": "Yaesu" },
    "fields": [
      { "key": "az", "name": "Azimuth", "type": "number", "unit": "°", "class": "azimuth",
        "state_class": "measurement", "writable": true, "min": 0, "max": 450, "step": 1,
        "command": { "action": "set_az", "value_key": "az", "value_type": "float" } },
      { "key": "target_az", "name": "Target Azimuth", "type": "number", "unit": "°",
        "class": "azimuth", "state_class": "measurement" },
      { "key": "moving", "name": "Moving", "type": "boolean" },
      { "key": "rotor_state", "name": "Rotor State", "type": "string" },
      { "key": "device_online", "name": "Device Online", "type": "boolean" }
    ],
    "actions": [
      { "key": "stop", "name": "Stop",      "command": { "action": "stop" } },
      { "key": "fwd",  "name": "Rotate CW", "command": { "action": "fwd" } },
      { "key": "rev",  "name": "Rotate CCW","command": { "action": "rev" } }
    ]
  }
}
```

Notes: `role` is the canonical slot role `rotator`, never the device name. `capabilities.axes`
is always `["az"]` — azimuth-only, hard-coded (no elevation). `expose` is a consumer-neutral
field/action surface; a separate station component (`hadiscovery`) reads it to render Home
Assistant home-automation entities; **this bridge itself publishes nothing under
`homeassistant/…`**. `min:0, max:450, step:1` on `az` are advertised metadata only — the
bridge does **not** clamp or validate incoming azimuths against them (see Known defects).
No `area` field is published; the discovery component supplies a default area.

### 3.3 `/state` payload (retained live snapshot)

Single JSON document, published **only when a published field changes** (the WRC streams
constantly; dedup keeps the bus quiet while the antenna is parked). Dedup compares
`az`, `target_az`, `moving`, `rotor_state`, `device_online`, `error` (exact equality, floats
included — safe because raw WRC values pass through unchanged).

```json
{
  "ts": "2026-07-06T12:34:56Z",
  "az": 123.5,
  "target_az": 180.0,
  "moving": true,
  "rotor_state": "rotating",
  "device_online": true
}
```

| Field | JSON type | Unit | Semantics |
|---|---|---|---|
| `ts` | string | — | RFC 3339, UTC, time of *this publish* (not the WRC frame time — the WRC sends no timestamp) |
| `az` | number | degrees | current azimuth, raw WRC value passed through |
| `target_az` | number | degrees | commanded target from WRC `tdeg`; **key omitted** when 0/absent (`omitempty`) |
| `moving` | boolean | — | derived from `rotor_state` via the substring rule in §2 |
| `rotor_state` | string | — | raw WRC state string; key omitted when empty |
| `device_online` | boolean | — | true iff the WRC WebSocket is currently up; false while the bridge itself is still alive but the WRC link is down |
| `error` | string | — | human-readable fault: WRC `fmsg` when connected, or `wrc: <go error>` when the link drops; key omitted when empty |

Publish triggers, exactly: (a) every parsed WRC status frame that differs from the last
published snapshot in any of the six fields above; (b) the WebSocket dropping or the reconnect
attempt cycle changing `device_online`/`error`. A rotator state document **never** carries
`freq_hz`/`band`/`mode` — those are radio concerns and belong to other slots.

### 3.4 `/status` (liveness)

Plain string payload (not JSON): `online` or `offline`, retained, QoS 1. `online` is published
by the bridge on every MQTT (re)connect. `offline` is registered as the MQTT Last Will
(set at connect: topic `muehle/hf/rotator/status`, payload `offline`, QoS 1, retained), so the
broker publishes it if the bridge dies uncleanly. **This flag reflects the bridge process,
not the WRC link** — two-layer liveness: if the WRC vanishes while the bridge runs,
`/status` stays `online` and `/state.device_online` flips to `false`. Consumers must AND both
signals. (Discrepancy: the API doc claims `offline` is "published on clean shutdown" too, but
the code never publishes it and a graceful MQTT disconnect does not trigger a Last Will — see
Known defects.)

## 4. Command surface

### 4.1 MQTT `/cmd` (JSON, not retained, QoS 1)

| Action | Payload | Behavior |
|---|---|---|
| `set_az` | `{"action":"set_az","az":180}` | Rotate to 180°. Sends `{"az":"180"}` to the WRC. `az` is a JSON number (any float accepted; no range check). |
| `stop` | `{"action":"stop"}` | Sends `{"az":"stop"}` — halt motion. |
| `fwd` | `{"action":"fwd"}` | Continuous jog clockwise; sends `{"az":"fwd"}`. Runs until the rotator hits a limit or a `stop` arrives. |
| `rev` | `{"action":"rev"}` | Continuous jog counter-clockwise; sends `{"az":"rev"}`. |

Rules: the argument key for `set_az` is **`az`** (this bridge predates/parallels the station
convention of a generic `value` key; the `expose.command.value_key: "az"` in `/meta` is the
authoritative declaration, so consumers should read the value key from `/meta`). Unknown
actions are logged and ignored. `set_az` with missing/absent `az` is logged and ignored.
Malformed JSON is logged and ignored. No acknowledgment/reply topic exists — success is
observed by watching `/state` (`moving` → true, `az` → target, `target_az` → commanded value).

**Dispatch pipeline (behavior contract)**: the MQTT message handler only enqueues the raw
payload onto a bounded queue of capacity **8**; a single worker goroutine parses and executes
commands one at a time. If more than 8 commands are queued, excess commands are **dropped**
with a warning log (never blocks the MQTT receive path). Commands execute strictly FIFO.

**Failure mode**: if the WRC WebSocket is down when a command is dequeued, the write fails
(`wrc: websocket not connected` / `wrc write: …`), the failure is logged, and the command is
**discarded — no retry, no queueing until reconnect**. A `stop` issued during a brief WRC
outage is therefore lost.

### 4.2 GS-232B TCP server (optional, default enabled on `0.0.0.0:7373`)

One line per command, terminated by `\r` or `\n` (either, not necessarily both). Input is
trimmed and upper-cased before matching.

| Line in | Behavior | Response |
|---|---|---|
| `C` or `C2` | query position | `+0aaa+0000\r` where `aaa` = current azimuth zero-padded to 3 digits (truncated to integer, e.g. 180 → `+0180+0000\r`; the `+0000` is a fixed zero elevation — azimuth-only rotator) |
| `M` + 1–3 digits (e.g. `M090`) | rotate to that azimuth | `\r` |
| `W` + 1–3 digits + whitespace + integer (e.g. `W180 000`) | rotate to that azimuth (elevation argument parsed and ignored) | `\r` |
| `S` | stop | `\r` |
| anything else | — | `?>\r` |

Edge behaviors: the `M`/`W` matchers anchor **only at the start of the line** and silently
ignore trailing characters after the captured digits (regexes `^M(\d{1,3})` and
`^W(\d{1,3})\s+\d+` in `internal/gs232/server.go`). So `M1234` prefix-matches, captures `123`,
acks `\r`, and rotates to 123°; `M12.5`/`M12X` likewise rotate to 12°. For `W`, the 1–3 azimuth
digits plus the whitespace-separated integer elevation argument are required; trailing
characters after the elevation integer are ignored (`W180 000abc` → rotate to 180°, ack). Only
an `M` with zero following digits (or a `W` lacking the whitespace+integer elevation part)
gets **no response at all** (no `\r`, no error). The line reader stops at the first `\r` or
`\n` and deliberately does not consume a following `\n` (a `\r\n` pair leaves the `\n` to be
read as a harmless empty line). The azimuth source for `C` replies is the last WRC status
frame — if the WRC has never been reached since process start it answers `+0000+0000`.
Commands funnel into the same serialized WRC write path as MQTT; motion surfaces in `/state`.

### 4.3 PSTRotator UDP listener (optional, default enabled on `0.0.0.0:12040`)

Datagrams up to 1024 bytes; XML-ish content matched case-insensitively with regexes.

| Datagram | Behavior | Response |
|---|---|---|
| contains `AZ?` (e.g. `<PST>AZ?</PST>`) | position query | `<PST><AZIMUTH>aaa</AZIMUTH></PST>` (integer azimuth) sent as a UDP datagram to the **source IP on port+1** (i.e. 12041 by default) — the PSTRotator program's reply convention |
| contains `<STOP>…</STOP>` | stop motion | none |
| contains `<PARK>…</PARK>` | logged, ignored (no park support) | none |
| contains `<AZIMUTH>-?\d+(\.\d+)?</AZIMUTH>` (whitespace tolerant, negatives and decimals allowed; e.g. `<PST><AZIMUTH>180</AZIMUTH><ELEVATION>45</ELEVATION></PST>` → 180, elevation ignored) | rotate to that azimuth | none |
| anything else | logged | none |

Precedence: `AZ?` check first, then `STOP`, then `PARK`, then `AZIMUTH` — so a datagram carrying
both `<STOP>` and `<AZIMUTH>` stops. All drive the same WRC write path.

## 5. Behavior & state machine

### 5.1 Startup (in order)

1. Load config: defaults → TOML file (`-config`, default `/etc/wrc-rotator-bridge/config.toml`)
   → environment overrides → flag overrides (`-log.level`, `-debug`). Missing file is fine
   (defaults); malformed TOML or validation failure exits with code **2**.
2. Validate: `mqtt.site` and `mqtt.station` non-empty; `rotor.url` non-empty; `gs232.port` and
   `pstrotator.port` non-zero when the respective server is enabled. Violations exit 2.
3. Install SIGINT/SIGTERM handler (cancellation context).
4. Connect MQTT **first** (with Last Will `offline` registered). On connect: publish retained
   `online` to `/status`, publish retained `/meta`, subscribe `/cmd` QoS 1. **MQTT connect
   failure is fatal** — the process exits 1 and systemd restarts it after 5 s.
5. Start the GS-232 TCP server and the PSTRotator UDP listener (each in its own goroutine).
   A listen failure (e.g. port already taken) is only **logged as an error** — the process
   keeps running without that path.
6. Enter the WRC WebSocket loop (`wsLoop`), which runs until shutdown.

Shutdown: SIGTERM/SIGINT cancels the context → WRC read loop is nudged closed → MQTT client
disconnects (500 ms quiesce) → exit code **0** (deliberately, so `systemctl stop` reports
success). Any other error exits 1.

### 5.2 WRC WebSocket loop (reconnect behavior — behavior contract)

```
attempt = 0; backoff = 2 s
loop:
    dial WRC (handshake timeout 10 s)
    on success:
        publish nothing yet; device-online state goes true internally,
        then read frames; each parsed frame updates state and publishes /state on change
        (first frame after a reconnect flips /state.device_online back to true)
    on dial or read error (and shutdown not requested):
        publish /state with device_online=false and error="wrc: <error>"
        sleep backoff (interruptible by shutdown)
        backoff = min(backoff * 1.5, 60 s)   →  2s, 3s, 4.5s, 6.75s, … capped at 60s
        retry
```

Key exact values: initial backoff **2 s**, growth factor **1.5×**, cap **60 s**. The backoff
resets only on process restart (it does not reset after a successful connection — the retry
interval keeps growing across repeated failures within one process lifetime). During all WRC
retries, the bridge stays connected to MQTT: `/status` stays `online`, `/meta` stays retained;
only `/state.device_online` reflects the outage. The last known azimuth is preserved across
reconnects (published snapshots after a reconnect keep the old `az` until a fresh frame
arrives). On reconnect, internal state between connect and the first status frame has
`moving=false`, `target_az` absent, `rotor_state` absent — but `/state` is only published on
change, so the visible effect is `device_online` returning true (with the next frame's data).

### 5.3 MQTT reconnect behavior

paho auto-reconnect (retry interval 5 s) handles broker outages transparently. On every
(re)connect: `online` → `/status` (retained), `/meta` republished (retained), `/cmd`
resubscribed (QoS 1). Commands that arrive while the broker link is momentarily down are not
received (nothing retained). Publishes issued while disconnected are lost silently (the
publisher never blocks on MQTT delivery and never retries; retained documents self-heal on
the next publish). During an MQTT outage `/state` snapshots produced are dropped, but the
next change after reconnect republishes current truth.

### 5.4 Error paths summary

| Event | Visible effect | Recovery |
|---|---|---|
| WRC dial fails / WS drops | `/state`: `device_online:false`, `error:"wrc: …"`; `/status` unchanged | backoff retry loop §5.2 |
| WRC sends unparsable JSON | warning log; frame skipped; connection kept | none needed |
| `/cmd` JSON malformed / unknown action / set_az missing az / no commander | warning log, ignored | — |
| `/cmd` while WRC down | warning log, command dropped | operator re-issues |
| >8 commands queued | warning log, command dropped | operator re-issues |
| GS-232/PSTRotator listen fails | error log, that path disabled for process lifetime | restart process |
| GS-232 client disconnects | connection closed, logged | — |
| MQTT connect fails at boot | process exit 1 | systemd `Restart=on-failure`, `RestartSec=5` |
| Bridge crash / broker link loss | broker fires LWT → `/status` = `offline` (retained) | systemd restart |

## 6. Configuration

File: TOML at `/etc/wrc-rotator-bridge/config.toml` (flag `-config`). Layering: built-in
defaults ← TOML ← environment ← `-log.level` flag. Unknown TOML keys are ignored (not an
error). All keys:

| Key | Default | Meaning |
|---|---|---|
| `host` | `shari` | compute node name, published in `/meta.host` |
| `rotor.url` | `ws://192.168.1.108/wsrotor` | WRC WebSocket endpoint (mandatory non-empty) |
| `gs232.enabled` | `true` | run the GS-232B TCP server |
| `gs232.bind` | `0.0.0.0` | listen address |
| `gs232.port` | `7373` | listen port (must be non-zero when enabled) |
| `pstrotator.enabled` | `true` | run the PSTRotator UDP listener |
| `pstrotator.bind` | `0.0.0.0 | listen address |
| `pstrotator.port` | `12040` | listen port (must be non-zero when enabled) |
| `device.model` | `Yaesu G-450DC` | identity string in `/meta.device.model` and `expose.device.model` |
| `device.link` | `ethernet` | transport label in `/meta.link` |
| `mqtt.broker` | `tcp://192.168.1.50:1883` | broker URI |
| `mqtt.client_id` | `""` → `muehle-hf-rotator` | MQTT client ID |
| `mqtt.user` | `hf` | MQTT username |
| `mqtt.password` | `""` (overridden by env) | MQTT password — keep out of TOML |
| `mqtt.site` / `mqtt.station` / `mqtt.slot` | `muehle` / `hf` / `rotator` | topic address; site+station mandatory |
| `mqtt.location` | `bauwagen` | physical-location label in `/meta.location` |
| `mqtt.discovery_prefix` | `homeassistant` | legacy key; unused (embedded discovery not wired) |
| `mqtt.publish_ha_discovery` | `false` | legacy migration key; parsed but **not wired to anything** — no embedded HA discovery exists in this component |
| `log.level` | `info` | `debug`\|`info`\|`warn`\|`error` |

Flags: `-config <path>`, `-log.level <level>` (overrides config), `-debug` (logs raw WRC
WebSocket frames both directions).

Environment overrides (systemd EnvironmentFile): `WRC_ROTATOR_BRIDGE_MQTT_BROKER`,
`_MQTT_CLIENT_ID`, `_MQTT_USER`, `_MQTT_PASSWORD` (the secret), `_MQTT_SITE`, `_MQTT_STATION`,
`_MQTT_SLOT`, `_ROTOR_URL` (full prefix `WRC_ROTATOR_BRIDGE_`). Non-empty values replace the
TOML value.

Secrets: the MQTT password lives only in
`/etc/wrc-rotator-bridge/wrc-rotator-bridge.env` (0600, owned by the service user) as
`WRC_ROTATOR_BRIDGE_MQTT_PASSWORD="…"`, loaded by systemd `EnvironmentFile=`. It never
appears in the TOML, the unit file, or the process command line.

## 7. Deployment

- Target host: Raspberry Pi `shari` at `192.168.1.139`, SSH user `io` (configurable via
  `SSH_HOST`/`SSH_USER` env for `deploy.sh`).
- `deploy.sh` behavior: cross-compiles for `linux/arm64` (`GOOS=linux GOARCH=arm64
  CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"`) to `dist/wrc-rotator-bridge-linux-arm64`;
  generates the systemd unit; generates **seed** config.toml and seed env file (umask 077,
  password deliberately in a separate temp file); scp's binary, unit, seeds to `/tmp` on the
  Pi; then over SSH: creates system user `wrc-rotator-bridge` (no home, nologin) if missing;
  installs binary to `/opt/wrc-rotator-bridge/`; **seed-once** — installs
  `/etc/wrc-rotator-bridge/config.toml` (0600, service-user owned) and the env file only if
  they don't already exist (the device owns its settings thereafter; redeploy never
  overwrites them); stops service, moves binary+unit into place, `daemon-reload`, `enable`,
  `restart`, prints status.
- systemd unit `wrc-rotator-bridge.service`: `Type=simple`,
  `ExecStart=/opt/wrc-rotator-bridge/wrc-rotator-bridge -config /etc/wrc-rotator-bridge/config.toml`,
  `EnvironmentFile=/etc/wrc-rotator-bridge/wrc-rotator-bridge.env`,
  `Restart=on-failure`, `RestartSec=5`, `After=network-online.target`, runs as
  `User=wrc-rotator-bridge` / `Group=wrc-rotator-bridge`, `ConfigurationDirectory` and
  `StateDirectory` = `wrc-rotator-bridge`. Hardening: `NoNewPrivileges=true`,
  `ProtectSystem=strict`, `ProtectHome=true`, `PrivateTmp=true`, `PrivateDevices=true`
  (network-only service, no serial devices), `ProtectKernelTunables/Modules/ControlGroups=true`,
  `RestrictAddressFamilies=AF_INET AF_INET6`, `RestrictNamespaces=true`, `LockPersonality=true`,
  `RestrictRealtime=true`, `RestrictSUIDSGID=true`, `RemoveIPC=true`,
  `CapabilityBoundingSet=` (empty), `AmbientCapabilities=` (empty),
  `ReadWritePaths=/var/lib/wrc-rotator-bridge`, `MemoryMax=256M`, `TasksMax=64`,
  logs to journal with identifier `wrc-rotator-bridge`.
- One-time `migrate-from-rotint.sh` (run **before** first deploy): on the Pi, creates the new
  service user and config dir; extracts the MQTT password from the old `rotor-bridge.service`
  unit's `-password="…"` command line **on the device** and writes it to the new env file
  (0600) — fixing the old bridge's command-line secret exposure; stops+disables the old
  rotint service and removes its unit; leaves `/home/io/rotint` and user `io` untouched.
  Idempotent; backs up any pre-existing env file to `*.pre-migration`.
- Dependencies: Go module `wrc-rotator-bridge` (go 1.26), deps: `gorilla/websocket` v1.5.3,
  `eclipse/paho.mqtt.golang` v1.5.1, `BurntSushi/toml` v1.6.0, and in-repo shared module
  `codeberg.org/kgbvax/stationa/shared` (topic builders + ctx-aware MQTT connect).

## 8. Invariants & safety rules

1. **`/cmd` is never treated as retained state.** No desired-azimuth is stored or re-applied
   on any restart or reconnect; a move is one-shot intent. A reimplementation must not add
   "reconciliation" that replays the last commanded azimuth.
2. **Two-layer liveness must stay two-layer.** `/status` reflects the bridge process only;
   the WRC link state lives exclusively in `/state.device_online`. Never flip `/status`
   offline when the WRC drops, and never report bridge death in `/state`.
3. **`/state` and `/meta` are single retained JSON documents**; `/status` is a plain
   `online`/`offline` string. No per-field topics, no HA-vocabulary topics under
   `homeassistant/…`.
4. **All writes to the WRC must be serialized** (one writer at a time across MQTT, GS-232 and
   PSTRotator paths) and **absolute azimuths must be sent as quoted strings**
   (`{"az":"180"}`), never numbers — the controller ignores numeric values.
5. **The paho MQTT receive path must never block on WRC I/O** (here: bounded queue of 8 with
   drop-on-overflow; commands processed by a single worker). Blocking the MQTT dispatch
   thread deadlocks the client library.
6. **Exit code 0 on SIGTERM/SIGINT**; 2 on config errors; 1 on runtime failure. Clean
   shutdown must not look like a crash to systemd.
7. **No secrets in TOML, unit file, or command line** — password only via the 0600
   EnvironmentFile, and deploy.sh must never overwrite the on-device env file after first
   install (seed-once).
8. **A rotator never publishes radio fields** (`freq_hz`, `band`, `mode`).
9. `stop` takes precedence over embedded azimuth in a PSTRotator datagram; a stop that loses a
   race with a set is resolved by whichever command reaches the serialized write path first
   (FIFO).

## 9. Known defects & fragilities

- **`target_az` of 0 is unrepresentable**: `FromStatus` sets `target_az` only when `tdeg != 0`,
  so a commanded target of exactly 0° (north) is dropped/omitted. (Conflation of "absent" and
  "zero" — a JSON `omitempty`-style shortcut applied to upstream data.)
- **No watchdog on the WRC stream**: the read loop has no read/idle timeout. If the WRC TCP
  connection hangs without closing (half-open, silent device), the bridge reports
  `device_online:true` forever with a frozen azimuth. Only a socket error triggers recovery.
- **Commands are silently dropped when the WRC is down** — including `stop`. There is no
   pending-command queue for the reconnect. A stop issued during an outage is lost; the
   antenna keeps turning per the controller's own last command.
- **No azimuth range validation anywhere**: MQTT `set_az`, GS-232 (up to `M999`) and
   PSTRotator (arbitrary decimals/negatives) are forwarded verbatim. `/meta` advertises
   `min:0 max:450` but nothing enforces it; out-of-range handling is entirely up to the WRC
   firmware.
- **Doc/code discrepancy on clean-shutdown `offline`**: `docs/wrc-rotator-bridge-mqtt-api.md`
  §3 says `/status` `offline` is "published on clean shutdown", but the code never publishes
  `offline` itself, and a graceful MQTT disconnect does not fire the Last Will — after a
  clean `systemctl stop` the retained `/status` can remain `online` until the process is
  next seen. (Other stationa consumers are advised to treat `/status` as advisory; the
  memory note "stationa two-layer liveness" exists because of exactly this class of gap.)
- **Publish errors are invisible**: the publisher fires-and-forgets MQTT publishes (never
  waits on delivery tokens, always returns nil). A failed `/state` publish is silently lost.
- **GS-232 `M`/`W` with non-matching digit patterns get no reply at all** (no `\r`), which
  can hang naive line-oriented clients waiting for an acknowledgment.
- **`moving` detection is substring-based** (`rotat`/`moving`) — robust to firmware renames
  but would misclassify any future state string that merely *contains* those substrings while
  meaning something else (e.g. `pre-rotating-check`).
- **Backoff never resets** after a successful connection within a process lifetime; a
  pattern of alternating success/long-failure drives the retry interval to the 60 s cap and
  keeps it there until restart.
- **`expose.device.name` "HF Rotator" and `manufacturer "Yaesu"` are hard-coded** in the
  binary, not config (minor; everything else in `/meta` is configurable).
- **Legacy dead config**: `mqtt.publish_ha_discovery` and `mqtt.discovery_prefix` are parsed
  but wired to nothing (embedded HA discovery does not exist in this component); a leftover
  `publishDiscovery` helper in the bridge package is dead code "kept for symmetry".
- **`rotor_state`/`device_online` transient flattening on reconnect**: between a successful
  WRC re-dial and the first status frame the device's cached state is rebuilt with only the
  old azimuth preserved (`moving=false`, empty `rotor_state`, empty `target_az`) — published
  snapshots in that window look "cleaner" than reality for one frame.

## 10. Re-implementation notes

**Must be preserved verbatim (behavior contract):**
- Exact topic strings and plane semantics: `muehle/hf/rotator/{meta,state,status,cmd}`;
  retained meta/state/status, non-retained cmd; `/status` payload exactly `online`/`offline`;
  LWT registered as `offline` retained QoS 1 at connect; `online` + `/meta` + `/cmd`
  resubscribe on every MQTT reconnect.
- Exact `/meta` JSON shape including the full `expose` block (fields, actions, the `az`
  setpoint descriptor `action=set_az, value_key=az, value_type=float`, and
  `capabilities.axes=["az"]`) — `hadiscovery` consumes this structure programmatically.
- Exact `/state` field names, types, and omission rules (`ts` RFC3339 UTC publish time;
  `az` always; `moving`/`device_online` always; `target_az`/`rotor_state`/`error` omitted when
  empty/zero) and the six-field change-dedup rule.
- Exact `/cmd` grammar: `{"action":…}` with argument under key `az` for `set_az`; unknown →
  ignore; queue cap 8 with drop-on-overflow; FIFO single-worker execution.
- WRC wire protocol exactly: status JSON field names (`state`,`name`,`az`,`lim1`,`lim2`,
  `tdeg`,`fmsg`); command JSON `{"az":"…"}` with **string** values (`"<int>"`/`stop`/`fwd`/
  `rev`); 10 s handshake timeout; 2 s→×1.5→60 s backoff; two-layer liveness split
  (WS loss ⇒ `device_online:false` + `error:"wrc: …"`, `/status` untouched).
- GS-232B subset exactly: command letters, terminators, `+0aaa+0000\r` / `\r` / `?>\r`
  responses; PSTRotator UDP subset exactly: regex tolerance (case-insensitive, decimals,
  negatives), precedence (AZ? > STOP > PARK > AZIMUTH), park = no-op, reply to port+1 with
  `<PST><AZIMUTH>aaa</AZIMUTH></PST>`.
- Config layering (defaults → TOML → env → flag), the exact default values of §6, the secret
  in the EnvironmentFile only, and the seed-once deploy rule.
- Exit codes (0 SIGTERM / 2 config / 1 runtime) and `Restart=on-failure, RestartSec=5`.

**Free to change (implementation detail):**
- Go, paho, gorilla/websocket, TOML library, slog logging, the package layout, the
  ctx-aware-connect workaround (any mechanism giving "SIGTERM interrupts a hanging initial
  MQTT connect" is fine).
- The mutex/channel plumbing (any equivalent serialization is fine, as long as invariants
  4–5 hold).
- The dead `publishDiscovery` helper and the unused `publish_ha_discovery` /
  `discovery_prefix` config keys (a reimplementation should simply drop them).
- The GS-232 custom line reader (any reader tolerating `\r` or `\n` singly is fine).
- Log message wording; log destination (journal via stderr).

**Deliberate decisions a reimplementation should not "fix" without PRD-level sign-off**
(each is a behavior change, not a bug fix): adding `/cmd` retention or desired-state
reconciliation; merging device liveness into `/status`; clamping/validating azimuths;
retrying or queueing commands across WRC outages; resetting backoff on success.