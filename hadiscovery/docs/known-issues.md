# Salvage: hadiscovery.md
> Extracted from PRD/03-components/hadiscovery.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [defect] `class` passes through verbatim as the HA `device_class`
A bridge that publishes a `class` HA does not recognize yields an invalid discovery
payload. The neutral/HA boundary is only convention here — nothing validates the neutral
`class` string against HA's vocabulary. (PRD §9.4.)

Verbatim code-check: **still open.** `hadiscovery/internal/ha/render.go` —
`deviceClassFor` returns `f.Class` verbatim when set (line ~344), no validation against
HA's `device_class` vocabulary anywhere.

## [defect] `sanitize` is lowercase-only (case-collision)
Two distinct keys that differ only by case collide into the same HA object id (`Freq` and
`freq` both → `freq`). The later one silently overwrites the former's discovery topic.
(PRD §9.6.)

Verbatim code-check: **still open.** `hadiscovery/internal/ha/render.go` `sanitize`
lowercases and replaces invalid chars, with no disambiguation step.

## [decision] `meta_filter` trailing-`/meta` validation
The config validator checks only that `meta_filter` is non-empty. It does not check the
trailing `/meta`. A filter without it breaks address parsing silently (the service parses
the slot address from the topic). It is open whether to add validation.

Verbatim code-check: **still open.** `hadiscovery/internal/config/config.go` `Validate()`
requires only `mqtt.meta_filter is required`; no suffix check.

## [decision] HA-birth republish strategy
`homeassistant/status` = `online` re-publishes the *stored* entity sets (exact, cheap —
current behavior). The alternative is re-rendering from `/meta` (fresher, needs a re-read
path). The current behavior stays consistent because retained `/meta` replay on reconnect
covers changes, but the design choice was not recorded in the component docs. Unresolved;
a re-implementation must decide explicitly. (PRD §9.5 / §11.)

Code-check: still the stored-sets behavior; the trade-off rationale remains unrecorded
outside this note.

## [decision] Broker topology and HA-instance facts
PRD text (2026-08-29): the deployed production broker was `192.168.1.50:1883` (the HA
broker, config example and seed default); a migration to a broker on shari
(`192.168.1.139`) was committed but **not deployed**. The renderer must take the broker
address purely from config. The repo also does not hold the location, discovery-prefix
configuration, or MQTT credentials of the actual Home Assistant instance — an integration
with a fresh HA must configure its discovery prefix to match (default `homeassistant`).

Update (2026-09-03): the shack-local broker on shari (`tcp://127.0.0.1:1883` for
shari-local services) is now the repo default — `hadiscovery/deploy.sh` seeds
`127.0.0.1:1883` — but the shari-side two-broker cutover is still pending on-site; treat
the broker address as config-only either way. The HA-instance facts (prefix, credentials)
remain unknown to this repo.

## [unique] `options_ref` stringifies non-string capability elements
When a field's `options` are empty, the renderer resolves `options_ref` against the slot's
`capabilities` map at render time. Non-string capability elements are stringified, so an
integer list `[1,2,3]` resolves to `["1","2","3"]`. (PRD §2.2; implemented in
`hadiscovery/internal/expose/meta.go` `CapStringList` via `fmt.Sprint` — not stated in
`docs/station-integration-model.md` Appendix C or `hadiscovery/docs/discovery-mqtt-api.md`.)

## [unique] Device-block `name` fallback includes `meta.device.model`
The HA `device.name` fallback chain is: `expose.device.name` → the *effective* model value
(`expose.device.model`, else `meta.device.model`) → `"<role> <addr>"`.
`hadiscovery/docs/discovery-mqtt-api.md` §2.4 states only the shorter chain (it omits the
`meta.device.model` step in the name fallback).

## [unique] `/meta` parser address rule
The service derives the slot address from the message **topic**, never from the payload
(strip trailing `/meta`), and rejects a message whose topic is not exactly 4 segments with
segment 4 equal to `meta` (that is `<site>/<station>/<slot>/meta`). It must ignore unknown
`/meta` keys for forward compatibility, and treats `expose: null` exactly like absence.
(PRD §2.1.)

## [unique] Render contract: JSON field order and idempotency
JSON field order inside discovery payloads is not contractual for HA, but the reference
golden tests byte-compare it. The engine's idempotency comparison runs on the service's
own rendered bytes, so a re-implementation only needs self-consistent serialization — but
it must stay deterministic (render walks `fields[]` then `actions[]` in declared order; no
map iteration in the render path, because the byte-stable idempotency check depends on it).
(PRD §4.2 / §10.)

---

Verified fixed (checked 2026-09-03, not salvaged):
- Writable `number` without a `command` (PRD §9.3): **fixed** — `render.go` skips it via
  `writableCommandValid` (nil/empty-value_key command → entity dropped), matching the enum
  branch.
- Silent job drop under load (PRD §9.2): **fixed** — `shared/mqtt.Enqueue` now logs
  `"[mqtt] jobs queue full: dropping job (worker saturated)"` when it drops.
- README "pending on-device deploy" vs reality (PRD §11): **resolved** — component
  CLAUDE.md states deployed and running on shari (22 entities, 5 slots).
- Handler deadlock (PRD §9.1): constraint already carried by `shared/mqtt` comments and
  the repo memory notes (paho handlers must not call blocking Publish); not re-salvaged.