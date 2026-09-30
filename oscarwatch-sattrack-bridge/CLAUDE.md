# CLAUDE.md — oscarwatch-sattrack-bridge

oscarwatch-sattrack-bridge fronts **OscarWatch** (the satellite tracker on the shack
PC, BWPC) as the `muehle/uhf/sat-track` slot (role `sat-track`). It dials OscarWatch's
"Satellite link" WebSocket (`ws://192.168.1.197:7373/`) and publishes the focused
satellite as one retained `/state` snapshot. The snapshot holds the name, NORAD ID,
transponder label, look angle (az/el), slant range, range rate, sunlit flag,
radio-corrected uplink/downlink frequencies and canonical modes. It also holds the
**sub-satellite point** (lat/lng/alt), which the bridge derives from the look angle and
the station QTH. Read-only: no `/cmd`.

The slot is the sat-ops analogue of `muehle/hf/spots`. The planned consumers are:
- the hf_console map (a pin and footprint from `sub_lat`/`sub_lng`/`alt_km`)
- the vhfcam-restream overlay, and pass-triggered recording on the `in_range` edge

Neither consumer exists yet.

## Commands

```bash
go build ./cmd/oscarwatch-sattrack-bridge
go test ./... -race
go vet ./... && gofmt -s -w .
./deploy.sh                       # cross-compile linux/arm64 → scmino, hardened systemd unit
MQTT_PASSWORD=... ./deploy.sh     # first deploy (seeds the env file once)
```

Flags:
- `-config <path>`: default is `config.toml` next to the exe; the unit passes
  `/etc/oscarwatch-sattrack-bridge/config.toml` explicitly.
- `-log.level`: overrides the config.
- `-check`: prints the effective config redacted, checks the password source, resolves
  the broker and OscarWatch hosts, then exits. deploy.sh runs it before every restart.

## Layout

- `internal/oscarwatch`: a pure decoder for Satellite-link v1 frames.
  - `PeekHeader` / `Classify` route a frame; `DecodeStatus` decodes it.
  - It tolerates unknown fields, and a `noradId` sent as a number.
- `internal/geo`: WGS84 math.
  - `SubPoint` (look angle → geodetic) and `LookAngles` (its inverse).
  - A Maidenhead decoder copied from logger-spot-bridge; `internal/` can't be imported
    across modules.
- `internal/bridge`:
  - `Build(Input, observer, now) State`: the entire mapping, including canonical-mode
    normalization and nulls.
  - `SlotBridge`: dedup, with `ts` excluded from the key.
  - `Meta` with the `expose` block.
- `internal/source`: the gorilla WebSocket client.
  - `Run` dials, reads until failure, and returns the error.
  - A stop-channel ctx watcher, so no goroutine leaks per reconnect.
  - Ping keepalive; 64 KiB read limit.
- `cmd/.../main.go`: the wiring.
  - `wsLoop`: reconnect with backoff 2 s ×1.5 → 60 s, reset after every successful
    connect.
  - The state worker; paho with LWT; the birth republish; `offline` published on
    clean stop.

## Facts that are NOT derivable from the code

1. **Latest-wins, not a job queue.** The reader goroutine replaces a
   `bridge.Input{Online, Err, Status}` under a mutex and wakes the worker through a
   cap-1 channel. The worker builds the snapshot from the newest input.
   `shared/mqtt.Enqueue` drops jobs when its queue is full. At 1 Hz, with a 10 s
   publish wait during a broker stall, it would drop frames, and could drop the
   link-down transition itself. Coalescing makes a stale backlog impossible.
   `TestLatestWins` pins this.
2. **`device_online` = the WebSocket is open.** It is not "a satellite is up". On link
   loss the bridge publishes `device_online:false`, `error`, **and clears every
   satellite key to null**. A frozen position on a map is worse than none. On connect,
   OscarWatch re-sends its current snapshot.
3. **Null, not omitted.** Satellite keys are pointers without `omitempty`:
   - `null` whenever nothing is tracked.
   - HA's MQTT sensor treats a rendered `None` as *unknown*, where a missing key would
     log template errors every second.
   - Consumers get a stable key set.
   - `error` is the one `omitempty` key, because it only exists while offline.
4. **Keepalive is a ping, not a data-silence deadline.** OscarWatch documents
   change-driven sends. Live on 2026-09-30 it also re-sent the idle
   `** NO SATELLITE **` frame about once a second, but that is undocumented and could
   change. It answers WebSocket pings (verified live, 2 s ping / 5 s pong-wait over
   25 s). Default ping 30 s, read deadline 2.5× that, extended by every pong and every
   frame; `ping_interval = "0s"` disables it. Dedup keeps the idle resends off the bus.
5. **The sub-point needs the right QTH.**
   - The station position must be the QTH OscarWatch computes its look angles from.
   - The locator `JO32WE` cell centre (52.1875 N, 7.875 E) is within ~3 km. Explicit
     `[station] lat/lon/alt_m` beat the locator.
   - Rounding: az/el to 0.1°, range to 0.1 km, range rate to 0.001 km/s, sub-point to
     0.001°. The sub-point is computed from the unrounded source values.
   - A 0.1° look-angle error at 2000 km slant range moves the sub-point about 3.5 km.
6. **Modes are canonical or null.**
   - `uplink_mode`/`downlink_mode` go through `CanonicalMode` (band-mode-reference: never
     publish raw mode strings).
   - A bare `SSB` has no sideband and so stays null.
   - `mode_type` is OscarWatch's transponder *label* (`FM VOICE`), not a mode, and passes
     through verbatim.
7. **QSO frames are ignored in v1** (`qsoLogged/Updated/Deleted`, logged at Debug). A
   future event topic would be the place for them.
8. **Port 7373 is used on two different hosts.** OscarWatch's default Satellite-link port
   on BWPC is 7373. wrc-rotator-bridge's GS-232 server also listens on `:7373`, on shari.
   They don't conflict, because this bridge is a client and the two are different hosts.
   Keep the two apart in docs and configs.
9. **Deployment target is scmino** (192.168.1.178, Debian 12, aarch64 — same arm64
   build as shari), which is replacing shari. The broker is the live hassio at
   `tcp://192.168.1.50:1883` (user `hf`).
   - `/etc/oscarwatch-sattrack-bridge/{config.toml,oscarwatch-sattrack-bridge.env}` are
     seed-once host state.
   - The unit is network-only and has no writable paths.
   - OscarWatch side, one-time operator setup: Satellite link enabled, "Allow
     connections from local network" on, Windows firewall allowed.

## Testing patterns

- The decoder tests use the vendor-doc frames: in range, no satellite, qso, version 2
  with extra keys, missing tracking keys, malformed input.
- The geo tests check two things:
  - Independent closed forms: due-east horizon target from (0,0), and the geostationary
    elevation from 51.5 N.
  - A LookAngles→SubPoint round trip for LEO through GEO geometries.
- `internal/bridge` tests pin:
  - the null-set on every "nothing to show" path
  - mode normalization
  - dedup ignoring `ts`
  - that expose keys ⊆ state keys
- `internal/source` tests run an `httptest` gorilla server. They cover snapshot-on-
  connect, server close, dial failure, pongs keeping a silent link alive, a missing pong
  dropping it, oversize frames, and goroutine leaks across 20 reconnects.
- `main_test.go` drives the real `wsLoop` and worker against a fake OscarWatch through
  track → clear → link down. It also covers birth-republish-verbatim and latest-wins.
