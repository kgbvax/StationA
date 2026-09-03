# Salvage: shelly-power-bridge.md
> Extracted from PRD/03-components/shelly-power-bridge.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [defect] Heartbeat `true` falsifies `power` (the heartbeat false-on defect)

The `<shelly_id>/online` = `true` handler refreshes the *whole* telemetry snapshot,
including `power` (it sets it to `"on"`) — not just `device_online`. If the relay is
actually off, the next periodic heartbeat publishes `/state` with `power:"on"` — a
wrong value. It stays until the plug's next native status announcement corrects it.
The announcements are periodic but sparse, so the window can be minutes. The legacy
comment calls it "a no-op if power unchanged," which is only true when the relay is
on. A re-implementation should make the heartbeat refresh only
`device_online`/`error` and leave `power` untouched.

Code-check verdict (2026-09-03): **still open.** `cmd/shelly-power-bridge/main.go`
(subscribe to `nativeOnline`, `true` branch) enqueues `b.HandleTelemetry("on")` with
the same stale "no-op if power unchanged" comment; `internal/bridge/bridge.go`
`HandleTelemetry` sets `st.Power = power` unconditionally on every call. The fix is
to enqueue a dedicated heartbeat-refresh path that touches only
`device_online`/`error` and leaves `power` to the native status announce.

## [defect] Metering parsed but discarded (the metering gap)

The Plus 1PM reports `apower`/`aenergy`/`voltage`/`current` in every native
announcement. The bridge decodes them and publishes none of them — the canonical
`/state` has no power/energy fields. The parse already exists in the code, so
closing the gap is cheap. Whether `/state` gains metering fields is an open
decision (below); any addition is a change to the `/state` schema — coordinate with
`docs/station-integration-model.md` §3.

Code-check verdict (2026-09-03): **still open.** `internal/shelly/shelly.go`
`ParseStatus` returns a `SwitchStatus` carrying `Apower`/`Aenergy`/`Voltage`/
`Current`, but `main.go` discards the struct and uses only the canonical power
string; `internal/bridge` has no metering fields.

## [defect] Shared `client_id` is a collision trap

Setting `mqtt.client_id` non-empty makes all slots share one MQTT client id. The
broker kicks the older client on each connect (session take-over), and the slots
fight over the session. The default (empty → per-slot `<site>-<station>-<slot>`)
is safe. A re-implementation should suffix a configured id per slot, or reject a
non-empty shared id.

Code-check verdict (2026-09-03): **still open.** `main.go` `runSlot` uses
`cfg.MQTT.ClientID` verbatim for every slot when non-empty; `internal/config`
offers no per-slot id or validation.

## [defect] Deploy script does not refuse placeholder Shelly ids

A docs-vs-code disagreement: the script's own header says "deploy will refuse to
seed a placeholder id" and "a placeholder is refused," but the code only defaults
to the placeholders (`shellyplus1pm-aabbccddeeff`,
`shellyplus1pm-112233445566`) and never checks. A first deploy with placeholders
silently binds to nonexistent devices (all telemetry silent, all slots
`device_online:false`).

Code-check verdict (2026-09-03): **still open.** `shelly-power-bridge/deploy.sh`
contains the refusal claim in comments but no check; the placeholder defaults at
lines 76–77 seed unverified.

## [defect] Staleness threshold coupled to invisible device config

The 75 s heartbeat timeout assumes the plug's device-side heartbeat period. The
bridge neither queries nor configures that period, and the bridge's config does
not expose the 75 s value. Lengthen the plug's announce period past 75 s and the
slot flaps offline.

Code-check verdict (2026-09-03): **still open.** `main.go` hard-codes
`const heartbeatTimeout = 75 * time.Second` (and a 10 s ticker); neither is
configurable.

## [defect] `/state` is not refreshed on reconnect

After a broker reconnect the bridge republishes `/meta` only. A state change
during the disconnect waits for the next event. This is a minor freshness gap (in
practice the heartbeat path usually republishes soon after).

Code-check verdict (2026-09-03): **still open.** The paho `OnConnect` handler
publishes `online` + `/meta` and re-subscribes only; held state is published on
the next event, not on reconnect.

## [defect] Silent event drop under load — FIXED, do not re-salvage

The PRD asked that queue-overflow drops be counted and logged. `shared/mqtt`
`Enqueue` now logs `[mqtt] jobs queue full: dropping job (worker saturated)` on
every drop (drops still not counted, but the silent-drop hazard is gone).

## [defect] Committed build artifacts — FIXED, do not re-salvage

The compiled binaries in the project directory are now covered by
`shelly-power-bridge/.gitignore` (`/dist/`, `/shelly-power-bridge`) and are not
tracked by git.

## [decision] The `uhf/radio` mystery feed

The seeded `feeds` list for `psu-13v8` (the same in `config.example.toml` and the
deploy script's seed generator) holds `uhf/radio`. The legacy narrative says the
rail feeds "the radios" of both stations. Variants: (a) a UHF radio exists
physically but has no bridge/slot, and the feed refers to it. (b) The entry is
stale or wrong. Evidence points both ways: the entry appears in every config
artifact (so it is deliberate, not a typo), while the slot registry contradicted
it.

Update since the PRD was written: `docs/station-integration-model.md` now declares
`muehle/uhf/radio` — an **IC-9700** — but still marks it TBD (listed among slots
whose hardware is unresolved), and no bridge fronts it; the monorepo CLAUDE.md
slot table still omits it. The open question is therefore narrowed, not closed:
the operator must confirm at the device that the 13.8 V rail physically feeds the
IC-9700 (and that a UHF radio slot/bridge will exist). Until then the seeded list
stays as-is for parity.

## [decision] Feeds list omissions: `uhf/pol-ctrl` and `hf/pa`

The seeded list omits `uhf/pol-ctrl` (PLC #2, a registered slot the narrative says
the rail feeds — though note the PLC #2 firmware itself is a documented repo gap)
and `hf/pa` (the HF power amplifier). Possibly the PA takes mains power directly
(it is on the `master` tree), and the site added PLC #2 after someone wrote that
list. Unresolved — the operator must confirm what the rail physically feeds.
Code-check verdict: still open; the seeded eight-entry list in
`config.example.toml` and `deploy.sh` is unchanged.

## [decision] Metering in `/state`

Publish `apower`/`aenergy`/`voltage`/`current` to `/state` (or a sibling topic),
or keep discarding them? The device gives the data, and the code already parses
it, but the deployed `/state` schema has none of it. Any addition changes the
`/state` contract — coordinate with `docs/station-integration-model.md`. (See the
metering-gap defect above.)

## [decision] Heartbeat false-on fix: preserve or fix

Preserve the falsifying heartbeat behavior exactly, or fix the handler to refresh
only `device_online`? The recommendation is the fix, but it changes observable
behavior (heartbeat no longer publishes `power:"on"`) and must be a flagged
decision, not an accident. (See the heartbeat defect above.)

## [decision] `device_online` wire form: explicit-true vs omitted-when-true

Deployed bridges publish `device_online: true` explicitly. The integration-model
document describes an "omitted when true" form. Consumers must treat both forms as
the same (absence = true), or the bus must mandate explicit-true. Unresolved
station-wide.

Code-check verdict (2026-09-03): **mismatch still live.** `internal/bridge`
`powerState.DeviceOnline bool` has `json:"device_online"` with no `omitempty`, so
this bridge always publishes the explicit field, while
`docs/station-integration-model.md` §3 still reads "omitted when true". Pick one
side station-wide.

## [decision] Real Shelly device ids live only on shari

The example/seed configs carry placeholder ids (`shellyplus1pm-aabbccddeeff`,
`shellyplus1pm-112233445566`) and example serials. The real production ids live
only in the seeded config on shari (`/etc/shelly-power-bridge/config.toml`); they
could not be read from the workstation when the PRD was written. Get them from the
operator or the live host. Do not copy the placeholders.

## [decision] Slot-error exit semantics: wait-all vs fail-fast

The process waits for all slot workers and only then returns the first collected
error (`run()`: `wg.Wait()`, then first error from the channel). A single failed
slot (e.g. one slot's first MQTT connect fails while the other stays connected)
leaves the process running with that slot dark until SIGTERM; the healthy slot is
unaffected and its `/status` stays `online`. When all slots fail together, the
process exits 1 and systemd restarts it after 5 s (`Restart=on-failure`,
`RestartSec=5`).

The alternative is fail-fast on the first slot error. That changes observable
behavior — the healthy slot's Will fires and its `/status` flips to `offline` on
an error that today leaves it untouched. Preserve wait-all or reverse it
knowingly; do not split the difference.
Code-check verdict (2026-09-03): **wait-all still implemented** (`main.go` `run()`).

## [requirement] Push-driven contract: never poll, never configure the plug

The bridge is **entirely push-driven**: it must not poll the plug, must not send
status requests, and must not try to configure the plug. Its announce/heartbeat
periods are device-side settings the bridge neither reads nor writes. The bridge
is the only writer of the plug's RPC topic, and the only plug-bound message it
ever sends is the exact `Switch.Set` RPC. The plugs must be pre-provisioned out of
band to speak MQTT to the same broker under their device-id prefix — the bridge
cannot configure them.

## [requirement] `fail_safe` metadata must match the plug's out-of-band setting

`fail_safe` (default `off`) means the plug's power-on default after a mains
outage. `off` means a mains blip drops the station rather than re-energizing it
unexpectedly — a deliberate safety posture: humans or the sequencer, never the
plugs themselves, decide when power returns. Informational metadata only: the
bridge never programs the plug. The plug's actual out-of-band setting must match
the published value.

## [requirement] Native status edge cases

- A payload that is valid JSON but lacks `output` decodes as `false` → power
  `"off"`. Treat an announcement without an `output` key as a report of "relay
  off" (Go's JSON zero-value behavior; the contract keeps it).
- The bridge logs malformed payloads (invalid JSON) at WARN and drops them with no
  state change.
- Recovery of `device_online` comes only from a native status announce or a
  heartbeat `true`; the staleness watcher only marks offline, never online.
- The never-heard case (no heartbeat since process start) counts as elapsed 1
  hour, so it trips the staleness threshold on the first tick (≤10 s of startup):
  an unknown plug must not read as online.

## [unique] Consumer context: blast radius and cascading device-link loss

`powerseq` is the primary automated consumer: it drives the startup/shutdown
sequence over these two slots' `/cmd` topics and reads their `/state` to confirm
ordered power transitions. The `feeds` metadata lets any consumer compute blast
radius: if `psu-13v8` drops, everything in its `feeds` list goes dark — and most
of those devices then also lose their bridges' device links (the `device_online`
fields in their `/state` go false), a cascading pattern consumers must expect.
Because `master` is the upstream of `psu-13v8` itself, switching `master` off
takes the PSU (and all its feeds) dark too.