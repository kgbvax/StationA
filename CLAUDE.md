# CLAUDE.md — hadiscovery

hadiscovery is the **central Home Assistant discovery consumer** for the station bus
(integration model §3.1, §9). It implements the `muehle/hf/discovery` logic slot. It has
**no device**: it subscribes to slot `/meta` announcements, reads each slot's
consumer-neutral `expose` block, and renders HA MQTT discovery. It writes no `/cmd`.

This is the design's "3b": bridges carry a neutral `expose` block in their `meta` and
contain **zero HA knowledge**; hadiscovery is the single HA renderer of that neutral
surface (the same `expose` is reusable by InfluxDB / Node-RED / dashboards / Prometheus).
`meta` is the single source of truth.

Runs on `shari`.

> **Status: scaffolded, pending live deploy.** The Go service — config, neutral `expose`
> parsing, the deterministic neutral→HA render, the discovery lifecycle engine, the MQTT
> client, golden unit tests, `deploy.sh` + hardened systemd unit — is in place and passes
> `go test ./... -race`. What remains is the on-device deploy + smoke against the live
> broker and the bridge migrations (publish `expose`, gate embedded discovery off).

---

## Commands

```bash
go build ./...                 # build
go test ./...                  # all tests
go test ./internal/ha          # the neutral→HA render contract (golden)
go test -race ./...            # race detector
go vet ./... && gofmt -l .     # vet + format check

# Run locally against a broker (no hardware — passive MQTT consumer)
go run ./cmd/hadiscovery -config ./config.example.toml
go run ./cmd/hadiscovery -broker tcp://192.168.1.50:1883   # broker via flag
```

## Layout

| Path | Purpose |
|---|---|
| `cmd/hadiscovery/main.go` | entry point: config load + flag overrides, connect, run |
| `internal/config` | TOML config (`[mqtt]` + deployment identity) + `Validate()` |
| `internal/expose` | the neutral `expose` types (`schema.go`) and `/meta` parser (`meta.go`) — no HA vocabulary |
| `internal/ha` | **the only HA knowledge** — `render.go` maps neutral→HA; `diagnostic.go` for no-expose slots |
| `internal/engine` | discovery lifecycle: known-slots map, republish on HA birth, clear on meta-clear, idempotent, no-expose diagnostic |
| `internal/mqtt` | thin bus layer: own LWT + `/meta`, subscribe `meta_filter` + `homeassistant/status`, feed engine |
| `docs/discovery-mqtt-api.md` | **authoritative** HA mapping + discovery topic layout + rebirth/removal |
| `config.example.toml` | broker, site/station/slot, discovery prefix, location/host |

## Deploying to shari

`deploy.sh` cross-compiles for `linux/arm64`, copies the binary + systemd unit + seed
config to `shari`, and installs the `hadiscovery` service (seed-once; see
`../docs/conventions/deployment.md`). Passive consumer — no serial, no HTTP, no listeners
— so the unit carries no `DeviceAllow`/`SupplementaryGroups`.

```bash
./deploy.sh                                      # defaults: shari, broker 192.168.1.50:1883
MQTT_PASSWORD=... ./deploy.sh                    # seed the password on first deploy
ssh io@192.168.1.139 'journalctl -u hadiscovery -f'
```

The seed config bakes broker/site/station/slot/discovery_prefix/location/host; the MQTT
password is set on the device. After the first deploy, edit
`/etc/hadiscovery/config.toml` on the device — `deploy.sh` will not overwrite it.

---

## The rules that must not drift

- **hadiscovery is passive.** It reads `/meta` (and, for availability, the slot `/status`
  it already references). It never writes `/cmd` and never publishes under any slot's
  address other than its own. Its only writes are under `<discovery_prefix>/...` and its
  own `meta`/`status`.
- **`expose` is consumer-neutral.** No HA vocabulary (no `device_class` strings, no Jinja,
  no `payload_on/off`) ever appears in `meta`. The HA-specific mapping lives **only** in
  `internal/ha`. Keep it that way — that is the whole point (model §9: HA is one renderer).
- **Deterministic render order.** `Render` walks fields then actions in declared order;
  the engine's idempotency depends on byte-stable entity sets. Do not introduce maps or
  other nondeterministic ordering in the render path.
- **Idempotent / no churn.** A byte-identical retained `/meta` re-delivery must be a
  no-op. When a slot's `expose` shrinks, clear dropped topics (empty retained) — never
  leave stale discovery.
- **freq_hz in Hz as integer; canonical mode names.** hadiscovery does not invent field
  values, but the `expose` it consumes must follow the bus conventions
  (`../docs/conventions/band-mode-reference.md`).
- **Credentials never on the command line / in shell history / in the unit.** Config in
  0600 TOML (`../docs/conventions/config-and-secrets.md`).

---

## Station model and shared conventions

Shared docs live in `../docs/` (this repo is a subdirectory of the `stationa` meta-repo).

| Document | Path |
|---|---|
| Station integration model | `../docs/station-integration-model.md` (§3.1, §9, Appendix C) |
| Config and secrets convention | `../docs/conventions/config-and-secrets.md` |
| Deployment convention | `../docs/conventions/deployment.md` |
| Canonical band/mode reference | `../docs/conventions/band-mode-reference.md` |
| MQTT schema template | `../docs/templates/mqtt-schema.md` |
| This component's HA mapping | `docs/discovery-mqtt-api.md` |