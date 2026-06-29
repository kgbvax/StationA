# ubctrl

Go project initialized with Git.

UltraBeam antenna controller for the RCU-06 protocol, with:

- live status polling
- minimal web UI with auto-refresh
- optional Home Assistant-compatible MQTT discovery

## Run

go run ./cmd/ubctrl

Use a real serial device with:

go run ./cmd/ubctrl -port /dev/cu.usbserial-XXXX

Optional MQTT:

go run ./cmd/ubctrl -mqtt-broker tcp://127.0.0.1:1883

## Configuration

ubctrl reads settings from a single TOML config file, defaulting to
`/etc/ubctrl/config.toml`. Point at a different file with `-config`:

go run ./cmd/ubctrl -config ./config.toml

Settings resolve with the precedence **flag > config file > built-in default**,
so any explicit flag overrides the file. If the *default* config path is absent,
ubctrl runs on defaults (and flags) — handy for local/mock runs. If a path given
explicitly via `-config` is missing or malformed, startup fails.

Example `config.toml`:

```toml
http_addr   = "0.0.0.0:8080"
serial_port = "/dev/ttyUSB0"   # empty -> mock device
baud        = 19200

[mqtt]
broker    = "tcp://127.0.0.1:1883"  # empty -> MQTT disabled
client_id = "ubctrl"
prefix    = "ubctrl"
user      = "ham"
password  = "change-me"
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

go build ./cmd/ubctrl
