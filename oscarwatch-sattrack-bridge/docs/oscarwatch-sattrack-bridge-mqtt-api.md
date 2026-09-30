# MQTT schema — oscarwatch-sattrack-bridge

This document describes the MQTT interface exposed by **oscarwatch-sattrack-bridge**
(the satellite-tracking source bridge). It implements the `muehle/uhf/sat-track` slot
(role `sat-track`) of the station integration model
(`../../docs/station-integration-model.md`).

It is the authoritative on-the-wire contract — derived from `internal/bridge/bridge.go`
and `cmd/oscarwatch-sattrack-bridge/main.go`.

The slot answers "which satellite is the station tracking, and where is it": the
satellite OscarWatch has focused, its transponder, the live look angle from the
station, and the sub-satellite point. It is the sat-ops analogue of `muehle/hf/spots`.
It is **read-only** (no `/cmd`).

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1 (plain TCP, `tcp://192.168.1.50:1883` — the live hassio broker) |
| Authentication | username/password (`hf` account; password from `OSCARWATCH_SATTRACK_BRIDGE_MQTT_PASSWORD`) |
| Clean session | No |
| Auto-reconnect | yes; a failed *initial* connect exits non-zero and systemd restarts the unit |
| Client ID | `<site>-<station>-<slot>` = `muehle-uhf-sat-track` (configurable via `mqtt.client_id`) |

---

## 2. Topic addressing

```
muehle/uhf/sat-track/meta
muehle/uhf/sat-track/state
muehle/uhf/sat-track/status
```

| Suffix | Retained | Direction | Purpose |
|--------|----------|-----------|---------|
| `/meta` | yes | bridge → bus | birth certificate: identity, capabilities, `expose` |
| `/state` | yes | bridge → bus | tracked satellite (JSON snapshot) |
| `/status` | yes | broker LWT | bridge liveness: `online` / `offline` |

Site/station/slot come from `config.toml` (`[mqtt] site`, `[slot] station/slot`).

---

## 3. `/status` — liveness

Plain string, retained, QoS 1.

| Value | When |
|-------|------|
| `online` | published on every (re)connect |
| `offline` | broker Last Will on unclean disconnect; published by the bridge on clean shutdown |

`/status` is the **bridge** process. Whether OscarWatch itself is reachable is
`/state.device_online` — consumers AND both (two-layer liveness, model §3).

---

## 4. `/meta` — birth certificate

Retained JSON, published on every connect.

```json
{
  "schema": "1.0",
  "role": "sat-track",
  "device": { "name": "OscarWatch Satellite link", "model": "OscarWatch" },
  "link": "websocket",
  "location": "bauwagen",
  "host": "scmino",
  "capabilities": { "source": "oscarwatch", "protocol": 1 },
  "expose": {
    "device": { "name": "sat-track", "model": "OscarWatch" },
    "fields": [
      { "key": "sat_name", "name": "Satellite", "type": "string" },
      { "key": "az", "name": "Azimuth", "type": "number", "unit": "°", "state_class": "measurement" },
      "…one entry per /state key listed in §5 except the band/beacon keys…"
    ]
  }
}
```

| Field | Notes |
|-------|-------|
| `role` | `sat-track` — tracker-agnostic; a later gpredict/SatPC32 adapter publishes the same role |
| `capabilities.source` | the tracker application fronted (`oscarwatch`) |
| `capabilities.protocol` | the OscarWatch Satellite-link protocol version decoded (1) |
| `expose` | read-only fields only (no `writable`, no `actions`); `hadiscovery` renders HA sensors from it |

---

## 5. `/state` — tracked satellite

Retained JSON snapshot, QoS 1. Flat keys (HA `value_json.<key>`, testui and the vhfcam
overlay read them directly).

```json
{
  "ts": "2026-09-30T12:00:00Z",
  "device_online": true,
  "tracking": true,
  "in_range": true,
  "sat_name": "SO-50",
  "norad_id": "27607",
  "mode_type": "FM VOICE",
  "az": 91.7,
  "el": 1.9,
  "range_km": 2100.5,
  "range_rate_km_s": -4.92,
  "sunlit": true,
  "sub_lat": 48.212,
  "sub_lng": 35.496,
  "alt_km": 402.2,
  "uplink_hz": 435300000,
  "downlink_hz": 145850000,
  "uplink_mode": "fm",
  "downlink_mode": "fm",
  "uplink_band": "70cm",
  "downlink_band": "2m",
  "beacon_only": false,
  "doppler": "full"
}
```

| Field | Type | Unit | Notes |
|-------|------|------|-------|
| `ts` | string | — | RFC 3339 UTC time of this publish |
| `device_online` | bool | — | the WebSocket to OscarWatch is open |
| `tracking` | bool | — | OscarWatch has a satellite focused (with its "only broadcast when above horizon" option on: focused **and** above the horizon) |
| `in_range` | bool | — | OscarWatch's `inRange` |
| `sat_name` | string\|null | — | OscarWatch/LoTW name (= ADIF `SAT_NAME`) |
| `norad_id` | string\|null | — | NORAD catalogue number, kept a string (Alpha-5) |
| `mode_type` | string\|null | — | transponder label as OscarWatch shows it (`FM VOICE`, `SSB/CW`…) — not a canonical mode |
| `az` / `el` | number\|null | ° | look angle from the OscarWatch QTH (0.1°); `el` < 0 = below the horizon |
| `range_km` | number\|null | km | slant range (0.1 km) |
| `range_rate_km_s` | number\|null | km/s | negative = approaching |
| `sunlit` | bool\|null | — | satellite in sunlight |
| `sub_lat` / `sub_lng` | number\|null | ° | sub-satellite point (0.001°), derived by the bridge — §7 |
| `alt_km` | number\|null | km | satellite height above the WGS84 ellipsoid (0.1 km), derived |
| `uplink_hz` / `downlink_hz` | int\|null | Hz | radio-corrected (Doppler-applied) frequencies OscarWatch drives CAT with; null when OscarWatch sends 0 |
| `uplink_mode` / `downlink_mode` | string\|null | — | canonical mode (`cw usb lsb am fm data`); null when OscarWatch's mode has no canonical equivalent (e.g. a bare `SSB`) |
| `uplink_band` / `downlink_band` | string\|null | — | ADIF band labels (`70cm`, `2m`, `23cm`) |
| `beacon_only` | bool\|null | — | the selected transponder is beacon-only |
| `doppler` | string\|null | — | `full`, `downlink_only`, `uplink_only` (unknown future values pass through) |
| `error` | string | — | present only while `device_online` is false: why the link is down |

**Null, not omitted.** With nothing tracked, or the feed down, every satellite key is
JSON `null`. Consumers see a stable key set; Home Assistant shows *unknown* instead of
logging template errors.

**Feed down clears the satellite.** On a WebSocket failure the bridge publishes
`device_online:false`, `error`, and nulls — a frozen position on a map is worse than
none. OscarWatch re-sends its snapshot on reconnect.

Publish triggers:
- any change in the derived snapshot (the dedup key is the snapshot without `ts`) —
  during a pass that is ~1 Hz (OscarWatch's update interval); OscarWatch also re-sends
  identical frames about once a second while idle, which publish nothing
- link up / link down
- MQTT (re)connect: the last snapshot is republished verbatim

---

## 6. `/cmd`

None — the slot is read-only.

---

## 7. Sub-satellite point

The bridge derives `sub_lat`/`sub_lng`/`alt_km` from the look angle and the station
position — no orbital elements (TLE) needed. The station position is geodetic → WGS84 ECEF.
The local East-North-Up vector (`e = ρ·cosEl·sinAz`, `n = ρ·cosEl·cosAz`,
`u = ρ·sinEl`) is rotated into ECEF and added, and the result converted back to geodetic
(`internal/geo`).

The station position comes from `[station] lat/lon/alt_m`, else the locator's cell centre
(`JO32WE` → 52.1875 N, 7.875 E; a 6-character cell is ≤ ~3 km from the true QTH — fine
for a map pin). It must be the QTH OscarWatch computes its look angles from. With neither
configured, the three keys stay null.

---

## 8. Home Assistant discovery

Via the standalone `hadiscovery` consumer: it reads `expose` from `/meta` and renders one
HA device (`muehle-uhf-sat-track`) with read-only sensors / binary sensors. The bridge
itself contains no HA knowledge. Classes are set only where an HA device class fits the
unit (`km` → `distance`; `Hz` → `frequency` via the unit fallback).

---

## 9. Upstream: OscarWatch Satellite link v1

OscarWatch (Settings → Integrations → Satellite link) runs a WebSocket server that
replaced its SatPC32 DDE interface. The operator must enable:

- **Enable Satellite link WebSocket server**; **Port** 7373 (default)
- **Allow connections from local network** — otherwise it binds 127.0.0.1 only; on
  Windows allow the firewall prompt (private networks)
- *Only broadcast when satellite is above horizon* — optional: when on, a focused
  satellite below 0° el is reported as `** NO SATELLITE **` (`tracking:false` here)

Protocol facts the bridge relies on:

- `ws://<pc>:<port>/`, no authentication (LAN only). On connect the server sends the
  latest snapshot, then pushes updates.
- Frames are JSON text with `type` + `version` (1). `satelliteStatus` carries
  `inRange`, `satellite{name,noradId,modeType}` (null when none), `frequencies{uplinkHz,
  downlinkHz,uplinkMode,downlinkMode,nominal*KHz,isBeaconOnly}`, `bands{tx,rx}`,
  `tracking{azimuthDeg,elevationDeg,rangeKm,rangeRateKmPerSec,isSunlit}`,
  `dopplerStrategy` (`full|downlinkOnly|uplinkOnly`) and the legacy `wispDde` string.
- `qsoLogged` / `qsoUpdated` / `qsoDeleted` (logbook mirror with an ADIF record) share
  the socket. The bridge ignores them in v1.
- Clients must ignore unknown fields. Observed live 2026-09-30: a `"signature"` key
  (`"empty"` with no satellite) and a ~1 Hz resend of the idle frame.
- The server answers WebSocket pings (verified live 2026-09-30).
