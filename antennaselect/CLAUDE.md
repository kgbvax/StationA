# CLAUDE.md — antennaselect

antennaselect is the **antenna-selection reconciler** for the HF station. It implements the
`muehle/hf/antenna-select` logic slot (integration model §5, §7.1). It has **no device**:
it subscribes to state (radio, station, switch, operator), applies a priority ladder over a
wiring map and band policy, and emits one intent stream to the `ant-switch`.

Runs on `shari`.

> **Status: implemented, pending deployment.** The Go service — config, MQTT wiring,
> the decision logic with unit tests, and the `deploy.sh` + hardened systemd unit — is
> in place and passes `go test ./...`. What remains is the on-device deploy to `shari`.

---

## Commands

```bash
go build ./...                 # build
go test ./...                  # all tests
go test ./internal/reconcile   # decision-logic tests (the important ones)
go vet ./... && gofmt -l .     # vet + format check

# Run locally against a broker (no hardware needed — it is pure logic over MQTT)
go run ./cmd/antennaselect -config ./config.example.toml
go run ./cmd/antennaselect -broker tcp://192.168.1.50:1883   # broker via flag
```

## Layout

| Path | Purpose |
|---|---|
| `cmd/antennaselect/main.go` | entry point: config load + flag overrides, connect, run |
| `internal/config` | TOML config (`[mqtt]`, `[wiring_map]`, `[band_policy]`, `[band_follow]`, `[pa_follow]`, `[tuner_follow]`) + `Validate()` |
| `internal/reconcile` | **the heart** — priority ladder (§5), cold-switch sequencing (§6), band-follow + PA/tuner bindings (§7); pure logic, fully unit-tested |
| `internal/mqtt` | thin bus layer: subscribes to inputs (§8), feeds the reconciler, emits its actions |
| `docs/antenna-select-mqtt-api.md` | **authoritative** slot contract + decision logic |
| `config.example.toml` | wiring map, band policy, ladder |

The `[priority]` section in the example config is documentation only — the ladder order is
fixed in code (`internal/reconcile`).

## Deploying to shari

`deploy.sh` cross-compiles for `linux/arm64`, copies the binary + systemd unit + seed
config to `shari`, and installs the `antenna-select` service (seed-once; see
`../docs/conventions/deployment.md`). antennaselect is pure logic over MQTT — no serial
device, no HTTP server — so the unit carries no `DeviceAllow`/`SupplementaryGroups`.

```bash
./deploy.sh                                      # defaults: shari, broker 192.168.1.50:1883
MQTT_PASSWORD=... ./deploy.sh                    # seed the password on first deploy
ssh io@192.168.1.139 'journalctl -u antenna-select -f'
```

The seed config bakes the Mühle wiring map and band policy (matching
`config.example.toml`); the MQTT connection details, `location`/`host`, the
`[band_follow]` controller map (`resource`/`slot`), and the `[tuner_follow]` binding
(`resource`/`slot`/`atu_bands`) are env-overridable. After the first deploy, edit
`/etc/antenna-select/config.toml` on the device — `deploy.sh` will not overwrite it.

## Still to do

- On-device deploy + smoke test against the live broker on `shari`.

---

## The rules that must not drift (integration model)

- **React to state, emit intent** (§1). Never assume a `select` took — confirm via
  `ant-switch/state`.
- **Ladder order is fixed**: idle > operator > auto (§5). `mode` is *derived* (`manual` iff
  a hold is active), never a separate switch.
- **Cold-switch sequencing** (§6): the ant-switch is `hot_switch: false`. Do not move the
  port under TX — wait for RX, emit `select`, confirm via `selected` (`settled`-gating is
  backlog, lands with antswitchbridge). Enforcement is hardware; the reconciler owns
  ordering only, never the enforcement path.
- **Never trust retained state for safety** (§10): act on `radio/state` only when the
  radio is online — `radio/status` (broker LWT, bridge liveness) `online` **and**
  `radio/state.device_online` (radio-link liveness) `true`. `/status` alone is not enough:
  it stays `online` while flexbridge is up but the radio link is down, which is exactly when
  `radio/state` carries a stale/empty `band` (reconnect Reset). An empty `band` holds the
  last selection; only a known-but-unmatched band (160m, `gen`) reaches the `fallback`.
- **Idle overrides operator** (§10): station-inactive beats an operator hold (walk-away
  safety). Documented, deliberate.
- **Unmatched bands** (incl. 160m) → `fallback` (fan-dipole via ATU; §11 item #1).

---

## Known dependencies / residuals

- The station `activity` flag is **inferred** by this reconciler (not operator-set): a
  `freq_hz` change or `tx == "tx"` marks `active`; after `[idle].timeout_minutes` (default 30)
  with neither, it marks `inactive` and resolves `target = off` (walk-away lightning
  protection). No dedicated override command, but an operator **hold is presence**: a
  non-empty non-`auto` `/cmd` request resets the idle clock, so a hold doubles as a manual
  re-arm while the radio is down (tier 1 would otherwise override it there);
  `[idle].timeout_minutes` later, tier 1 retakes the antenna.
- 30/60/80/160m on the fan dipole are non-resonant; the `[tuner_follow]` binding now engages
  the `hf/tuner` ATU in-line for those bands and bypasses it otherwise (integration model
  §7.1 soft binding `tuner.set_inline ← band_policy`), closing the former §10 residual.
- This reconciler is a coordination single point (§10): if it dies, band-follow, tuner
  inline-follow, and antenna selection stop and the station degrades to manual —
  acceptable only because safety is hardware. Consider supervision/restart and an
  explicit "reconciler offline" indication.

---

## Station model and shared conventions

Shared docs live in `../docs/` (this repo is a subdirectory of the `stationa` meta-repo).

| Document | Path |
|---|---|
| Station integration model | `../docs/station-integration-model.md` |
| Config and secrets convention | `../docs/conventions/config-and-secrets.md` |
| Deployment convention | `../docs/conventions/deployment.md` |
| Canonical band/mode reference | `../docs/conventions/band-mode-reference.md` |
| MQTT schema template | `../docs/templates/mqtt-schema.md` |
