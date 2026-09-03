# Salvage: wrc-rotator-bridge.md
> Extracted from PRD/03-components/wrc-rotator-bridge.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [protocol] AF6SA WRC WebSocket device protocol
Target home: `wrc-rotator-bridge/docs/wrc-device-protocol.md` (the MQTT-facing
`docs/wrc-rotator-bridge-mqtt-api.md` does not cover the WRC side at all).

The WRC ("Web Remote Control", third-party rotator controller by radio amateur AF6SA)
exposes a WebSocket at `ws://192.168.1.108/wsrotor` (config `rotor.url`). Plain
(unencrypted, unauthenticated) WebSocket; no HTTP header customization, no subprotocol
negotiation, no authentication, no application-level handshake. The bridge is the client.
Once the socket is open the WRC pushes JSON status frames continuously (even when the
antenna is stationary; the firmware sets the exact cadence) and the bridge can push JSON
command frames at any time.

Normative connection requirements:
- The WebSocket handshake must time out after **10 seconds** if the WRC does not complete
  the upgrade.
- A shutdown signal received during the handshake must stop the dial.
- The bridge must not need any frame from the WRC before it sends its first command frame.

### Downstream frames (WRC → bridge): status documents

```json
{
  "state": "rotating",
  "name":   "…",
  "az":     123.5,
  "lim1":   0.0,
  "lim2":   450.0,
  "tdeg":   180.0,
  "fmsg":   "…"
}
```

| Key | JSON type | Needed | Meaning |
|---|---|---|---|
| `state` | string | yes | controller state string. Observed values: `rotating` while turning. `stopped` or `idle` at rest. |
| `name` | string | no | rotor label. The bridge parses it but does not use it. |
| `az` | number | yes | current azimuth in degrees. |
| `lim1` | number | no | counter-clockwise rotation limit in degrees. The bridge parses it but does not use it. |
| `lim2` | number | no | clockwise rotation limit in degrees. The bridge parses it but does not use it. |
| `tdeg` | number | no | commanded target azimuth in degrees. |
| `fmsg` | string | no | fault message. |

The bridge must skip frames with unparsable JSON and log a warning. It must not close the
connection.

### Upstream frames (bridge → WRC): command documents

Every command frame is a single JSON object with exactly one key, `az`. Its value is
**always a string**:

| Command | Wire frame | Meaning |
|---|---|---|
| Rotate to azimuth | `{"az":"180"}` | Rotate to 180°. |
| Stop | `{"az":"stop"}` | Stop motion immediately. |
| Jog clockwise | `{"az":"fwd"}` | Continuous CW rotation. It runs until a stop or a limit. |
| Jog counter-clockwise | `{"az":"rev"}` | Continuous CCW rotation. It runs until a stop or a limit. |

**NORMATIVE — string quoting**: the bridge must send the azimuth number as a quoted string
that holds an integer. The string has no decimal point and no exponent (for example `"180"`,
never `180` and never `"180.0"`). The controller firmware ignores numeric JSON values. A send
of `{"az":180}` is a silent no-op. This is the most easily missed requirement of the upstream
protocol. (Code confirms the quoting: `strconv.FormatFloat(az, 'f', 0, 64)` in
`internal/rotor/device.go`.)

The bridge must serialize all writes to the WRC: at most one in-flight command frame at a
time across all inbound paths (MQTT, GS-232B, PSTRotator). Commands from different paths must
not interleave on the wire.

### Canonicalization rules (WRC → bus)

- The bridge derives `moving` from the raw `state` string. It lower-cases the string.
  `moving` is `true` if and only if the string contains the substring `rotat` OR the substring
  `moving`. An empty state string gives `false`. (The rule is deliberately fuzzy, so renamed
  firmware strings still map.)
- The bridge takes `target_az` from `tdeg` **only when `tdeg != 0`**. A commanded target of
  exactly 0° (north) is therefore unrepresentable, and the bridge omits it.
- The bridge preserves the raw `state` string in the published field `rotor_state` for
  diagnostics.
- `fmsg` maps to the published field `error`.
- The bridge keeps the last known `az` across a reconnect. The bridge never zeroes the cached
  azimuth while the process lives.

### Connection-loss detection

The read loop exits on any read failure. Loss of the WRC link therefore shows up only as a
WebSocket read error, or when process shutdown closes the socket. There is no ping/pong
keepalive and no idle/read timeout. On loss, the bridge marks the device offline
(`/state.device_online=false`, `error:"wrc: <text>"`) and retries with backoff.

## [protocol] GS-232B edge semantics (beyond the basic command table)
Target home: `wrc-rotator-bridge/docs/wrc-device-protocol.md`. The basic `C`/`M`/`W`/`S`
response table is already in `docs/wrc-rotator-bridge-mqtt-api.md` §7; the edge semantics
below are NOT.

One command per line. The line ends with `\r` or `\n` (either alone). The bridge trims the
input and upper-cases it before matching.

Edge behaviors (normative. They match the deployed protocol as legacy clients observed it):

- The `M` and `W` matchers anchor **only at the start of the line**. They prefix-match and
  silently ignore any trailing characters after the captured digits.
  - For `M`: the matcher captures the **first 1–3 leading digits** after the letter and
    ignores everything after them. The bridge treats `M1234` as `M123` (ack `\r`, rotate to
    123°). `M12.5` and `M12X` likewise rotate to 12° and ack. Only `M` followed by **zero**
    digits gets **no response at all** — no `\r`, no error.
  - For `W`: the line must carry the 1–3 azimuth digits plus the whitespace-separated integer
    elevation argument (a `W` line that lacks either gets no response at all). The bridge
    ignores trailing characters after the elevation integer (`W180 000abc` → rotate to 180°,
    ack `\r`).
  - This lenient prefix matching is normative deployed behavior. A re-implementation with
    strict full-line matching (rejecting `M1234`) produces observably different wire behavior.
- The digit-less no-response case can hang naive line-oriented clients that wait for an
  acknowledgment.
- The line reader stops at the first `\r` or `\n` and does not read a following `\n`. (A
  `\r\n` pair leaves the `\n` in place. The next read then sees a harmless empty line.)
- The azimuth used in `C` replies is the last WRC status frame. If the process has never
  reached the WRC since start, replies are `+0000+0000`.
- Commands funnel into the same serialized WRC write path as MQTT. Resulting motion surfaces
  in `/state`.

(The deployed matchers are `^M(\d{1,3})` and `^W(\d{1,3})\s+\d+` in
`internal/gs232/server.go`.)

## [protocol] PSTRotator UDP edge semantics (beyond the basic datagram table)
Target home: `wrc-rotator-bridge/docs/wrc-device-protocol.md`. The basic datagram table is
already in `docs/wrc-rotator-bridge-mqtt-api.md` §8; the details below are NOT.

Datagrams up to **1024 bytes**. The listener matches XML-style content case-insensitively
with tolerant (whitespace-flexible) pattern rules.

- `AZ?` (for example `<PST>AZ?</PST>`) → position query. Reply
  `<PST><AZIMUTH>aaa</AZIMUTH></PST>` (integer azimuth) as a UDP datagram to the **source IP,
  port + 1** (12041 by default) — the PSTRotator reply convention.
- `<AZIMUTH>-?\d+(\.\d+)?</AZIMUTH>` → rotate to that azimuth. Negatives and decimals
  allowed; elevation ignored. Pattern is whitespace-flexible.
- `<STOP>…</STOP>` → stop motion, no response. `<PARK>…</PARK>` → logged, ignored (no park
  support).

**Precedence is normative**: `AZ?` first, then `STOP`, then `PARK`, then `AZIMUTH` — so a
datagram that carries both `<STOP>` and `<AZIMUTH>` stops. All drive the same serialized WRC
write path.

## [defect] Clean-shutdown `/status` never goes offline (and the live API doc is wrong)
The component API doc claims the code publishes `/status` `offline` on clean shutdown. The
code does not do this. On a CLEAN process shutdown the broker does not fire the Last Will,
and the bridge never publishes `offline` itself. After a clean stop the retained `/status`
stays `online` indefinitely until the process next appears. Consumers MUST NOT trust
`/status` alone.

Code-check verdict (2026-09-03): still open. `cmd/wrc-rotator-bridge/main.go` only registers
the Last Will (`opts.SetWill(avail, "offline", 1, true)`); nothing publishes `offline` on a
graceful exit. Note: the live doc `wrc-rotator-bridge/docs/wrc-rotator-bridge-mqtt-api.md`
(§3, "published on clean shutdown") states the wrong behavior and should be corrected —
either make the code publish `offline` before a graceful disconnect (making the doc true) or
fix the doc and rely on consumers treating `/status` as advisory.

## [defect] `target_az` of 0 is unrepresentable
The bridge sets `target_az` only when `tdeg != 0`, so it drops a commanded target of exactly
0° (north). The design conflates the notions "absent" and "zero".

Code-check verdict: still open — `internal/rotor/protocol.go` `FromStatus`: `if s.TDeg != 0`.

## [defect] No watchdog on the WRC stream
There is no read/idle timeout. If the WRC TCP connection hangs without closing (half-open,
silent device), the bridge reports `device_online:true` forever with a frozen azimuth. Only a
socket error triggers recovery.

Code-check verdict: still open — `internal/rotor/device.go` `Run` sets no
`SetReadDeadline`/ping handler.

## [defect] Commands silently dropped when the WRC is down — including `stop`
No pending-command queue exists for reconnects. If the WRC WebSocket is down when the worker
dequeues a command, the write fails; the bridge logs the failure and **discards the command —
no retry, no pending queue across reconnect**. A `stop` issued during a WRC outage is
therefore lost, and the antenna keeps turning per the controller's own last command. This is
safety-relevant; changing it is a deliberate behavior change needing sign-off.

Code-check verdict: still open — `internal/bridge/bridge.go` `HandleCommand` logs the
`Commander` error and returns; no retry/queue.

## [defect] No azimuth range validation anywhere
The bridge forwards MQTT `set_az` (any float), GS-232 (up to `M999`), and PSTRotator
(arbitrary decimals and negatives) verbatim. `/meta` advertises `min:0 max:450` on the `az`
field but nothing enforces it — the range is metadata only, and a consumer must not rely on
the bridge enforcing it. Out-of-range handling belongs entirely to WRC firmware.

Code-check verdict: still open — no clamp/validation in `internal/bridge/bridge.go`.

## [defect] Publish errors are invisible
The publisher fires-and-forgets MQTT publishes (never waits on delivery tokens). A failed
`/state` publish is silently lost (self-heals only on the next change).

Code-check verdict: still open — `internal/bridge/bridge.go` `publishState`: `_ =
b.pub.Publish(...)`.

## [defect] GS-232B lenient prefix matching; digit-less lines get no reply
The matchers capture only the first 1–3 digits and ignore trailing characters. The bridge
turns the antenna to 123° for `M1234` and gives no rejection; a client that sends a malformed
long azimuth then gets an ack for a truncated value. Only a digit-less `M` (or a `W` missing
its whitespace+integer elevation part) gets no reply at all, which can hang naive
line-oriented clients that wait for an acknowledgment. Tightening to strict full-line
matching is a behavior change that needs sign-off (see [decision] below).

Code-check verdict: still open — `internal/gs232/server.go`: `^M(\d{1,3})`,
`^W(\d{1,3})\s+\d+`.

## [defect] `moving` detection matches substrings
The rule (`rotat`/`moving`) survives firmware renames, but the bridge misclassifies a future
state string that merely *contains* those substrings (for example `pre-rotating-check`) as
moving.

Code-check verdict: still open by design — `internal/rotor/protocol.go` `IsMoving` uses
`strings.Contains`.

## [defect] Backoff never resets on success
First backoff 2 s, growth factor 1.5×, cap 60 s (sequence: 2 s, 3 s, 4.5 s, 6.75 s, … capped
at 60 s). The backoff resets ONLY on process restart — it does not reset after a successful
connection. Alternating success and long failure drives the retry interval to the 60 s cap
and keeps it there until restart.

Code-check verdict: still open — `cmd/wrc-rotator-bridge/main.go` `wsLoop`/`scaleBackoff`
(`maxBackoff = 60s`, `backoff = 2s`, scaled after each failure only).

## [defect] Reconnect transient flattening
Between a successful re-dial and the first status frame, the device rebuilds the cached state
with only the old azimuth preserved (`moving=false`, empty `rotor_state`, empty `target_az`).
A published snapshot in that window can look "cleaner" than reality for one frame.

Code-check verdict: still open — `internal/rotor/device.go` `setOnline(true, "")` rebuilds
the state keeping only `Az`.

## [defect] Dead config keys (rotint leftovers)
The bridge parses `mqtt.publish_ha_discovery` and `mqtt.discovery_prefix` but wires them to
nothing (embedded home-assistant discovery does not exist in this component). A clean
re-implementation can drop both keys.

Code-check verdict: still open — both fields parsed in `internal/config/config.go`
(`DiscoveryPrefix`, `PublishHADiscovery`).

## [defect] Hard-coded identity strings
`expose.device.name` (`"HF Rotator"`) and `manufacturer` (`"Yaesu"`) are binary constants,
not config (minor). Everything else in `/meta` comes from config.

Code-check verdict: still open — hard-coded in `internal/bridge/bridge.go`.

## [decision] Do the legacy inbound paths stay, and which desktop software uses them
The deployed system turns the GS-232B TCP server (:7373) and the PSTRotator UDP listener
(:12040) on by default. At least one of them exists because desktop rotator software in the
shack drives the rotator through it — **the sources do not record which one**. The legacy
paths are unauthenticated plaintext network listeners (security-relevant surface). Options
were: (a) keep both, byte-compatible; (b) keep one (which?); (c) drop both and go bus-only.
"Which software operators actually use" is still pending confirmation on site — this feeds
the keep/drop decision.

## [decision] `set_az` argument key `az` vs the station-wide `value` convention
The station-wide `/cmd` convention puts every command argument under the generic key `value`
as a string, with one documented exception (the ultrabeam controller's frequency). This
bridge instead uses a field-specific key `az` carrying a JSON *number* — it predates the
convention. The `/meta.expose` descriptor (`value_key:"az"`, `value_type:"float"`) is the
authoritative machine-readable declaration, so well-behaved consumers need no special case.
Open decision: preserve the deployed `az`-key grammar for compatibility with existing
consumers, or migrate to `value` (which can break a consumer that hard-codes `az`). Do not
change it silently.

## [decision] GS-232B line-matching strictness
Keep the lenient prefix matching byte-compatible (`M1234` → rotate to 123°, ack), or tighten
to strict full-line matching (rejecting `M1234` with `?>` or no reply). The latter is a
deliberate behavior change that alters what legacy clients observe on the wire. Do not
change silently.

## [decision] WRC firmware facts unverified (no vendor documentation)
No vendor documentation for the WRC exists in the repo. The reference code and live
observation gave the WebSocket URL (`ws://192.168.1.108/wsrotor`) and the JSON frame
vocabulary above. The exact frame cadence, the meaning of all `state` strings beyond
`rotating`/`stopped`/`idle`, and the `name`/`lim1`/`lim2` semantics are firmware-set and
unverified. The firmware rationale for ignoring numeric `az` values is unknown (only the
quoted-string requirement itself is code-confirmed). Needs on-device confirmation at the WRC.

## [requirement] Normative behavior bounds stated nowhere else
These component-specific normative bounds/behaviors are not captured in the component
CLAUDE.md, the MQTT API doc, or the shared docs:

- **Shutdown latency bound**: a shutdown signal during a hanging first MQTT connect, the
  WebSocket handshake, or a backoff sleep must end the process within **1 second**. Do not
  rely on network timeouts or service-supervisor kill timeouts. Test: issue the shutdown
  signal while the broker address is black-holed (and, separately, during a backoff sleep);
  assert process exit within 1 s.
- **Exit codes are normative**: 0 on SIGTERM/SIGINT, 2 on config errors (missing/malformed
  config, validation failure), 1 on runtime failure (MQTT connect failure at boot is fatal —
  the service supervisor restarts it after 5 s).
- **WRC reconnect backoff**: first backoff 2 s, growth factor 1.5×, cap 60 s; backoff resets
  only on process restart (see the [defect] section for the reset-on-success question).
- **`/cmd` dispatch pipeline**: the MQTT receive-path handler must only enqueue the raw
  payload onto a bounded queue of capacity **8**; a single worker parses and executes
  commands one at a time, strictly FIFO; on overflow the excess command is dropped with a
  warning log. The receive path must NEVER block on device I/O (a blocking handler deadlocks
  the MQTT client library; this failure class has occurred live elsewhere in this station).
- **No desired-state reconciliation**: `/cmd` is never treated as retained state — no
  desired azimuth is stored, and none is re-applied on restart or reconnect. A move is
  one-shot intent. A stale azimuth replayed after a restart can spin the antenna
  unexpectedly.
- **A rotator `/state` document must never carry `freq_hz`, `band`, or `mode`.**
- **Listen failures are non-fatal**: if the GS-232B TCP server or PSTRotator UDP listener
  fails to listen (for example a port already taken), it is logged as an error only — the
  process runs without that path for its whole lifetime; only a restart can retry it.
- **`stop` takes precedence over an embedded azimuth within a single PSTRotator datagram**;
  FIFO arrival at the serialized write path resolves a stop that races a separate set
  command.
- **`/meta` azimuth range (`min:0, max:450, step:1`) is metadata only** — the bridge neither
  clamps nor validates incoming azimuths against it.