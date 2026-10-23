# CLAUDE.md — flexbridge

flexbridge is a **read-only** bridge for radio tuning state: it observes a FlexRadio
SmartSDR radio (the station's FLEX-8400; the SmartSDR TCP/IP API is common to the 6000 and
8000 series) over the SmartSDR TCP/IP API, and publishes state to MQTT. The exceptions are
**band changes**, **slice mode**, the **Digital Voice Keyer (DVK)**, and **mic profiles**:

- **Band changes** (`set_band`): the bridge drives SmartSDR's native band-stacking from
  the `/cmd` plane — `display pan s <pan_handle> band=<wavelength>` changes the band on a
  panadapter and the radio restores the last-used frequency/mode for that band. `/state`
  stays frequency-derived: after a band change the bridge republishes the radio's tuned
  `freq_hz` and `band` is still derived from it (the model's band/freq-can't-disagree
  invariant holds).
- **Slice mode** (`set_mode`, `cw`/`usb`/`lsb`): `slice set <idx> mode=<MODE>` on the
  slice `/state` reports (TX, else active). The caller picks the sideband; the bridge only
  maps the name.
- **DVK** (SmartSDR v4+): the bridge drives DVK playback (play/stop) from `/cmd` and
  observes DVK status on `/state`.
- **Mic profiles** (`set_mic_profile`): the bridge drives SmartSDR's **native** mic profiles
  (`profile mic load`) from `/cmd`. The available list is queried once via the one-shot
  `profile mic info` command (in the handshake) and published on `/state.mic_profiles`; the
  active name (`/state.mic_profile`) is tracked client-side as "last loaded" (SmartSDR
  reports no active mic profile). This is deliberately the radio's own profile mechanism, not
  a custom preset layer. There is **no save** — `profile mic save` is obsolete on SmartSDR
  v4+ (malformed reply); profile creation uses a file-transfer mechanism out of scope here.

Apart from band changes, slice mode, DVK, and mic profiles it never sends commands to the radio.

---

## Commands

```bash
make build        # local binary -> bin/flexbridge
make pi           # cross-compile for Raspberry Pi arm64 -> bin/flexbridge-linux-arm64
make test         # unit tests (no network required)
make test-race    # tests with race detector
make vet          # go vet
make fmt          # gofmt -s -w .
make run          # build + run with config.example.toml
```

Run a single test package or test:
```bash
go test ./internal/flexradio/...
go test ./internal/bridge/... -run TestGate
```

---

## Architecture

**Data flows:**

1. **UDP discovery** (`flexradio.Discover`) — broadcast to `255.255.255.255:4992`,
   receives the radio's IP, serial, and model.
2. **TCP connection** (`flexradio.Client`) — port 4992 (SmartSDR TCP/IP API). On connect
   runs a handshake: sends `version`, subscribes to `slice/radio/interlock/atu/pan all`,
   queries `info` (model/serial/firmware), then sends best-effort `sub dvk all` (SmartSDR
   v4+) and `profile mic info` (fire-and-forget so a v3/unlicensed radio rejecting them
   cannot break the handshake). After that, `Client.Run` blocks reading async status lines
   under a probe-based liveness watchdog: after 10 s of silence Run sends an eliciting
   `version` probe and drops the connection if nothing arrives within 5 s. SmartSDR is
   on-change-only — silence alone is never fatal; only silence after an eliciting write is.
   The probe runs on Run's own goroutine (`sendAwaitReply` shares the bufio reader — single
   reader, always).

There is **no UDP meter stream**. `internal/flexradio` still holds a VITA-49 decoder
(`vita49.go`) and a meter registry (`meters.go`) with tests, but nothing wires them: no UDP
listener, and the handshake sends no `client udpport`/`meter list` (dead code, see
`docs/sdr-wire-protocol.md`).

The TCP read loop (`Client.Run`) is the only radio-event feed: it calls
`Bridge.HandleStatus`. A process-lifetime goroutine, `Bridge.StateHeartbeat`, republishes
the `/state` snapshot every 5 s while `device_online` is true; every other `/state` publish
is change-gated.

A further path drives the radio: the paho `/cmd` subscription. Its handler must not call the
bridge inline (a band-change or DVK command is a blocking TCP write), so it funnels payloads
through a bounded channel to a single `sharedmqtt.RunJobs` worker that calls
`Bridge.HandleCommand` serially. `HandleCommand` dispatches `set_band`, `set_mode`, DVK
and `set_mic_profile` intent to the radio through the `Commander` interface
(`*flexradio.Client` implements it, injected per connect cycle via `SetCommander`;
`Reset()` clears it on disconnect). No ack is published — consumers confirm on `/state`
(`freq_hz`/`band`/`mode` for band changes, `mode` for `set_mode`, `dvk_status`/`dvk_id` for
DVK, `mic_profile` for mic profiles), the fire-and-observe plane discipline. Panadapters are tracked from `sub pan all` status so
`set_band` can target a pan handle.

`Bridge` (`internal/bridge/bridge.go`) owns all shared state under `sync.RWMutex`.

**Reconnect loop** (`cmd/flexbridge/main.go:radioLoop`): connects, runs until disconnect, then
exponential-backoffs and retries (2 s → 60 s, ×1.5). The backoff resets to 2 s after any
connection that lived ≥ 1 min — without that it only ever grew, so one long outage doomed
every later reconnect to the full 60 s wait. Calls `Bridge.Reset()` between attempts to clear stale
state and force republish on reconnect.

---

## MQTT topics

flexbridge publishes to the station integration model topics:

```
muehle/hf/radio/meta      retained  birth certificate (capabilities + expose JSON)
muehle/hf/radio/state     retained  live state JSON snapshot
muehle/hf/radio/status    retained  online | offline (LWT)
muehle/hf/radio/cmd       not retained  band / mode / DVK / mic-profile intent (bus → bridge)
```

The `site`, `station`, and `slot` values are configurable via `config.toml`.

**State is a single retained JSON document** (not per-field topics). Fields:
`ts`, `freq_hz` (Hz integer), `band` (derived), `mode` (canonical: `cw`/`usb`/`lsb`/`am`/`fm`/`data`),
`tx` (`rx`/`tx`), `tuning` (bool), `drive` (0–100), `device_online` (radio link liveness),
`dvk_status` (`idle`/`recording`/`preview`/`playback`/`disabled`), `dvk_id` (active DVK memory 1–12), `dvk_memories` (`[{id,name,duration_ms}]`, slot names from SmartSDR),
`mic_profile` (active mic profile name), `mic_profiles` (available mic profile names, sorted).

**flexbridge is read-only except for band changes, slice mode, DVK, and mic profiles.** `/cmd` carries
one-shot intent only (not retained — a stale command must not re-fire on restart):

- `set_band` + `value` (band label, e.g. `"20m"`) — native band-stacking; the radio restores
  the last-used frequency/mode for that band. `/state.band` stays derived from `freq_hz`.
- `set_mode` + `value` (`cw`/`usb`/`lsb` only) — `slice set <idx> mode=<MODE>` on the TX
  slice, else the active one; confirm on `/state.mode`. Not yet confirmed live on the
  FLEX-8400 (130dc0f).
- `dvk_play_<N>` / `dvk_play`+`value` / `dvk_stop` — DVK playback.
- `set_mic_profile` + `value` (profile name) — native `profile mic load`; confirm on
  `/state.mic_profile`.

The mic-profile **list** (`mic_profiles`) is a `/state`-only dynamic field — it is not in
`expose.fields` (the expose schema has no array type). It is populated from the radio's
reply to the one-shot `profile mic info` command (`profile mic list=A^B C^…` status frames;
queried once in the handshake) — not via `sub radio all` (profiles are not broadcast).
SmartSDR does **not** report an active mic profile (mic profiles are load-only presets with
no "current" pointer, unlike global profiles), so `mic_profile` is tracked client-side as
the name most recently loaded via `set_mic_profile` (best-effort); it is empty until the
first load via the bus. The `set_mic_profile` known-name guard drops unknown names only once
the list is populated (an empty list — before
the first `profile mic info` response — does not block).

It is not a general radio-control channel (no `set_freq_hz`/`set_drive`; `set_mode` takes
only `cw`/`usb`/`lsb`, and `mode` is not writable in `expose`).
Panadapters are tracked via `sub pan all`; `set_band` targets the active slice's panadapter,
falling back to the single/lowest tracked pan. If no panadapter is open, `set_band` is a
logged no-op.

See `docs/radio2mqtt-schema.md` for the full on-the-wire contract.

---

## Key packages

| Package | Role |
|---|---|
| `internal/flexradio` | Protocol: discovery, TCP client, frame parser, status parsers, band lookup; VITA-49 decoder + meter registry (unwired dead code) |
| `internal/bridge` | Radio events → MQTT: state tracking, `/state` heartbeat, expose/actions surface, `/cmd` dispatch (band, mode, DVK, mic profile) via the `Commander` interface, discovery payloads |
| `internal/ha` | Home Assistant discovery payload builders and topic helpers |
| `internal/config` | TOML config, flags, `FLEXBRIDGE_*` env overrides |

---

## Configuration and secrets

Config is TOML (`/etc/flexbridge/config.toml` by default, or `-config <path>`). The MQTT
password is **not** in the TOML — it is loaded from an `EnvironmentFile` so it never
appears in the unit file or process command line:

```bash
# /etc/flexbridge/flexbridge.env  (0600, owned by flexbridge user)
FLEXBRIDGE_MQTT_PASSWORD=<password>
```

The systemd unit contains `EnvironmentFile=/etc/flexbridge/flexbridge.env`.

See `../docs/conventions/config-and-secrets.md` for the full convention.

---

## Deployment

Target: Raspberry Pi `scmino` (`192.168.1.178`, user `io`) since 2026-10-12; was `shari`.
The radio is reached over the LAN (TCP 4992, radio at `192.168.1.118`), so the bridge is
host-agnostic. Never run two instances: they share the MQTT client_id and kick each other off.

```bash
./deploy.sh              # cross-compile, ship, install as a hardened systemd service
```

See `../docs/conventions/deployment.md` for the general pattern and systemd
hardening requirements.

---

## Testing notes

Tests in `internal/flexradio/status_real_test.go` capture real FLEX-8400 firmware output
formats observed in production. These are regression guards for parser mismatches that
broke the live deployment — keep them faithful to actual firmware output.

---

## Station model and shared conventions

Shared documentation lives in `../docs/` (this component is a subdirectory of the stationa monorepo).

| Document | Path |
|---|---|
| Station integration model (three-plane MQTT contract) | `../docs/station-integration-model.md` |
| Config and secrets convention | `../docs/conventions/config-and-secrets.md` |
| Deployment convention | `../docs/conventions/deployment.md` |
| Canonical band/mode vocabulary | `../docs/conventions/band-mode-reference.md` |

This project must conform to those conventions. Component-specific schema lives in
`docs/radio2mqtt-schema.md` in this repo.
