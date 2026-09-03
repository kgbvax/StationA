# Salvage: antennaselect.md
> Extracted from PRD/03-components/antennaselect.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [decision] Ultrabeam / fan-dipole wiring-map port contradiction (PRD §18.1, §16.6)

The repo disagrees with itself on the switch-port wiring (PRD text, 2026-08-29):

- `antennaselect/config.example.toml` and the deploy seed map `ultrabeam` → **`port4`**
  and `fan-dipole` → **`port6`**.
- The unit tests, the repo-top-level CLAUDE.md, and the integration-model docs say the
  Ultrabeam is on **`port3`**. (The console UI's antenna map also says `port4`.)
- The integration model's own passive-resource list once said `ant/fan-dipole` 80/40 is
  **port 2**, contradicting every other source (which all say `port6`).
- `port1 = dummy-load` is consistent everywhere.

Re-verified 2026-09-03: `antennaselect/config.example.toml` still says
`port4 = "ultrabeam"`, `port6 = "fan-dipole"` (ports 2/3/5 commented out), while the
root `CLAUDE.md` slot table still says `ant/ultrabeam` port 3 and `ant/fan-dipole`
port 6. **Still unresolved in the repo.**

The live `/etc/antenna-select/config.toml` on shari is authoritative; it was seeded as
`port4` and may have been hand-edited since. It was not readable from the workstation
when the PRD was written. Consequence: the port mapping is pure configuration — nothing
may hard-code it, and **every** wiring-map port number (not only the ultrabeam port)
must be confirmed against `/etc/antenna-select/config.toml` on shari before any
acceptance test asserts a port.

## [decision] `device_online` field form (PRD §18.2)

The integration model says the `device_online` state field is "omitted when true"; the
deployed bridges publish `device_online: true` explicitly. Consumers (antennaselect
included) must accept both — absence = `true`. Whether a rebuilt bus mandates
explicit-true or omission-when-true is a system-wide open decision (integration model /
interface spec). Noted 2026-08-29 as one of the M0 decisions still needing on-device
resolution.

## [decision] Tuner `atu_bands` includes `80m` (PRD §18.5)

The deployed `tuner_follow.atu_bands` is `["30m","60m","80m","160m"]`, yet documents
describe the fan dipole as resonant on 80 m (and a config comment lists the list as
"non-resonant bands"). The deployed configuration is authoritative. Whether 80 m
genuinely needs the ATU in line on this antenna is a physical fact that cannot be
checked from the repo — needs the human at the device.

## [decision] Idle-over-operator precedence and remaining open decisions (PRD §18.6–§18.8)

- **Tier 1 (idle) overriding a stale operator hold** is deliberate walk-away safety but
  surprising to operators who expect a manual hold to stick. A redesign must either
  preserve it (documented) or introduce an explicit presence model — never silently
  change precedence.
- No committed design exists yet for: a TX-deferral timeout, restart-stable idle state,
  operator-request validation, `settled`-gating, or a "reconciler offline" indication
  beyond the plain `/status` LWT (recommended but unimplemented). The fix *directions*
  are known; each needs a committed design decision.
- Broker-topology decision (PRD §18.3, production = `192.168.1.50:1883` at PRD time) is
  RESOLVED: the shack-local broker on shari was merged to main 2026-08-29 and
  `deploy.sh` now defaults to `tcp://127.0.0.1:1883`. Dropped.

## [defect] First key-up after grounding transmits into the grounded switch (PRD §16.1)

PRD evidence: re-arming activity is *itself* a TX. The select the reconciler wants in
that same resolution stays TX-deferred (cold-switch rule). So the first transmission
after an idle ground keys the radio into the grounded/short port; the select fires only
at un-key, when `tx=="rx"` arrives. (The PA stays disarmed through a separate
`antenna_ready` chain, so the amplifier does not key — but the radio keys into a short.)
Structural gap: recovery from ground had no non-TX trigger.

Verdict 2026-09-03: **still open** in the auto path. The operator-hold-as-presence fix
(`applyOperatorHold` in `antennaselect/internal/mqtt/client.go`) gives a *manual*
non-TX re-arm, but an operator who just keys the mic still transmits into the short;
memory note "antenna grounding recovery gaps" (2026-08-29) confirms this live.

## [defect] Change-only radio-state publishing starves recovery; PA-arm heartbeat (PRD §16.2)

PRD evidence: the radio bridge publishes `radio/state` only on content change (no
periodic heartbeat). During a quiet receive period no messages arrive; the reconciler
cannot distinguish "idle-but-alive" from "abandoned". Also a separate consumer — the
PA-arm relay controller, which needs a `radio/state` message within its 10 s heartbeat
window — silently drops its arm and cannot re-arm. The reconciler cannot fix this
alone; the documented fix is a periodic radio-state heartbeat (about ≤5 s while the
link is live) on the radio bridge.

Verdict 2026-09-03: **still open** — the flexbridge-side heartbeat fix is built but not
running (memory: m5stamp/flexbridge "radio feed stale" fix built but not deployed).

## [defect] TX deferral has no timeout (PRD §16.4, §9)

PRD evidence: a port change is withheld while `radio/state.tx == "tx"` (log line:
`port change to "X" deferred: radio is transmitting (cold-switch)`); it fires on the
next input change that finds the radio back in `rx`. Current behavior has **no
timeout** on the deferral: a frozen `tx=="tx"` (e.g. a radio bridge that dies mid-TX
leaves stale retained state) freezes *all* actuation — idle grounding, operator holds,
everything — until a fresh `tx=="rx"` arrives.

Verdict 2026-09-03: **still open** — `Reconciler.Next()`
(`antennaselect/internal/reconcile/reconcile.go`) sets `DeferredForTX` with no timer
and no timeout anywhere in the codebase.

## [defect] Operator hold targets go unvalidated (PRD §16.8, §4)

PRD evidence: any non-empty, non-`auto` string on `antenna-select/cmd`
(`{"request": …}`) engages a hold with that literal string as target. The reconciler
forwards it verbatim as `{"select":"<garbage>"}` and never rejects unknown
ports/resources. (The empty/absent payload is treated as a release.)

Verdict 2026-09-03: **still open** — `holdActive` accepts any non-empty non-`auto`
string and `Resolve()` returns `in.OperatorRequest` as the target unchanged; no
wiring-map validation exists.

## [defect] Restart always re-arms the station and resets the walk-away clock (PRD §16.5, §6.3)

PRD evidence: at process start the idle clock is initialized to "now" and the last-seen
frequency to 0, so the retained `radio/state` replay at startup always looks like a
frequency change. A restart during an unattended idle period *un-grounds* the station
and resets the 30-minute walk-away clock for a full period.

Verdict 2026-09-03: **still open** — `antennaselect/internal/mqtt/client.go`
initializes `lastActivity = time.Now()` and `RadioFreqHz = 0`; idle state is not
persisted or derived from the last `radio/state` timestamp.

## [defect] `settled` received but unused; `radio/state.tuning` not an input (PRD §16.7)

PRD evidence: the switch's `settled` (movement-complete) flag arrives but no logic
gates on it; a settled-wait handshake is listed as backlog (to land with the
ant-switch bridge). Likewise `radio/state.tuning` is not yet an input.

Verdict 2026-09-03: **still open** — `SwitchSettled` is parsed into `reconcile.Inputs`
but `Next()` never reads it; matches the live
`antennaselect/docs/antenna-select-mqtt-api.md` §6, which documents gating as backlog.

## [defect] Queue drops and publish-failure dedup advance (PRD §16.9, §16.10)

Two residual behaviors, each deliberate but with costs a redesign should weigh:

- When the bounded job queue (capacity 256) is full, incoming jobs are silently dropped
  (by design, so a message handler never blocks). A dropped operator command is
  invisible until the next event on that topic. A drop is recoverable because the next
  message on that topic re-arms the same idempotent re-resolution.
- A failed intent publish is logged and never retried; the dedup marker has already
  advanced, so the same re-resolution does not re-emit. Recovery relies on a later
  input change or the reconnect retained replay.

Verdict 2026-09-03: **still open by design** (bounded queue in `shared/mqtt`; no
publish retry path). Also: PA `set_band` is *not* gated on station activity or TX — it
fires whenever the radio is online and reporting a band, even while idle-grounded
(harmless to RF safety; deliberate per the live MQTT-API doc).

## [requirement] Recovery fix requirements a rebuild must include (PRD §6.4)

Three items were the deliberate exceptions to the "indistinguishable on the bus"
rebuild goal — a rebuild must include these fixes. (Item 1 is now implemented; items
2–3 remain open.)

1. ~~**A non-TX re-arm path must exist.**~~ — FIXED: an operator hold on
   `antenna-select/cmd` is now treated as operator *presence* — `applyOperatorHold`
   (`internal/mqtt/client.go`) resets the idle clock and marks the station active
   without RF, so a hold works as a manual re-arm while the radio link is down or
   silent. Walk-away safety preserved: the idle check re-grounds after
   `[idle].timeout_minutes` with no further hold or radio activity. A release (`auto`)
   resets nothing (it is not evidence of presence).
2. **Radio heartbeat constraints (the heartbeat itself is a requirement on the radio
   bridge, out of this component's scope).** Requirements on this component:
   (a) a `radio/state` message whose content equals the last-seen state must not reset
   the idle clock or re-arm activity (an added heartbeat cannot defeat idle grounding);
   (b) the reconciler must stay correct when the radio bridge publishes state only on
   change — the design must not depend on the presence of a heartbeat;
   (c) the heartbeat (periodic radio-state publish, about ≤5 s while the link is live)
   is a hard requirement on the radio bridge (flexbridge).
3. **Restart-stable idle state.** The reconciler must persist the idle clock (or derive
   it from the last `radio/state` timestamp) across restarts, so a restart during an
   unattended idle period does not un-ground the antennas.

## [requirement] Decision-ladder rules and rationale not stated in the live docs (PRD §5–§6)

Normative ladder rules and their rationale beyond what `antennaselect/CLAUDE.md` and
`antennaselect/docs/antenna-select-mqtt-api.md` state:

- **Determinism requirement:** tier-3 resolution scans the configured
  `band_policy.bands` in **sorted resource order** — a determinism requirement, not an
  incidental implementation detail.
- **Unknown activity state is treated as `active`** — fail toward the less drastic
  tier.
- **`mode` must be `manual` whenever a hold is active, even when tier 1 wins the
  target.** An idle-grounded station with a stale hold reports
  `mode:"manual"`, `source:"idle"`, `target:"off"`.
- **A tier-2 operator hold asserts while the radio is offline** — a hold does not
  depend on radio liveness.
- **Empty-target publish rule.** The decision triple is published on change even when
  the target is the internal empty "hold last selection" marker. `target:""` appears on
  `antenna-select/state` in two cases: (1) as the first publish after a process start
  with unresolved inputs, and (2) on every later transition into hold-last (radio
  offline, empty band). This must be kept to stay indistinguishable on the bus.
- **Empty-band-hold rationale:** resolving the empty band (`""`, flexbridge's
  transient "reconnecting, no slice reported yet" state) to the fallback chattered the
  antenna on every reconnect cycle. The reference regression tests cover this rule and
  must stay.
- **Radio offline likewise holds the last selection** — no chatter, no grounding from
  liveness alone.
- Two-layer liveness gate rationale, from a live incident: earlier code keyed on
  `/status` alone and **chattered the antenna** — a bridge-up-but-radio-link-down
  window carries a stale or empty-band `radio/state`, and the reconciler flapped the
  antenna to the fallback and back. The AND gate and the empty-band-hold rule are the
  two halves of that fix. If either liveness layer drops *after* a selection, the
  reconciler must hold the last selection (no chatter, no grounding from liveness
  alone).
- The "reconciler is never part of the RF-safety enforcement path" framing: the
  hardware interlock chain enforces safety (TX-inhibit, PA-arm, cold-switch hard
  limits); the reconciler owns *ordering and intent* only. Killing it degrades the
  station to manual operation but cannot make it unsafe.

## [unique] Rebuild constraints worth keeping (PRD §2.1, §13, §14, §17, §11)

- **Clean-session semantics are deliberate and load-bearing:** with clean session
  enabled, every (re)connect makes the broker drop the prior session and replay all
  retained input topics into the fresh subscriptions; the stateless reconciler re-seeds
  all inputs and re-emits follow intents after any outage. A persistent session does
  *not* replay retained messages for existing subscriptions — a reconciler on one wakes
  up blind.
- **Fixed input slot names (deliberate deviation):** the `radio` and `ant-switch` slot
  segments are hard-coded canonical role names (`slotRadio = "radio"`,
  `slotAntSwitch = "ant-switch"`); no config key exists. This is a deliberate,
  documented deviation from the station rule that slot names are configuration, never
  code. The commanded follow slots (`ant-ctrl`, `pa`, `tuner`) come from configuration.
- **Resource→port inversion excludes the `off = "grounded"` entry** — it is a switch
  position, not a routable resource.
- **Config-file absence semantics:** a missing *default-path* config file is tolerable
  (runs on built-in defaults + flags, keeping a local mock workflow working); a missing
  *explicitly requested* file, or any malformed file, is fatal. (A `[priority]` section
  in the example config is documentation only — the ladder order is fixed in code.)
- **Emission dedup markers** (all compared on the single worker): last published
  decision triple `(mode,target,source)`; `lastSelect` (a repeated `ant-switch/cmd`
  while the switch position is unknown / already matching; reset by any target change
  or switch-position mismatch — while `selected` is unknown the reconciler sends at
  most one command per target, then re-asserts on every mismatch; so it automatically
  re-asserts a manual override of the switch; a same-band frequency change emits no
  command); `lastFollowFreq`; `lastPaBand`; `lastTunerInline`.
- **Timing constants (defaults):** idle check interval 5 s (fixed); idle grounding
  timeout 30 min (`idle.timeout_minutes`, positive integer); job queue capacity 256
  (fixed); shutdown quiesce 250 ms (fixed). Grounding therefore lands within ~5 s after
  the timeout expires.
- **Reference regression tests worth re-implementing:** the handler non-blocking test
  (`TestOnRadioStateDefersReconcile`), the TX-defer test, the
  re-assertion-after-manual-override test, the same-band-no-command test, and the
  two-layer gate in all four liveness combinations plus both retained-replay arrival
  orders.