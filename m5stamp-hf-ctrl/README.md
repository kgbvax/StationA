# m5stamp-hf-ctrl

Firmware for **M5 Stamp PLC #1** (StamPLC K141: ESP32-S3 + AW9523B expander, 4
relays + 8 DIs) for the Mühle station automation ecosystem. A **compound
embedded node**: one PLC publishes two station integration-model slots that
share `device{model,serial}`:

| Slot | Role | Relays | Purpose |
|------|------|--------|---------|
| `muehle/hf/switch` | `switch` | 3 (PA remote-on), 4 (TRX remote-on) | PA / TRX remote-on control lines; read-write |
| `muehle/hf/pa-arm` | `pa-arm` | 1 (arm) | PA-enable arm relay, **fail-safe-open**, arm logic embedded |

Relay 2 is spare. The PLC is the station's embedded safety node: it subscribes
`hf/radio/state` and computes `armed = enabled ∧ radio_online ∧ ¬tuning ∧
band_safe ∧ heartbeat`, driving relay 1 so any failure drops the arm open → PA
disabled (model §6, §11.3).

See `CLAUDE.md` for architecture and `docs/m5stamp-hf-ctrl-mqtt-api.md` for the
on-the-wire contract. Shared conventions live in `../docs/`.

---

## Build / flash

This is embedded firmware (Arduino C++ / PlatformIO), not a Go service — no
`go.mod`, no `deploy.sh`. Flash over USB from a workstation with PlatformIO:

```bash
cp src/secrets.example.h src/secrets.h   # then edit: WIFI_*, MQTT_*, DEVICE_*
pio run                                   # build
pio run -t upload                         # flash
pio device monitor                        # serial monitor
```

---

## Configuration

Non-secret config is compile-time in `src/config.h` (slot addresses, relay
map, band allow-list, heartbeat window). Secrets live in `src/secrets.h`
(gitignored — see `src/secrets.example.h`). This is the embedded-firmware
secrets pattern, distinct from the Go services' systemd EnvironmentFile; see
`../docs/conventions/config-and-secrets.md`.