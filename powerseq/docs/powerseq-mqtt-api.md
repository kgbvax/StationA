# MQTT schema — powerseq

This document describes the MQTT interface exposed by **powerseq**, the station
startup/shutdown sequencer. It implements the `sequencer` role of the station
integration model (`../../docs/station-integration-model.md`, §4, §7.1) at slot
`muehle/hf/power-seq`. It is the authoritative on-the-wire contract — derived
from `internal/seq/seq.go` and `internal/mqtt/client.go`.

powerseq is a **logic slot** — no device. It subscribes state from the slots its
sequence references and emits ordered intent to them. It is one writer of those
slots but does not lock them — any channel stays directly toggleable for
troubleshooting while the sequencer is idle.

The sequence is **config-driven**: a pair of ordered step lists (`[[startup]]` /
`[[shutdown]]`) in `config.toml` define it, not Go code. The model §7.1 sequence
below is the default shipped in `config.example.toml`; edit the step lists to
change the order, targets, waits, or delays. The subscribed topics (§7) and the
`/meta` `controls`/`watches` (§3) are derived from the configured sequence.

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1 (plain TCP, `tcp://192.168.1.139:1883`) |
| Authentication | Username/password (`hf` user) |
| Clean session | false; own `/cmd` is re-subscribed on every reconnect |
| Auto-reconnect | yes |
| Client ID | `muehle-hf-power-seq` (configurable via `mqtt.client_id`) |

The initial connect is ctx-aware: a SIGTERM while the broker is unreachable
interrupts the connect (it does not hang until systemd SIGKILL).

The persistent session (`CleanSession=false`) keeps the `/status` and `/state`
subscriptions across reconnects so their last retained value replays. The
**own `/cmd` subscription is QoS 0** on purpose: a QoS-1 subscription would let
the broker queue a `/cmd` published while powerseq is offline and replay it on
reconnect — re-energizing the station, the exact case the "own `/cmd` not
retained" rule guards against. QoS 0 means the broker never queues it; a
`/cmd` issued while we are down is simply lost (correct for a one-shot
operator command).

---

## 2. Topic addressing

The sequencer's own address is `<site>/<station>/<slot>` with defaults
`muehle/hf/power-seq`:

```
muehle/hf/power-seq/meta
muehle/hf/power-seq/state
muehle/hf/power-seq/status
muehle/hf/power-seq/cmd
```

| Suffix | Retained | Direction | Purpose |
|--------|----------|-----------|---------|
| `/meta` | yes | sequencer → bus | birth certificate: capabilities + `expose` |
| `/state` | yes | sequencer → bus | `{ phase, step, fault?, ts }` |
| `/status` | yes | broker LWT | liveness of **the service**: `online` / `offline` |
| `/cmd` | **no** | bus → sequencer | operator one-button: `start` \| `stop` |

`/cmd` is **not retained** (model §8): it is a one-shot operator command. A
stale retained `start` replaying on service restart could re-energize the
station unexpectedly, so powerseq never publishes `/cmd` retained.

---

## 3. `/meta` — birth certificate

Retained. Published on every (re)connect.

```json
{
  "schema": "1.0",
  "role": "sequencer",
  "link": "none",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": {
    "controls": [
      "muehle/power/master", "muehle/power/psu-13v8",
      "muehle/hf/switch", "muehle/hf/pa-arm"
    ],
    "watches": [
      "muehle/power/master", "muehle/power/psu-13v8",
      "muehle/hf/switch", "muehle/hf/pa-arm",
      "muehle/hf/ant-switch", "muehle/hf/radio", "muehle/hf/pa"
    ]
  },
  "expose": {
    "device": { "name": "Station power sequencer" },
    "fields": [
      { "key": "phase", "name": "Phase", "type": "enum",
        "options": ["idle", "starting", "running", "stopping",
                    "entering_standby", "standby", "waking"] },
      { "key": "step",  "name": "Step",  "type": "string" },
      { "key": "fault", "name": "Fault", "type": "string" }
    ]
  }
}
```

`controls` and `watches` are **derived from the configured sequence**:
`controls` = the slots any `cmd` step targets (one writer); `watches` = every
slot the sequence references (`cmd` + `wait_status` + `wait_state` targets). The
fields are read-only sensors (a logic slot reacts to state and emits intent,
model §1) — the operator surface is the one-shot `/cmd`, which is not retained
and so not an exposed writable field.

---

## 4. `/state` — sequencer phase

Retained. Published on every phase/step/fault change.

```json
{ "ts": "2026-07-14T18:42:01Z", "phase": "starting", "step": "psu-on" }
{ "ts": "2026-07-14T18:42:31Z", "phase": "running", "step": "" }
{ "ts": "2026-07-14T18:50:02Z", "phase": "idle", "step": "", "fault": "psu liveness timeout (...)" }
```

| Field | Type | Meaning |
|-------|------|---------|
| `ts` | string (RFC3339) | snapshot timestamp (UTC) |
| `phase` | `idle` \| `starting` \| `running` \| `stopping` \| `entering_standby` \| `standby` \| `waking` | sequencer state (`idle` = station stopped; `standby` = idle power-down, §6.1) |
| `step` | string (always present) | the current step within a sequence (e.g. `psu-on`); empty at idle/running/standby |
| `fault` | string, omitempty | why a sequence aborted; omitted when there is no fault |

`step` is always present (only `fault` is omitempty), per model §7.1 — a
consumer reading `state.step` gets `""` (not a missing key) at idle/running.
`phase=idle` with no `fault` is the resting state. `fault` is set only when a
step times out; the slots driven so far remain in whatever state the retained
`/cmd` last set them (self-healing from the steady-state intent).

On (re)connect the runner re-publishes the retained `/state` (in addition to
`/meta` and `/status=online`), so a broker wipe that drops retained messages
restores an idle sequencer's `/state` instead of leaving it absent.

---

## 5. `/cmd` — operator one-button

**Not retained.** Accepted payload (argument under `action`):

```json
{ "action": "start" }
{ "action": "stop" }
```

- `start` is honored when `phase=idle` (runs the startup sequence) or
  `phase=standby` (runs the **wake** sequence — the station is already energized).
- `stop` is honored when `phase=running`, `phase=standby` or `phase=idle`; runs the shutdown
  sequence. An idle stop is the teardown path for slots a faulted sequence
  left energized: the in-memory fault latch does not survive a restart, and
  the shutdown re-states "off" — it never energizes.
- A command that arrives mid-sequence (e.g. `start` while `starting`) is
  dropped and logged; aborting an in-progress sequence is deferred.

`start` / `stop` are published by an operator (or HA as one writer) directly to
`muehle/hf/power-seq/cmd`. Example:

```bash
mosquitto_pub -h 192.168.1.139 -t 'muehle/hf/power-seq/cmd' \
  -m '{"action":"start"}'
```

---

## 6. Sequences

The startup and shutdown sequences are **config-driven** step lists. Each step
is one of five kinds:

| Kind | Action | Fields |
|------|--------|--------|
| `cmd` | emit a retained `/cmd` to a slot | `slot`, `action`, `value` (string), `retain?` (default true) |
| `wait_status` | wait until every listed slot's `/status` == `state` | `slots` (AND), `state?` (`online`\|`offline`, default `online`), `hold_ms?`, `timeout_s?` |
| `wait_state` | wait until a slot's `/state` field == `value` | `slot`, `field`, `value`, `hold_ms?`, `timeout_s?` |
| `delay` | sleep a fixed duration | `duration_s` (literal) **or** `duration` (`network`\|`stagger`, refs `[timing]`) |
| `wol` | send a Wake-on-LAN magic packet (UDP) | `mac`, `broadcast?` (`host:port`, default `255.255.255.255:9`) |

`slot` addresses are site-relative (e.g. `power/master` → `muehle/power/master`).
`value` is always a string (the value-key convention; model value_type is
string|int|float, no bool). For `wait_state`, `value` may be the empty string —
this waits for the field to **clear** (become absent/`nil`/`""`), which is a
legitimate wait the runtime supports. `wait_state` has an **implicit liveness
precondition** with two layers: the slot's `/status` must be `online` (a dead
bridge whose LWT fired cannot pass on a stale retained `/state`), and its
`/state.device_online` must not be `false` when the field is present (a dead
*device* behind a live bridge cannot pass either — model §3 two-layer liveness).
When a wait fails, check both layers: the fault string does not say which one
blocked. A `wait_state` on the sequencer's own slot is a config error.

`hold_ms` is optional (omitted → `[timing].default_hold_ms`; an **explicit `0`
means edge-triggered** even when a default hold is set). `timeout_s` is optional
(nil → `[timing].step_timeout_s`) and **must be > 0** if given. `duration_s`
(literal delay) **must be > 0** if given. `wait_status` `slots` entries must
each be non-empty. All of these are validated at load (fail fast, never at
runtime). A malformed `/state` snapshot drops the slot's prior snapshot (it
does not keep the stale value), so a good→malformed transition re-evaluates
against no field.

Phase transitions are implicit: entering the first startup step → `starting`,
completing the last → `running`; entering the first shutdown step → `stopping`,
completing the last → `idle`. A wait that times out → `phase=idle`, `fault =
"<step>: <reason>"`, **no rollback** (driven slots hold their last retained
`/cmd`). A broker disconnect gates `cmd` + wait steps (fault, no stall); `delay`
steps are local and continue.

### Default startup (`start`)

Cold start (2026-10-10): TRX PSU → 5 s → TRX → 1 s → PA → antenna power, then
the radio / PA confirmations before the PA is armed.

| Step | Kind | Emit / wait |
|------|------|-------------|
| `master-on` | `cmd` | `power/master` `set_power` `on` |
| `network-delay` | `delay` | ~30 s (`duration = "network"`) |
| `psu-on` | `cmd` | `power/psu-13v8` `set_power` `on` |
| `psu-settle` | `delay` | 5 s |
| `wait-plc-online` | `wait_status` | `hf/switch` `/status` = `online` — guard: `hf/switch/cmd` is ONE retained topic, so `set_trx` + `set_pa` published while the PLC boots would collapse to `set_pa` only |
| `trx-on` | `cmd` | `hf/switch` `set_trx` `on` |
| `trx-settle` | `delay` | 1 s |
| `pa-on` | `cmd` | `hf/switch` `set_pa` `on` |
| `antennas-on` | `cmd` | `power/antennas` `set_power` `on` |
| `wait-radio-online` | `wait_status` | `hf/radio` `/status` = `online` |
| `wait-pa-power-on` | `wait_state` | `hf/pa` `/state` `.power` = `on` (+ `/status` online) |
| `pa-arm-enable` | `cmd` | `hf/pa-arm` `set_enabled` `true` → `phase=running` |

### Default shutdown (`stop`)

Reverse, with short staggers (`duration = "stagger"`) for inrush → `phase=idle`:
`pa-arm-disable` → stagger → `antennas-off` → stagger → `pa-off` → stagger →
`trx-off` → stagger → `psu-off` → stagger → `master-off`.

### 6.1 Idle standby (`[[standby]]` / `[[wake]]`, optional)

Configured with an `[idle]` block; omit all three to disable.

- **Standby** (from `running`): `[idle].activity_slot` (`hf/antenna-select`) is
  fit and reports `/state.activity = "inactive"` (it has grounded the antenna),
  no `[[idle.watch]]` field moved within `timeout_minutes`, and `running` has
  lasted at least `timeout_minutes` (no standby right after a start or wake).
  → `entering_standby` → `[[standby]]` → `standby`.
- **Wake** (from `standby`): the activity slot reports a `last_activity` newer
  than the standby entry (VFO change, TX, operator hold — antennaselect
  publishes no `last_activity` after a restart, so a restart never wakes), a
  watch moves, or `start` arrives. → `waking` → `[[wake]]` → `running`.
  antennaselect reconnects the antenna on the same activity.
- A watch is a `/state` field (`slot`, `field`, `min_delta?`): a change of at
  least `min_delta` (numeric) since the last counted value is activity. Changes
  while the slot is offline / `device_online=false`, and a field appearing or
  vanishing, are not — so the antenna power going off is not read as activity.
  The defaults watch UHF use (`uhf/radio.freq_hz`, `uhf/az-rotator.az`,
  `uhf/el-rotator.el` with a 2° deadband against the el readback dither).
- Idle is evaluated only in `running` and only with the broker connected. A
  fault in either list → `phase=idle` + `fault`, like startup.

| List | Steps |
|------|-------|
| standby | `antennas-off` (`power/antennas` off) → `pc-sleep` (`power/shack-pc` `sleep`, **`retain = false`** — a retained sleep would replay on wake) |
| wake | `pc-wake` (`wol`, shack-PC MAC) → `antennas-on` → `wait-antenna-gear-online` (`hf/ant-ctrl` + `hf/rotator` online, 120 s) |

All emitted `/cmd` use the stationa value-key convention
(`{"action":"<act>","value":"<val>"}`) and are **retained** (the steady-state
self-healing exception, model §8) — the controlled slots re-apply the last
intent on their own reconnect. A step may set `retain = false` for a one-shot
command.

---

## 7. Watched topics (subscribed)

Subscriptions are **derived from the configured sequence** (not hard-coded): the
`/status` of every slot any step references (a `cmd`, `wait_status`, or
`wait_state` target), plus the `/state` of every `wait_state` target, plus the
sequencer's own `/cmd`. For the default §7.1 sequence this yields:

| Topic | Used for |
|-------|----------|
| `muehle/power/master/status` | observability (controlled slot, model §7.1) |
| `muehle/power/psu-13v8/status` | observability (controlled slot) |
| `muehle/hf/switch/status` | `wait-controllers-online` liveness gate |
| `muehle/hf/pa-arm/status` | `wait-controllers-online` liveness gate |
| `muehle/hf/ant-switch/status` | `wait-controllers-online` liveness gate |
| `muehle/hf/radio/status` | `wait-radio-online` gate (radio online) |
| `muehle/hf/pa/status` | `wait-pa-power-on` liveness precondition |
| `muehle/hf/pa/state` | `wait-pa-power-on` (`.power` = `on`) |
| `muehle/power/antennas/status` | observability (controlled slot) |
| `muehle/power/shack-pc/status` | observability (standby `pc-sleep` target) |
| `muehle/hf/antenna-select/status` + `/state` | idle standby trigger (`activity`, `last_activity`) |
| `muehle/uhf/radio/…`, `muehle/uhf/az-rotator/…`, `muehle/uhf/el-rotator/…` (`/status` + `/state`) | `[[idle.watch]]` activity sources |
| `muehle/hf/power-seq/cmd` | operator one-button `start` \| `stop` (not retained) |

A `/status` of `online`/`offline` sets the slot's liveness (absent ≠ offline —
`wait_status state="offline"` needs an actual offline payload); the runner polls
these with a step timeout (`timing.step_timeout_s`, default 120 s, overridable
per step via `timeout_s`). Adding a wait target in TOML extends this set with no
Go change.