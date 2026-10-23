# flexbridge

A small Go service that **observes** a FlexRadio SmartSDR radio (the SmartSDR TCP/IP API
is common to the 6000 and 8000 series; deployed against the station's FLEX-8400) and
publishes its state to MQTT as the `radio` slot of the station integration model
(`muehle/hf/radio`). Home Assistant discovery is rendered by the separate `hadiscovery`
service from this bridge's `/meta` `expose` block. A narrow `/cmd` surface drives band
changes, slice mode (`cw`/`usb`/`lsb`), DVK playback and mic-profile loads.

It is designed to run on a Raspberry Pi as a hardened systemd service.

## How it works (no polling)

flexbridge never polls the radio on a timer. The FlexRadio streams status changes; we
just listen:

1. **Discovery** — UDP broadcast to `255.255.255.255:4992`; the radio replies
   with its model, serial, and IP. (Optional; you can hardcode the IP.)
2. **TCP handshake** — connect to `<radio>:4992`, send `version`,
   `sub slice/radio/interlock/atu/pan all` and `info`, then best-effort `sub dvk all`
   and `profile mic info`.
3. **Observe** — one long-lived TCP listener: the radio pushes status (frequency, mode,
   PTT, ATU, drive, DVK, mic-profile list) as it changes. A `version` probe after 10 s of
   silence detects a dead link. There is no UDP meter stream (the VITA-49/meter code in
   `internal/flexradio` is not wired).

On disconnect (radio reboot, network blip) it reconnects with backoff and
re-publishes retained state.

## What gets published

One retained JSON snapshot on `/state` (not per-field topics): `freq_hz` (Hz integer),
`band` (derived from `freq_hz`), canonical `mode`, `tx`, `tuning`, `drive`,
`device_online`, DVK status/memories and mic-profile fields. It is published on change
and every 5 s while the radio link is up. `/meta` carries identity, capabilities and the
consumer-neutral `expose` block. The full contract, including the `/cmd` actions, is
`docs/radio2mqtt-schema.md`.

## MQTT topics

```
muehle/hf/radio/meta      # retained birth certificate (capabilities + expose)
muehle/hf/radio/state     # retained JSON snapshot
muehle/hf/radio/status    # bridge LWT: online/offline
muehle/hf/radio/cmd       # not retained: set_band, set_mode, dvk_*, set_mic_profile
homeassistant/<component>/flexradio-<serial>/<object_id>/config   # legacy embedded discovery, off by default
```

## Build

Requires Go 1.26.5+ (the `go` directive in `go.mod`).

```bash
make build       # local binary
make pi          # cross-compile for Raspberry Pi 3B+/4/5 (arm64)
make test        # unit tests (no network)
```

## Install on the Pi

```bash
./deploy.sh                 # build + ship + install as a hardened systemd service
                            # (generates the unit inline, seeds config once)
ssh io@192.168.1.178 'journalctl -u flexbridge -f'
```

`deploy.sh` cross-compiles for arm64, copies the binary + generated systemd
unit + seed config to `scmino` (192.168.1.178), and installs the service as a
dedicated `flexbridge` user with a hardened unit (`ProtectSystem=strict`, no
capabilities). The binary uses only high ports (>1024), so it runs fully
unprivileged. Set the MQTT password on the device in
`/etc/flexbridge/flexbridge.env` (the unit's `EnvironmentFile=`).

## Configure

`/etc/flexbridge/config.toml` (see `config.example.toml`):

```toml
radio_host = ""            # empty = UDP autodiscovery (deploy.sh seeds 192.168.1.118)
radio_serial = ""          # empty = first radio found

[mqtt]
broker = "tcp://192.168.1.50:1883"   # deploy.sh seed (HA broker); built-in default tcp://127.0.0.1:1883
# user here; password via EnvironmentFile (see below)
site     = "muehle"
station  = "hf"
slot     = "radio"
location = "bauwagen"
publish_ha_discovery = false         # legacy embedded discovery; hadiscovery renders HA

[log]
level = "info"
```

Secrets can go in `/etc/flexbridge/flexbridge.env` (mode 0600), read by the
unit's `EnvironmentFile=`: `FLEXBRIDGE_MQTT_PASSWORD=...`.

## Home Assistant

The standalone `hadiscovery` service renders HA discovery from the `expose` block in
`/meta` (node ID `muehle-hf-radio`). The legacy embedded discovery (node ID
`flexradio-<serial>`) is off unless `publish_ha_discovery = true`. See
`docs/radio2mqtt-schema.md` and `../hadiscovery/docs/discovery-mqtt-api.md`.

## Project layout

```
cmd/flexbridge/main.go        # wiring, signals, reconnect loop
config.example.toml           # example config (on-device: /etc/flexbridge/config.toml)
deploy.sh                     # build + ship + install systemd service (unit generated inline)
internal/config/              # TOML config + flags + env
internal/flexradio/           # protocol: discovery, TCP API, status parsers (VITA-49/meters unwired)
internal/bridge/              # radio events -> MQTT (change-gated /state + 5 s heartbeat), /cmd dispatch
internal/ha/                  # Home Assistant discovery payloads (legacy embedded path, off by default)
```

## References

- [SmartSDR API wiki — Metering Protocol](https://github.com/flexradio/smartsdr-api-docs/wiki/Metering-Protocol)
- [SmartSDR API wiki — Discovery Protocol](https://github.com/flexradio/smartsdr-api-docs/wiki/Discovery-protocol)
- [SmartSDR TCP/IP API](https://github.com/flexradio/smartsdr-api-docs/wiki/SmartSDR-TCPIP-API)
- Meter conversion math and meter list cross-checked against FlexLib via
  [AetherSDR](https://github.com/aethersdr/AetherSDR) `MeterModel.h`.

## License

Copyright © 2026 Ingomar Otter.

Licensed under the GNU Affero General Public License v3.0 or later
(SPDX: `AGPL-3.0-or-later`) — see [LICENSE](LICENSE).
