# feat(logger-spot-bridge): operator's selected station on the console maps

**Date:** 2026-09-15 · **Status:** implemented on branch `feat/logger-spot-bridge`

## Ask

When the operator selects/keys a station in the shack logging software (the station
the radio is then tuned to), the tablet console shall show that station's location
on its maps — callsign, bearing, distance — so the operator can judge whether
turning the beam is worth it, and where to. Must support **Log4OM** and **DXLog**.
Terminology note (user): this is *not* about spots — a call entered manually must
work the same way. "Selected station" is the model; the `spots` slot name is
historical from the logging-integration draft (§3 `bandmap` role at `…/spots`).

## Signal research (verified against vendor docs)

| Logger | Trigger | Message | Location data |
|---|---|---|---|
| DXLog | call entered / Space / Tab / call changed | N1MM-style XML UDP **`lookupinfo`**: `logger, contestname, mycall, band, txfreq, operator, mode, call, countryprefix, wpxprefix, azimuth, distance, stationid, stationtype, period, reason` | `azimuth` + `distance` (from DXLog's CTY db, relative to the logging PC's QTH = shack); **no lat/lng, no grid** |
| N1MM+ (same family) | call + spacebar | `lookupinfo` (= contactinfo field set): includes `gridsquare, qth, countryprefix, continent, zone` | grid when a lookup filled it |
| Log4OM | call entered in main/keyer/contest input | `[OUTBOUND] CALLSIGN` service type — **packet format undocumented** in the basic + advanced guides | unknown; tolerant decoder ships, finalize after a packet capture |

Frequency units: N1MM-family `Freq`/`txfreq` are **10 Hz units** (×10 → Hz; both
DXLog examples `352376` = 3523.76 kHz and Log4OM's manual "tens digit" note agree).

Sources: N1MM+ external-UDP appendix (n1mmwp.hamdocs.com), DXLog wiki
(Additional_Information), Log4OM basic + advanced guides (log4om.com), Log4OM
forum t=9359 (RadioInfo request history).

## Design

**Bridge `logger-spot-bridge`** (new Go module, runs on the **Windows shack PC**
next to the loggers — pelcobridge2 deploy pattern: interactive host, no systemd,
seed-once `config.toml` next to the exe):

- One UDP listener per `[[listener]]` (default one on `:12060`, the N1MM default);
  `kind = "n1mm" | "log4om"` selects the decoder. Unknown XML roots logged at debug.
- `lookupinfo` → resolve into the canonical **selected-station** record:
  - `freq_hz` = txfreq × 10; `band` from the canonical band table (band-mode
    reference); `mode` canonicalized (CW/USB/LSB/AM/FM/*→data).
  - Coordinates: locator (when present, N1MM) → center; else DXLog
    azimuth+distance → great-circle **direct problem** from the configured
    `station_locator`. Console-computed azimuth/distance when only coords exist
    (inverse problem) so the record carries bearing+distance whenever derivable.
  - Empty call → `selected` cleared (operator wiped the entry field).
- Publishes slot `muehle/hf/spots` (site/station/slot = `muehle/hf/spots`):
  - `/meta` retained — role `bandmap`, capabilities `{source, actions:[selected]}`.
  - `/state` retained — `{ts, device_online, selected:{call, freq_hz, band, mode,
    locator, lat, lng, azimuth, distance_km, country_prefix, wpx_prefix, source, ts}}`.
    Change-dedup; republished on every reconnect (REQ-RT-5). No heartbeat by
    design: retained snapshot + console-side ts-age dimming cover consumers;
    nothing consumes liveness off it.
  - `/status` retained online/offline + LWT.
  - `device_online` = "UDP heard from a logger within `stale_after`" (default
    10 min) — the two-layer liveness model; false until the first packet.
- shared/mqtt discipline: ctx-aware connect, handlers (UDP readers) enqueue onto
  one bounded jobs channel + single worker (REQ-RT-1..3), no /cmd (read-only).
- Logging per docs/conventions/logging.md (`component` attr, Warn/Error real).

**Console** (`hf_console`):

- `lib/store/selected_spot.dart` — parse `selected` out of the
  `muehle/hf/spots` `/state` snapshot (BusStore already ingests `muehle/#`).
- Compass painter: pin at the AEQD point when lat/lng known (same projection as
  spots), else a bearing ray at `azimuth`; callsign label; age-dim (>5 min) and
  hide (>15 min) via a 30 s repaint tick while a selection is live.
- Mercator painter: amber pin + callsign label after the QTH marker.
- Read-out chip bottom-left of the compass card: `CALL · az° · dist km`.
- Display-only by design: the turn-the-beam decision stays with the operator
  (existing tap-to-aim + presets remain the actuators).

## Out of scope (deliberate)

- `…/spots/event` stream (DXLog `spot` packets) — no consumer yet; the console's
  spot layer remains horstreporter. Decoder surface (n1mm package) leaves room.
- Log4OM RadioInfo → radio-context publishing (console already reads
  `muehle/hf/radio` from flexbridge).
- Callsign lookup / QRZ resolution — location is CTY-level (DXLog) or grid-level
  (N1MM/Log4OM-locator), which is beam-assessment precision, not pileup precision.

## Verification

- `logger-spot-bridge`: `go build ./... && go test ./...` (decoder fixtures for
  DXLog lookupinfo + N1MM lookupinfo + unknown-root tolerance; geo direct/inverse
  round-trips; band/mode tables; state dedup).
- `hf_console`: `tool/prebuild.sh` (analyze + tests, incl. new selected-spot
  parse + painter tests via TestHarness).
- Live bring-up (operator): enable DXLog `Options|Broadcast` lookup/Radio
  broadcast → UDP 12060 on the shack PC, run bridge, watch
  `mosquitto_sub -t 'muehle/hf/spots/#' -v`; Log4OM: add `[OUTBOUND] CALLSIGN`
  listener → capture one packet to finalize that decoder.
