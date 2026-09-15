# Residual review findings — feat/icom9700-radio-bridge-lfg

Source: LFG run `20260915-164424-8e4e1635` (ce-code-review mode:agent, 8 personas + validator batch),
review of `e8e64e2..021c6fb` on branch `feat/icom9700-radio-bridge-lfg`. Applied in `021c6fb`: 10 of 26
actionable findings (3 validated P1s: admit-clear, transport length gate, safety keyed/watchdog lifecycle;
plus passcode validation, echo-on-failure, silent-drop masking, 4 taxonomy doc rows, pol-ctrl comment).
The 16 below were deferred to the tracker (single-reviewer confidence-75s, behavior-policy calls,
structure refactors, operator decisions). Decision gates owned by the operator (not filed):
deploy-host truth (.139 vs .140) and the broker-authority doc alignment (.50 vs topology doc) —
see the run artifact's triage groups.

## Filed

- Session config shuttled through three parallel structs — https://github.com/kgbvax/StationA/issues/2
- Any rejected /cmd overwrites the watchdog-trip safety fact;  — https://github.com/kgbvax/StationA/issues/3
- admit's 30s doTimeout is shorter than a cmd-driven connect s — https://github.com/kgbvax/StationA/issues/4
- buildReadPower hand-rolls CI-V wire bytes in the bridge laye — https://github.com/kgbvax/StationA/issues/5
- BuildReadID/BuildSetCIVTransceive zero production callers; B — https://github.com/kgbvax/StationA/issues/6
- UDP sockets accept datagrams from any source: peer address d — https://github.com/kgbvax/StationA/issues/7
- radio.Manager interface: one implementor whose only consumer — https://github.com/kgbvax/StationA/issues/8
- Queued telemetry poll can be misclassified as a cmd-driven c — https://github.com/kgbvax/StationA/issues/9
- Console TX chip renders from a dead bridge retained snapshot — https://github.com/kgbvax/StationA/issues/10
- Console fixture derives the top-level mirror from selected_v — https://github.com/kgbvax/StationA/issues/11
- tx_power field row contradicts the hybrid-mirror rule — https://github.com/kgbvax/StationA/issues/12
- internal/config uses BurntSushi/toml; convention names pelle — https://github.com/kgbvax/StationA/issues/13
- applyReplyLocked takes/releases b.mu from eight separate poi — https://github.com/kgbvax/StationA/issues/14
- Poll-tick send failures silently discarded — https://github.com/kgbvax/StationA/issues/15
- applyReplyLocked coverage / Config.Validate duration branche — https://github.com/kgbvax/StationA/issues/16
- naming.md not updated for ICOM9700_ env prefix deviation — https://github.com/kgbvax/StationA/issues/17

## Failed
none

## No sink
none

## Settled-decision conflicts
none — no finding challenges a session-settled KTD

## Validator outcome
4 P1 findings entered the validation batch: 3 confirmed,
1 refuted (stranded-waiter scenario contradicts the session manager's serial-closure model) and dropped.

## Run artifacts
/tmp/compound-engineering-501/ce-code-review/20260915-164424-8e4e1635/ (review.json, per-reviewer JSON, full.diff)
