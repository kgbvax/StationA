# Plan — Review Wave 3 (console/stimulator) + Systemic Patterns

Status: PLANNED (not implemented). Source: 2026-09-17/18 full-monorepo review
(see memory `stationa-monorepo-review-2026-09`); every fix site re-scouted at
file:line precision on 2026-09-18. Waves 1 (safety criticals) and 2
(live-service majors) are implemented and deployed; branches
`fix/wave1-safety-criticals`, `fix/wave2-live-majors`.

Conventions assumed throughout: `/cmd` args under the `value` key as strings
(JSON booleans break the m5stamp firmware parser — except where a receiver
contractually requires a real bool, see T3); change-only `/state` publishing
must always have a dedup-bypassing reconnect ritual; paho handlers never
publish inline.

---

## Part 1 — Wave 3: hf_console + testui

### T1. hf_console — sat GOTO publishes the pre-step bearing

**Finding.** On the sat-ops panel (`SatRotatorPanel`/`_AxisControl`), tapping
`+`/`−` updates the bearing TextField *without* rebuilding the widget, but the
GOTO button's `onPressed` closure was built in the earlier frame and captured
the older parsed value. The operator steps to 46°, the field shows 46, GOTO
publishes 45.

**Sites** (`hf_console/lib/ui/widgets/sat_rotator_panel.dart`):
- `:195-196` — `parsed` computed once per build from the controller text.
- `:203-211` — `publishGoto()` closure captures `parsed` from its build frame.
- `:213-224` — `step(dir)` writes `_controller.text = _fmtDeg(next)` with **no
  `setState`**; `onChanged` (`:287`) does not fire for programmatic writes, so
  no rebuild happens. `valid`/`gotoEnabled` go stale the same way.
- `:312-321` — the GOTO button wires the stale closure. `_AxisControl` uses
  `context.read` (`:170`), so only parent-driven rebuilds mask the bug.

**Fix (both, they compose):**
1. In `step()` (`:223`): `_controller.text = _fmtDeg(next); setState(() {});`
2. Belt-and-braces: in `publishGoto()` (`:204`), re-read at publish time —
   `final deg = double.tryParse(_controller.text.trim());` — instead of the
   captured `parsed`. The TextField controller is the single source of truth.

**Tests** (`hf_console/test/ui/widgets/sat_rotator_panel_test.dart`, harness
`FakeMqttService` records publishes): new regression — `enterText('45')` → tap
`sat-az-step-up` → tap `sat-az-goto` → expect payload
`{'action':'goto','value':'46.0'}` (fails on current code; the existing step
test at `:281` asserts only the text and never taps GOTO — exactly why the bug
survived). Also pin the disabled-state refresh after a step clamps out of
limits.

**Deploy/verify.** `tool/prebuild.sh` (analyze + test), `flutter build apk
--release`, sideload to the tablet (adb; `adb install -r` keeps credentials;
wireless adb port must be read off the tablet screen — LAN mDNS is dead), and
rebuild/deploy the web surface via `hf_console/deploy.sh` (shari :8091) — both
surfaces build from the same Dart fix. Verify: on the sat panel, step then GOTO
moves to the stepped bearing (watch `muehle/uhf/az-rotator/cmd`).

### T2. hf_console — BusStore.apply hard casts abort the MQTT batch

**Finding.** `BusStore.apply` blind-casts decoded payloads; a non-object JSON
payload (`[1,2]`, `42`, `true`) or plain garbage on any `muehle/#` topic throws
`TypeError` out of the ingestion batch. Every message *after* the bad one in
that batch is silently dropped; with `.startClean()` + retained replay on every
(wake/resume) reconnect, one poisoned retained payload re-aborts the same
replay batch indefinitely — all slots sorted after it stay unpopulated.

**Sites** (`hf_console/lib/store/bus_store.dart`, `lib/mqtt/mqtt_service.dart`):
- casts: `:160` (meta), `:163` (state), `:174` (status), `:178` (cmd) — no
  try/catch anywhere in `apply`.
- `_decodePayload :186-195` passes non-JSON text through raw (deliberate).
- caller: `mqtt_service.dart:160-165` `_apply` → `store.apply`, no try/catch,
  no `onError`; subscription `muehle/#` QoS 0 (`:188`); any address accepted
  (`putIfAbsent :140`).

**Fix.** Guard at the store (single choke point), matching the house
getter-diagnostics style (no logger exists in lib/):
- meta/state/cmd: assign only `if (value is Map<String, dynamic>)`, else drop —
  keep last-good, never clobber, never clear.
- status: `value is String` or drop.
- add `int _malformedPayloads` + `int get malformedPayloads` diagnostic counter.
- expose `@visibleForTesting void ingest(String topic, List<int> bytes,
  {bool retained = false})` on MqttService (delegating to `_apply`, mirroring
  the dxspot `@visibleForTesting` precedent) so the batch semantics are
  testable — today the `updates` listener is broker-bound and untested.

**Tests.**
- `test/store/bus_store_test.dart`, new group: `'[1,2]'`, `'42'`, `'true'`,
  `'garbage'` on state/meta/cmd → no throw, prior value intact, counter
  increments; object on `/status` dropped with prior status intact; the
  existing empty-payload clear test still passes.
- new mqtt-service test via `ingest`: bytes [good state A, garbage, good state
  B] → BOTH slots applied, counter == 1 (one bad payload must not take out its
  batch).

**Deploy/verify.** Same build as T1. Verify: publish garbage retained to a
harmless test topic under `muehle/`, watch the console keep applying other
slots' updates across a reconnect.

### T3. testui — JSON-boolean /cmd args (m5stamp parses them as disarm)

**Finding.** The browser UI emits raw JSON booleans in `/cmd` payloads. On
`muehle/hf/pa-arm/cmd set_enabled`, the m5stamp firmware's ArduinoJson parse
(`doc["value"] | ""`) cannot convert bool→string, yields `""`, and
`enabled = (value == "true")` → **false**: clicking "Enabled=true" in testui
silently disarms the PA (fail-safe direction, but the control is dead and both
clicks look identical). The Go relay is a verbatim passthrough — the bug is
entirely client-side.

**Sites:**
- `testui/internal/web/static/app.js:699-703` (`buildSetpointRow`) — boolean
  decided by the **field** type (`f.type === 'boolean'`), ignores
  `cmd.value_type`; emits JS `true/false`.
- `app.js:758` (`buildActionRow`) — same for `value_type === 'bool'`.
- choke point: `buildPayload` `app.js:835-842`.
- receiver truth gap: `m5stamp-hf-ctrl/src/main.cpp:307` advertises
  `value_type = "boolean"` in its expose while its own parser (`:87-94`,
  `:143-146`) and API doc (`docs/m5stamp-hf-ctrl-mqtt-api.md:153-154`) define
  the wire form as **strings**.
- hard constraint: atr1k tuner `set_inline` **requires** a real JSON bool
  (`atr1k-tuner-bridge/internal/bridge/bridge.go:260-269`, exposes
  `ValueType:"bool"`) — a blanket stringify breaks the tuner send button.

**Fix.**
1. testui: one typed choke point `cmdValue(cmd, val)` inside `buildPayload`:
   `value_type 'boolean'` → `String(val)` (`"true"`/`"false"`);
   `value_type 'bool'` → real boolean (atr1k); `int`/`float` → number;
   `string`/absent → `String(val)`. Removes the per-row duplication at
   `:701`/`:758`.
2. m5stamp (truth fix, rides the next USB flash): expose
   `value_type = "string"` to match its parser and doc.
   **Deployment independence:** against the *currently flashed* firmware the
   testui string fix alone is correct (the old parser compares
   `value == "true"` on the string), so testui can ship first.

**Tests.** No JS test infra exists. Add a dependency-free `node --test` file
beside the asset (`internal/web/static/app.test.mjs`) asserting
`buildPayload({action:'set_enabled',value_key:'value'}, true)` →
`{action:'set_enabled',value:'true'}` and the atr1k case stays `value:true`;
wire into the Makefile `test` target. (Fallback if a JS runner is unwanted:
a value_type-aware guard in `handlePublish`, tested via `stubMQTT.lastPayload`
— a blanket bool rejection would wrongly block atr1k.)
m5stamp side: no test infra; the expose change is doc-consistent and reviewed.

**Deploy/verify.** `testui/deploy.sh` (shari, service on :8090). Verify: from
the UI, pa-arm enable on → `mosquitto_sub` shows
`{"action":"set_enabled","value":"true"}` and the arm stays set; atr1k inline
toggle still sends a JSON boolean.

### T4. testui — publishes queue during broker outages, replay late

**Finding.** testui's publishes (operator stimulus!) go through a goroutine
that does `tok.Wait()` with no timeout (`internal/mqtt/client.go:139-165`).
During an outage paho is `reconnecting` but `IsConnected()` stays true, so QoS 1
publishes land in paho's unbounded in-memory outbound store and **replay
DUP=1, in order, on reconnect** — a stale `/cmd` (no `ts` is ever attached)
executes on real hardware minutes-to-hours after the click; a late retained
simulated `/state` clobbers the slot's real state. Meanwhile each `/api/publish`
HTTP request hangs for the whole outage and then returns 200. QoS 0 publishes
during `reconnecting` are silently dropped with a false 200.

**Sites:** `testui/internal/mqtt/client.go:139-165` (Publish), `:76-87`
(options: CleanSession=false, AutoReconnect, OnConnect resubscribe only);
`internal/web/handlers.go:58` (publish), `:121-127` (error mapping).

**Fix (drop — testui owns no bus state, so there is nothing to republish and
no latest-wins queue is wanted; each publish is distinct operator intent):**
- sentinel `ErrDisconnected`; in `Publish`, after the done fast-path and before
  spawning the goroutine: nil-client guard + `if !c.client.IsConnectionOpen()
  { return ErrDisconnected }` (paho: false exactly while reconnecting).
- `writePublishErr` maps `ErrDisconnected` → 503 "not connected, dropped —
  republish".
- bound the goroutine's wait: select on `tok.Wait()`-completion vs a timeout
  (and `c.done`), so a publish that races into a just-dropped conn cannot hang
  the HTTP handler either.
- OnConnect: deliberate no-op beyond the existing resubscribe — the tree
  self-heals from the retained burst; do NOT replay old browser commands (§8).

**Tests.** `internal/mqtt` has no test file: add one with an unexported
`connOpen func() bool` seam (defaulting to `c.client.IsConnectionOpen`) or a
fakePaho that overrides `IsConnectionOpen`; assert ErrDisconnected while
"reconnecting", success when open. handlers test mirrors
`TestPublishErrShuttingDownMaps503` for the 503 mapping.

**Deploy/verify.** `testui/deploy.sh`. Verify: stop the mosquitto/HA broker
path or firewall it briefly → a UI cmd click returns 503 immediately (no hang,
no backlog); reconnect → no DUP replay of pre-outage clicks.

---

## Part 2 — Systemic patterns (fleet track)

Four cross-cutting patterns came out of the review. Waves 1–2 already retired
their worst instances (spid bounded reads, wrc write deadline, flexbridge
probe watchdog, ultrabridge reconnect ritual, icom9700 session deadlines).
What remains, ordered by hazard:

### S1. Deadline-less I/O — bounded-token / bounded-serial sweep

Fleet reference patterns: powerseq `WaitTimeout(10s)`
(`powerseq/internal/mqtt/client.go:40-71`) for paho tokens; ultrabridge 500 ms
serial read timeout + acom 1 s read + 30 s silence watchdog for serial.

| # | Module | Site | Exposure | Fix |
|---|--------|------|----------|-----|
| S1a | antennaselect | `internal/mqtt/client.go:472,478` — QoS 1 `token.Wait` on the single jobs worker | reconciler dead while broker stalls: idle walk-away grounding, select, band-follow all stop; `/status` still online | powerseq WaitTimeout pattern; drop + fault on timeout |
| S1b | shelly-power-bridge | `internal/bridge/publish.go:24`, `cmd/.../main.go:258` — same on per-slot workers | power plane freezes with commands in flight; powerseq shutdown steps queue but never apply | same |
| S1c | acom1200s-pa-bridge | `internal/acom/device.go:491` — serial `Write` under `d.mu` | /cmd worker parks holding `d.mu` → telemetry + ACK path + ctx-close all block → shutdown hangs to SIGKILL | bound the write (worker-side timeout; read side already bounded) |
| S1d | pelcobridge2 | `internal/serialio/serial.go:65` (read), `:99` (write) — no SetReadTimeout, no write bound; `internal/rotctld/server.go:106,109` per-conn no deadlines | WORST: a wedged USB-RS485 adapter parks the one engine goroutine (TUI/rotctld/MQTT stop all dead against a possibly-moving head) AND removes the error that drives auto-reopen | SetReadTimeout on OpenPort (mirror ultrabridge), bounded write, per-conn rotctld deadlines. Shack-PC service — bench-verify; note all pelcobridge2 work is historically uncommitted — commit first |
| S1e | hadiscovery | `internal/mqtt/client.go:119,179,185` | availability-only (discovery renders stall) | same WaitTimeout pattern |
| S1f | fleet cluster | OnConnect `Subscribe` `token.Wait` inside paho's OnConnect goroutine: antennaselect `:463`, hadiscovery `:170`, shelly `main.go:187,199,213`, powerseq `:233`, acom `:170`, pelco `slot.go:75` | a stalled SUBACK parks the OnConnect goroutine → remaining subscriptions never execute (silent partial subscribe) | WaitTimeout + Warn (or move subscribes onto the jobs worker) |

Sequencing: S1a+S1b are small, mechanical, safety-relevant — do first (deploy
with wave-3's testui deploy run). S1c/S1e/S1f next. S1d last (needs the shack
PC and a bench; includes the uncommitted-work hazard).

### S2. Reconnect state restoration — atr1k is the last known gap

Reference: ultrabridge `onConnect`/`republishState` (this worktree,
`internal/mqtt/client.go:200-219`) — status → meta → state (dedup-bypass) →
subscribe. spid `mqttslot/slot.go:284-291` is the second exemplar.

**atr1k-tuner-bridge** (scout-confirmed):
- CleanSession is paho-default **true**, OnConnect exists
  (`cmd/atr1k-tuner-bridge/main.go:158-172`) but re-lands only `/meta`; `/state`
  is change-only (`internal/bridge/bridge.go:235-245`, no `hasLast` flag) →
  after a broker flush, a steady-SWR tuner state stays missing/stale forever.
- Fix: add `hasLast bool` next to `last` (`bridge.go:51-54`); new
  `Bridge.RepublishState()` (no-op until first telemetry — guards the
  all-zero pre-connect snapshot); wire into OnConnect at `main.go:171` (pass
  the real `b` into `connectMQTT` and retire the throwaway-bridge
  `publishMetaOnReconnect` hack `:189-204`). Tests: `MemoPublisher`
  (`publish.go:21-71`) — latch dedup → RepublishState → assert second publish;
  mirror ultrabridge's `TestOnConnectRepublishesStateUnchanged`.
- Audit rider: confirm flexbridge/antennaselect/shelly/logger-spot OnConnect
  paths restore (or legitimately don't publish) `/state`; apply the ritual
  where change-only dedup exists. flexbridge already restores via
  Reset+republish on the radio reconnect, but its *MQTT*-plane reconnect
  republish needs the same one-glance audit.

### S3. ctx-parked goroutine leak per reconnect

Pattern (acom `internal/acom/device.go:115-125`): per-Run `stop := make(chan
struct{}); defer close(stop)`; the watcher selects on `ctx.Done()` **or**
`stop`, so it always exits when Run returns.

- **atr1k** `internal/tuner/device.go:107-110` — watcher parks on the root ctx;
  one leaked goroutine per reconnect cycle; systemd `TasksMax=64` makes a
  flapping link walk toward the cap. Apply the pattern verbatim.
- **wrc** `internal/rotor/device.go:89-92` — same shape (follow-up to wave 2's
  write-deadline commit; 3-line change, same file).
- Regression test (both): httptest server, Run, close server, poll
  `runtime.NumGoroutine()` back to baseline. atr1k has the harness
  (`device_test.go:58-64`); wrc's new `device_test.go` (wave 2) gets the same
  test.

### S4. atr1k deadline-less websocket read (the last unbounded read fleet-side)

`internal/tuner/device.go:113` — `ReadMessage()` with no read deadline, no
keepalive: a silently dead link wedges Run forever, `wsLoop` never reconnects,
retained `/state` keeps `device_online:true` with frozen values — and
antennaselect engages the ATU off this slot. (Write side is already bounded,
5 s.)

Fix: per-iteration `SetReadDeadline(now + readTimeout)` before ReadMessage;
the ATR streams meter frames continuously (never legitimately quiet, incl.
during tune settling), so a silence bound is honest — ~30 s, parity with
acom's `silenceLimit`. Make `readTimeout`/`writeTimeout` package vars "so
tests can compress them" (acom precedent, `device.go:249-271`). Test:
wedged-peer httptest (accept, never write) → Run returns within the
compressed bound. Optional (note, don't block): SetInline/Tune optimistically
publish local state after a write that only drained into a dead kernel buffer
— the read deadline converts the wedge into a reconnect, which is the honest
signal; revisit optimistic-publish separately.

---

## Suggested execution order

1. **testui pair (T3, T4)** — one module, one deploy, removes a live
   disarm-on-click hazard; UI fix is correct against the currently flashed
   firmware.
2. **hf_console pair (T1, T2)** — one Flutter build, both surfaces; APK
   sideload needs the tablet (manual step).
3. **S1a/S1b** (antennaselect, shelly) — mechanical WaitTimeout ports,
   safety-relevant, deploy with the next shari batch.
4. **atr1k bundle (S2 + S3 + S4)** — one module, three known-shape fixes,
   full test coverage exists in harnesses already present.
5. **S1c/S1e/S1f** (acom write bound, hadiscovery, OnConnect SUBACK cluster).
6. **S1d** (pelcobridge2 serial) — shack PC, bench session, commit-first.

Wave 4 (docs/register rot: pelco known-issues FIXED claim wrong, ultrabeam
register, hf_console ×2, clean-shutdown register missing logger-spot-bridge,
logging.md notes, testui retained-/cmd vs m5stamp self-heal tension,
m5stamp expose value_type fix) and wave 5 (carried-findings triage) stay as
previously scoped.
