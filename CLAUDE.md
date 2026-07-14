# CLAUDE.md — waveshare_relay-ant-switch-bridge

waveshare_relay-ant-switch-bridge bridges the station's **1:6 antenna switch** to MQTT, exposing the
`muehle/hf/ant-switch` slot on the station bus. The switch is a **dumb actuator**: it
selects one of six ports (or off) and reports what is actually selected — it holds no band
policy or selection logic. *Which* antenna is chosen is decided by the `antenna-select`
reconciler (see `../antennaselect/`).

> **Status: implemented as an ESPHome config.** There is no Go bridge — the ESPHome
> firmware **is** the bridge. `esphome/station-at1.yaml` runs on a WaveShare
> ESP32-S3-POE-ETH-8DI-8DO board and publishes the canonical `meta`/`state`/`status`/`cmd`
> topics directly over MQTT. Home Assistant stays connected over ESPHome native API as a
> secondary consumer / manual bring-up surface (`discovery: false`, `topic_prefix: null`).
> The authoritative contract the reconciler binds to is `docs/ant-switch-mqtt-api.md`.

The `antenna-select` reconciler wires a subset of the six ports in its `wiring_map` (at the
Mühle HF station: `port1` = dummy-load, `port3` = ultrabeam, `port6` = fan-dipole; ports 2,
4, 5 are unused). All six ports remain hardware-selectable, and the reconciler may command any
wired port — including `port6`. No `antennaselect` change is required for this 1:6 surface.

---

## What lives here

| Path | Purpose |
|---|---|
| `esphome/station-at1.yaml` | **The bridge** — ESPHome config implementing the canonical slot (1:6) |
| `docs/ant-switch-mqtt-api.md` | **Authoritative** canonical MQTT contract (the surface the reconciler binds to) |
| `CLAUDE.md` | this file |

### Relay → port mapping (relays 3–8, exclusive group on PCA9554 expander pin 0x20)

| Port | Relay | PCA9554 pin |
|------|-------|-------------|
| `off` | — | all off |
| `port1` | relay_3 | 2 |
| `port2` | relay_4 | 3 |
| `port3` | relay_5 | 4 |
| `port4` | relay_6 | 5 |
| `port5` | relay_7 | 6 |
| `port6` | relay_8 | 7 |

Relays 1–2 (independent), the buzzer, RGB LED, digital inputs, RS485 UART, and RTC are
unrelated to the switch slot and are left as-is.

### Configuring / deploying the firmware

Use the standard ESPHome flow (dashboard or CLI); this project does **not** use the
meta-repo's systemd/deploy conventions (those are for the Go services on shari). Secrets
(`wifi_ssid`, `wifi_password`, `mqtt_password`) live in the ESPHome `secrets.yaml` next to
the config, which is gitignored — never commit them. Validate locally with
`esphome config esphome/station-at1.yaml` (a throwaway `secrets.yaml` is required for the
check).

---

## Key contract points (do not drift from these)

- **`exclusive`** — exactly one port selected at a time.
- **`selected`** — best-effort readback from the relay group state. This hardware has **no
  relay-contact feedback** (coils driven by a PCA9554 I²C expander), so `selected` reflects
  the driven coil state, not contact closure, and is never echoed from the last `/cmd`. See
  the readback note in `docs/ant-switch-mqtt-api.md` §5.
- **`settled`** — a **conservative timed guard** (`relay_settle_ms`), not a hardware signal:
  hold `false` for the relay's worst-case travel time after a commanded change, then
  `true`. Never optimistic. Load-bearing for the reconciler's cold-switch sequencing
  (`hot_switch: false`).
- **`/cmd` is retained** (position-based, idempotent select — safe to re-apply on
  reconnect; model §8 actuator exception). The bridge re-applies it without relay chatter
  when the position is unchanged.
- The switch **never gates on TX state**; the reconciler sequences port changes around RX.

---

## Station model and shared conventions

Shared docs live in `../docs/` (this repo is a subdirectory of the `stationa` meta-repo).

| Document | Path |
|---|---|
| Station integration model | `../docs/station-integration-model.md` |
| Config and secrets convention | `../docs/conventions/config-and-secrets.md` |
| Deployment convention | `../docs/conventions/deployment.md` |
| MQTT schema template | `../docs/templates/mqtt-schema.md` |
