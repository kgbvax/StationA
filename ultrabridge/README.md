# ultrabridge

UltraBeam RCU-06 antenna-controller bridge — implements the `ant-ctrl` slot of the
station integration model (`../docs/station-integration-model.md`), with:

- live status polling
- minimal web UI with auto-refresh
- optional Home Assistant-compatible MQTT discovery

## Run

go run ./cmd/ultrabridge

Use a real serial device with:

go run ./cmd/ultrabridge -port /dev/cu.usbserial-XXXX

Optional MQTT:

go run ./cmd/ultrabridge -mqtt-broker tcp://127.0.0.1:1883

## Configuration

ultrabridge reads settings from a single TOML config file, defaulting to
`/etc/ultrabridge/config.toml`. Point at a different file with `-config`:

go run ./cmd/ultrabridge -config ./config.toml

Settings resolve with the precedence **flag > config file > built-in default**,
so any explicit flag overrides the file. If the *default* config path is absent,
ultrabridge runs on defaults (and flags) — handy for local/mock runs. If a path given
explicitly via `-config` is missing or malformed, startup fails.

Example `config.toml`:

```toml
http_addr   = "0.0.0.0:8080"
serial_port = "/dev/ttyUSB0"   # empty -> mock device
baud        = 19200
location    = "bauwagen"       # published in /meta
host        = "shari"          # published in /meta

[mqtt]
broker           = "tcp://127.0.0.1:1883"  # empty -> MQTT disabled
site             = "muehle"
station          = "hf"
slot             = "ant-ctrl"              # canonical role (default)
discovery_prefix = "homeassistant"
user             = "ham"
password         = "change-me"
# client_id defaults to "<site>-<station>-<slot>"
```

> **Secret handling:** the file holds the MQTT password in plaintext, so on the
> target machine it must stay `0600` and owned by the service user. Because the
> app reads the password from this file, it never appears in the systemd unit
> (`ExecStart`) or the process command line (`ps`). `deploy.sh` enforces these
> permissions when it seeds the file.

When deployed via `deploy.sh`, the config file is **seeded once** on the first
deploy from the script's environment variables, then left untouched on later
deploys — so the device owns its own settings. To change a setting afterward,
edit the file on the device (or delete it and redeploy to re-seed).

## Build

go build ./cmd/ultrabridge

## License

Copyright © 2026 Ingomar Otter.

Licensed under the GNU Affero General Public License v3.0 or later
(SPDX: `AGPL-3.0-or-later`) — see [LICENSE](LICENSE).
