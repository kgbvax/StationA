# CLAUDE.md — logger-spot-bridge

logger-spot-bridge fronts the shack logging software (**DXLog**, **Log4OM**) as
the `muehle/hf/spots` slot (role `bandmap` per the logging-integration draft).
It listens on UDP next to the loggers for the **operator-entered callsign**
broadcasts and publishes the selected station — call, RF context, map
position, beam bearing/distance — as the retained `/state` `selected` record.
hf_console renders it (pin + callsign on both maps, read-out chip on the
compass) so the operator can judge whether to turn the beam.

**What this is not:** a spot feed. The trigger is the entry-window selection
("the station I am about to work"), not the cluster stream. A call typed by
hand and a call picked from a spot list are indistinguishable here — by
design (the user's ask).

## Commands

```bash
go build ./cmd/logger-spot-bridge
go test ./...
go test ./... -race
go vet ./... && gofmt -s -w .
./deploy.sh          # cross-compile windows/amd64 → shack PC (pelcobridge2 pattern)
```

Flags: `-config <path>` (default: `config.toml` next to the exe — the shack PC
is Windows, no `/etc`), `-log.level` (overrides config).

## Facts that are NOT derivable from the code

1. **The signal chain was verified against vendor docs, not guessed** (see
   `../docs/plans/2026-09-15-001-feat-logger-spot-bridge-plan.md`):
   - DXLog broadcasts N1MM-family XML; `lookupinfo` fires on call entry /
     Space / Tab / call-change and carries `call, txfreq, band, mode,
     countryprefix, wpxprefix, azimuth, distance, reason` — **azimuth +
     distance are CTY-centroid values relative to the logging PC's QTH**.
     Enabled under Options|Broadcast; ports under Options|Network configuration.
   - N1MM's `lookupinfo` is contactinfo-shaped (has `gridsquare`, usually
     empty). `Freq`/`txfreq` are **10 Hz units** in the whole family — both
     DXLog's wiki examples and Log4OM's manual agree; every ×10 conversion
     lives in `Resolver.FromN1MM`.
   - Log4OM's outbound service type **CALLSIGN** ("call signs entered into the
     input field … are broadcasted") is documented by *name only*. **PINNED BY
     LIVE CAPTURE 2026-09-15: the datagram IS the bare callsign as plain
     ASCII text ("VU2ATN") — no XML, no frequency, no locator.** Consequence:
     a Log4OM selection carries only `call` (+`source`) on the bus — no pin,
     no bearing — because there is nothing to resolve from. The XML probe in
     `internal/log4om` remains as a robustness fallback.
   - Log4OM has NO N1MM-style spot/lookup UDP out (forum t=9359: requested,
     not implemented); its documented N1MM-shaped output is the RemoteControl
     RADIO STATUS (unsolicited RadioInfo). We ignore it: radio context is
     already on the bus via muehle/hf/radio.
2. **Position resolution order** (internal/bridge `finish`): logger locator →
   coordinates; else logger azimuth+distance → direct problem from
   `station_locator` (config, must equal the loggers' QTH or the pin lands
   wrong); else coordinates → inverse problem fills bearing/distance.
   Logger-provided azimuth wins when both paths fire. (0,0) is used as the
   "no coordinates" sentinel — acceptable for ham QTHs, consistent with
   `omitempty`.
3. **Selection datagrams are events, liveness is state** (deliberate
   asymmetry in `SlotBridge`): `SetSelected` publishes EVERY datagram with a
   fresh top-level ts — re-keying the same call must re-brighten the console
   marker, and rapid CallChanged→SpaceOrTab sequences inside one second must
   not collapse (RFC3339 is second-granular). Only the `SetDeviceOnline`
   path dedups against the last (selection, online) pair. Do not add dedup
   to the selection path.
4. **No heartbeat, on purpose** (runtime-library.md's change-only-publisher
   limit does NOT bite here): `/state` is retained and nothing consumes
   liveness off it; the console ages the marker from `selected.ts` (dim > 5
   min, hide > 15 min). The pa-arm starvation incident was a *command
   liveness* consumer; there is none downstream of this slot.
5. **device_online here is "a logger keyed something recently"** (UDP heard
   within `stale_after`, default 10 min) — NOT bridge liveness (`/status`
   LWT covers that) and NOT "logger process alive" (UDP broadcast is
   connectionless; RadioInfo would be the only periodic proof and we don't
   decode it). False from start until the first datagram.
6. **The shack PC is Windows** (192.168.1.197, host `BWPC`, user `iotte`,
   German cmd.exe behind sshd — backslash paths + `cmd /c`, per
   pelcobridge2's deploy findings). Interactive host: no systemd; the bridge
   runs as the schtasks logon task `logger-spot-bridge` → `start-bridge.cmd`
   next to the exe (holds the MQTT password as env — the Windows env-file
   equivalent; `@echo off` first or cmd echoes the batch line, password and
   all). Bridge stderr lands in `bridge.log`.
7. **DEPLOYED 2026-09-15** — live config: broker `tcp://hassio.kgbvax.net:1883`
   (the HA box at 192.168.1.50, which serves `muehle/#`; the shari mosquitto
   is inactive and the two-broker migration undeployed). `bwbroker` is
   NXDOMAIN on the unifi DNS and its stale record (192.168.0.50) is a dead
   host — the user chose hassio when offered. Logon-task gotcha: schtasks
   runs with CWD = System32, so the wrapper MUST `cd /d %~dp0` before the
   relative exe path.
8. **`lookupinfo` with an empty call = the clear signal** (operator wiped the
   entry window). `contactinfo` (QSO logged) is deliberately NOT a clear —
   operators stay on the call after logging it.
9. **The QRZ gap-fill lives in the bridge, not the console** (user decision,
   2026-09-15: "we don't want to rely on Log4OM during runtime") — Log4OM's
   broadcast is the bare call, so `muehle/hf/spots` resolves the position via
   the QRZ XML API (`internal/qrz`, needs a paid XML-access subscription).
   Bridge-side because: one disk cache serves every console platform, QRZ
   credentials stay off the app (the web build couldn't reach
   xmldata.qrz.com anyway — CORS), and the resolver already owned position
   resolution. Position priority is logger-first (DXLog az+dist is
   station-relative and live; a QRZ grid can be stale): `Resolver.Enrich`
   runs only when the record has no locator/coords/bearing. Lookup runs on
   the jobs worker — the one place a ~2×5 s HTTP stall is acceptable; ctx
   dies with run so shutdown never waits on QRZ. QRZ errors never block
   publication (call+RF-only record, pre-QRZ shape). Session key is
   in-memory (re-login on restart is one request); cache TTL 30 d positive /
   10 min negative, cap 2000, corrupt file = cold start. Not yet deployed —
   enabling needs QRZ username + `LOGGER_SPOT_BRIDGE_QRZ_PASSWORD` in
   `start-bridge.cmd` and `[qrz] enabled = true` in the live config.

## Testing patterns

- Decoder tests feed raw datagram fixtures (real shapes from the vendor docs)
  and assert decoded fields; the 10 Hz unit conversion is asserted at the
  resolver, not the decoder.
- Resolver tests pin the resolution matrix (locator / az+dist / both /
  neither, station set vs unset) and the clear-on-empty-call rule.
- `SlotBridge` tests pin dedup: same call re-keyed republishes; identical
  snapshot doesn't; birth via `LastJSON()` round-trips.
- `qrz` tests run against `httptest` servers with golden XML from the QRZ
  spec (login / lookup / session-timeout / not-found / auth-fail); the
  resolution-matrix additions live at the resolver (Enrich fills/skips/
  passes-through), not the client.
