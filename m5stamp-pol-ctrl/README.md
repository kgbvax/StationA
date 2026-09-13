# m5stamp-pol-ctrl

Firmware for **M5 Stamp PLC #2** (StamPLC K141: ESP32-S3 + AW9523B expander,
4 relays, 1.14" panel) for the Mühle station automation ecosystem. It
implements the station integration-model slot

| Slot | Role | Relays | Purpose |
|------|------|--------|---------|
| `muehle/uhf/pol-ctrl` | `pol-ctrl` | AW9523 pins 0–2 (H / CL / CR; all-off = V) | live X-Quad polarization — one shared phase for the co-mounted 2 m + 70 cm X-Quads |

This is an **ESPHome config** (not PlatformIO, not a Go service): the
config `esphome/xphasectrl.yaml` publishes the canonical
`meta`/`state`/`cmd`/`status` MQTT surface directly — firmware is the bridge
(the waveshare ant-switch pattern). State is honest: `/state.pol` derives
from relay readback, the device boots truthfully at `v` (relays all-off), and
the retained `/cmd` re-applies the operator's last intent after reconnect.
Invalid commands are rejected loudly into the `/state` `error` field and on
the display — never silently dropped.

Polarization is **operator-driven** (console, Button A on the device, or a
manual UI); nothing binds it to band or tracking.

The AW9523 pin-3 **FlexRadio power** relay is deliberately local-only
(Button C): `internal: true` keeps it out of web_server and the native API,
and no `/cmd` action reaches it.

See `CLAUDE.md` for architecture and `docs/m5stamp-pol-ctrl-mqtt-api.md`
for the on-the-wire contract, the KTD4 sixth-vector exposure decisions
(recorded in `../docs/known-issues.md`), and the bench acceptance checklist.
Shared conventions live in `../docs/`.

---

## Build / flash

```bash
cp secrets.example.yaml esphome/secrets.yaml   # then edit: wifi, mqtt, ota, api_key, web auth
esphome config esphome/xphasectrl.yaml    # compile check (secrets resolve NEXT TO the config)
esphome run esphome/xphasectrl.yaml       # build + flash
esphome logs esphome/xphasectrl.yaml      # logs
```

ESPHome can live in a throwaway venv (`python3 -m venv /tmp/esphome-venv &&
/tmp/esphome-venv/bin/pip install esphome`). PLC #2 runs pre-OTA field
firmware: the **first flash must be physical USB**; subsequent flashes can
go OTA (password via secrets). The secrets file lives at
`esphome/secrets.yaml` (ESPHome resolves `!secret` next to the config file)
and is gitignored — see `secrets.example.yaml` for the required keys.

---

## Configuration

Slot addressing (`site`/`station`/`slot` = `muehle`/`uhf`/`pol-ctrl`), broker
address, and the firmware version (`fw_version`, carried in
`/meta.device.firmware` — bump per released flash) live in `substitutions` at
the top of `esphome/xphasectrl.yaml`. Secrets (WiFi + MQTT creds, OTA
password, native API key, web_server auth) live in `esphome/secrets.yaml`
(gitignored). This is the
embedded-firmware secrets pattern, distinct from the Go services' systemd
EnvironmentFile; see `../docs/conventions/config-and-secrets.md`.

## License

Copyright © 2026 Ingomar Otter.

Licensed under the GNU Affero General Public License v3.0 or later
(SPDX: `AGPL-3.0-or-later`) — see [LICENSE](LICENSE).