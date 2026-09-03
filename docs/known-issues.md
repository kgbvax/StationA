# Station-wide known issues, decisions, and safety gaps

> Salvaged from the PRD folder (2026-09-03) just before PRD deletion. Prose is verbatim
> PRD text unless marked. Each section names its PRD source in the heading. Per-component
> known issues live in `<project>/docs/known-issues.md`; this file holds the cross-cutting
> register: ops gaps, safety gaps, and station-wide decisions a rebuild or future session
> must not silently re-open.

# 05-deployment-ops.md
> Extracted from PRD (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [defect] UHF rotator auto-heal gives up permanently after one failed reopen (PRD §5.3 row 9)

Symptom: UHF rotator link dead after adapter re-enumeration. No auto-recovery.
Cause: Auto-heal retries once after a 2 s cooldown, then gives up permanently on a failed reopen.
Remedy / required behavior: Manual: the TUI's manual reopen key (always works). A reconstruction must retry indefinitely at a 2 s reopen cooldown (the current auto-heal interval).

Code-check verdict (2026-09-03): **still open** — `pelcobridge2/internal/control/engine.go` `onReadErr` (lines 220–240): on a failed `reopen()` it only records the error and returns; no reader generation is running afterwards, so no further read error can re-trigger healing, and no background timer retries. Only the manual ctrl+r `ReopenIntent` path recovers. The PRD remedy (indefinite retry at the 2 s cooldown) is the required fix.

## [defect] hf_console: initial-connect failure swallowed, no retry; publishes silently dropped (PRD §5.3 row 10)

Symptom: Console (tablet) shows offline forever after booting before the broker was up. Taps do nothing.
Cause: The code swallows the initial-connect failure with no retry loop. It silently drops publishes made while disconnected.
Remedy: restart the app after the broker is reachable. A reconstruction must add an app-level connect retry and surface dropped commands.

Code-check verdict (2026-09-03): **still open** — `hf_console/lib/main.dart` lines 86–95: `await _connect(...)` is wrapped in `catch (_) { // offline start is allowed ... }` with no retry; `hf_console/lib/mqtt/mqtt_service.dart` line 81: `publish` returns silently when `connectionStatus.state != connected`. (`autoReconnect = true` only covers a connection lost *after* it was established.)

## [defect] hf-mqtt-capture rotation null-writer panic on disk-full / permission loss (PRD §5.3 row 11, §4.2)

Symptom: Bus recorder crashes on hour rollover under disk-full / permission loss.
Cause: Rotation closes the old file before opening the new. A null-writer panic results.
Remedy: Open-before-close fixes the defect. Check host disk space in the checks.

PRD context: the rotation path closes the old file **before** it opens the new one. If the open fails (disk full, permission loss), the writer dereferences a null writer and crashes. A reconstruction must open the new file before it releases the old one.

Code-check verdict (2026-09-03): **still open** — `hf-mqtt-capture/cmd/hf-mqtt-capture/main.go` lines 152–176: `rotateIfNeeded` flushes and closes the old file, sets `w.file = nil; w.bw = nil`, then attempts to open the new one and returns the error on failure, leaving `bw` nil; `writeLog` (line 134–150) logs "rotate failed" to stderr and *continues* to `fmt.Fprintf(w.bw, ...)` → nil-pointer panic on the next message.

## [defect] First key-up after an auto-ground can go "into the short" (PRD §5.3 row 13)

Symptom: First key-up (key-up = starting a transmission) after an auto-ground goes "into the short".
Cause: Reconciler re-activation can race the switch settling after a ground event.
Remedy: Recorded live gap. After any grounded→selected transition, the operator must confirm the switch reports `settled` before transmitting. A reconstruction must close the race.

Code-check verdict (2026-09-03): **still open** — `antennaselect/internal/mqtt/client.go` line 261 copies the switch's `settled` field into the reconcile input (`SwitchSettled`), but `antennaselect/internal/reconcile/reconcile.go` never reads it: re-activation is not gated on the switch having settled.

## [defect] Hard-coded browser WebSocket endpoint (PRD §3.1, recorded deviation)

The browser client ignores the configured broker host. It connects to a hard-coded `ws://192.168.1.139:8091/mqtt`. This deviates from the no-hard-coded-host-addresses rule. A reconstruction must make the endpoint configurable, or must record the deviation in the same explicit way.

Code-check verdict (2026-09-03): **still open** — `hf_console/lib/mqtt/client_factory_web.dart` line 10: `const _wsUri = 'ws://192.168.1.139:8091/mqtt';`.

## [decision] Capture retention granularity and capture-filter width (PRD §4.2 / §6.7)

Retention: on rotation, delete whole date-directories older than `retention_hours` (default 72). **Documented consequence (actual behavior, not the idealized claim)**: cleanup granularity is whole days. With the default, the tool keeps between **72 and 96 hours** of logs. A reconstruction can fix the granularity, but tooling must not assume hour-precision deletion.

Decision needed: keep the day-directory scheme so ops tooling matches, or fix it to true hourly. Do not promise hour-precision deletion either way. Also decide whether to widen the default capture filter to include the UHF subtree (the tool records none of it now — default subscription is `muehle/hf/#` only).

Code-check verdict (2026-09-03): **still open** — `hf-mqtt-capture/cmd/hf-mqtt-capture/main.go` `cleanupOldLogs` (lines 178–196) deletes whole date directories only, matching the day-granular behavior; default filter remains `muehle/hf/#`.

## [decision] Ultrabeam antenna-switch port: 3 or 4 (PRD §6.2)

Repo-root documentation and the integration model say the Ultrabeam beam is on antenna-switch port 3 (fan dipole port 6, dummy load port 1). The deployed reconciler seed config *and* the console's antenna map say port 4. A third conflicting claim exists inside the integration model itself: its passive-resource list says the fan dipole is on port 2. The authoritative artifact — the live config on the Raspberry Pi — stayed unreadable while the PRD was written. Port numbers are per-site configuration. Never hard-code them. The deployed on-device config plus physical inspection is the only resolution path.

Code-check verdict (2026-09-03): **still unresolved** — the conflict persists in-tree: repo docs/CLAUDE.md say port 3; `antennaselect/config.example.toml` (`port4 = "ultrabeam"`) and `hf_console/lib/store/wiring.dart` (`'port4': 'Ultrabeam'`) say port 4. Only reading the live config on shari + physical inspection settles it.

## [decision] `device_online` publication form: explicit vs omitted-when-true (PRD §6.3)

The integration model says the field is "omitted when true". The deployed bridges publish `device_online: true` explicitly. Consumers must treat both forms as the same (absence = true) — that consumer rule is normative. A reconstruction must pick a producer convention (PRD recommendation: mandate the explicit boolean for all device slots, and update the model).

Code-check verdict (2026-09-03): **half-resolved in code, model doc still open** — producers now publish the explicit boolean (e.g. `flexbridge/internal/bridge/bridge.go` line 92: "device_online is always present (true/false)"); `docs/station-integration-model.md` still defines it as optional. Remaining action: update the model text to mandate the explicit boolean.

## [decision] Host-liveness nodes are model-only, nothing publishes them (PRD §6.8)

`muehle/host/shari` (fields `online`, `temp_c`, `load`) and `muehle/host/shack-pc` (field `online`) are reserved as host-liveness nodes in the bus model, but **no component in the current repo publishes them** — they are model-only. Decide: add a host-liveness publisher, or remove the nodes from the model.

Code-check verdict (2026-09-03): **still open** — repo-wide search for `muehle/host` hits only documentation files (docs, PRD, sas mockups), no code.

## [decision] PLC #2 firmware (`muehle/uhf/pol-ctrl`) does not exist (PRD §6.9)

The X-Quad polarization slot is attributed to the m5stamp project, but no PLC #2 firmware exists in the repo. Treat the slot as unimplemented, not as a component to deploy. Decide: add it or formally descope it.

Code-check verdict (2026-09-03): **still open** — `m5stamp-hf-ctrl/src/` implements only the two `hf/` slots (`hf/switch`, `hf/pa-arm`); no `uhf/pol-ctrl` slot anywhere.

## [decision] Reconciler-offline operator indication (PRD §6.14)

antennaselect is a coordination single point of failure: when it dies, all soft bindings stop together (band-follow, tuner-follow, PA-follow, antenna selection) and retained state goes stale. The supervisor restarts it automatically, but no component shows "reconciler offline" to the operator meanwhile. Decide: add an explicit operator indication, or accept the stale-retained-state window.

Code-check verdict (2026-09-03): **partially mitigated in the console only** — `hf_console/lib/ui/widgets/antenna_panel.dart` falls back to a manual-operation header when the `antenna-select` slot is offline (`managed = selectOnline && mode != null`); there is still no bus-level or bridge-level "reconciler offline" indication.

# 06-safety.md
> Extracted from PRD (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [defect] Arm relay freezes during WiFi outage

From PRD §2.3 (and repeated in §7.2): "the main loop now early-returns before re-evaluating `armed` when WiFi is down. The relay *freezes* in its last position for the outage duration — a contract violation of \"any failure drops the relay\". The arm evaluation must run regardless of link state. The safe direction drops the arm. Never hold the arm."

Code-check verdict: STILL OPEN — `m5stamp-hf-ctrl/src/main.cpp` loop step 3 early-returns while `WiFi.status() != WL_CONNECTED`, so `recomputeArm` never runs during an outage (relay holds last position). Also tracked by the m5stamp PRD extractor; treat as duplicate there, keep one copy.

## [defect] Silent disarm on a JSON-boolean /cmd value

From PRD §2.3: "the parser extracts `/cmd` values as JSON strings. A sender publishing `\"value\": true` — a JSON boolean — yields the empty string. For `set_enabled` that **silently disarms**. The command parser must either accept JSON booleans or reject the command loudly. Silent disarm on a well-formed-looking payload is a defect."

Code-check verdict: STILL OPEN — `m5stamp-hf-ctrl/src/main.cpp` `parseCmd` does `value = doc["value"] | \"\"` with no type check, and `handlePaArmCmd` does `enabled = (value == \"true\")`; a JSON boolean `true` yields a non-`"true"` string, i.e. disarm.

## [defect] antenna_ready input has no staleness bound

From PRD §2.3: "the antenna-switch input has no freshness window, so a stale \"ready\" stays ready forever. The ant-switch slot's LWT is not used here either. The arm can therefore close on data from a dead slot whose retained state shows a selected antenna. The antenna-ready input must carry a freshness bound and/or the `/status` liveness layer."

Code-check verdict: STILL OPEN — `main.cpp` reads `antennaReady = (sel != "" && sel != "off")` from the ant-switch snapshot with no freshness timestamp and no `/status` subscription on the ant-switch slot; only the radio input is freshness-gated.

## [defect] Missing `tuning` field defaults fail-open

From PRD §2.2/REQ-S13: "One input defaults the other way: a missing `tuning` field counts as `false` (not tuning), the permissive side. This is a known fail-open gap: a radio state snapshot without the `tuning` key lets the arm close during a real tune cycle."

From PRD §8 item 16: "Either default the missing field to `true` (fail closed — a deliberate contract change against the deployed firmware) or keep the deployed default and document the fail-open risk."

Code-check verdict: STILL OPEN — `main.cpp` `handleRadioState`: `radioTuning = doc["tuning"] | false;` (absent key → false → permissive).

## [decision] Ultrabeam and fan-dipole switch ports: the repo contradicts itself

From PRD §7.3/§8 item 1: "The repo disagrees with itself on which physical switch port carries the Ultrabeam beam. The integration model, root docs, and the arbitrator's tests say **port 3**. The arbitrator's example config and deploy seed — and the console's antenna map — say **port 4**. The live on-device config on the deployment host is authoritative, but it stayed **unreadable** while the authors wrote this PRD. Port 1 (dummy load) is consistent everywhere. The fan dipole's port is itself contested (6 or 2 — the integration model's passive-resource list says port 2). Safety consequence: the wiring map is pure configuration and must never appear in code. The port truth must get on-device confirmation before commissioning. A wrong map sends RF to the wrong antenna. The hardware exclusivity (REQ-S17) and arm chain still hold. But the band policy and band-follow can then tune the wrong antenna."

Code-check verdict: STILL OPEN — `docs/station-integration-model.md` says `ant/ultrabeam` (port 3), `ant/fan-dipole` (port 2); root `CLAUDE.md` says port 3 and fan-dipole **port 6**; `antennaselect/config.example.toml` and `antennaselect/deploy.sh` seed say `port4 = "ultrabeam"`; `hf_console/lib/store/wiring.dart` maps `'port4': 'Ultrabeam'`. On-device confirmation on shari (`/etc/antenna-select/config.toml`) is still the missing truth.

## [decision] `60m` in the arm band allow-list

From PRD §2.2/§8 item 17: "the PLC firmware's `SAFE_BANDS` list contains 11 bands including `60m`. The ACOM 1200S covers 10 bands and has no `60m` filter (`set_band` on the PA bridge accepts the same 10). Either drop `60m` from `band_safe`, or check on hardware and document why `60m` operation with the PA is safe. Until resolved, the arm can close on a band the PA does not cover."

Code-check verdict: STILL OPEN — `m5stamp-hf-ctrl/src/config.h` `SAFE_BANDS[]` = `160m, 80m, 60m, 40m, 30m, 20m, 17m, 15m, 12m, 10m, 6m` (11 entries, `60m` present).

## [decision] Arm evaluation during a link outage — deliberate contract change needed

From PRD §8 item 4: "current code freezes the relay (§7.2). The recommendation is compute the arm condition always — drop is the safe direction. This is a deliberate contract change. A reimplementation must make it explicitly." Related §8 item 3: the error reason for heartbeat staleness ("radio feed stale" is now the shipped string; the related sub-questions — whether a stale `antenna_ready` must produce an error, and whether the arm must use the ant-switch `/status` LWT — remain undecided).

## [requirement] REQ-S6 — handler isolation (messaging-plane robustness)

From PRD §1.4: "An incoming-message handler must never block — in particular it must never synchronously publish and wait for broker acknowledgment on the receive/dispatch path. Handlers must only parse and enqueue work onto a bounded queue drained by a single worker. On overflow the work is **dropped**, never blocking. *Rationale*: in the reference stack, handlers run inline on the MQTT client's dispatch thread. A handler that blocks or publishes synchronously deadlocks the whole client. This deadlocked the station's discovery consumer live, and the antenna arbitrator had the same latent pattern." (Companion REQ-S2 plane discipline: "Consumers must treat every safety-relevant state published on the bus as *advisory mirror only*. Commands are fire-and-observe. Consumers react to `/state`, never to intent.")

## [requirement] REQ-S7 — prompt shutdown (messaging-plane robustness)

From PRD §1.4: "A shutdown signal (SIGTERM-class) must interrupt every blocking wait — *especially* a broker connect at startup during a broker outage. An implementation must not rely on the supervisor's kill-timeout to end a hung connect. *Rationale*: the reference MQTT library's connect call blocks ignoring cancellation. A SIGTERM during a broker outage hung a deployed bridge until the service manager SIGKILLed it." (Companion REQ-S3: "Consumers must never trust retained bus state as fresh for safety purposes… apply the two-layer liveness rule before they act on any state.")

## [requirement] REQ-S11a — local front-panel toggles vs retained-cmd replay

From PRD §2.1: "The front-panel buttons B and C of the PLC directly toggle the PA and TRX remote-on relays. They work with WiFi down. A local toggle made during an MQTT outage is silently reverted on reconnect, because the broker replays the retained `/cmd` intent. A rebuild must reproduce this revert behavior or must consciously change it (for example, re-publish the local intent after the replay). Either way, the rebuild must document the choice. The arm relay stays outside this: no button can force it (REQ-S11)." (REQ-S11 companion: "The arm relay must not be commandable… no arm, disarm, or force action… no local override: no physical control on the device can force it closed.")

Code-check verdict: behavior confirmed as still current — `main.cpp` `handleButtons()` runs before the WiFi gate ("so the front-panel buttons work even when WiFi/MQTT are down"), and the retained `/cmd` replay on reconnect re-applies the broker intent.

## [requirement] REQ-S12 — firmware update de-energizes the arm before reboot

From PRD §2.1: "A firmware update must de-energize the arm relay before rebooting. The device then cold-boots with all relays open (REQ-S9) and re-converges from retained MQTT state."

Code-check verdict: implemented in current firmware — `ArduinoOTA.onStart` in `m5stamp-hf-ctrl/src/main.cpp` does `relaySet(RELAY_PA_ARM, false)` (commented best-effort) before the update reboot.

---

Items from PRD §2.3/§7.2 checked and **dropped as fixed** (not salvaged): "silent arm drop on heartbeat timeout" — the firmware now publishes `"radio feed stale"` in `currentPaArmError`, and flexbridge now republishes `/state` on a heartbeat cadence while the radio link is live (`flexbridge/internal/bridge/bridge.go` + tests). Note: the fielded PLC #1 runs pre-OTA firmware, so the firmware-side fix is built but not yet flashed (first flash must be physical USB).

# 00-system-overview.md + 02-interface-spec.md + 07-priorities-milestones.md
> Extracted from PRD (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [defect] Clean-shutdown `/status` offline self-publish compliance

From PRD 00 §4.6:

> **Two-layer liveness, and checks must cover both.** A slot is "alive" only if BOTH
> checks hold. (a) Its `/status` topic says `online`. It carries the bridge process
> liveness as the plain string `online`/`offline`, maintained through MQTT
> last-will. (b) Its `/state` document's `device_online` boolean — the hardware
> behind the bridge — is true. Consumers acting on retained state must check both
> first. Keying on `/status` alone caused real failures (§6). The contract: on a clean
> shutdown, a component must publish retained `offline` to its own `/status` itself.
> The last-will covers only an abnormal disconnect. Known defect: the flexbridge,
> acom1200s, atr1k, shelly, and wrc bridges omit that self-publish today. A stopped
> service can then leave a retained `online` on the bus. The powerseq, antennaselect,
> hadiscovery, and ultrabridge components do publish `offline` on a clean shutdown.
> Consumers must not trust `/status` alone for anything safety-relevant.

From PRD 02 §5.3 (actual behavior, not the ideal):

> On a **clean** process shutdown (SIGTERM, service stop), the broker does **not**
> fire the LWT. The retained `/status` therefore stays `"online"` indefinitely after
> a stopped service. … For bridges that quiesce with the will suppressed, **the
> retained `online` outlives the process**. (Reference note: the radio bridge
> disconnects within ~500 ms on clean shutdown and its LWT does not fire —
> retained `online` persists.)

Code-check verdict (2026-09-03): **still open.** flexbridge, acom1200s-pa-bridge,
atr1k-tuner-bridge, shelly-power-bridge, and wrc-rotator-bridge have no `offline`
self-publish in their shutdown path (their `internal/mqtt` layers do not publish
`"offline"` to `/status`; `shared/mqtt` provides none either). The compliant set is
unchanged: powerseq (`internal/mqtt/client.go` Close publishes offline),
antennaselect (`internal/mqtt/client.go` Close), hadiscovery (`internal/mqtt/client.go`
Close), ultrabridge (`internal/mqtt/client.go` Close).

## [defect] Idle-grounding recovery gaps (first key-up after ground)

From PRD 00 §6:

> **First key-up after grounding can hit the short.** Recovery from auto-grounding is
> fragile: re-activation can race. A transmit can begin before the antenna switch has
> settled on the new port. The cold-switch ordering contract (§4.1) and the switch's
> `settled` state exist to close this. Three more documented gaps sit in the same
> recovery chain. (a) The reconciler defers antenna changes during transmit, and that
> deferral has no timeout. A frozen transmit state then freezes the whole arbitration
> ladder, including the idle grounding, until the radio bridge reconnects. (b) On a
> reconciler restart, the idle clock starts at the restart time, and retained-state
> replay resets it. A restart during a transmit re-arms the 30-minute ground timer.
> (c) While the radio link is down, the idle rule overrides an operator hold, and no
> manual re-arm path exists. Only a radio state change counts as activity today.
> Intended fixes: give the deferral a timeout, make a restart not reset the idle
> clock, and decide whether an operator command counts as operator presence.

From PRD 02 §7.3 item 7 (live-observed 2026-08-28):

> (a) the first key-up after grounding transmits into the grounded switch (re-arm is
> itself a TX, so the reconciler defers the select). (b) change-only publishing
> starves recovery (ties to #6). (c) there is no non-TX re-arm path (an operator at
> the desk who never touches the VFO cannot un-ground). (d) a reconciler restart
> always re-arms ACTIVE and resets the 30-minute clock. The auto-ground itself works
> live. The fix directions (operator-presence re-arm, TX-deferral timeout,
> restart-stable idle state) are **new design**, not contract.

Code-check verdict (2026-09-03): **largely still open.** Gap (b) is partially
resolved by the fix direction chosen: the radio bridge now republishes `/state`
every 5 s (flexbridge `StateHeartbeat`), so a quiet band no longer starves
recovery. The reconciler now treats an operator hold as presence ("a hold marks
presence; a release does not", `antennaselect/internal/reconcile/reconcile.go`),
which is part of (c). Still open: the TX deferral has **no timeout**
(`DeferredForTX` in `reconcile.go`, re-resolved only on the next input change), and
the idle clock is still in-process, so a reconciler restart re-arms the 30-minute
timer on the retained-state replay. First key-up after ground remains a live race.

## [decision] Antenna-switch wiring: two contested ports (requires on-device confirmation)

From PRD 02 §7.1 (the richer variant; 00 §7.1 agrees):

> Two port numbers in the wiring map are **not knowable from the repository**. The
> Ultrabeam's switch port (3 or 4) and the fan-dipole's switch port (6 or 2) are both
> contested facts.
>
> **Ultrabeam — `port3` or `port4`:**
> - **Evidence for `port3`:** the repo-root project instructions and the
>   station-integration model's wiring map list the Ultrabeam on switch port 3.
>   The antennaselect unit tests assert `port3`.
> - **Evidence for `port4`:** `antennaselect/config.example.toml`, the antennaselect
>   `deploy.sh` **seed** (what a fresh device can get), and the operator console's
>   antenna port map all say `port4`.
>
> **Fan-dipole — `port6` or `port2`:**
> - **Evidence for `port6`:** the repo-root project instructions, the
>   station-integration model's wiring map, `antennaselect/config.example.toml`, and
>   the operator console's antenna port map all say `port6` (80/40 m fan dipole).
> - **Evidence for `port2`:** the station-integration model's own passive-resource
>   list says "fan-dipole … (port 2)". This contradicts the model's wiring map in
>   the same document.
>
> **Uncontested:** `port1` is the dummy load in every source. No source documents
> anything wired to `port5`. The operator console nevertheless renders `port5` as a
> selectable position and publishes `{"select":"port5"}` when an operator picks it.
>
> **The live truth** lives in `/etc/antenna-select/config.toml` on shari (seeded as
> `port4`/`port6`, possibly hand-edited since) and is **not readable from the
> workstation** (0600, on-device).
>
> Both variants of each port are plausible wiring. **Any re-implementation MUST treat
> the wiring map as pure configuration, and MUST confirm both contested ports on the
> device before deploying**. If the wiring is wrong, the band policy routes 20 m–6 m
> transmissions into the wrong antenna, or into an unwired port (an open/short).
> The console then displays a wrong label. Getting a port wrong does not affect the
> bus contract — the `selected`/`select` values are port keys either way.

Status 2026-09-03: **still unresolved.** The contradiction persists in the live
docs — `docs/station-integration-model.md` line 564 (wiring map: `port3: ultrabeam,
port6: fan-dipole`) vs line 579 (passive resources: fan-dipole port 2). The repo
sources still split the same way: `antennaselect/config.example.toml` and the
`deploy.sh` seed say `port4 = "ultrabeam"` with `# port2, port3, port5 unused for
now`; the reconciler unit tests assert `port3`; the console wiring
(`hf_console/lib/store/wiring.dart`) uses the port-4 variant.

## [decision] `/cmd` value-key exception policy (reproduce or migrate)

From PRD 00 §7 item 4:

> The universal `/cmd` shape is `{"action":"<name>","value":"<string>"}` (arguments
> ALWAYS under `value`, as strings, booleans as `"true"`/`"false"`). Two documented
> exceptions exist in the field. The antenna controller's frequency command uses an
> integer `freq_hz` key, declared by its `expose` descriptor. Several console-driven
> payloads (`{"select":…}`, `{"request":…}`, rotator `{"action":"set_az","az":…}`,
> tuner's real-boolean `set_inline`) deviate. Open decision: reproduce the deployed
> byte shapes exactly (deployed bridges parse them as-is), or clean them up with a
> coordinated migration.

Status 2026-09-03: unresolved. Deployed code still parses the deviating shapes; the
live docs state the `value` convention but no doc resolves the exception policy.

## [decision] Pelco-P addressing assumption (bench check before any P-mode use)

From PRD 00 §7 item 10:

> **Pelco-P addressing assumption.** The bench assumed that the UHF head uses the
> same address byte in both serial protocols. If it is actually zero-indexed under
> Pelco-P, the head silently ignores every P frame. This assumption has no bench
> confirmation. The rotator console does not use Pelco-P framing since 2026-08-29,
> so this question has no effect on the present component. A re-introduction of
> P-mode framing must first check the addressing on the bench.

Status 2026-09-03: still unconfirmed; harmless while the console stays on Pelco-D,
but load-bearing if P framing is ever reintroduced.

## [unique] Ham-radio glossary (plain-language definitions)

From PRD 02 §0.1. Candidate home: appendix of `docs/conventions/band-mode-reference.md`.

- **Amateur radio ("ham") station**: a licensed, non-commercial radio installation, here
  the "Mühle" station. Its parts are a transceiver, a power amplifier, an antenna
  tuner, antennas, and the machinery to route and point them.
- **HF / UHF**: high frequency (roughly 3–30 MHz, long-distance "shortwave" bands) and
  ultra-high frequency (roughly 300 MHz–3 GHz). The station has one HF chain and one UHF
  rotator.
- **Transceiver (TRX, "the radio")**: a combined radio transmitter and receiver — here a
  FLEX-8400. "TX" = transmitting, "RX" = receiving.
- **PA (power amplifier)**: a device that boosts the transceiver's output to high power —
  here an ACOM 1200S, up to 1200 W.
- **ATU / antenna tuner**: an impedance-matching network placed in the feed line ("in
  line") or bypassed. Needed when an antenna is not resonant on the operating band —
  here an ATR-1000.
- **SWR (standing-wave ratio)**: a measure of feed-line impedance match. 1.0 is perfect,
  ≥ 3.0 is dangerous (reflected power heats the PA).
- **Rotator**: a motor that turns a mast-mounted directional antenna. Azimuth (az) is the
  horizontal pointing angle in degrees.
- **Ultrabeam**: a motorized tubular beam antenna whose element lengths are motor-tuned
  per band. It supports forward / 180°-reversed / bi-directional radiation patterns and
  full element retraction, driven by an "RCU-06" controller.
- **Antenna switch**: a relay board connecting exactly one of several antennas to the
  radio. Its `off` position shorts all antenna feeds to electrical ground (lightning
  protection).
- **Dummy load**: a heat-dissipating resistor that radiates nothing. Used for testing.
- **Bridge**: a small service that translates one physical device's protocol (serial,
  Ethernet, WiFi relay, and so on) onto the MQTT bus.
- **MQTT**: a lightweight publish/subscribe message protocol. Clients publish messages
  to hierarchical **topics** (slash-separated strings). A central **broker** relays them
  to subscribers.
- **Retained message**: a message the broker stores per topic and re-delivers to every
  future subscriber until overwritten or cleared by a zero-length retained publish.
- **QoS**: how firmly MQTT delivers a message. QoS 0 = at most once. QoS 1 = at least
  once, and QoS 1 messages can arrive twice. All station publishes use QoS 1 unless
  stated otherwise.
- **LWT (last will and testament)**: a message registered with the broker at connect
  time. The broker publishes it on the client's behalf if the client disconnects
  *uncleanly* (crash, network loss, kill -9).
- **Slot**: one component's topic namespace on the bus, an address of the form
  `<site>/<station>/<slot>` — for example `muehle/hf/pa`. This is the unit of station identity.
- **Plane**: one of the four per-slot topic suffixes `meta`, `state`, `status`, `cmd`.
- **Logic slot**: a slot with no physical device behind it (a pure MQTT service).
- **Band**: the wavelength name of an operating frequency — for example `20m` for the 14 MHz
  amateur band.
- **Mode**: the emission mode of a transceiver (CW = Morse on/off keying, USB/LSB =
  upper/lower sideband voice, AM, FM, and `data` for all digital modes).
- **Slice**: one receive channel inside the radio. The radio can run several at the
  same time. The bridge reports the lowest-index active one.
- **Key-up / keying line**: the transceiver's transmit-request signal path — the wire
  that switches the amplifier into transmit. The PA amplifies only while this line is
  active, so a relay in this line works as the PA arm interlock.
- **IARU Region 1**: the amateur-radio band-allocation plan for Europe, Africa, and
  the Middle East.
- **DX spot**: a report that says another operator heard a distant ("DX") station on
  some frequency.
- **Maidenhead locator / grid square**: a ham-radio geographic encoding. 4 characters
  denote a 2°×1° "square", 6 characters a ~5′×2.5′ "subsquare".
- **Shari**: the Raspberry Pi (192.168.1.139) that hosts the station's services.
- **shack-pc**: the Windows PC in the radio shack that hosts the interactive UHF
  rotator console.

## [unique] Resolved since the PRD (do not re-open)

PRD 00 §7 items later resolved in code, recorded so nobody re-opens them:

- **Radio state heartbeat starvation (PRD 00 §7.5, §6).** FIXED: flexbridge now
  republishes `/state` every 5 s while the radio link is live
  (`flexbridge/internal/bridge/bridge.go`, `DefaultStateHeartbeat = 5 * time.Second`,
  started from `cmd/flexbridge/main.go`), sitting well inside the PA-arm PLC's
  10 s heartbeat window.
- **UHF rotator self-check-disable cadence (PRD 00 §7.6).** FIXED: the engine now
  re-sends the self-check disable (preset set 105) after every successful link
  reopen, not once per process start (`pelcobridge2/internal/control/engine.go`,
  `resendSelfCheckDisable`).
- **UHF rotator auto-heal gives up (PRD 00 §6).** FIXED: the engine now auto-heals
  by reopening with a `reopenCooldown` (2 s throttle) instead of giving up
  permanently after one failure (`pelcobridge2/internal/control/engine.go`).
- **Broker topology (PRD 00 §7.2).** Resolved in the docs and merged to main: the
  shack-local Mosquitto on shari (192.168.1.139:1883) is authoritative for
  `muehle/#`, bridged to the HA broker at 192.168.1.50 (see
  `docs/conventions/mqtt-topology.md`). Deployment to shari was still pending as of
  2026-09-03 per project memory.
- **Legacy bridge naming (PRD 00 §7.12).** Tracked in
  `docs/conventions/naming.md` (legacy → target: `flexbridge` → `flex-radio-bridge`,
  `ultrabridge` → `ultrabeam-ant-ctrl-bridge`); rename deferred because it touches
  live service units. Slot addresses stay unchanged either way.
