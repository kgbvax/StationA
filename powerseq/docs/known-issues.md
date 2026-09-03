# Salvage: powerseq.md
> Extracted from PRD/03-components/powerseq.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [defect] Deploy seed config omits the step lists — first deploy crash-loops

The config file that the deploy script seeds on a fresh device holds `host`, `location`, `[mqtt]`, `[timing]`, `[log]`. It has **no `[[startup]]`/`[[shutdown]]` step lists**, which validation needs. A first-deploy service therefore crashes at startup with `at least one [[startup]] step is required`. With `Restart=on-failure`/`RestartSec=5`, it crash-loops until an operator hand-adds the step lists from the example config. The example file has them. The seed generator does not copy them. **A re-implementation must ship a full seed config (or a built-in default sequence) so a fresh deploy boots green.**

Code-check verdict (2026-09-03): **still open** — `powerseq/deploy.sh` (the `SEED_CONFIG` heredoc, ~lines 100–128) emits only `host`/`location`, `[mqtt]`, `[timing]`, `[log]`; no step lists.

## [decision] Seed mechanism choice

Full step lists in the seed config vs. built-in default sequence — both resolve the crash-loop above. The deployed system has neither. The fix needs one deliberate choice.

## [defect] No cancel of an in-progress sequence

There is **no cancel command**. A `stop` issued during `starting`, or a `start` during `stopping`, is silently dropped (logged at warn). An operator who wants to cancel must wait out the current run (up to roughly 30 s + 3 × 120 s worst case with default timeouts) or fault it indirectly. The deployed system documents this as *intent*, not as an oversight. A change there is a deliberate contract change, not a bug fix.

Code-check verdict (2026-09-03): **still open** — `powerseq/internal/seq/seq.go` `begin()` only honors `start` from `phase=idle` and `stop` from `running`/`idle+fault`; no cancel path exists.

## [decision] Cancel support

Keep the drop-on-busy contract, or add an explicit cancel command that safely runs the shutdown list from wherever startup stands? Any change is deliberate, not a fix.

## [defect] The sequencer drops `stop` from `idle` without a fault

Even when the station is fully hot. That can happen when an operator started it by hand, or when a previous process lifetime did. There is no way to shut it down through the sequencer.

Code-check verdict (2026-09-03): **still open** — same `begin()` guard in `powerseq/internal/seq/seq.go`: `stop` is honored only from `running` or `idle` **with a fault**.

## [decision] Unconditional `stop`

Whether the sequencer must honor `stop` from `idle` regardless of fault state, closing the hand-started-station hole above.

## [defect] Fire-and-forget publishes on connect

`/status` and `/meta` publishes on (re)connect go out with no log line and without a bounded retry. A failure there is invisible. Only the `/state` republish goes through the bounded publisher.

Code-check verdict (2026-09-03): **still open** — `powerseq/internal/mqtt/client.go` `OnConnect` publishes `online` and `/meta` with plain `cl.Publish(...)` (no `WaitTimeout`, no error logging); only the `/state` republish routes through the bounded `Publisher`.

## [defect] Stale subscriptions accumulate across config changes

With a persistent MQTT session (`CleanSession=false`), changing the config (and thus the derived subscription set) re-subscribes on top of the old session. Subscriptions for slots no longer referenced are never unsubscribed, so the broker keeps delivering (and the sequencer keeps discarding) their messages until the session is reset.

Code-check verdict (2026-09-03): **still open** — no `Unsubscribe` call anywhere in `powerseq/internal/`; `subscribeAll` only adds.

## [defect] No self-watchdog

There is no metric or heartbeat beyond `/status`. A wedged-but-connected process (for example a deadlocked runner) stays undetected on the bus.

Code-check verdict (2026-09-03): **still open** — no watchdog/heartbeat mechanism exists in the code.

## [defect] Legitimate operator double-presses log at warn

The fast-path guard logs a normal repeated `start` at warn level. This puts noise into the log.

Code-check verdict (2026-09-03): **still open** — `powerseq/internal/seq/seq.go` `request()` logs `cmd %s ignored: phase=%s fault=%q` at warn for the benign fast-path case.

## [defect] PA-arm heartbeat starvation when the radio bridge publishes `/state` only on change

Cross-component hard requirement. The PA arm relay de-energizes when the radio bridge does not refresh its enabling input (the radio's `/state`) within 10 s. The radio bridge publishes `/state` only on change. This starves the heartbeat when the radio is idle-but-healthy. The radio bridge must add a periodic heartbeat republish (for example every ≤5 s) or something equal. powerseq's `wait-pa-power-on` gate does not protect against this once startup has completed.

Code-check verdict (2026-09-03): **fix exists in source, live deployment unverified** — `flexbridge/internal/bridge/bridge.go` defines `DefaultStateHeartbeat = 5 * time.Second` and a `StateHeartbeat` goroutine that republishes `/state` while the radio link is live; whether the shari-hosted flexbridge is running this build is not verified from here.

## Notes on items dropped as fixed or duplicated (not salvage-worthy)

- `wait_state` trusting an arbitrarily old `/state` snapshot — **fixed**: `powerseq/internal/seq/seq.go` now requires the target's `/state.device_online` to not be `"false"` (absent field does not block), with a regression test in `seq_test.go`.
- Doc lag (`stop` honored only from `running`) — live docs (`powerseq/CLAUDE.md`, `docs/powerseq-mqtt-api.md`) now match the code's `idle+fault` behavior.
- Broker migration to shari (`192.168.1.50` → `192.168.1.139`) — resolved; the shari-local broker is merged and is the documented production topology (`CLAUDE.md`, `docs/conventions/mqtt-topology.md`).
- QoS-0 own-`/cmd` + non-retained rationale, busy-guard truth table, step-kind schema, timing defaults, topic tables, deployment knobs — all covered by `powerseq/CLAUDE.md`, `powerseq/docs/powerseq-mqtt-api.md`, and the station-wide convention docs.
- Station-wide question on `device_online` explicitness — resolved in practice: flexbridge states `device_online` is always present (true/false).