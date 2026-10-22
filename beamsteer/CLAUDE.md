# CLAUDE.md — beamsteer

HF smart-rotation logic slot `muehle/hf/beam-steer`. It emulates PstRotator's UDP interface so the contest logger can drive it.

For each rotate request it picks the cheapest way to put an Ultrabeam lobe on the station:
- **Station behind the beam:** flip the Ultrabeam 180°.
- **Station outside both lobes:** rotate to whichever lobe needs the least rotator travel.

When the toggle is off, requests pass straight through to the rotator. The operator toggle lives in the hf_console Ultrabeam panel (`AUTO` — not the Ant switch row AUTO).

API: `docs/beam-steer-mqtt-api.md`.

## Layout

| Path | What |
|---|---|
| `internal/steer` | Pure decision (`Decide`, `EffectiveHeading`). No I/O; table tests are the spec. |
| `internal/engine` | Runtime state, guards (liveness, TX / moving hold, 6m, antenna-mode lock), command emission, `/state` `/meta`. Single-threaded on the jobs worker; only `Readback` and the recorded ant-mode state (`AntModeState`) are locked. |
| `internal/mqtt` | Paho wiring + the PstRotator handler adapter. Every input is `Enqueue`d; no handler publishes. |
| `internal/config` | TOML config (`config.example.toml`). |
| `../shared/pstrotator` | Datagram grammar + UDP loop (shared). |

## Rules

- **Two-layer liveness:** a sibling is live only when `/status` is `online` **and** `/state.device_online` is not false.
- **Hold direction changes:** never send one while ant-ctrl is `moving` or the radio transmits. Hold it as `pending` and replace it with the next request.
- **6m is forward only.**
- **Travel is linear:** the G-450 cannot cross its stop. `max_az` stays 360 until the overlap readback is verified on the hardware.
- **Own `/cmd` is subscribed at QoS 0:** the retained value carries the steady state.
- **Antenna-mode lock (`hf/ant-mode`, `steer.ant_mode_slot`):** read only through `shared/antmode`. While storm/maintenance: `Goto`/`aim` ignored with a reason (toggle on or off), held flips dropped, nothing published to `hf/ant-ctrl` or `hf/rotator`, logger STOP **not** forwarded (must not cancel the park); `AZ?` readback keeps working; `/state.lock` = mode name. Unlock never replays. No retained state = normal; malformed = keep last known + Error; 3 s hold after **every** connect (OnConnect calls `AntModeConnecting` before subscribing, so only this connection's replay ends it; on lapse the last known lock stands); one Warn per refusal reason. The paho handler only records under `amu`; every motion path calls `syncAntMode` first, so a dropped job cannot hide a lock.

## Build / test / deploy

```bash
go test ./...
./deploy.sh        # (host: scmino since 2026-10-12; PstRotator UDP :12050 moved with it) seed-once config at /etc/beamsteer/config.toml, systemd unit "beamsteer"
```
