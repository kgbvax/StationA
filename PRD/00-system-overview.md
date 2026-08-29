# 00 — System overview: the Mühle amateur-radio station automation

**Purpose of this document.** This is the entry document of the PRD. The PRD reconstructs
"stationa", the automation system for the Mühle amateur-radio station. A competent
software engineer can read this document STANDALONE. The engineer has never seen the
station, never used MQTT, and knows nothing about amateur radio. This document defines
what the system is, what hardware exists, and what an operator does with it. It also
defines what "correct" means. It gives the scope and the honest list of what is broken
or unresolved. The PRD defines every specialized term at first use. Later documents
carry the exact contracts (topic strings, JSON fields, timings). This document orients
the reader and links to them:

- `01-architecture.md` — the bus, the planes, the component model.
- `02-interface-spec.md` — exact topic/JSON/payload/timing contracts.
- `03-components/<slug>.md` — one per component plus a common-runtime note.
- `04-console.md` — the operator tablet console.
- `05-deployment-ops.md` — hosts, deployment, operations.
- `06-safety.md` — the safety model in full.
- `07-priorities-milestones.md` — build order and priorities.

---

## 1. What the system is

**Amateur radio** (ham radio) is the licensed hobby of two-way radio communication.
Operators hold government-issued licenses and callsigns, and exchange contacts ("QSOs")
with other operators worldwide. They communicate by voice, Morse code (CW), or
narrow-bandwidth digital modes. The hobby involves radios that transmit on many
frequency bands. The antennas must match the frequency in use. A wrong switching order
can damage the equipment — or harm the operator.

A **station** is a complete radio installation. The **Mühle** station (German for
"mill", site address prefix `muehle`, operator callsign DL9ET) is a two-station
installation at a rural property. It has an **HF** station (high-frequency, roughly
1.8–54 MHz — long-distance shortwave) and a **UHF** station (144–440 MHz —
local/line-of-sight). The HF station is the automated one and the subject of this PRD.

**stationa** is the software that automates this station end-to-end. It consists of
many small independent services. People call them **bridges**. Each bridge fronts
exactly one piece of hardware (or one pure-logic role such as "antenna selection
policy") and mirrors it onto a shared message bus. The bus protocol is **MQTT**, a
lightweight publish/subscribe protocol. Clients publish messages addressed to
hierarchical text *topics* on a central server — the **broker**. Any client subscribed
to a matching topic filter receives them. A **retained** message is one the broker
stores and re-delivers immediately to every future subscriber. A late-joining component
instantly sees the last known state without polling. **LWT (Last Will and Testament)**
is an MQTT feature. A client registers a farewell message at connect time. The broker
publishes it automatically if the client disappears without a clean disconnect.

There is deliberately **no central server** in stationa. Components integrate by
subscribing to one another's state topics, never by calling one another. The live
configuration *is* the documentation. Anyone can read the component inventory off the
bus at any moment. It never drifts from reality the way a hand-maintained document does.

The bus is also deliberately **consumer-agnostic**. Any number of **consumers** can
attach to the bus. Consumers are separate processes that only subscribe. No core
component needs them. The reference consumer is **Home Assistant**, an open-source
home-automation platform that renders device states as a dashboard. One logic component
exists to serve consumers of that kind. The **discovery renderer** (slot
`muehle/hf/discovery`) reads each component's self-describing `/meta` "expose" block —
a machine-readable list of the slot's observable and controllable fields. It translates
the block into Home Assistant's discovery configuration. New devices then appear there
with no per-device integration code. Consumers are not necessary: **deleting every
consumer must leave the station running identically**. Core components know nothing
about any consumer and never publish under a consumer's topic tree. (One accepted,
temporary deviation exists in the current implementation. The radio and
antenna-controller bridges carry a legacy embedded Home-Assistant-discovery code path.
The path is off by default, and removal comes once the discovery renderer is proven.
With the path off, the canonical bus does not change.)

The system, in one sentence: an operator taps "START STATION" on a tablet and picks a
frequency band. The automation powers the equipment in a safe order and routes the
correct antenna. It pre-tunes the antenna and amplifier for that band, keeps the
station safe while transmitting, and grounds the antennas when idle. One more tap shuts
everything down in reverse order.

**Reference-implementation note (non-normative).** The existing implementation has a
set of Go services, two embedded C++/YAML firmwares, and one Flutter tablet app. They
deploy on a Raspberry Pi named `shari` at `192.168.1.139`. The reconstruction team can
use any stack. Only the bus behavior, safety behavior, and timings are normative.
`02-interface-spec.md` and `06-safety.md` state them as such.

---

## 2. What the station physically consists of

Every physical device in the automated station has a one-sentence plain-language
description. (The slot address is the MQTT topic namespace that mirrors a device onto
the bus. See `01-architecture.md` §2.)

### 2.1 HF chain (the automated station)

| Device | What it is, in one sentence | Slot address(es) |
|---|---|---|
| **FLEX-8400 transceiver** | The radio itself — a software-defined radio (a radio whose filtering and demodulation run in software). It both receives and transmits on all HF bands. People call it "TRX" (transceiver) throughout. | `muehle/hf/radio` |
| **ACOM 1200S power amplifier (PA)** | A device that boosts the radio's transmit signal from roughly 40 W up to as much as 1200 W. A transmission into a badly matched antenna can damage it. | `muehle/hf/pa` |
| **ATR-1000 antenna tuner (ATU)** | An impedance-matching network of inductors and capacitors. Operators can switch it *in line* with the feed cable to make a non-resonant antenna look like a proper electrical load. That matters on bands where the antenna does not match naturally. "SWR" (standing-wave ratio) is the mismatch measure. 1.0 is perfect. 3.0 or more is dangerous, because reflected power heats the amplifier. | `muehle/hf/tuner` |
| **Ultrabeam RCU-06 antenna controller** | The controller for a motorized beam antenna. The beam's aluminum element tubes physically lengthen and shorten on command to tune the antenna to the operating frequency. The beam can point forward, backwards (180°), or both directions (bi-directional). It can also retract fully in storms. | `muehle/hf/ant-ctrl` |
| **1:6 antenna switch** | A relay board that connects the station's single feed line to exactly one of six antenna ports (or none). Switching must never happen while radio-frequency power flows, or the relay contacts arc. | `muehle/hf/ant-switch` |
| **Yaesu G-450DC rotator** | The motor that turns the HF antenna mast to point the beam at different compass **azimuths** (bearings, in degrees). An AF6SA "WRC" controller drives it over websocket. | `muehle/hf/rotator` |
| **M5 Stamp PLC #1 (relay controller)** | A small programmable-logic controller (a tiny embedded computer with relays) with a fixed relay mapping. **Relay 3 closes the PA's "remote power-on" trigger line, and relay 4 closes the radio's "remote power-on" trigger line**. **Relay 1 is the PA arm relay** — the software-controlled permission. Hardware conditions must also hold before the amplifier can transmit. | `muehle/hf/switch` and `muehle/hf/pa-arm` |
| **Shelly smart plug (master mains)** | A wifi-controllable mains outlet. It switches the station's master supply. | `muehle/power/master` |
| **Shelly smart plug (13.8 V PSU)** | A wifi-controllable outlet for the 13.8 V DC power supply. It switches the supply that feeds the station's control equipment (antenna switch, relay controllers, and more). | `muehle/power/psu-13v8` |

### 2.2 Antennas (passive — no software presence)

Antennas themselves are deliberately **not** bus components. They are passive resources
named only in configuration:

- `ant/ultrabeam` — the motorized beam described above. (Wired to a switch port. See
  the open port-number decision in §7.)
- `ant/fan-dipole` — a multi-band wire antenna (two arms fan out from the feed point).
  It is naturally resonant on the 80 m and 40 m bands. (Band labels like "80 m" are
  wavelength shorthand for named frequency allocations. The 80 m band spans 3.5–4.0
  MHz, roughly an 80 m wavelength. The exact allocation table lives in
  `02-interface-spec.md`.)
- `ant/dummy-load` — a heat-dissipating test load. It radiates nothing. Operators use
  it for testing and as a safe place to park transmit power.
- `ant/k9ay-loop` — a receive-only directional wire loop. (The model defines a separate
  controller slot `hf/rx-loop-ctrl`, but no component implements it. See §7 item 9.)
- The antenna switch's `off` position **shorts the open ports to ground**. This is the
  station's lightning/idle protection, and "grounded" is the safe idle state.

### 2.3 UHF station (partially automated)

| Device | What it is | Slot address |
|---|---|---|
| **PTS-303Z/3050DZ pan/tilt head** | The UHF antenna rotator — a pan/tilt motor head (the kind used for CCTV cameras). The host steers it over an RS-485 serial line through a USB adapter with the Pelco-D protocol. (RS-485 is a multi-drop serial electrical standard.) An interactive terminal application on the shack PC drives it — deliberately not a headless service. | `muehle/uhf/rotator` |
| **IC-9700 radio, X-Quad antennas, masthead preamps** | The UHF radio and antennas — **not automated** and **not planned** (see §5). No component implements the polarization relay controller for the X-Quad antennas. That is a documented gap. | `muehle/uhf/pol-ctrl` (unimplemented) |

The UHF rotator console exposes exactly one *remote* motion interface: a
**rotctld-compatible TCP server**. Rotctld is the text protocol of Hamlib (an
open-source radio/rotator control library). Tracking and control clients such as
gpredict (a satellite-tracking program) speak it. **Arming** is the safety gate for
precisely this path. Remote rotctld clients can move the head only after the operator
has armed it from the interactive console. Arming is never possible remotely. The MQTT
surface exposes only a `stop` command and can never move the head at all.

### 2.4 Compute and infrastructure

| Thing | What it is | Notes |
|---|---|---|
| **shari** | A Raspberry Pi (single-board Linux computer) at `192.168.1.139`. It hosts all unattended services. Each runs as a hardened systemd service (a Linux service-manager unit with restricted privileges). | See `05-deployment-ops.md` |
| **shack-pc** | The shack's Windows PC. It hosts only the interactive UHF rotator console. A human starts it by hand. Arming the rotator for remote control is a deliberate manual keyboard act. | |
| **MQTT broker** | The message-relay server the whole system publishes through. The normative requirement is **any MQTT broker with a persistent store** (retained messages survive broker restarts). The deployed reference implementation uses Mosquitto at `tcp://192.168.1.50:1883`. | Broker topology is an open decision, §7 |
| **Android tablet** | A fixed-mount tablet on the operating desk. It runs the operator console app — the only human interface to the HF chain. | See `04-console.md` |
| **horstreporter** | The owner's own external web service. It aggregates **DX spots** (reports that operators heard a distant — "DX" — station, with location and signal strength). It serves them over HTTPS Server-Sent Events. The console plots them on its maps. | External dependency, read-only |

---

## 3. What an operator does with the system

A day-in-the-life narrative. `04-console.md` gives the exact UI detail.
`02-interface-spec.md` §3 and `06-safety.md` give the sequences and timings behind
it.

**Powering up.** The station is cold: mains off, antennas grounded. The operator walks
to the tablet and taps **START STATION**. A logic component called the **sequencer**
(`powerseq`, slot `muehle/hf/power-seq`) executes an ordered startup with real
confirmations at each step. The automation must follow this order exactly:

1. Master mains plug on.
2. Wait 30 s for network equipment (the wifi smart plugs and the broker) to boot. The
   shipped default network delay (`network_delay_s`) is configurable per site.
3. 13.8 V PSU plug on.
4. Wait until the antenna switch, both relay channels, and the PA arm report alive on
   the bus. Those controllers boot from that supply.
5. Switch the radio remote-on relay on. Wait until the bus confirms the radio bridge
   alive.
6. Switch the PA remote-on relay on. Wait until the PA's own telemetry confirms
   power.
7. Enable the PA arm permission. The permission translates into "armed" only when the
   hardware conditions are also true: radio alive and online, not tuning, antenna
   connected, band safe. The relay controller's firmware computes the arm relay.
   Software never commands it directly.

Each wait step has a deadline (default 120 s). A timeout stops the sequence, records a
fault naming the failed step, and does **not** roll back already-completed steps. The
operator watches the sequence phase (`starting` → `running`) on the console.

**Picking a band.** The operator taps a band button (for example, 20 m). Two things
happen automatically, driven by the **antenna reconciler** (`antennaselect`, slot
`muehle/hf/antenna-select`) — the policy component that is the single writer of
antenna routing intent:

- Band policy resolves the band to an antenna resource (20 m → the Ultrabeam beam, 80/
  40 m → the fan-dipole, anything else → the fan-dipole through the tuner) and routes
  the antenna switch there — **without hot-switching**. A port change must wait while
  any transmit power flows. The reconciler only withholds the port change while the
  radio transmits, and it emits the change once the radio confirms receive. The
  reconciler never inhibits RF itself — the hardware interlock chain does that
  enforcement (§4.5).
- **Band-follow**: because the reconciler selected the Ultrabeam, it also forwards the
  operating frequency to the antenna controller, so the beam's elements motor-tune to
  resonance. **PA-follow** pre-positions the amplifier's band setting, so it does not
  trip on the first transmission on a new band. **Tuner-follow** engages or bypasses
  the antenna tuner per band. The deployed rule: the ATU goes in line on the **30 m,
  60 m, 80 m, and 160 m** bands (the configurable `atu_bands` list). That rule
  applies when the selected resource is the fan-dipole. Otherwise it stays bypassed.
  Note the tension.
  The docs describe the fan-dipole as naturally resonant on 80 m, yet the deployed
  configuration engages the ATU there too. §7 item 13 records this as an open
  decision.

The operator can override any of this: manual antenna selection, forcing the antenna
to the dummy load, or switching the reconciler to manual mode. The idle rule (below)
itself overrides an operator hold — deliberately, so that a forgotten hold cannot
defeat walk-away safety.

**Transmitting.** The operator speaks or **keys**. "Keying" is ham shorthand for
starting a transmission (from the Morse-code key). The moment when a transmission
begins has the name **key-up**. Transmit permission works in layers. The fast hardware
key line — a physical wire that inhibits transmit within the radio — runs in series
through the interlock chain. The chain ANDs the software arm permission into it
electrically. Software never sits in the per-transmission path — it only arms or
disarms. On the console the operator watches forward power, SWR, PA temperature, tuner
state, and any faults. Every control stays disabled while its owning component is not
confirmed alive.

**Watching DX spots.** The console's map shows FT8/FT4 digital-mode spots from
horstreporter as a live overlay on an azimuthal (true-bearing-and-distance) compass
and a world map. The operator can see where activity is and turn the beam toward it
(tap-to-aim) or pick a more promising band. (FT8 and FT4 are specific narrow-bandwidth
digital modes. Stations on them exchange short, structured, computer-encoded messages
on fixed timed slots. Spot aggregators receive reports of contacts as automated spots.
Most of horstreporter's data comes from there.)

**Walking away.** If the operator stops touching the radio for 30 minutes (no dial
change, no transmit), the reconciler infers the station inactive and routes the antenna
switch to `off`. It then grounds the antennas (lightning protection). This idle rule
outranks operator holds by design.

**Shutting down.** The operator taps **STOP STATION**. The sequencer runs the exact
reverse order: disarm PA → PA off → radio off → PSU off → mains off. It staggers the
steps by 2 seconds to limit electrical inrush. One button, ordered, confirmed.

---

## 4. System qualities — what "correct" means

The reconstruction must satisfy these. The full normative statements live in
`02-interface-spec.md` §6 and `06-safety.md`.

1. **Never hot-switch antennas.** The antenna switch declares `hot_switch: false`. A
   port change must happen only with RF inhibited and receive confirmed first. The
   reconciler withholds changes during transmit. The console refuses direct-drive port
   changes unless it can positively confirm that the radio receives. Unknown state
   blocks the change — fail closed.
2. **Never transmit the amplifier into a bad load.** The PA arm permission needs an
   explicit enable command, the radio bridge alive, and a safe band. The radio state
   must also be fresh (heartbeat within 10 s), the radio not tuning, and the antenna
   on a valid port (not grounded). The arm relay is fail-safe **open**. Any loss of
   power, software, or heartbeat de-energizes it and removes transmit permission.
3. **Antenna grounded when idle.** After 30 minutes without dial or transmit activity,
   the system must ground the antennas. This idle rule must outrank operator holds
   (walk-away safety).
4. **One command to start and stop.** A single non-retained `/cmd` message (`start` /
   `stop` on the sequencer slot) drives the whole ordered, confirmation-based power
   sequence. No operator multi-step power-up is ever necessary. The sequencer is the
   one writer of the ordered chain, but it must not lock the power channels. Each
   channel stays directly commandable for troubleshooting while the sequencer runs.
5. **Safety lives on hardware, software only mirrors it.** The transmit-inhibit
   interlock is a physical line in series through the equipment. The messaging plane
   must never sit in the enforcement path. Loss of any or all software degrades the
   station to inaction (manual operation), never to hazard.
6. **Two-layer liveness, and checks must cover both.** A slot is "alive" only if BOTH
   checks hold. (a) Its `/status` topic says `online`. It carries the bridge process
   liveness as the plain string `online`/`offline`, maintained through MQTT
   last-will. (b) Its `/state` document's `device_online` boolean — the hardware
   behind the bridge — is true. Consumers acting on retained state must check both
   first. Keying on `/status`
   alone caused real failures (§6). The contract: on a clean shutdown, a component
   must publish retained `offline` to its own `/status` itself. The last-will covers
   only an abnormal disconnect. Known defect: the flexbridge, acom1200s, atr1k,
   shelly, and wrc bridges omit that self-publish today. A stopped service can then
   leave a retained `online` on the bus. The powerseq, antennaselect, hadiscovery,
   and ultrabridge components do publish `offline` on a clean shutdown. Consumers
   must not trust `/status` alone for anything safety-relevant.
7. **Observable state.** Every component publishes its full state as one retained JSON
   snapshot on change, with a UTC timestamp. The bus is the single source of truth. A
   late joiner reconstructs the whole station picture from retained messages alone.
   Stale or missing data must present as loud failure, not as plausible health. The
   console's link-down banner, offline rows with when-it-went-dark timestamps, and
   "unknown ≠ off" presentation rules exist for exactly this.
8. **Fail-safe defaults everywhere.** Power plugs power up off. The arm relay
   de-energizes to the safe state. The switch's `off` position grounds the antennas.
9. **Commands are fire-and-observe.** A command publisher never assumes success. Every
   consumer reacts to observed state, never to intent. Commands are normally not
   retained. The only retained commands are idempotent actuator setpoints that a
   component can safely replay on restart (power, relay positions, arm enable,
   antenna selection). A component clears a retained one-shot command immediately
   after execution.
10. **One arbiter per contested actuator.** Antenna routing has exactly one policy
    writer (the reconciler). It resolves an explicit priority ladder — idle >
    operator > auto. It publishes *why* the actuator is where it is (`source: idle |
    operator | auto`), so the bus explains itself.

---

## 5. Scope: what this PRD specifies, and what stays out

**In scope (specified by this PRD):**

- The HF chain automation: radio, PA, tuner, antenna switch, antenna controller,
  rotator, relay controller (power-on channels + PA arm), smart plugs, sequencer,
  antenna reconciler, discovery renderer — and the operator console.
- The UHF rotator console (Pelco pan/tilt head) with its deliberate manual-arming
  safety model.
- The bus contract itself (topics, payloads, liveness, timings) and the deployment/
  operations model.

**In scope as specification, but specified-not-implemented in the current system:**

- **The logging layer.** The written model `docs/logging-integration-model.md`
  documents an extension to the bus. It is a `qso-log` role. The role keeps the
  station's contact logbook and publishes per-QSO and DX-spot *events* (non-retained,
  one JSON object per contact). No implementing component exists today. The
  reconstruction treats it as future work. See `07-priorities-milestones.md`. (ADIF —
  the standard amateur-radio logbook interchange format — is the event vocabulary.)
- **Host liveness nodes** appear in the system model: `muehle/host/shari` publishes
  the fields `online`, `temp_c`, and `load`. `muehle/host/shack-pc` publishes
  `online` only. No current component publishes them. Model-only. Host liveness
  matters because shari is a single point (§6). Whether a reconstruction builds
  them is an open scope decision, not a settled contract. See §7 item 8 here and
  OD-12 in `01-architecture.md`. A re-implementation must not depend on them.

**Out of scope / known gaps, honestly:**

- **PLC #2 polarization firmware does not exist.** The slot table attributes
  `muehle/uhf/pol-ctrl` (X-Quad antenna polarization relays) to the M5 Stamp project.
  No firmware for that second controller exists in the repository. This is a
  documented gap — a component to build, not a component to reproduce.
- **The UHF radio (IC-9700) and UHF receive chain stay unplanned.** No bridge, no slot,
  no automation exists. Only the UHF rotator stays automated.
- **HVAC/climate control** is not wired anywhere. The console's climate panel is a
  static placeholder.
- The **auxiliary tooling** (documented in `05-deployment-ops.md` §4, with the
  console design mockups in `04-console.md` §6) is tools, not station components.
  The set: a passive bus traffic recorder, a browser-based bus
  inspection/stimulation tool, and a bench tool that reverse-engineered the UHF
  rotator's serial protocol.
  The pan/tilt head ignores
  absolute-position commands, its tilt readback is meaningless, and it emits
  checksum-valid garbage while moving. These bench-tool *findings* are load-bearing
  contract for any UHF rotator implementation.
- High-rate data streams (audio, spectrum, signal-strength samples) are explicitly
  **not** on the bus, by design.

---

## 6. Documented gaps and known incidents

An honest summary of the fragilities and faults in the current system. A
reconstruction must not blindly copy these. Where the research notes state the intended
fix, the PRD includes it. Detail lives in the per-component documents.

**Live incidents that shaped hard requirements:**

- **PA-arm heartbeat starvation (live).** The PA arm relay de-energizes if the radio's
  state is not refreshed within 10 s. But the radio bridge publishes state *only on
  change*. A healthy but idle radio then starves the heartbeat and silently drops the
  arm. **Hard requirement derived from this incident:** the radio state path must
  include a periodic republish/heartbeat (at most every 5 s) or a liveness mechanism
  with the same effect. (Project memory: "antenna grounding recovery gaps".)
- **Blocking MQTT handlers deadlock the client (live, twice).** In the reference MQTT
  library, incoming-message handlers run on the connection's dispatch thread. A
  handler that publishes synchronously deadlocks the whole client (it hit the
  discovery component live). A blocking connect also ignores shutdown signals (it hung
  a bridge at shutdown until the service manager force-killed it). **Library-independent
  constraint:** handler work must run isolated from the receive path (queue + single
  worker, drop on overflow rather than block). Shutdown must complete promptly even
  during a broker outage.
- **Consumer keyed on bridge liveness alone (live).** The reconciler originally watched
  only `/status`. When a device link died while its bridge stayed up, it chattered on
  stale retained state. Hence the two-layer AND rule (§4.6) as a hard requirement.
- **Command-payload convention violated (live).** A bridge parsed its command argument
  from a key named after the action, not from the universal `value` key. The command
  was silently wrong on the air. Hence the exact `/cmd` payload contract
  (`02-interface-spec.md` §1.4).
- **First key-up after grounding can hit the short.** Recovery from auto-grounding is
  fragile: re-activation can race. A transmit can begin before the antenna switch has
  settled on the new port. The cold-switch ordering contract (§4.1) and the
  switch's `settled` state exist to close this. Three more documented gaps sit in the
  same recovery chain. (a) The reconciler defers antenna changes during transmit, and
  that deferral has no timeout. A frozen transmit state then freezes the whole
  arbitration ladder, including the idle grounding, until the radio bridge reconnects.
  (b) On a reconciler restart, the idle clock starts at the restart time, and
  retained-state replay resets it. A restart during a transmit re-arms the 30-minute
  ground timer. (c) While the radio link is down, the idle rule overrides an operator
  hold, and no manual re-arm path exists. Only a radio state change counts as
  activity today. Intended fixes: give the deferral a timeout, make a restart not
  reset the idle clock, and decide whether an operator command counts as operator
  presence.
- **Serial self-heal (live-proven).** USB-serial adapters drop and re-enumerate under
  a new device file. The antenna-controller bridge must reopen by stable device path
  on I/O error rather than dying. A reconstruction on the same hardware needs the same
  self-heal.

**Structural fragilities (accepted, documented):**

- **The reconciler is a coordination single point.** If it dies, all soft automation
  (antenna routing, band-follow, tuner-follow, PA-follow) stops together. The station
  degrades to manual — acceptable only because safety is hardware. A reconstruction
  must add supervision/restart and an explicit "reconciler offline" indication.
- **shari is a single point.** It fronts most bridges and hosts all logic slots. Its
  loss takes the automation cluster offline simultaneously (correctly, through
  last-wills, but completely). Host liveness is load-bearing, yet no component
  implements it now (§5).
- **Retained state can be stale.** Retained messages survive crashes. Every consumer
  must check liveness before acting on them. Nothing safety-relevant can trust
  retained state. Relatedly: a component must publish retained `offline` itself on a
  clean shutdown (§4.6). Several deployed bridges omit this (§4.6), so a stopped
  service can leave a retained `online` on the bus.
- **`device_online` convention drift.** The model says the field is "omitted when
  true". The deployed bridges publish `device_online: true` explicitly. Consumers must
  treat both forms. See §7.
- **160 m has no resonant antenna.** It routes to the fan-dipole through the tuner at
  high SWR — a known operating condition, not a fault.
- **UHF rotator device quirks** (measured, contract): the head ignores
  absolute-position commands (open-loop jog/stop only). The tilt readback is
  meaningless. The readback is garbage while the motor runs. The head re-homes
  itself unless the host sends a self-check-disable command at start. In the current
  code the host sends that disable once per process start, not once per reconnect (a
  known defect). A reconstruction must send it after every successful link reopen.
- **UHF rotator auto-heal gives up.** After a serial failure, if the automatic reopen
  fails once, healing stops permanently until a human intervenes — known defect.
- **Console-side known issues** (fully enumerated in `04-console.md` §7): the web
  channel stores the MQTT password in plain browser storage. Operators cannot change
  broker credentials in-app after the first setup. The START button stays enabled
  while the shutdown sequence is still in flight. The climate panel is hard-coded mock
  data.
- **Tooling hygiene**: a real-looking MQTT password sits committed in a checked-in
  bench config of the bus-inspection tool — a leaked credential. The team must rotate
  it and not reproduce it. The bus-inspection tool's publish endpoint has no
  authentication on the LAN.
- The current system accepts **plaintext secrets in 0600 config files** as a
  trade-off (file permissions instead of a secrets manager). `05-deployment-ops.md`
  notes the stronger option.

---

## 7. Open decisions and unresolved facts

Each item below has more than one defensible answer or an unknown fact. The PRD gives
the evidence for every variant. The reconstruction team must resolve them, or, where
marked, confirm on the physical device. The team must not silently pick one.

1. **Which antenna-switch port carries the Ultrabeam — 3 or 4?** The integration
   documentation and repo-level notes put the Ultrabeam on switch **port 3**. The
   reconciler's example config, its deploy seed script, and the console's antenna map
   say **port 4**. (Everywhere else consistent: port 6 = fan-dipole, port 1 = dummy
   load. The docs' own passive-resource list mentions fan-dipole on port 2 in one
   place.) The live config file on the Raspberry Pi is authoritative. The workstation
   had no read access to it at PRD-writing time. The team must decide on the device.
   Port numbers are per-site configuration in any case. A reconstruction must never
   hard-code them, and every wiring-map document must present both variants until
   confirmation. The console's UI port map (off, port1 = dummy load, port4 =
   Ultrabeam, port5, port6 = fan-dipole) reflects the port-4 variant and omits
   unwired ports.
2. **Broker topology.** All deployed defaults point at the MQTT broker
   `192.168.1.50:1883`, and this PRD treats that as production. However, committed
   work exists on an unmerged feature branch (**not deployed** as of 2026-08-29) to
   migrate components to a broker on shari (`192.168.1.139`), bridged to the house
   broker. The console's first-launch setup form defaults to `192.168.1.50`. Decision:
   single broker or two-broker topology for the reconstruction, and when. Note:
   operators cannot edit the console's broker credentials in-app after first setup.
   A migration today therefore forces a tablet app-data reset.
3. **`device_online` wire form.** The model says the field is "omitted when true".
   Deployed bridges publish `device_online: true` explicitly. Either the contract
   mandates explicit `true` (simpler for consumers), or it mandates absence-means-true
   and consumers must treat both forms the same way (compatibility-friendlier).
   Unresolved. `02-interface-spec.md` states the consumer-side rule that is safe under
   both.
4. **Value-key exception policy.** The universal `/cmd` shape is
   `{"action":"<name>","value":"<string>"}` (arguments ALWAYS under `value`, as
   strings, booleans as `"true"`/`"false"`). Two documented exceptions exist in the
   field. The antenna controller's frequency command uses an integer `freq_hz` key,
   declared by its `expose` descriptor. Several console-driven payloads
   (`{"select":…}`, `{"request":…}`, rotator `{"action":"set_az","az":…}`, tuner's
   real-boolean `set_inline`) deviate. Open decision: reproduce the deployed byte
   shapes exactly (deployed bridges parse them as-is), or clean them up with a
   coordinated migration. The per-component documents (`03-components/`) and
   `04-console.md` §2.8 list every exact shape.
5. **pa-arm heartbeat fix shape.** The 10 s arm heartbeat is a hard requirement (§6).
   Whether the fix is a ≤5 s periodic state republish from the radio bridge, a
   dedicated liveness topic, or an arm-controller-side grace change stays open. The
   requirement is the outcome (the arm must not drop while the radio is healthy but
   idle), not the mechanism.
6. **UHF rotator self-check disable cadence.** The current code sends the head's
   self-check-disable command once per process start. The correct behavior is arguably
   once per *link* (re)connect. Changing it needs confirmation against the head's
   firmware behavior. Flagged as an improvement, not verified.
7. **Logging layer implementation.** Specified (events, QSO log role, spots plane) but
   unimplemented. Whether the reconstruction builds it in the first pass or defers it
   is a scoping decision. The team records that decision in
   `07-priorities-milestones.md`.
8. **Host liveness.** Whether `muehle/host/*` nodes get an implementing component (and
   what "load" means on a Pi) stays undecided. The model gives the shape only.
9. **K9AY receive-loop controller.** The model describes a `hf/rx-loop-ctrl` slot
   (receive-loop direction control, preamp dropped by a hardware transmit-detect
   line). No bridge exists in the repo, and no research file covers it. Fact unknown:
   whether hardware for it exists at the site. Out of scope until confirmed.
10. **Pelco-P addressing assumption.** The bench assumed that the UHF head uses the
    same address byte in both serial protocols. If it is actually zero-indexed under
    Pelco-P, the head silently ignores every P frame. This assumption has no bench
    confirmation. The rotator console does not use Pelco-P framing since 2026-08-29,
    so this question has no effect on the present component. A re-introduction of
    P-mode framing must first check the addressing on the bench.
11. **`m5stamp` PLC #2.** No firmware exists (§5). The reconstruction either writes it
    or drops the `pol-ctrl` slot. Unknown: whether the site has PLC #2 hardware
    physically installed.
12. **Legacy naming.** Two deployed bridge names violate the project's own naming
    convention (`flexbridge`, `ultrabridge`). The team defers renaming because it
    touches live service units. A reconstruction with no legacy can adopt the target
    names (`flex-radio-bridge`, `ultrabeam-ant-ctrl-bridge`) — free to decide,
    provided the *slot addresses* (the actual contract) stay unchanged.
13. **ATU engagement on 80 m.** The deployed `atu_bands` list puts the antenna tuner
    in line on the 80 m band (§3). The documentation also calls the fan-dipole
    naturally resonant on 80 m, which argues for a bypass there. Keep the deployed
    rule or change it to bypass — the site rationale is unknown at PRD-writing time.
    A reconstruction must decide this together with the `atu_bands` default it ships.

---

*Cross-references: the exact bus contract in `02-interface-spec.md`. The safety model in
full in `06-safety.md`. Per-component behavior in `03-components/`. The console in
`04-console.md`. Hosts and deployment in `05-deployment-ops.md`. Build order in
`07-priorities-milestones.md`. Raw research notes in `_research/`.*