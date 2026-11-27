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
| Protocol | MQTT 3.1.1 (plain TCP; `deploy.sh` seeds `tcp://127.0.0.1:1883`, the station broker on scmino) |
| Authentication | Username/password (`hf` user) |
| Clean session | true (since 2026-10-18); own `/cmd` is re-subscribed on every reconnect |
| Auto-reconnect | yes |
| Client ID | `muehle-hf-power-seq` (configurable via `mqtt.client_id`) |

The initial connect is ctx-aware: a SIGTERM while the broker is unreachable
interrupts the connect (it does not hang until systemd SIGKILL).

The session is clean (`CleanSession=true`, since 2026-10-18): every input is
retained and replays on each subscribe, and a resumed persistent session on
the HA broker (then the station broker) stopped delivering QoS-1 messages. The
**own `/cmd` subscription is QoS 0** on purpose: with a persistent session a
QoS-1 subscription would let the broker queue a `/cmd` published while powerseq is offline and replay it on
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
| `/state` | yes | sequencer → bus | `{ phase, step, fault?, stop_reason?, chain?, ts }` |
| `/status` | yes | broker LWT | liveness of **the service**: `online` / `offline` |
| `/cmd` | **no** | bus → sequencer | operator one-button: `start` \| `stop`; the 75AUTO Start: `auto_start` |

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
  "host": "scmino",
  "capabilities": {
    "controls": [
      "muehle/hf/pa-arm", "muehle/hf/switch",
      "muehle/power/antennas", "muehle/power/psu-13v8",
      "muehle/power/shack-pc", "muehle/power/shack-pc-mains"
    ],
    "watches": [
      "muehle/hf/ant-ctrl", "muehle/hf/ant-mode", "muehle/hf/antenna-select",
      "muehle/hf/pa", "muehle/hf/pa-arm", "muehle/hf/radio",
      "muehle/hf/rotator", "muehle/hf/switch",
      "muehle/power/antennas", "muehle/power/psu-13v8",
      "muehle/power/shack-pc", "muehle/power/shack-pc-mains",
      "muehle/uhf/az-rotator", "muehle/uhf/el-rotator", "muehle/uhf/radio"
    ]
  },
  "expose": {
    "device": { "name": "Station power sequencer" },
    "fields": [
      { "key": "phase", "name": "Phase", "type": "enum",
        "options": ["idle", "starting", "running", "stopping",
                    "entering_standby", "standby", "waking"] },
      { "key": "step",  "name": "Step",  "type": "string" },
      { "key": "fault", "name": "Fault", "type": "string" },
      { "key": "stop_reason", "name": "Stop reason", "type": "string" }
    ]
  }
}
```

`controls` and `watches` are **derived from the configured sequence**:
`controls` = the slots any `cmd` step targets (one writer); `watches` = every
slot the sequence references (`cmd` + `wait_status` + `wait_state` targets),
plus, with idle standby, the activity slot and the watch slots, the ant-mode
slot (with idle standby or an `[[auto_start]]` list), the 75auto slot
(`auto_op_slot`) and the PA slot (`pa_slot`, also in `controls`: it gets the
session-start standby, §6.3). The
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
{ "ts": "2026-10-19T03:12:00Z", "phase": "idle", "step": "", "stop_reason": "idle_shutdown" }
```

| Field | Type | Meaning |
|-------|------|---------|
| `ts` | string (RFC3339) | snapshot timestamp (UTC) |
| `phase` | `idle` \| `starting` \| `running` \| `stopping` \| `entering_standby` \| `standby` \| `waking` | sequencer state (`idle` = station stopped; `standby` = idle power-down, §6.1) |
| `step` | string (always present) | the current step within a sequence (e.g. `psu-on`); empty at idle/running/standby |
| `fault` | string, omitempty | why a sequence aborted; omitted when there is no fault |
| `stop_reason` | `operator` \| `idle_shutdown`, omitempty | why the station was last stopped: an operator `stop`, or the idle shutdown (§6.2); cleared by `start` |
| `chain` | object, omitempty | progress of the last `auto_start` (§6.3); absent before the first one |
| `chain.state` | `running` \| `ready` \| `failed` | `ready` = the last step passed; the page then sends the signed 75auto `start_session` |
| `chain.step` | string (always present) | the step name = the progress row: `station` while the startup or wake list runs first, then the `[[auto_start]]` names (`bwpc`, `aethersdr`, `dax`, `wsjtx`, `75auto` in the example). Empty for a refused `auto_start` |
| `chain.fault` | string, omitempty | why the chain failed or was refused (see §5) |
| `chain.requester` | string (always present) | the `requester` of the `auto_start` (the device id of the page) |
| `chain.ts` | string (RFC3339) | when `chain.state` last changed (accept, ready, failure) |

```json
{ "ts": "2026-11-27T09:00:05Z", "phase": "running", "step": "",
  "chain": { "state": "running", "step": "dax", "requester": "tablet-1", "ts": "2026-11-27T09:00:01Z" } }
{ "ts": "2026-11-27T09:02:10Z", "phase": "running", "step": "",
  "chain": { "state": "failed", "step": "aethersdr",
             "fault": "program_aethersdr=failed (program_aethersdr_error=not started by the chain)",
             "requester": "tablet-1", "ts": "2026-11-27T09:02:10Z" } }
```

The chain is separate from `phase`: a chain step that fails leaves `phase` as
it was (normally `running`), so standby and the idle shutdown keep working.
A failure in the startup or wake list that runs first is a station fault as
with `start` (`phase=idle` + `fault`), and `chain.fault` carries the same text.

`step` is always present (only `fault` is omitempty), per model §7.1 — a
consumer reading `state.step` gets `""` (not a missing key) at idle/running.
`phase=idle` with no `fault` is the resting state. `fault` is set when a
sequence aborts (a step timed out or failed, the service was stopped
mid-sequence: `"<step>: interrupted"`, or it was killed mid-sequence, see
below); the slots driven so far remain in whatever state the retained `/cmd`
last set them (self-healing from the steady-state intent).

**Across a restart** (deploy, crash, power cut of the Pi) the phase is kept: the
sequencer writes it to its state file (`state_file`, default
`/var/lib/powerseq/state.json`) and the first `/state` after the restart is the
restored phase, not `idle`. No `/cmd` is sent because of a restart.

| Phase at the restart | First `/state` after it |
|---|---|
| `idle`, `running`, `standby` | the same phase, with its `fault` and `stop_reason`; the standby timeout and the idle-shutdown clock keep counting from the real events (§6.1, §6.2) |
| `starting`, `stopping`, `entering_standby`, `waking` (killed mid-sequence) | `phase=idle`, `fault="<step>: interrupted by a powerseq restart (<phase>)"`, `stop_reason` kept. The sequence is **not** re-run; `start` or `stop` recovers |
| `standby`, but `[[standby]]` no longer configured | `phase=idle` with a fault |
| none (missing or corrupt state file) | `phase=idle` (logged as Error) |
| any, with `chain.state=running` (killed during an `auto_start`) | `chain.state=failed`, `chain.fault="interrupted by a powerseq restart"`. The chain is not resumed and no `/cmd` is sent. A `ready` or `failed` chain comes back as it was |

So a consumer that sees `idle` after a powerseq restart can take it at face
value: an `idle` caused by the restart itself always carries a `fault`.

On (re)connect the runner re-publishes the retained `/state` (in addition to
`/meta` and `/status=online`), so a broker wipe that drops retained messages
restores an idle sequencer's `/state` instead of leaving it absent.

---

## 5. `/cmd` — operator one-button

**Not retained.** Accepted payload (argument under `action`):

```json
{ "action": "start" }
{ "action": "stop" }
{ "action": "auto_start", "value": { "requester": "tablet-1" } }
```

- `start` is honored when `phase=idle` (runs the startup sequence) or
  `phase=standby` (runs the **wake** sequence — the station is already energized).
- `stop` is honored when `phase=running`, `phase=standby` or `phase=idle`; runs the shutdown
  sequence. An idle stop is the teardown path for slots a faulted sequence
  left energized: it does not depend on the fault latch (lost with a missing
  or corrupt state file), and the shutdown re-states "off" — it never
  energizes.
- A command that arrives mid-sequence (e.g. `start` while `starting`) is
  dropped and logged; aborting an in-progress sequence is deferred. The
  exception is a running `auto_start` chain: `stop` ends it at once (its own
  context, not the command queue), `chain.state=failed` with `stopped by the
  operator` is published before the shutdown runs, and no `ready` follows.
  This also holds while the chain runs its startup or wake list, right after
  the chain turned `running` (before its first step), and for an
  `auto_start` that is queued but not yet started (nothing of it is sent). A
  `stop` while `chain.state=ready` also turns the chain `failed` with
  `stopped by the operator`, so a page that waits to send `start_session`
  does not start a session on a stopped station. A stop never blocks the next
  `auto_start`.
- `auto_start` runs the 75AUTO Start chain (§6.3). `value.requester` is echoed
  as `chain.requester`. What runs depends on the phase:

  | Phase / condition | Result |
  |---|---|
  | `idle` | the startup list, then the chain |
  | `standby` | the wake list, then the chain |
  | `running` | the chain only |
  | `starting`, `stopping`, `entering_standby`, `waking` | refused: `chain.fault="busy: <phase>"` |
  | a chain runs | refused, `/state.chain` stays unchanged (logged only) |
  | ant-mode `storm` or `maintenance` | refused: `chain.fault="antenna mode <mode>"` |
  | a 75auto session is active (§6.3) | refused: `chain.fault="busy: session active"` (a program restart would break it) |
  | no `[[auto_start]]` configured | refused: `chain.fault="auto_start not configured"` |

  Every refusal except "a chain runs" publishes `chain.state=failed` with the
  reason, so the page always gets an answer.

`start` / `stop` are published by an operator (or HA as one writer) directly to
`muehle/hf/power-seq/cmd`. Example:

```bash
mosquitto_pub -h 192.168.1.178 -u hf -P "$MQTT_PASSWORD" -t 'muehle/hf/power-seq/cmd' \
  -m '{"action":"start"}'
```

---

## 6. Sequences

The startup and shutdown sequences are **config-driven** step lists. Each step
is one of five kinds:

| Kind | Action | Fields |
|------|--------|--------|
| `cmd` | emit a retained `/cmd` to a slot | `slot`, `action`, `value` (string), `retain?` (default true) |
| `wait_status` | wait until every listed slot's `/status` == `state` (`online` is two-layer since 2026-10-18: `/state.device_online` must not be `false` either) | `slots` (AND), `state?` (`online`\|`offline`, default `online`), `hold_ms?`, `timeout_s?` |
| `wait_state` | wait until a slot's `/state` field == `value` | `slot`, `field`, `value`, `fresh?`, `fail_value?`, `hold_ms?`, `timeout_s?` |
| `delay` | sleep a fixed duration | `duration_s` (literal) **or** `duration` (`network`\|`stagger`, refs `[timing]`) |
| `wol` | send a Wake-on-LAN magic packet (UDP) | `mac`, `broadcast?` (`host:port`, default `255.255.255.255:9`), `poke?` (`host:port`, one TCP connect after the packet) |

`slot` addresses are site-relative (e.g. `power/psu-13v8` → `muehle/power/psu-13v8`).
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

`wait_state` options: `fresh = true` accepts `value` (or `fail_value`) only
after a `/state` of the slot received after the step started has shown the
field at a non-terminal value (any value other than `value` and
`fail_value`, e.g. `starting`). The actuator always publishes `starting`
first, so neither a retained value from an earlier run nor a `/state` tick
queued before the command can pass the step. When the step right before is a
`cmd` to the same slot, the step starts at that `cmd` (the `starting` can
arrive before the wait runs). `fail_value = "<v>"` fails the step at once when the
field takes that value, with the reason `<field>=<v>`. A `wait_state` failure
or timeout names the `<field>_error` sibling when the `/state` has one, e.g.
`program_dax=failed (program_dax_error=no window)`.

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
| `pc-mains-on` | `cmd` | `power/shack-pc-mains` `set_power` `on` (never switched off by the sequencer) |
| `psu-on` | `cmd` | `power/psu-13v8` `set_power` `on` |
| `psu-settle` | `delay` | 5 s |
| `pc-boot` | `wol` | Wake-on-LAN to the bwpc MAC (no-op when the PC already runs) |
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
`trx-off` → stagger → `psu-off`. The site master mains is out of scope. `power/shack-pc-mains`
is not in the shutdown: cutting mains hard-powers-off Windows.

### 6.1 Idle standby (`[[standby]]` / `[[wake]]`, optional)

Configured with an `[idle]` block; omit all three to disable.

- **Standby** (from `running`): `[idle].activity_slot` (`hf/antenna-select`) is
  fit and reports `/state.activity = "inactive"` (it has grounded the antenna),
  no `[[idle.watch]]` field moved within `timeout_minutes`, and `running` has
  lasted at least `timeout_minutes` (no standby right after a start or wake).
  → `entering_standby` → `[[standby]]` → `standby`.
- **Wake** (from `standby`): the activity slot reports a `last_activity` newer
  than the standby entry (VFO change, TX, operator hold, a live station hold
  such as bmrec's HF lab recording — antennaselect
  publishes no `last_activity` after a restart, so a restart never wakes), a
  watch moves, a live 75auto session is active (§6.3), or `start` arrives.
  → `waking` → `[[wake]]` → `running`.
  antennaselect reconnects the antenna on the same activity.
- A watch is a `/state` field (`slot`, `field`, `min_delta?`): a change of at
  least `min_delta` (numeric) since the last counted value is activity. Changes
  while the slot is offline / `device_online=false`, and a field appearing or
  vanishing, are not — so the antenna power going off is not read as activity.
  Nor is a change during a sequence or within `[idle] settle_s` (default 30)
  of any phase change: it re-baselines the watch. The UHF rotator controllers
  keep `device_online=true` but report a stepped position when the antenna
  supply goes off; counted, that woke every standby ~2 s after entry
  (2026-10-19). A wake logs its reason (`wake: watch moved (slot.field a→b)`
  or `wake: <activity slot> last_activity <ts>`).
  The defaults watch UHF use (`uhf/radio.freq_hz`, `uhf/az-rotator.az`,
  `uhf/el-rotator.el` with a 2° deadband against the el readback dither).
- Idle is evaluated only in `running` and only with the broker connected. A
  fault in either list → `phase=idle` + `fault`, like startup.
- A powerseq restart keeps `running`/`standby` and the time of the last entry
  into them (§4), so a station restored in `running` stands by as usual. After
  the restart every watch takes its first value as a new baseline, and changes
  within `settle_s` are not activity.
- **Antenna-mode power hold** (`[mqtt] ant_mode_slot`, default `ant-mode` →
  `muehle/hf/ant-mode/state`, contract `../../antmode/docs/ant-mode-mqtt-api.md`):
  `/state.power_hold` tells powerseq to hold its idle logic. antmode sets it
  in three cases: while it borrows `power/antennas` to park the HF beam with
  the station in standby (a borrow); for the whole maintenance mode, borrowed
  or not (no power cut and no wake power-up with someone at the mast); and in
  storm mode until the park is `parked` or `failed` (a standby must not cut
  the gear mid-park). The hold is independent of the powerseq phase. A
  display reads antmode's `power_borrow`, not `power_hold`, for "antenna power
  borrowed". While `power_hold` is `true`, a watch change only re-baselines
  the watch (the borrowed power or the work at the mast moves the UHF rotator
  readbacks) and is never activity, and no standby, wake or idle shutdown
  starts (a `start`/`stop` from the operator still works). When the hold ends
  a fresh `settle_s` window starts, so the readback step when antmode switches
  the power back off is not activity either. The end of a hold also restarts
  the idle-shutdown clock (watch changes during a hold are not counted, and a
  maintenance hold can last hours with the operator on site), so the idle
  shutdown comes `shutdown_minutes` after the hold ends at the earliest. A
  standby or wake that became due during the hold runs after it. After a
  broker (re)connect the idle logic waits up to 3 s for the retained ant-mode
  `/state` (a borrow can start while powerseq is disconnected). Reading
  rules: no retained ant-mode state (or an empty one) = no hold; a malformed
  state keeps the last known hold (logged as Error); a valid hold is honored
  even while `hf/ant-mode/status` is offline. powerseq never locks a slot for antmode and
  antmode never changes powerseq's phases. Read only with idle standby
  configured; `ant_mode_slot = ""` disables it.

| List | Steps |
|------|-------|
| standby | `antennas-off` (`power/antennas` off) → `pc-sleep` (`power/shack-pc` `sleep`, **`retain = false`** — a retained sleep would replay on wake) |
| wake | `pc-wake` (`wol`, shack-PC MAC) → `antennas-on` → `wait-antenna-gear-online` (`hf/ant-ctrl` + `hf/rotator` online, 120 s) |

### 6.2 Idle shutdown (`[[idle_shutdown]]`, optional)

Configured with `[idle] shutdown_minutes` (> `timeout_minutes`; default
example 120) plus a `[[idle_shutdown]]` list; omit both to stay in standby
indefinitely.

- **Trigger** (from `standby` only, broker connected, no wake due): the last
  activity is `shutdown_minutes` ago. The last activity is the latest of the
  activity slot's `last_activity`, the last counted watch change, and the last
  entry into `running` (a start or a wake). An antennaselect restart publishes
  no `last_activity`, so it does not reset the clock. A powerseq restart does
  not reset it either: the clock's inputs are kept in the state file (§4). The
  idle shutdown waits until the activity slot's `/state` has been received
  (after a restart, until its retained `/state` is re-delivered), because its
  `last_activity` can be the latest activity.
- → `stopping` → `[[idle_shutdown]]` → `idle` with `stop_reason =
  "idle_shutdown"`. Only `start` revives the station (a full cold start).
- The default list is the `[[shutdown]]` list plus `pc-sleep` (`power/shack-pc`
  `sleep`, `retain = false`; the PC agent refuses while someone uses the PC).
  The manual `stop` does not sleep the PC.
- Limits: UHF TX is not visible on the bus (the IC-9700 bridge is receive-only),
  so UHF use counts only through the watches (frequency, rotator position).

### 6.3 The `auto_start` chain and the 75auto session (`[[auto_start]]`, optional)

The 75AUTO Start of hf_console publishes `auto_start` (§5). powerseq runs the
startup or wake list when needed, then `[[auto_start]]`, an ordered step list
of the same kinds, validated like the others. Consecutive steps may share a
name: the name is the progress row the page shows (`chain.step`). The chain
runs under a context of its own, so `stop` ends it at once. The example list
(`config.example.toml`):

| Row | Steps |
|---|---|
| `bwpc` | `wol` with `poke` (same MAC and poke as `pc-boot`: bwpc enters Modern Standby by itself while the station stays `running`); `wait_status power/shack-pc` (300 s, a cold boot and the auto logon) |
| `aethersdr`, `dax`, `wsjtx`, `75auto` | `cmd power/shack-pc ensure_program <name>` with `retain = false` (winpc-power-bridge drops retained commands); `wait_state program_<name> = ready` with `fresh = true`, `fail_value = "failed"` |
| `75auto` | `wait_status hf/auto-op` (75auto publishes its slot at program start) |

**Session hold.** powerseq reads `hf/auto-op` (`[mqtt] auto_op_slot`,
default `auto-op`; `""` disables the session hold, the `busy: session active`
refusal and the PA standby). A session is **active** while the last
`/state` has `session.source` `live` and `session.state` `connecting` or
`running`, and the last `/beat` or
live `/state` is younger than `[mqtt] auto_op_beat_s` (default 30 s; 75auto
beats every 2 s in a session). A retained `/state` (at a subscribe) can be
old and does not count as a beat, so a crashed 75auto does not hold the power.
A `replay` session (or one without `source`) is not active: it does not
transmit, so it gives no hold, no PA standby and no `busy: session active`
(75auto holds the station by the same rule). The standby and the idle
shutdown hold while a session is active, while a chain runs, and for 30 min
after `chain.state` turns `ready` (the page needs that long at most to send
its `start_session`). The hold does not stop a wake: in `standby` an active
session is activity and wakes the station (75auto can start a session on
bwpc without the chain, and must not transmit with the antenna gear off). An accepted `auto_start` is activity. When the hold ends,
both idle clocks restart: standby waits a full `timeout_minutes` and the idle
shutdown a full `shutdown_minutes` from that moment. An `[[idle.watch]]`
cannot do this, because it counts only changes.

**PA standby.** When a live `hf/auto-op/state` turns `session.state` from not
active to `connecting` or `running`, powerseq sends
`{"action":"set_mode","value":"standby"}` to `hf/pa/cmd` (`[mqtt] pa_slot`,
default `pa`; **not retained**) once for that session. The operator can then
put the PA in operate; nothing switches it back during that session. Only a
live session (`session.source` `live`) makes the edge. A new
session after `ended` gets a new standby. A retained `/state` only sets the
baseline, so a powerseq restart or a reconnect during a session sends nothing.
This also covers a session started at the bwpc window.

All emitted `/cmd` use the stationa value-key convention
(`{"action":"<act>","value":"<val>"}`) and are **retained** (the steady-state
self-healing exception, model §8) — the controlled slots re-apply the last
intent on their own reconnect. A step may set `retain = false` for a one-shot
command.

---

## 7. Watched topics (subscribed)

Subscriptions are **derived from the configured sequence** (not hard-coded): the
`/status` of every slot any step references (a `cmd`, `wait_status`, or
`wait_state` target), plus the `/state` of every `wait_state` target and every
`wait_status` `online` target (for its `device_online`), plus the
sequencer's own `/cmd`. For the default §7.1 sequence this yields:

| Topic | Used for |
|-------|----------|
| `muehle/power/psu-13v8/status` | observability (controlled slot) |
| `muehle/hf/switch/status` + `/state` | `wait-plc-online` liveness gate |
| `muehle/hf/pa-arm/status` | observability (controlled slot) |
| `muehle/hf/radio/status` + `/state` | `wait-radio-online` gate (radio online) |
| `muehle/hf/ant-ctrl/…`, `muehle/hf/rotator/…` (`/status` + `/state`) | wake `wait-antenna-gear-online` gate |
| `muehle/hf/pa/status` | `wait-pa-power-on` liveness precondition |
| `muehle/hf/pa/state` | `wait-pa-power-on` (`.power` = `on`) |
| `muehle/power/antennas/status` | observability (controlled slot) |
| `muehle/power/shack-pc-mains/status` | observability (controlled slot) |
| `muehle/power/shack-pc/status` | observability (standby `pc-sleep` target) |
| `muehle/hf/antenna-select/status` + `/state` | idle standby trigger (`activity`, `last_activity`) |
| `muehle/uhf/radio/…`, `muehle/uhf/az-rotator/…`, `muehle/uhf/el-rotator/…` (`/status` + `/state`) | `[[idle.watch]]` activity sources |
| `muehle/hf/ant-mode/state` | antenna-mode `power_hold` (§6.1) and the `auto_start` refusal in `storm`/`maintenance` (§5); with idle standby or `[[auto_start]]`, and `ant_mode_slot` set |
| `muehle/hf/auto-op/state` + `/beat` (+ `/status`) | 75auto session hold and PA standby edge (§6.3; `auto_op_slot`), the `75auto` row's `wait_status` |
| `muehle/power/shack-pc/state` | `program_<name>` of the `auto_start` chain (§6.3) |
| `muehle/hf/power-seq/cmd` | operator one-button `start` \| `stop`, and `auto_start` (not retained) |

A `/status` of `online`/`offline` sets the slot's liveness (absent ≠ offline —
`wait_status state="offline"` needs an actual offline payload); the runner polls
these with a step timeout (`timing.step_timeout_s`, default 120 s, overridable
per step via `timeout_s`). Adding a wait target in TOML extends this set with no
Go change.