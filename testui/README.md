# testui — stationa MQTT test/monitor console

A workstation-side Go relay + schema-aware browser UI for watching and stimulating the
Mühle station bus. The browser cannot speak raw MQTT (the broker at `192.168.1.50`
exposes only `1883`/`8883`, no WebSocket listener), and the `hf` MQTT password must not
reach a web page — so a small Go binary acts as **relay + static server**: it connects to
the broker with paho (credentials from a local `0600` config), subscribes `muehle/#`,
holds the live slot tree in memory, and exposes HTTP+SSE to the browser. The browser uses
plain `fetch` + `EventSource`, no MQTT.js.

The UI is **schema-aware, not a JSON viewer**: it renders each slot's three planes,
formats fields by role/field/unit, derives band from `freq_hz`, shows liveness from
`/status`, dims stale state when a slot is offline, and builds typed `/cmd` payloads
from each slot's `/meta.expose` block (with a role-registry fallback for `antennaselect`'s
operator-hold surface). See `docs/cmd-convention-audit.md` for the cross-bridge audit
that backs the command builder.

## Run

**On the workstation** (loopback only — the dev/iterate mode):

```bash
cp config.example.toml config.toml
chmod 600 config.toml
$EDITOR config.toml          # set [mqtt].password (or export TESTUI_MQTT_PASSWORD)
make run                      # => ./bin/testui -config config.toml
```

Then open <http://127.0.0.1:8090>. (Or use the env var instead of the file:

```bash
export TESTUI_MQTT_PASSWORD=...
./bin/testui -config config.toml
```

)

**On shari** (LAN-served — no local server needed; the browser hits the Pi directly):

```bash
./deploy.sh        # cross-compile, ship, install as a hardened systemd service
```

Then open <http://192.168.1.139:8090>. The unit binds `0.0.0.0:8090` and pulls the `hf`
MQTT password from an existing station service env on the Pi. `/api/publish` +
`/api/clear` are unauthenticated — fine for the trusted home LAN, not for an untrusted
network. See `CLAUDE.md` and `deploy.sh` for the hardening details.

Flags override the file: `-http`, `-site`, `-mqtt-broker`, `-mqtt-client-id`,
`-mqtt-user`. No `-mqtt-password` flag exists — the secret never reaches the command line.

## What it does

- **Monitor** every slot under `<site>/#`: live cards with role, device, liveness pill,
  capabilities chips, and typed state (freq+band chip, mode pill, tx pill, keyed, fault,
  port grid, SWR, power/temp, device_online, …). A bus-activity ticker shows recent raw
  messages; the left rail is a filterable station tree.
- **Stimulate** via typed command panels built from `expose`:
  - `radio` and `discovery` have no `/cmd` handler → command panel hidden.
  - `pa` (set_mode/set_band/set_power), `tuner` (set_inline/tune), `ant-ctrl` (frequency/band/
    direction/retract), `rotator` (set_az/stop/fwd/rev), `ant-switch` (select) are all
    driven from their `expose.command` descriptors — the exact `value_key` per field
    (`value` for most, `freq_hz` for ant-ctrl frequency, `az` for rotator, `select`
    value-key-only for the switch).
  - `antennaselect` exposes the operator-hold `request` surface via the role registry.
  - Each command is fire-and-observe: send, then watch `/state` for confirmation.
- **Simulate** retained `/state` (the "tools" expander per card) — publish a fake state
  to watch `antennaselect`/`hadiscovery` react.
- **Clear** retained topics (`/state`, `/meta`, `/cmd`) per slot.

## Safety guards

- `/api/publish` rejects any topic outside `<site>/`.
- `/api/publish` rejects `retain:true` on a `/cmd` topic (integration model §8: intent is
  never retained). A per-command "retain" checkbox defaults **off**.
- `/api/clear` only clears (zero-length retained publish).

## Layout

```
cmd/testui/main.go            flags, config, wire mqtt+web, ctx shutdown
internal/config/              TOML config + TESTUI_MQTT_PASSWORD env override
internal/mqtt/                paho client, jobs worker (no Publish in handlers)
internal/web/                 tree (in-mem slot model), SSE hub, handlers, static (go:embed)
internal/web/static/          index.html, app.js (schema-aware), styles.css
docs/cmd-convention-audit.md cross-bridge /cmd + expose audit (ground truth)
```

## Conventions followed

Mirrors the stationa bridges: `module testui`; pelletier `go-toml/v2` config with
`Default()`/`Load()` preserving `fs.ErrNotExist` and flag>file>default precedence; paho
with `SetAutoReconnect(true)` + `SetCleanSession(false)` and re-subscribe on reconnect;
the `jobs chan func()` worker so no `Publish` runs inside a paho message handler (the
documented deadlock — see `hadiscovery/internal/mqtt/client.go`). `deploy.sh` installs it
on shari as a hardened systemd service (network-service sandbox: `PrivateDevices=true`,
`RestrictAddressFamilies=AF_INET AF_INET6`, seed-once config + env); the workstation
`make run` mode stays loopback-only.

## Verify

```bash
make build vet test test-race
```

Live smoke test (with broker creds in `config.toml` and bridges running on shari):
open the UI, watch the HF cards populate, set a radio freq, watch `antennaselect`
resolve and the switch move, simulate a 30m `radio/state` to engage the ATU, clear a
stale retained topic. See the plan file's verification section.