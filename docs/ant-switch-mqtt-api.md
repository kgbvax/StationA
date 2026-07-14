# ant-switch MQTT API

This document describes the MQTT interface exposed by **waveshare_relay-antswitch-bridge** (the bridge to
the station's 1:6 antenna switch). waveshare_relay-antswitch-bridge implements the `ant-switch` slot of the
station integration model (`../../docs/station-integration-model.md`).

> **Status: implemented as an ESPHome config.** The bridge **is** the ESPHome firmware at
> `esphome/station-at1.yaml` running on a WaveShare ESP32-S3-POE-ETH-8DI-8DO board: it
> publishes the canonical `meta`/`state`/`status`/`cmd` topics directly over MQTT. Home
> Assistant remains connected over ESPHome native API as a secondary consumer / manual
> bring-up surface; it is not an MQTT-discovery consumer of this slot. Everything below is
> the **stable, authoritative surface** — downstream (the `antenna-select` reconciler)
> binds to it, not to the ESPHome-native entities.

The switch is a **dumb actuator** (integration model §5): it holds no policy. It selects
exactly one of six ports (or off), reports what is actually selected, and reports when the
relay has physically settled. *Which* antenna to select is decided by the `antenna-select`
reconciler, never here.

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1 (plain TCP, e.g. `tcp://host:1883`) |
| Authentication | Username/password if the broker requires it |
| Clean session | **No** — subscriptions survive restarts |
| Auto-reconnect | yes |
| Client ID | `ant-switch` (configurable via `mqtt.client_id`) |

---

## 2. Topic addressing

```
<site>/<station>/<slot>/<suffix>
```

Configured via `[mqtt]` in `config.toml`:

```toml
site    = "muehle"      # physical site
station = "hf"          # transmitting entity
slot    = "ant-switch"  # role (default: ant-switch)
```

Example for the Mühle HF station:

```
muehle/hf/ant-switch/meta
muehle/hf/ant-switch/state
muehle/hf/ant-switch/status
muehle/hf/ant-switch/cmd
```

| Suffix | Retained | Direction | Purpose |
|--------|----------|-----------|---------|
| `/meta` | yes | bridge → bus | birth certificate: identity + capabilities |
| `/state` | yes | bridge → bus | live selected port + settled flag (JSON snapshot) |
| `/status` | yes | broker LWT | liveness: `online` / `offline` |
| `/cmd` | yes | bus → bridge | desired port (see §6 on retention) |

---

## 3. `/status` — liveness

Plain string, retained, QoS 1. `online` on every (re)connect; `offline` via broker Last
Will on unclean disconnect and published on clean shutdown.

---

## 4. `/meta` — birth certificate

Retained JSON, published once per connect cycle.

```json
{
  "schema": "1.0",
  "role": "ant-switch",
  "device": { "model": "WaveShare ESP32-S3-POE-ETH-8DI-8DO (1:6 relay switch)" },
  "link": "wifi",
  "location": "bauwagen",
  "capabilities": {
    "ports": [1, 2, 3, 4, 5, 6],
    "off": true,
    "exclusive": true,
    "hot_switch": false
  },
  "expose": {
    "device": { "name": "Antenna switch", "model": "WaveShare ESP32-S3-POE-ETH-8DI-8DO (1:6 relay switch)" },
    "fields": [
      { "key": "selected", "name": "Selected port", "type": "enum",
        "options": ["off", "port1", "port2", "port3", "port4", "port5", "port6"],
        "writable": true,
        "command": { "value_key": "select", "value_type": "string" } },
      { "key": "settled", "name": "Settled", "type": "boolean" }
    ]
  }
}
```

| Capability | Meaning |
|---|---|
| `ports` | the selectable port numbers (six; relay group 3–8 → `port1`..`port6`) |
| `off` | an explicit no-port / grounded position exists (all relays off) |
| `exclusive` | exactly one port is selected at a time (never two) |
| `hot_switch` | **false** — RF must be inhibited and RX confirmed before a port moves (§6, model §6 cold-switch ordering) |

### The `expose` block — consumer-neutral field surface

`expose` (integration model §3.1, Appendix C) is the **consumer-neutral** description of
this slot's observable/controllable field surface — no consumer vocabulary (no
`device_class`, no Jinja, no `payload_on/off`). The standalone `hadiscovery` consumer
renders Home Assistant discovery from it; other consumers can render theirs from the same
block. The switch is a dumb actuator, so it exposes just two fields:

- `selected` — a **writable** enum: HA renders it as a `select` whose state reads
  `/state.selected` and whose command publishes `{"select":"<value>"}` to `/cmd` (the
  `command` descriptor has no `action`, only `value_key: "select"`). The options are
  **inlined** (`off`, `port1`..`port6`) rather than `options_ref`-ed into `capabilities.ports`:
  `ports` is `[1,2,3,4,5,6]` (ints) while `selected` is `"port1".."port6"` (strings) — the shapes
  differ, so the consumer-neutral option list is stated directly.
- `settled` — a read-only `boolean` rendered as a `binary_sensor`; `true` only when the
  relay has physically finished moving (load-bearing for cold-switch sequencing, §6).

No `actions` — the switch's only control surface is the `selected` setpoint.

---

## 5. `/state` — live state

Retained JSON snapshot, QoS 1. Published whenever `selected` or `settled` changes.

```json
{
  "ts":       "2026-07-06T12:34:56Z",
  "selected": "port2",
  "settled":  true
}
```

| Field | Type | Notes |
|-------|------|-------|
| `ts` | string | RFC 3339 UTC timestamp of this publish |
| `selected` | string | the port the **hardware actually reports**: `off` \| `port1` \| `port2` \| `port3` \| `port4` \| `port5` \| `port6`. Never assumed from the last command — read back from the device. |
| `settled` | bool | `true` only when the relay has finished moving and RF may safely pass. `false` while transitioning. |

**Hardware readback note (this implementation).** The relay coils are driven by a PCA9554
I²C expander with **no relay-contact feedback** wired on this board. `selected` is therefore
derived from the relay group's driven state (the best available signal on this hardware —
the actual coil-drive level, read back from the relay switches rather than echoed from the
incoming `/cmd`). True contact-level readback would require hardware changes. `settled`
is a **conservative timed guard**, not a hardware signal: the bridge holds `settled: false`
for the relay's worst-case travel time after a commanded change (`relay_settle_ms`), then
publishes `settled: true` — never optimistically.

**`settled` is load-bearing.** Because `hot_switch` is false, the `antenna-select`
sequencer must observe `settled: true` before RF is re-enabled after a port change.
A bridge that cannot read a genuine settle signal from the hardware must derive a
conservative one (e.g. hold `false` for the relay's worst-case travel time after a
commanded change), never publish `settled: true` optimistically.

---

## 6. `/cmd` — desired port

JSON, QoS 1. Published by the `antenna-select` reconciler (and, in manual bring-up, an
operator or HA).

```json
{ "select": "port2" }
```

Valid values: `off`, `port1`, `port2`, `port3`, `port4`, `port5`, `port6`.

**Retention (model §8 actuator exception):** `/cmd` **is retained** with the desired
steady-state port, because re-applying the same select on reconnect reproduces the same
physical position (self-healing). This is safe *only* because the select is idempotent and
position-based; there are no one-shot ant-switch commands.

**Cold-switch discipline is the commander's responsibility, not the switch's.** The switch
executes a `select` when it receives one. The `antenna-select` reconciler must not emit a
`select` that changes the port while the station is transmitting — it sequences the change
around RX (§ integration model §6). The switch itself does not gate on TX state.

---

## 7. Home Assistant

HA is an optional consumer (integration model §9). In this implementation HA reaches the
switch over ESPHome **native API** (a secondary consumer / manual bring-up surface), not
over MQTT discovery — the `mqtt:` block sets `discovery: false`. The canonical surface is
the MQTT bus; nothing in the station core depends on the HA path. A separate HA-discovery
consumer rendered from the `expose` block (e.g. by `hadiscovery`) could be added later, but
is out of scope here.

---

## 8. Typical interaction flows

**Read current selection on startup:**
Subscribe to `<slot>/#`. The broker immediately delivers retained `/meta`, `/state`,
`/status`.

**Reconciler selects the dipole (port 2):**
1. Reconciler confirms the radio is in RX (§ model §6).
2. Reconciler publishes retained `<slot>/cmd`: `{"select":"port2"}`.
3. Bridge commands the hardware; `/state` → `selected:"port2"`, `settled:false`.
4. When the relay settles: `/state` → `selected:"port2"`, `settled:true`.
5. Reconciler observes `settled:true` before RF is re-enabled.

**Detect switch fault:**
- Bridge running but hardware unreachable: publish a stale-marking state (e.g. hold the
  last `selected` with `settled:false`) and log; `/status` stays `online`. (Exact
  fault-state shape is an implementation choice — keep it consistent and documented.)
- Bridge crash / broker loss: `/status` → `offline` (LWT fires).
