# feat(logger-spot-bridge): QRZ callsign→position gap-fill

**Date:** 2026-09-15 · **Status:** implemented on branch `feat/logger-spot-bridge`

## Ask

A Log4OM selection cannot be drawn: the outbound CALLSIGN datagram is the bare
callsign (pinned by live capture, see the 001 plan), so `selected` carries no
position and the console maps stay empty. The user ruled out relying on the
logger for location at runtime — the call shall be resolved via the **QRZ.com
XML API** instead (paid subscription available). The lookup was the deliberate
"out of scope" line of the 001 plan; this pass adds it.

## Signal research (QRZ XML API — official spec)

- Endpoint `https://xmldata.qrz.com/xml/current/`; login `?username=…;password=…`
  → `<Session><Key>`; lookups `?s=KEY;callsign=CALL` (`;`-separated per spec).
- Expired sessions answer `<Error>session timeout</Error>` → re-login once,
  retry once. Other stable fragments: `not found`, `username/password`,
  `not subscribed`.
- Fields used: `call`, `grid`, `lat`, `lon`, `country`, `addr2` (city).
  Sources: https://www.qrz.com/docs/xml/current_spec.html ,
  https://www.qrz.com/XML/specifications.1.9.html

## Design

**Where: the bridge, not the console** (user-confirmed). One disk cache serves
every console platform; QRZ credentials never reach the app (the web build
couldn't call xmldata.qrz.com anyway — CORS); `Resolver` already owns position
resolution. Contract additions are optional fields the console tolerates today:
`country`, `qth` (+ `locator` reused for the QRZ grid).

**Priority: logger position wins.** `Resolver.Enrich` runs only when the fresh
selection has no locator/coords/bearing (the Log4OM path, and N1MM datagrams
without a grid). DXLog's azimuth+distance is station-relative and live; a QRZ
grid can be stale. On a hit, `finish` recomputes the beam answer from
`station_locator` exactly as for logger-provided coordinates.

**Failure posture: publish anyway.** The lookup runs on the jobs worker with a
3×5 s ctx (login + query + slack; dies with run). Timeout / not-found / auth
leave the record call+RF-only — the pre-QRZ shape — so the console never waits
on QRZ and nothing regress when the API is down. ErrNotFound is negative-cached
(10 min) so a typo doesn't re-hit the API per keystroke; ErrAuth warns loudly
but is never cached (fixing credentials must take effect immediately).

**Cache:** JSON file next to the config (`qrz-cache.json`, 0600, tmp+rename),
30 d positive TTL / 10 min negative, 2000 entries drop-oldest, corrupt file =
cold start. Session key is in-memory only.

**Config** (`[qrz]`): `enabled` (default false — fail fast on missing
credentials at load), `username` (TOML), password **env-only**
(`LOGGER_SPOT_BRIDGE_QRZ_PASSWORD`, seed-once convention), `cache_path`,
`cache_days` (30), `negative_minutes` (10). `/meta.capabilities` gains
`"lookup": "qrz"` when enabled.

## Out of scope (deliberate)

- Console rendering of `country`/`qth` (chip keeps `CALL · az° · dist km`) —
  the fields are on the wire for a trivial console follow-up.
- App-side lookup: web CORS blocks xmldata.qrz.com; per-device caches and
  credentials multiply.
- QRZ session persistence across restarts (re-login is one request).
- WSJT-X `…/log/event` ingest — separate feature per the logging draft.

## Verification

- `go build ./... && go test -race ./...` in `logger-spot-bridge/`: qrz golden
  XML fixtures (success / session-timeout→relogin / not-found / auth-fail),
  cache TTL+cap+persist+corrupt-file, resolver Enrich matrix (fills / skips
  when logger placed / skips disabled / error passthrough / no-station),
  config (defaults, env creds, cache-path default, TOML-password rejection).
- Workspace build from stationa root (`go.work`).
- Live bring-up (operator): add QRZ username + `LOGGER_SPOT_BRIDGE_QRZ_PASSWORD`
  to `start-bridge.cmd`, set `[qrz] enabled = true`, redeploy, key a call in
  Log4OM → `mosquitto_sub -t 'muehle/hf/spots/state' -v` shows lat/lng;
  console compass/map place the pin.
