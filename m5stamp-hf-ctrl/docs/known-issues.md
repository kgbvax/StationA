# Salvage: m5stamp-relay-controller.md
> Extracted from PRD/03-components/m5stamp-relay-controller.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [defect] D1 / O1 — Arm relay frozen during WiFi outage (STILL OPEN)

The main loop's WiFi gate early-returns BEFORE arm recomputation. So with WiFi
down the arm relay holds its last physical position indefinitely — no heartbeat
enforcement, no recomputation. The controller enforces the "any failure drops
the arm" contract only while WiFi is up. On WiFi recovery the recomputation
runs and (radio feed stale) drops the arm within one loop pass. **Recommended:
compute the arm formula regardless of link state.** The safe direction is to
drop the arm, never to hold it. The team must resolve this before any
re-implementation.

Code-check 2026-09-03: still open — `m5stamp-hf-ctrl/src/main.cpp:541-549`
(WiFi gate `return`s early) before `recomputeArm()` at `main.cpp:569`.

## [defect] D5 / R6.1 / O4 — JSON-boolean `set_enabled` silently disarms (STILL OPEN)

The `value` field is carried as a **JSON string**; booleans ride as the strings
`"true"`/`"false"`. The implementation extracts `value` with a string-typed
default (`doc["value"] | ""`). A sender that publishes a JSON boolean
(`"value": true`) gets an empty string. For pa-arm `set_enabled` that
**silently sets `enabled=false` and disarms the PA**. The `/meta` `expose`
block even declares `value_type:"boolean"`, which invites exactly this bug.

A re-implementation MUST NOT reproduce the silent disarm. It MUST either
(a) accept JSON booleans and the strings `"true"`/`"false"` interchangeably, or
(b) reject any non-string `value` **loudly** (publish an error or log; never
silently apply the safe-side-but-wrong interpretation of a mistyped
authorization grant). Whichever option is chosen, a command
`{"action":"set_enabled","value":true}` (JSON boolean) MUST never set the
authorization to `false` without any indication.

Code-check 2026-09-03: still open — `parseCmd()` at
`m5stamp-hf-ctrl/src/main.cpp:92` (`value = doc["value"] | ""`), applied at
`main.cpp:144` (`enabled = (value == "true")`).

## [defect] D4 / O2 (antenna half) / O3 — `antenna_ready` has no staleness window (STILL OPEN)

A stale antenna-switch state stays "ready" forever. If the antenna-switch
bridge is down but its last retained state names a port, the arm can close on
data from a dead slot. The ant-switch's LWT is visible to other consumers, but
this component does not use it. If a staleness window is added, its size and
the ant-switch producer's publish cadence are one decision — the ant-switch
bridge publishes change-only, so the same producer-cadence problem that hit the
radio feed applies. (The heartbeat-staleness *error string* half of O2 is
already fixed — see the fixed-defects note below.)

Code-check 2026-09-03: still open — `handleAntSwitchState()` at
`m5stamp-hf-ctrl/src/main.cpp:162-170` records no timestamp and
`currentPaArmError()` checks only the boolean.

## [defect] D6 / O11 — No MQTT reconnect backoff (STILL OPEN)

The reference tries reconnect every loop pass (~every 50 ms, no backoff —
~20 tries/s per client) indefinitely. A re-implementation must use bounded
backoff; the backoff parameters are an open choice.

Code-check 2026-09-03: still open — `SlotMqtt::loop()` in
`m5stamp-hf-ctrl/src/mqtt_slot.h:44-54` calls `connect()` on every disconnected
loop pass. (Its comment says "Handles reconnect with a backoff" — no backoff
exists; the comment is drift.)

## [defect] D8 / O5 — Retained replay reverts outage-time button toggles (STILL OPEN)

A front-panel button press while disconnected still toggles the physical
relay, but the controller skips the `/cmd` echo. The retained intent on the
broker is therefore STALE. On reconnect the broker's retained-command replay
**overwrites the local change** (the relay reverts to the pre-outage intent).
A re-implementation must decide explicitly: keep this behavior, or reconcile
local and retained intent (for example republish the new intent on reconnect
before applying the replay).

Code-check 2026-09-03: still open — `publishSwitchCmd()` returns early when
disconnected (`main.cpp:262-263`), and `onConnect()` re-subscribes `/cmd`
(`mqtt_slot.h:95-103`), so the replay lands.

## [defect] D10 / O6 — Relay readback fail-safe claimed, not implemented (STILL OPEN)

The relay readback has no failure detection; a comment claims fail-safe-on-error
but the code returns the raw expander read. A re-implementation MUST treat a
failed/unverifiable readback conservatively (report `off`/open), OR show that
the read path cannot fail. It MUST NOT claim fail-safe readback unless the
readback code implements the fail-safe behavior.

Code-check 2026-09-03: still open — `relayGet()` in
`m5stamp-hf-ctrl/src/relay.h:31-33` is the raw `M5StamPLC.readPlcRelay(ch)`
call; the fail-safe exists only as a comment.

## [defect] D11 — OTA start drops only the arm relay (STILL OPEN)

The OTA start hook MUST de-energize the arm relay before the device reboots
into an update, but does NOT drop the PA/TRX remote-on relays at update start.
The window is short, and the post-update cold boot opens everything anyway. To
keep this or to fix this is a documented choice.

Code-check 2026-09-03: still open — `otaInit().onStart` at
`m5stamp-hf-ctrl/src/main.cpp:504-507` clears only `RELAY_PA_ARM`.

## [defect] D7 / O13 — error-vs-enabled doc drift (STILL OPEN, in the live doc)

The reference publishes the `error` string from the safety inputs
**regardless of `enabled`** (contrary to its own API doc). Decision: keep this
(informative — it tells the operator why the arm *would* be blocked), or
suppress errors while deliberately disarmed — coordinate with the operator
console's display logic.

Code-check 2026-09-03: drift persists — `currentPaArmError()`
(`main.cpp:321-328`) does not test `enabled`, while
`m5stamp-hf-ctrl/docs/m5stamp-hf-ctrl-mqtt-api.md:146-148` claims "When
`enabled` is false, `armed` is false with no `error`". One of the two must
change.

Note on PRD defects D2 and D3 — verified FIXED in current code, not carried:
D2 (silent arm drop on heartbeat timeout, no error string) is fixed by
`currentPaArmError()` returning `"radio feed stale"` as the second-highest
precedence (`main.cpp:321-328`). D3 (change-only radio producer starving the
10 s heartbeat) is fixed producer-side: `flexbridge/internal/bridge/bridge.go`
(`DefaultStateHeartbeat`) republishes `/state` while the radio link is live.

## [decision] O7 — The electrical contract is OUTSIDE the repository

The physical wiring exists only in the shack, not in the repo or docs. That
wiring includes: which relay contacts interrupt the PA's hardware enable line,
and how the arm relay ANDs into the hardware TX-inhibit series interlock
chain; the remote-on line polarity; and the contact ratings. The system safety
model places enforcement in this hardware chain; software only arms and
disarms. A re-implementation MUST get the wiring from the site. It MUST also
treat fail-safe-open as a hard requirement checked on the bench: pull power
with the arm energized and confirm that the PA goes unable to transmit.

## [decision] O8 — Deployed PLC's broker is unverified; field unit is pre-OTA

The repo's checked-in `secrets.h` / `secrets.example.h` now point at
`192.168.1.139` (the shack-local broker migration, committed 2026-08-29), but
the deployed PLC's actual broker is whatever its flashed firmware says. Note
the constraint that makes on-site confirmation mandatory: the PLC in the field
runs **pre-OTA firmware** (no mDNS, no network-update listener), so it cannot
be repointed or updated over the network at all — the first firmware update
must be a physical USB flash. Verify what the live PLC connects to before
trusting it on the new broker topology.

## [decision] O9 / O10 — Two documented deviations from station convention

- `ts` form: this component publishes `ts` as **PLC uptime in milliseconds** —
  a monotonic freshness marker, NOT wall-clock time (the controller has no
  RTC). The station model gives RFC 3339 UTC strings for `/state.ts`.
  Consumers already treat this component's `ts` as a freshness marker only.
  Decide whether the rebuilt component keeps uptime ms (documented deviation)
  or moves to RFC 3339 (needs an RTC or NTP on the embedded node).
- Publish QoS: the reference publishes state/meta at QoS 0 (retained); the
  station convention is QoS 1. Retention, not the QoS level, is what makes
  self-heal work (the LWT is QoS 1 retained). Pick one and conform.

## [decision] O12 — PLC #2 / `muehle/uhf/pol-ctrl` is attributed but nonexistent (GAP)

The station slot table attributes the slot `muehle/uhf/pol-ctrl` (X-Quad UHF
antenna polarization relay control, "PLC #2") to this component's project.
The repository holds **no PLC #2 firmware**: no second build environment, no
pol-ctrl code, no `uhf/` topics anywhere in the source (only the
`m5stamp-plc1` / `m5stamp-plc1-ota` environments exist in
`m5stamp-hf-ctrl/platformio.ini`). This is a documented gap, not a component.
Either the slot-table entry is aspirational, or the firmware lives elsewhere.
Any re-implementation must treat `muehle/uhf/pol-ctrl` as **not covered** and
raise it as its own scope decision.

Code-check 2026-09-03: still a gap — no `pol-ctrl`/`uhf` source in the repo.

## [decision] O14 — Ultrabeam switch-port conflict (inherited, station-wide)

Station docs say the Ultrabeam is on antenna-switch **port 3**;
`antennaselect` config and the console say **port 4**. The pa-arm component
only consumes `selected ∉ {"", "off"}`, so either variant satisfies its
`antenna_ready` input. But the on-site port map needs confirmation before any
wiring-dependent reconstruction.

Code-check 2026-09-03: still conflicting — root `CLAUDE.md` says
`ant/ultrabeam` (port 3) while
`antennaselect/config.example.toml:27` says `port4 = "ultrabeam"`.

## [requirement] Behavioral contract points stated nowhere else

Normative items from the PRD not covered by the live integration model,
conventions, or the component's MQTT-API doc:

- **Worst-case arm-drop latency bound** (R3.1): the main-loop period is 50 ms
  in the reference; any period ≤ 200 ms is acceptable provided the arm relay
  still opens within one evaluation cycle of the input heartbeat expiring —
  worst case = heartbeat window + one loop period (~10.05 s at 50 ms, ~10.2 s
  at 200 ms).
- **Unparseable input must not refresh the heartbeat** (R3.3): the controller
  MUST silently drop an invalid-JSON `hf/radio/state` message, and that message
  MUST NOT refresh the heartbeat clock. Never-received counts as stale from
  boot.
- **Cold-boot safe state beats retained replay** (R3.6): all safety inputs
  start safe/false on boot (`enabled=false`, `radio_online=false`,
  `tuning=false`, `band=""`, heartbeat=never, `antenna_ready=false`), so the
  arm cannot close until the controller has received BOTH a fresh radio state
  AND a non-off antenna-switch state — even if a retained `set_enabled`
  command replays at reconnect.
- **The arm relay is not commandable** (R3.7): the only accepted command that
  affects it is `set_enabled`. No arm, disarm, or force action exists, and the
  authorization alone must never close the relay.
- **No bus-level PSU guard** (R4.9): the controller MUST NOT subscribe to
  `muehle/power/psu-13v8`. The dependency on that supply is purely electrical —
  the controller boots from it, so its loss kills the controller and the arm
  relay opens by physics. A bus-visible "supply lost" input to the armed
  formula must be added only as an explicit, separately-specified extension;
  silently adding one changes the arm formula's failure behavior.
- **Cold boot opens all four relays as the FIRST act** (R5.1): write all
  relays de-energized before any network activity (arm open, PA/TRX remote-on
  off).
- **No local override on the arm relay** (R5.6): no button can force or drop
  it; it moves only with `armed`. Buttons B/C toggle only PA/TRX remote-on
  (relays 3/4), debounced to at most one action per 150 ms, and must work with
  WiFi down.
- **Silent drop of unknown commands** (R6.2): unknown actions or unparseable
  JSON on either `/cmd` are dropped — no reply, no error topic, never a crash.
- **No MQTT echo** (R6.3): commands received over MQTT are never echoed back
  to `/cmd`; only front-panel button presses produce an echo (of the intent,
  so the retained intent and local state stay consistent while connected). The
  last external commander owns the retained `/cmd` content.
- **Reconnect sequence is bus-observable contract** (R8.1): on (re)connect, in
  this exact order — (1) publish retained `online` to the slot's `/status`;
  (2) subscribe the slot's `/cmd` (broker replays the retained command, and it
  is re-applied); (3) re-publish retained `/meta` and `/state`; (4) the pa-arm
  connection then subscribes `hf/radio/state` and `hf/ant-switch/state`, whose
  retained snapshots replay onto the safety inputs at that point. The first
  post-reconnect `pa-arm/state` always reflects the pre-replay RAM state.
- **Update procedure contract** (R8.6): an update starts when the controller
  drops the arm relay; the device reboots; the new firmware cold-boots all
  relays open; the switch relays' commanded state stays lost until the
  broker's retained `/cmd` replays on reconnect; downstream consumers see both
  slots' LWTs fire `offline` during the reboot.
- **No persisted state** (R8.4): the controller persists no state across
  reboots — it rebuilds everything from retained MQTT messages plus safe
  defaults; there is no flash/NVRAM state.
- **Two distinct 10-second rhythms — do not conflate** (§3.2): the *input*
  heartbeat (10 000 ms) is the radio-state freshness window feeding the armed
  formula; the *output* heartbeat (10 000 ms) is the dedup-suppressed periodic
  republish of the retained `pa-arm/state`. The output heartbeat only refreshes
  the bus snapshot for downstream freshness judgment; it never touches any
  relay. The 10 s input window is a system-wide figure — every downstream
  consumer depends on it, so it must not be widened unilaterally, and any
  producer-cadence change must be coordinated with it as one decision.
- **WiFi radio configured for reliability over power savings** (R7.2): the
  device is mains/13.8 V powered, not battery.

## [unique] Reference-implementation library lineage (non-normative)

Why the embedded stack looks the way it does (any stack satisfying the
behavioral contract is conformant; this records the reasoning):

- Arduino C++ / PlatformIO (platform `espressif32`, board
  `esp32-s3-devkitc-1`, C++17) for the M5 Stamp PLC (StamPLC K141: ESP32-S3 +
  AW9523B I2C GPIO expander at address 0x59 — 4 relay outputs, 8 unused
  digital inputs). Flashing via PlatformIO `esptool` (USB, 921600 baud) or
  `espota` (network), wrapped by `deploy.sh usb|ota`. A 115200-baud serial
  link exists for development and exception decoding only — no runtime serial
  protocol.
- `PubSubClient ^2.8`: its **one-will-per-client limitation** is the reason for
  the two-connection pattern (one MQTT connection per slot, so a crash fires
  both slots' `/status` LWTs simultaneously with no stale-online gap); its
  256-byte default buffer is the reason for the 1024-byte buffer (the retained
  `/meta` with its `expose` block exceeds smaller defaults).
- `ArduinoJson ^7.0`: its `doc["value"] | ""` string-extraction default is the
  mechanism behind the JSON-boolean silent-disarm defect (D5 above).
- `M5StamPLC ^1.0` is the relay hardware seam (thin HAL in `relay.h`);
  `M5Unified ^0.2` provides LCD/buttons.
- Clean session: the shared docs' blanket "clean session = no" claim does not
  match this component — self-healing here relies entirely on retained
  messages (retained-command replay), not session state, so either session
  mode is acceptable provided the retained-command replay on reconnect holds.