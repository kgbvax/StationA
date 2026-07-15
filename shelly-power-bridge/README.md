# shelly-power-bridge

Shelly Gen2+ smart-plug → MQTT bridge for the Mühle station automation
ecosystem. Fronts Shelly Plus / Mini / Pro plugs that speak MQTT natively and
translates them into site-level station integration-model **`power`** slots
(`muehle/power/<slot>/{meta,state,status,cmd}`).

It is a **compound bridge**: one process fronts N Shelies. Each `[[slot]]`
runs its own paho client with its own LWT, so a process death takes every
fronted slot offline with no stale-online gap.

The two slots in use today are the station **master mains** (`power/master`)
and the **13.8 V PSU** (`power/psu-13v8`). The PSU is a site-level DC rail
feeding both HF and UHF, which is why the power layer sits at site level rather
than under one station. Historically the Shelies were controlled only through
Home Assistant — an HA-as-control-path anti-pattern; this bridge brings them
onto the canonical bus so the sequencer (`powerseq`) and operators drive them
directly.

See `CLAUDE.md` for architecture and `docs/shelly-power-bridge-mqtt-api.md` for
the on-the-wire contract. Shared conventions live in `../docs/`.

---

## Build / test / deploy

```bash
go build ./cmd/shelly-power-bridge
go test ./...
./deploy.sh
```

Cross-compile for the Pi (shari, `192.168.1.139`):

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o dist/shelly-power-bridge-linux-arm64 ./cmd/shelly-power-bridge
```

---

## Configuration

Config is a 0600 TOML file (`/etc/shelly-power-bridge/config.toml`) with one
`[[slot]]` per Shelly. See `config.example.toml`. The MQTT password is **not**
in the TOML — it is loaded from an `EnvironmentFile`
(`/etc/shelly-power-bridge/shelly-power-bridge.env`) per the
[config-and-secrets convention](../docs/conventions/config-and-secrets.md).