# CLAUDE.md — testui

testui is a **MQTT relay + static UI server** for the stationa station bus. It is NOT a
deployed slot: it has no `/meta`, `/state`, `/status`, or `/cmd` of its own on the bus — it
is a passive consumer that subscribes `<site>/#` and proxies browser publish/clear requests
back onto the bus with safety guards. It can run two ways: on the developer workstation
(`make run`, loopback only) or deployed to shari as a hardened systemd service
(`./deploy.sh`, HTTP bound to the LAN so the browser hits `http://shari:8090` with no local
server). The deployed form is a network service, not a serial bridge — no `/dev/tty*`, so
the unit runs under a stricter sandbox than the serial bridges (`PrivateDevices=true`).

## Why a relay (not a direct browser MQTT client)

The broker (`192.168.1.50`) has only `1883`/`8883` open — no WebSocket listener — so a
browser cannot use MQTT.js. The Go relay speaks raw MQTT to the broker (credentials from
a local `0600` config or `TESTUI_MQTT_PASSWORD`), and exposes HTTP+SSE to the browser.
The browser uses plain `fetch` + `EventSource`.

## The schema-aware bar

The UI must comprehend the stationa schema, not dump JSON. Two sources drive rendering:

1. **`/meta.expose` (authoritative, primary)** — each slot's consumer-neutral field
   surface. The UI builds typed state widgets AND the command panel from it. The
   `expose.command` descriptor `{action, value_key, value_type}` is the exact source for
   `/cmd` payload construction — **never hardcode `"value"`**: `value_key` is `value` for
   most setpoints but `freq_hz` (ant-ctrl frequency), `az` (rotator), and value-key-only
   (`select`, `request`) for the switch and reconciler.
2. **Role registry (fallback)** — for slots whose `expose` is read-only but still
   drivable. Only `antennaselect` (operator-hold `request`) needs it today.

Roles with no `/cmd` handler (`radio`/flexbridge, `discovery`/hadiscovery) hide the
command panel entirely. See `docs/cmd-convention-audit.md` for the full cross-bridge
audit.

## Critical correctness invariants

- **No `Publish` inside a paho message handler.** Publishes (including retained clears)
  run on their OWN goroutine in `internal/mqtt`; inbound store updates run on the separate
  `jobs chan func()` worker. Keeping them apart means a stuck/slow publish (broker down,
  reconnect backoff) cannot freeze inbound store updates or SSE fan-out. The documented
  live deadlock (hadiscovery, antennaselect) is the trap. See memory
  `paho-handler-no-blocking-publish`.
- **SSE snapshot + update share one schema.** Both the snapshot (embedded `web.Plane`) and
  the live `update` stream (`web.Event`, same shape plus `address`/`plane`/`cleared`) use
  lowercase JSON tags, a `json.RawMessage` payload, and a pre-decoded `object` view.
  NEVER marshal `mqtt.Message` to the wire — it has no JSON tags and a `[]byte` payload,
  so it serializes to capitalized keys + base64 and the browser's `applyUpdate` throws on
  every update (the UI then freezes after the initial snapshot). `TestBroadcastEventShape`
  and `TestStreamUpdateEventShape` guard this.
- **The browser renders `Plane.object`, it does not re-`JSON.parse` the payload.**
  Valid-JSON object payloads (`/state`, `/meta`) pass through as `json.RawMessage` and
  arrive in the browser as JS objects; calling `JSON.parse` on an object throws. So
  `parseMeta`/`stateObj` use `plane.object` first (the relay's decoded view).
- **The server is the sole authority for a clear.** An empty retained publish sets
  `Event.cleared = true` and omits the payload; the browser clears the plane on
  `ev.cleared` ONLY — never on a falsy `!ev.payload`. A real update may carry a
  JSON-falsy payload (`null`/`false`/`0`/`""`) which is a legitimate stored value; gating
  on `!ev.payload` would clear it client-side while the server keeps it, diverging
  state until a snapshot refresh. `TestBroadcastEventShape` covers the falsy cases.
- **`/cmd` is never retained** — enforced by `validateTopic` in `internal/web/handlers.go`
  (integration model §8). The browser's per-command "retain" checkbox defaults off.
- **Publish guard**: `/api/publish` and `/api/clear` reject topics outside `<site>/`. The
  site is normalized (leading/trailing `/` stripped) and validated non-empty at startup in
  `cmd/testui/main.go`, so an empty/malformed site cannot degrade the `site+"/"` prefix
  guard (empty → fail-open for `/`-prefixed topics; trailing slash → rejects everything).
- **`/status` is a bare string** (`online`/`offline`), not JSON. The tree wraps non-JSON
  payloads as a JSON string on store so the snapshot/update events serialize (the UI
  strips residual quotes). Device liveness is a separate `/state.device_online` bool,
  rendered once as a `devonline` pill (not duplicated as a meta chip).
- **Bus-derived strings never reach `innerHTML` unescaped.** Slot addresses and field
  values come off the bus (topics allow arbitrary UTF-8); every `innerHTML` interpolation
  of them goes through `escapeHtml` (rail, head, chips, ticker, edit-state) or is built
  with `textContent`/DOM (bar widget). A crafted topic must not execute script.
- **`value_key` convention** — `atr1k-tuner-bridge` historically keyed `set_inline` under
  `inline` and `tune` under `mode`; now fixed (both `value`). Do not code to the old
  shape. See memory `cmd-payload-value-key-convention`.

## Config + secrets

TOML via `github.com/pelletier/go-toml/v2`, `Default()`/`Load()` preserving
`fs.ErrNotExist`, flag>file>default precedence via `flag.Visit`. The password may be in
the `0600` file OR supplied via `TESTUI_MQTT_PASSWORD` (EnvironmentFile pattern). No
`-mqtt-password` flag exists.

## Search gotcha

`grep` on this machine is ripgrep and respects `.gitignore`. The nested stationa bridge
repos are gitignored at the stationa level, so `grep -r ... .` from `/Users/.../stationa`
SILENTLY SKIPS every bridge. Always scope searches to a named bridge dir, e.g.
`grep -rn ... /Users/.../stationa/flexbridge`. (This bit the initial "no bridge
publishes expose" claim — most do.)

## Build/test

```bash
make build vet test test-race
```

## Deploy to shari (LAN-served, no local server)

```bash
./deploy.sh        # cross-compile, ship, install as a hardened systemd service on shari
```

The unit binds HTTP to `0.0.0.0:8090` (config `http_addr`), so the browser reaches
`http://192.168.1.139:8090` directly. The `hf` MQTT password is pulled on-device from an
existing station service env (never leaves the Pi). `/api/publish` + `/api/clear` are
unauthenticated — this is intentional for the trusted home LAN; do not deploy this default
onto an untrusted network without an auth layer / reverse proxy. Config + env are seed-once,
like the bridges.

There is no live-broker test (creds live on shari). The HTTP/SSE/tree/handler/config
layers are covered by unit tests; the MQTT client mirrors the proven antennaselect/
hadiscovery pattern. Live verification is manual (see README).