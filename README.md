# flexbridge

A small Go service that **observes** a FlexRadio 6000-series radio (tested
against the FLEX-8400) and publishes its state and telemetry to MQTT, with
Home Assistant MQTT discovery so the entities appear automatically.

It is designed to run on a Raspberry Pi as a hardened systemd service.

## How it works (no polling)

flexbridge never polls the radio on a timer. The FlexRadio streams its
real-time data; we just listen:

1. **Discovery** — UDP broadcast to `255.255.255.255:4992`; the radio replies
   with its model, serial, and IP. (Optional; you can hardcode the IP.)
2. **TCP handshake** — connect to `<radio>:4992`, send `client udpport` so the
   radio knows where to stream meters, and `sub slice/radio/interlock/atu/meter all`.
   This also builds the meter index→name map.
3. **Observe** — two long-lived listeners:
   - **TCP** pushes status (frequency, mode, PTT, ATU, power) as it changes.
   - **UDP** pushes VITA-49 meter datagrams (forward/reflected power, SWR,
     compression, ALC, mic levels, S-meter, PA temp, supply rails) at 10–20 fps.

On disconnect (radio reboot, network blip) it reconnects with backoff and
re-publishes retained state.

## What gets published

Each value is a **separate MQTT field** (no JSON blobs). What's published:

- **Receiver state (per slice)** — frequency (MHz), **band** (derived),
  mode, AGC mode, filter low/high, active flag. Published on change, retained.
- **Transmit state** — `transmitting` binary_sensor, TX power, tune power,
  ATU status. Published on change, retained.
- **TX telemetry** (while transmitting) — forward/reflected power (W), SWR,
  compression, ALC, mic levels.
- **RX telemetry (per slice)** — S-meter (dBm), broadband (dBFS).
- **Hardware** — PA temp, supply voltage pre/post fuse.

TX-chain meters are only published while the radio is actually transmitting
(gated on `interlock state=TRANSMITTING`), so they never read full-scale
garbage during receive. `PACURRENT` is deliberately excluded — it is flagged
unreliable on the 8000-series.

The Home Assistant entity `unique_id` is `flexradio-<serial>_<object_id>`,
so the values below are the stable handles for dashboards/automations.

### Meter object_ids

| object_id | group | field | unit | rate |
|---|---|---|---|---|
| `tx_fwd_power` | tx | forward RF power | W | 2 Hz |
| `tx_ref_power` | tx | reflected RF power | W | 2 Hz |
| `tx_swr` | tx | SWR | ratio | 2 Hz |
| `tx_compression` | audio | speech compression | dB | 2 Hz |
| `tx_alc` | audio | ALC | dB | 2 Hz |
| `mic_level` | audio | microphone level | dBFS | 2 Hz |
| `mic_peak` | audio | microphone peak | dBFS | 2 Hz |
| `s_meter` | rx | S-meter (per slice) | dBm | 1 Hz |
| `broadband` | rx | broadband level 24 kHz (per slice) | dBFS | 1 Hz |
| `pa_temp` | hw | PA temperature | °C | 0.1 Hz |
| `supply_voltage_a` | hw | supply voltage, pre-fuse | V | 0.1 Hz |
| `supply_voltage_b` | hw | supply voltage, post-fuse | V | 0.1 Hz |

Meter MQTT topic: `flexbridge/<serial>/meter/<group>/[<slice>/]<object_id>`
(not retained). Per-slice meters (`s_meter`, `broadband`) carry the slice
index, e.g. `flexbridge/<serial>/meter/rx/0/s_meter`.

### Status object_ids (retained)

Radio-wide (component: `sensor`, except `transmitting` which is `binary_sensor`):

| object_id | field | unit |
|---|---|---|
| `transmitting` | TX/PTT state | TRANSMITTING / RECEIVING |
| `tx_power` | configured TX power | W |
| `tune_power` | tune (ATU/carrier) power | W |
| `atu` | ATU status | tuned / bypass / tuning / … |

Per-slice receiver state (one set per slice `<n>`; `object_id = slice_<n>_<suffix>`):

| object_id suffix | field | unit |
|---|---|---|
| `slice_<n>_frequency` | VFO frequency | MHz |
| `slice_<n>_band` | band (derived from frequency) | 20m, 40m, 6m, 2m, … |
| `slice_<n>_mode` | mode (USB/LSB/CW/…) | |
| `slice_<n>_agc` | AGC mode | |
| `slice_<n>_filter_low` | filter low edge | Hz |
| `slice_<n>_filter_high` | filter high edge | Hz |
| `slice_<n>_active` | slice active flag | 0 / 1 |

`band` is not sent by the radio — flexbridge derives it from the slice
frequency, so it covers all bands the radio can tune (160 m through 23 cm
including 6 m, plus `gen` for general-coverage HF). It only republishes
when the resolved band changes, not on every kHz of drift within a band.

Status MQTT topic: `flexbridge/<serial>/state/[slice/<n>/]<suffix>`.

## MQTT topics

```
flexbridge/<serial>/status                         # bridge LWT: online/offline
flexbridge/<serial>/state/...                      # retained status fields
flexbridge/<serial>/meter/tx/<object_id>           # live meters (not retained)
flexbridge/<serial>/meter/audio/<object_id>
flexbridge/<serial>/meter/rx/<slice>/<object_id>
flexbridge/<serial>/meter/hw/<object_id>
homeassistant/<component>/flexradio-<serial>/<object_id>/config   # discovery
```

## Build

Requires Go 1.21+.

```bash
make build       # local binary
make pi          # cross-compile for Raspberry Pi 3B+/4/5 (arm64)
make test        # unit tests (no network)
```

## Install on the Pi

```bash
# from your dev machine
scp bin/flexbridge-linux-arm64 pi@raspberrypi:/tmp/flexbridge
scp -r deploy pi@raspberrypi:/tmp/flexbridge-deploy
ssh pi@raspberrypi

# on the Pi
sudo bash /tmp/flexbridge-deploy/install.sh
sudoedit /etc/flexbridge/config.toml          # set broker + radio_serial
sudo systemctl enable --now flexbridge
journalctl -u flexbridge -f
```

The install script creates a dedicated `flexbridge` user and a hardened
systemd unit (no capabilities, `ProtectSystem=strict`, etc.). The binary
uses only high ports (>1024), so it runs fully unprivileged.

## Configure

`/etc/flexbridge/config.toml` (see `deploy/config.example.toml`):

```toml
radio_host = ""            # empty = UDP autodiscovery
radio_serial = ""          # empty = first radio found
radio_udp_port = 4991

[mqtt]
broker = "tcp://homeassistant.local:1883"
# user / password here, or via EnvironmentFile (see below)

[rates]
tx = 0.5
audio = 0.5
rx = 1.0
hw = 10.0
```

Secrets can go in `/etc/flexbridge/flexbridge.env` (mode 0600), read by the
unit's `EnvironmentFile=`: `FLEXBRIDGE_MQTT_PASSWORD=...`.

## Home Assistant

Nothing to configure beyond enabling MQTT discovery. On startup flexbridge
publishes discovery for every entity under one device ("FlexRadio 8400",
identified by serial), so multiple radios coexist cleanly. Entities:

- **sensors**: forward/reflected power, SWR, compression, ALC, mic levels,
  S-meter, broadband, PA temp, supply voltages, frequency, TX power, etc.
- **binary_sensor**: `transmitting`.

## Project layout

```
main.go                       # wiring, signals, reconnect loop
internal/config/              # TOML config + flags + env
internal/flexradio/           # protocol: discovery, TCP API, VITA-49, meters
internal/bridge/              # radio events -> MQTT, throttle/dedup
internal/ha/                  # Home Assistant discovery payloads
deploy/                       # systemd unit, config example, install script
```

## References

- [SmartSDR API wiki — Metering Protocol](https://github.com/flexradio/smartsdr-api-docs/wiki/Metering-Protocol)
- [SmartSDR API wiki — Discovery Protocol](https://github.com/flexradio/smartsdr-api-docs/wiki/Discovery-protocol)
- [SmartSDR TCP/IP API](https://github.com/flexradio/smartsdr-api-docs/wiki/SmartSDR-TCPIP-API)
- Meter conversion math and meter list cross-checked against FlexLib via
  [AetherSDR](https://github.com/aethersdr/AetherSDR) `MeterModel.h`.

## License

MIT.
