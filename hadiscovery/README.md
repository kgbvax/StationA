# hadiscovery

Central **Home Assistant MQTT discovery** consumer for the Mühle station bus.

hadiscovery is a passive service: it subscribes to slot `/meta` announcements, reads each
slot's consumer-neutral [`expose`](../docs/station-integration-model.md) block, and
renders HA MQTT discovery. It owns no device and writes no `/cmd`.

The bridges carry a neutral `expose` block in their `meta` and contain **no HA knowledge**.
hadiscovery is the single HA renderer of that neutral surface — the same `expose` is
reusable by InfluxDB, Node-RED, dashboards, or Prometheus. `meta` is the single source of
truth; adding a component or field needs no consumer edit.

Implements the `muehle/hf/discovery` logic slot. Runs on `shari`.

## Status

Scaffolded and tested (`go test ./... -race`), pending on-device deploy and the bridge
migrations (publish `expose`, gate embedded discovery off). See `CLAUDE.md`.

## Build & run

```bash
go build ./...
go test ./...
go run ./cmd/hadiscovery -config ./config.example.toml
```

## Deploy

```bash
./deploy.sh                      # cross-compile for linux/arm64, install on shari
ssh io@192.168.1.139 'journalctl -u hadiscovery -f'
```

## Docs

- `docs/discovery-mqtt-api.md` — the neutral→HA mapping, discovery topic layout, rebirth/removal.
- `../docs/station-integration-model.md` §3.1 / §9 / Appendix C — the neutral `expose` schema.

## Layout

| Path | Purpose |
|---|---|
| `cmd/hadiscovery` | entry point |
| `internal/config` | TOML config |
| `internal/expose` | neutral `expose` types + `/meta` parser (no HA vocabulary) |
| `internal/ha` | the only HA knowledge — neutral→HA render |
| `internal/engine` | discovery lifecycle (republish on HA birth, clear on meta-clear, idempotent) |
| `internal/mqtt` | bus client (own LWT/meta, subscribe meta filter + `homeassistant/status`) |