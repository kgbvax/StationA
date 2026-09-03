# m5dial-hf-rotctrl — M5Stack Dial HF-rotator control head

Firmware for an [M5Stack Dial](https://shop.m5stack.com/products/m5stack-dial-v1-1)
(ESP32-S3, round 240×240 display, rotary encoder with push) that turns it into
a **physical analog control head for the station's HF rotator** (Yaesu
G-450DC, slot `muehle/hf/rotator` via `wrc-rotator-bridge`).

- **Analog meter face** — fixed 0…360° **compass card** (N at 12 o'clock;
  azimuth wraps on the display, so az 390 renders at the 30° mark, with a
  small +360 hint when the rotator rides its 360…450 overlap pass), a damped
  black needle that sweeps like a physical meter movement, and a thin red
  target pointer. Liveness is a ring: green
  OK, amber = rotator link down, red = bridge/MQTT down, gray = no data.
- **The knob is the rotator** — every detent publishes a new target azimuth
  (22.5°/detent — one knob-flat, one compass step); pressing the knob publishes
  `stop`. The needle chases your hand.
- **Wireless OTA from the first image** — only the literal first flash needs
  USB; every routine update is over the network (`./deploy.sh`) — except a
  sim-flashed device, which has no radio and needs `./deploy.sh usb` to
  return to the live image.
- **Desk-simulation build** — `./deploy.sh sim` flashes a SIM_MODE image: the
  antenna becomes a local mast model (no WiFi, no MQTT), running the
  production face/knob logic at the real G-450DC slew rate. Develop on the
  bench with nothing else powered; `./deploy.sh usb` returns to the live
  image.

The device is a **consumer + `/cmd` stimulator, not a slot** (integration
model §9): no `/meta`/`/state`/`/status` of its own, no LWT. Its
entire bus footprint is two subscriptions and two publish shapes on
`muehle/hf/rotator/cmd`. Remove it and the station runs identically.

## Build & flash

Requires [PlatformIO](https://platformio.org/).

```bash
cp src/secrets.example.h.x src/secrets.h   # once; fill in WiFi/MQTT/OTA credentials
./deploy.sh                              # build + flash over the air (default)
./deploy.sh usb                          # first flash / recovery only
./deploy.sh sim                          # desk-simulation image (USB only)
pio device monitor                       # serial monitor
```

The Dial appears as `m5dial-rotctrl-1.local` on the shack LAN (ArduinoOTA
hostname = `DEVICE_SERIAL`).

## Documentation

- `CLAUDE.md` — architecture, commands, topic table, secrets pattern
- `docs/m5dial-hf-rotctrl-mqtt-api.md` — the full on-the-wire contract and a
  bench verification recipe with verbatim mosquitto commands
- `../docs/station-integration-model.md` — the station model this conforms to
- `../docs/design-notes.md` — design + open-decision notes

## License

AGPL-3.0-or-later (see `LICENSE`), like the rest of the stationa monorepo.