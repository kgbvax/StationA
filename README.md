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

## Build

go build ./cmd/ubctrl
