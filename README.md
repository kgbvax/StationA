# atr1k-tuner-bridge

A Go service that bridges the **ATR-1000 ATU** (BTR-1000 / N7DDC family) to the
station MQTT bus using the station integration model (slot
`muehle/hf/tuner`). It dials the tuner's binary WebSocket status stream,
publishes a canonical tuner state snapshot (SWR, forward power, L/C relay
state, in-line/bypass, settling, fault), and dispatches `/cmd` intent
(`set_inline`, `tune`) back to the tuner. It is **read-write**.

The ATU sits in the HF TX path; this slot does not drive TX itself — the
`antennaselect` reconciler engages the ATU in-line for non-resonant bands via
the `tuner.set_inline` soft binding (integration model §7.1, §10 residual).

It is part of the Mühle station automation ecosystem; see the meta-repo at
`../stationa/` for the shared integration model and conventions.

## Context — stationa and the adapter model

atr1k-tuner-bridge is one of a family of thin **adapters** in the *stationa*
ecosystem. The ecosystem is organised around a strict, vendor-neutral
**station integration model** (see `../stationa/docs/station-integration-model.md`):
every device is fronted by a disposable adapter that translates the vendor's
proprietary protocol into a canonical, three-plane MQTT contract at a fixed
slot address. The contract is *configuration-as-documentation* — each adapter
publishes a retained `/meta` birth certificate (identity, capabilities, and a
consumer-neutral `expose` block) and a retained `/state` snapshot; consumers
couple to **state**, never to intent, and a standalone `hadiscovery` consumer
renders Home Assistant discovery from `/meta.expose`. The address is
structural — `<site>/<station>/<slot>` — and the device filling a slot is an
attribute of the slot, not part of its name. The canonical vocabulary and slot
template are versioned and precious; the per-vendor adapters are thin and
cheap. This bridge is the ATR-1000 / N7DDC family member; its name follows the
convention `<devtag>-<function>-bridge`.

## Requirements

- ATR-1000 ATU (BTR-1000 / N7DDC family) reachable on the LAN over its binary
  WebSocket (default `ws://192.168.1.111:60001`)
- MQTT broker (e.g. Mosquitto at `192.168.1.50:1883`)
- (Optional) Home Assistant — discovery is rendered by the standalone
  `hadiscovery` consumer from this bridge's `expose` block; the legacy embedded
  discovery is gated off by default (model §9)

## Build & run

```sh
go build ./cmd/atr1k-tuner-bridge                 # local build
./deploy.sh                                       # cross-compile for the Pi and install as a systemd service
```

Cross-compile for the Pi:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o dist/atr1k-tuner-bridge-linux-arm64 ./cmd/atr1k-tuner-bridge
```

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `/etc/atr1k-tuner-bridge/config.toml` | Path to TOML configuration file |
| `-log.level` | (from config) | `debug` \| `info` \| `warn` \| `error`; overrides config |
| `-debug` | `false` | Log ATR-1000 WebSocket I/O (frame hex) |

The service auto-reconnects to the broker and re-dials the ATU WebSocket on
failure (exponential backoff). `/status` stays `online` while the bridge
retries; only `/state.device_online` flips when the ATU is unreachable.

## Configuration

All non-secret settings live in a TOML file (`config.example.toml` is a fully
annotated starting point). The binary works without one — built-in defaults are
used when the file is absent. The MQTT password is **not** in the TOML; it is
loaded from the `ATR1K_TUNER_BRIDGE_MQTT_PASSWORD` environment variable (systemd
`EnvironmentFile`), so it never appears in the unit file or process command
line. Env overrides use the `ATR1K_TUNER_BRIDGE_*` prefix (the dir name
uppercased, hyphens → underscores).

```sh
sudo mkdir -p /etc/atr1k-tuner-bridge
sudo cp config.example.toml /etc/atr1k-tuner-bridge/config.toml
# /etc/atr1k-tuner-bridge/atr1k-tuner-bridge.env  (0600):
#   ATR1K_TUNER_BRIDGE_MQTT_PASSWORD="..."
```

| Key | Default | Description |
|-----|---------|-------------|
| `tuner.url` | `ws://192.168.1.20:60001` | ATR-1000 binary WebSocket endpoint |
| `mqtt.broker` | `tcp://192.168.1.50:1883` | MQTT broker URL |
| `mqtt.site` / `station` / `slot` | `muehle` / `hf` / `tuner` | Station-model slot addressing — see below |
| `mqtt.location` | `bauwagen` | Physical location label (in `/meta`) |
| `mqtt.user` | `hf` | MQTT username |
| `mqtt.password` | _(empty — use EnvironmentFile)_ | MQTT password |
| `mqtt.discovery_prefix` | `homeassistant` | Legacy embedded HA discovery prefix |
| `mqtt.publish_ha_discovery` | `false` | Gate legacy embedded HA discovery (model §9) |
| `device.model` / `link` | `ATR-1000` / `wifi` | Identity published in `/meta` |
| `host` | `shari` | Compute node (in `/meta`) |
| `log.level` | `info` | Log level |

### Slot addressing — configurable per site and station

The slot address is **not** hardcoded. All topics are built from the three
`[mqtt]` fields:

```
<site>/<station>/<slot>/{meta,state,status,cmd}
```

- `mqtt.site` — physical site (default `muehle`)
- `mqtt.station` — transmitting entity (default `hf`)
- `mqtt.slot` — role (default `tuner`)

The defaults give `muehle/hf/tuner`. To place the slot elsewhere, set the
fields in `config.toml`, override via the `ATR1K_TUNER_BRIDGE_MQTT_SITE` /
`_STATION` / `_SLOT` env vars, or seed them via `deploy.sh`'s `MQTT_SITE` /
`MQTT_STATION` / `MQTT_SLOT`. Site and station are **mandatory** (model §2/§8.1)
— the adapter refuses to start if either is empty, so a malformed path can
never be published. The on-device config carries them explicitly so the slot's
location is owned by the device, not the binary.

## MQTT

atr1k-tuner-bridge publishes the station three-plane topics under
`<site>/<station>/<slot>`:

```
muehle/hf/tuner/meta     retained  birth certificate (capabilities + expose)
muehle/hf/tuner/state    retained  live tuner state JSON snapshot
muehle/hf/tuner/status   retained  online | offline (LWT — the bridge, not the ATU)
muehle/hf/tuner/cmd      not retained  set_inline | tune intent
```

`/state` is a single retained JSON document. Canonical fields: `inline`
(bool), `swr` (ratio), `fwd` (watts), `l_uh` (µH), `c_pf` (pF), `settling`
(bool — a tune cycle is in progress), `fault` (string, omitempty),
`device_online`/`error`. A tuner carries **no** `freq_hz`/`band`/`mode` — those
are radio concerns. `/cmd` is **not retained**: a tune is a one-shot command;
re-applying a stale `tune` on restart could re-key the ATU unexpectedly.

### `/cmd`

```json
{"action": "set_inline", "value": true}
{"action": "tune", "value": "full"}
{"action": "tune", "value": "mem"}
```

`set_inline` drives the ATR `TuneStatus` command and is the soft-binding target
the `antennaselect` reconciler drives for non-resonant bands. `tune` drives the
ATR `TuneMode` command; `value` is `mem` (memory recall) or `full` (full search).
While tuning, `/state.settling` is `true`; it clears when the relays update or
on a 12 s timeout (→ `fault: "tune timeout"`). See
`docs/atr1k-tuner-bridge-mqtt-api.md` for the full on-the-wire contract.

### Home Assistant discovery

Discovery is rendered by the standalone `hadiscovery` consumer from this
bridge's `expose` block in `/meta` (preferred, model §9). The legacy embedded
discovery is retained but gated behind `mqtt.publish_ha_discovery = false`
(default off); set it `true` only as a migration fallback.

## Deployment

Target: Raspberry Pi (`shari`, `192.168.1.139`, user `io`).

```sh
./deploy.sh              # cross-compile, ship, install as a hardened systemd service
```

The unit is hardened (network-only): `ProtectSystem=strict`,
`PrivateDevices=true` (no serial devices), `RestrictAddressFamilies=AF_INET
AF_INET6` (covers the outbound ATU WS + MQTT), `MemoryMax=256M`, `TasksMax=64`.
The bridge makes outbound TCP connections only — no udev rule, no
`DeviceAllow`. Config is seed-once: `deploy.sh` seeds the TOML + EnvironmentFile
on first deploy; subsequent deploys leave the on-device files untouched so the
Pi owns its own settings. To change a setting after the first deploy, edit the
file on the device (or delete it and redeploy to re-seed).

## Station model and shared conventions

| Document | Path |
|---|---|
| Station integration model (§7.1 tuner slot) | `../stationa/docs/station-integration-model.md` |
| Config and secrets convention | `../stationa/docs/conventions/config-and-secrets.md` |
| Deployment convention | `../stationa/docs/conventions/deployment.md` |
| Bridge-naming convention (`<devtag>-<function>-bridge`) | `../stationa/docs/conventions/naming.md` |
| Canonical band/mode vocabulary (not used by a tuner) | `../stationa/docs/conventions/band-mode-reference.md` |
| Tuner slot MQTT schema | `docs/atr1k-tuner-bridge-mqtt-api.md` |

## License

GPLv3 — see [LICENSE](LICENSE).