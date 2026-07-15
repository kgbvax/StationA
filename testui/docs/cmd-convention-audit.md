# stationa /cmd + expose convention audit

Extracted by a cross-bridge code audit (one agent per bridge) on 2026-07-12. This is the
ground truth the test UI's command builder and renderers are built against. The live
`/meta.expose` block is authoritative at runtime; this document is the point-in-time
fallback registry and the rationale.

## /cmd argument-key convention (NOT uniform — read `expose.command.value_key`)

| Bridge | Role | Action | value_key | type | Shape |
|--------|------|--------|-----------|------|-------|
| flexbridge | radio | — | — | — | **no /cmd handler (read-only)** |
| ultrabridge | ant-ctrl | `frequency` | `freq_hz` | int | `{"action":"frequency","freq_hz":21225000}` |
| ultrabridge | ant-ctrl | `band` | `value` | string | `{"action":"band","value":"15m"}` |
| ultrabridge | ant-ctrl | `direction` | `value` | string | `{"action":"direction","value":"reverse"}` |
| ultrabridge | ant-ctrl | `retract` | — (button) | — | `{"action":"retract"}` |
| acom1200s-pa-bridge | pa | `set_mode` | `value` | string | `{"action":"set_mode","value":"operate"}` |
| acom1200s-pa-bridge | pa | `set_band` | `value` | string | `{"action":"set_band","value":"20m"}` |
| acom1200s-pa-bridge | pa | `set_power` | `value` | enum(on\|off) | `{"action":"set_power","value":"on"}` |
| wrc-rotator-bridge | rotator | `set_az` | `az` | float | `{"action":"set_az","az":180}` |
| wrc-rotator-bridge | rotator | `stop`/`fwd`/`rev` | — (button) | — | `{"action":"stop"}` |
| ant-switch-bridge | ant-switch | **(empty)** | `select` | string | `{"select":"port2"}` (retained) |
| atr1k-tuner-bridge | tuner | `set_inline` | `value` | bool | `{"action":"set_inline","value":true}` |
| atr1k-tuner-bridge | tuner | `tune` | `value` | enum(mem\|full) | `{"action":"tune","value":"mem"}` |
| antennaselect | reconciler | **(empty)** | `request` | string | `{"request":"port2"}` (operator hold) |
| hadiscovery | discovery | — | — | — | passive consumer, no /cmd |
| pelcobridge | (none) | — | — | — | no MQTT surface |
| bwctrl | (none) | — | — | — | empty placeholder dir |

Four payload shapes coexist: `action+value`, `action+<semantic key>`, `action-only (button)`,
and `<value_key>-only` (no `action` field — ant-switch-bridge `select`, antennaselect `request`).

## expose publishing

Every real MQTT bridge populates `expose`. Writable fields carry
`command{action,value_key,value_type}`; buttons carry `actions[].command{action}`.
flexbridge and antennaselect publish read-only expose (no writable fields) — the UI must
hide commands for flexbridge (radio) and drive antennaselect via the registry fallback.

## /status

Universal: plain retained string `online`/`offline` (paho LWT). Tracks bridge process
liveness. Device liveness is a separate `/state.device_online` bool (acom, wrc, atr1k,
ultra) — the two can diverge (bridge up, device down).

## Retained /cmd

Varies. ant-switch-bridge `/cmd` is retained (idempotent position). ultrabridge
frequency/band/direction retained; retract one-shot (bridge clears after exec).
wrcrotor `set_az`, atr1k `set_inline`/`tune`, acom1200s-pa-bridge `set_band`/`set_power`,
antennaselect `request` are NOT retained. The UI exposes a "retain" checkbox defaulting **off**.

## UI command-builder rules

1. Read `/meta.expose` per slot. For each `fields[]` with `writable:true` + non-empty
   `command`: build a setpoint input. Payload:
   - `command.action != ""` → `{"action":<action>, <value_key>:<typed>}`
   - `command.action == ""` → `{<value_key>:<typed>}` (value-key-only)
   - coerce per `value_type`; enum options from inline `options` or `options_ref` into
     `capabilities` (bands/modes/directions/tune_modes/ladder).
2. For each `actions[]`: a button → `{"action":<action>}` (+ value if `value_key` set).
3. Hide the command panel for roles with no writable expose and no actions AND no
   registry entry (radio, discovery).
4. Role-registry fallback (only when expose has no writable field/action):
   - `reconciler` → `{"request":"<off|port1..port6|auto>"}` (operator hold; `auto` releases).
   - (ant-switch/pa/ant-ctrl/rotator/tuner are all expose-driven; no fallback needed.)
5. **Never hardcode `"value"`** — ultrabridge `freq_hz` and wrcrotor `az` would break.

## Historical note

atr1k-tuner-bridge originally keyed `set_inline` under `inline` and `tune` under `mode`
(the live bug recorded in memory `cmd-payload-value-key-convention`). It is now fixed
(both use `value`). Do not code to the old shape.