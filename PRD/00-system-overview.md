# 00 — System overview: the Mühle amateur-radio station automation

**Purpose of this document.** This is the entry document of the PRD that reconstructs
"stationa", the automation system for the Mühle amateur-radio station. It is written to be
read STANDALONE by a competent software engineer who has never seen the station, never
used MQTT, and knows nothing about amateur radio. It defines what the system is, what
hardware exists, what an operator does with it, what "correct" means, what is in and out
of scope, and what is honestly broken or unresolved. Every specialized term is defined
at first use. Later documents carry the exact contracts (topic strings, JSON fields,
timings); this one orients the reader and links to them:

- `01-architecture.md` — the bus, the planes, the component model
- `02-interface-spec.md` — exact topic/JSON/payload/timing contracts
- `03-components/<slug>.md` — one per component plus a common-runtime note
- `04-console.md` — the operator tablet console
- `05-deployment-ops.md` — hosts, deployment, operations
- `06-safety.md` — the safety model in full
- `07-priorities-milestones.md` — build order and priorities

---

## 1. What the system is

**Amateur radio** (ham radio) is the licensed hobby of two-way radio communication:
operators hold government-issued licenses and callsigns and exchange contacts ("QSOs")
with other operators worldwide, by voice, Morse code (CW), or narrow-bandwidth digital
modes. The hobby involves radios that transmit on many frequency bands, antennas that
must be matched to the frequency in use, and equipment that can be damaged — or damage
the operator — if switched in the wrong order.

A **station** is a complete radio installation. The **Mühle** station (German for "mill";
site address prefix `muehle`, operator callsign DL9ET) is a two-station installation at a
rural property: an **HF** station (high-frequency, roughly 1.8–54 MHz — long-distance
shortwave) and a **UHF** station (144–440 MHz — local/line-of-sight). The HF station is
the automated one and the subject of this PRD.

**stationa** is the software that automates this station end-to-end. It consists of many
small independent services — called **bridges** — where each bridge fronts exactly one
piece of hardware (or one pure-logic role such as "antenna selection policy") and mirrors
it onto a shared message bus. The bus protocol is **MQTT** — a lightweight
publish/subscribe protocol in which clients publish messages addressed to hierarchical
text *topics* on a central server called a **broker**, and any client subscribed to a
matching topic filter receives them. A **retained** message is one the broker stores and
re-delivers immediately to every future subscriber, so a late-joining component instantly
sees the last known state without polling. **LWT (Last Will and Testament)** is an MQTT
feature: a client registers a farewell message at connect time, and the broker publishes
it automatically if the client disappears without disconnecting cleanly.

There is deliberately **no central server** in stationa. Components integrate by
subscribing to one another's state topics, never by calling one another. The live
configuration *is* the documentation: the component inventory can be read off the bus at
any moment, so it never drifts from reality the way a hand-maintained document does.

The bus is also deliberately **consumer-agnostic**. Any number of **consumers** —
separate processes that only subscribe, required by no core component — may attach to
the bus. The reference consumer is **Home Assistant**, an open-source home-automation
platform that renders device states as a dashboard. One logic component exists to serve
consumers of that kind: the **discovery renderer** (slot `muehle/hf/discovery`) reads each
component's self-describing `/meta` "expose" block — a machine-readable list of the
slot's observable and controllable fields — and translates it into Home Assistant's
discovery configuration, so new devices appear there with no per-device integration
code. Consumers are strictly optional: **deleting every consumer must leave the station
running identically** — core components know nothing about any consumer and never
publish under a consumer's topic tree. (One accepted, temporary deviation in the current
implementation: the radio and antenna-controller bridges carry a legacy embedded
Home-Assistant-discovery code path, switched off by default and slated for removal once
the discovery renderer is proven; with it off, the canonical bus is unaffected.)

The system is used in one sentence: an operator taps "START STATION" on a tablet, picks a
frequency band, and the automation powers the equipment in a safe order, routes the
correct antenna, pre-tunes the antenna and amplifier for that band, keeps the station
safe while transmitting, grounds the antennas when idle, and shuts everything down in
reverse order with one more tap.

**Reference-implementation note (non-normative).** The existing implementation is a set
of Go services plus two embedded C++/YAML firmwares plus one Flutter tablet app, deployed
on a Raspberry Pi named `shari` at `192.168.1.139`. The reconstruction team may use any
stack; only the bus behavior, safety behavior, and timings are normative, and they are
stated as such in `02-interface-spec.md` and `06-safety.md`.

---

## 2. What the station physically consists of

Every physical device in the automated station, each with a one-sentence plain-language
description. (Slot addresses are the MQTT topic namespace each device is mirrored onto;
see `01-architecture.md` §2.)

### 2.1 HF chain (the automated station)

| Device | What it is, in one sentence | Slot address(es) |
|---|---|---|
| **FLEX-8400 transceiver** | The radio itself — a software-defined radio (a radio whose filtering and demodulation are done in software) that both receives and transmits on all HF bands; called "TRX" (transceiver) throughout. | `muehle/hf/radio` |
| **ACOM 1200S power amplifier (PA)** | A device that boosts the radio's transmit signal from roughly 40 W up to as much as 1200 W; it can be damaged if it transmits into a badly matched antenna. | `muehle/hf/pa` |
| **ATR-1000 antenna tuner (ATU)** | An impedance-matching network of inductors and capacitors that can be switched *in line* with the feed cable to make a non-resonant antenna look like a proper electrical load on bands where the antenna is not naturally matched; "SWR" (standing-wave ratio) is the mismatch measure — 1.0 is perfect, ≥3.0 is dangerous because reflected power heats the amplifier. | `muehle/hf/tuner` |
| **Ultrabeam RCU-06 antenna controller** | The controller for a motorized beam antenna: the beam's aluminum element tubes physically lengthen and shorten on command to tune the antenna to the operating frequency, and the beam can point forward, backwards (180°), or both directions (bi-directional), and retract fully in storms. | `muehle/hf/ant-ctrl` |
| **1:6 antenna switch** | A relay board that connects the station's single feed line to exactly one of six antenna ports (or none) — switching must never happen while radio-frequency power is flowing, or the relay contacts arc. | `muehle/hf/ant-switch` |
| **Yaesu G-450DC rotator** | The motor that turns the HF antenna mast to point the beam at different compass **azimuths** (bearings, in degrees). Driven through an AF6SA "WRC" controller over websocket. | `muehle/hf/rotator` |
| **M5 Stamp PLC #1 (relay controller)** | A small programmable-logic controller (a tiny embedded computer with relays) with a fixed relay mapping: **relay 3 closes the PA's "remote power-on" trigger line, relay 4 closes the radio's "remote power-on" trigger line**, and **relay 1 is the PA arm relay** — the software-controlled permit that must be energized (together with hardware conditions) before the amplifier may transmit. Relay 2 is spare. | `muehle/hf/switch` and `muehle/hf/pa-arm` |
| **Shelly smart plug (master mains)** | A wifi-controllable mains outlet switching the station's master supply. | `muehle/power/master` |
| **Shelly smart plug (13.8 V PSU)** | A wifi-controllable outlet switching the 13.8 V DC power supply that feeds the station's control equipment (antenna switch, relay controllers, etc.). | `muehle/power/psu-13v8` |

### 2.2 Antennas (passive — no software presence)

Antennas themselves are deliberately **not** bus components. They are passive resources
named only in configuration:

- `ant/ultrabeam` — the motorized beam described above (wired to a switch port; see the
  open port-number decision in §7).
- `ant/fan-dipole` — a multi-band wire antenna (two arms fanning out from the feed
  point), naturally resonant on the 80 m and 40 m bands. (Band labels like "80 m" are
  wavelength shorthand for named frequency allocations — the 80 m band spans
  3.5–4.0 MHz, roughly an 80 m wavelength; the exact allocation table lives in
  `02-interface-spec.md`.)
- `ant/dummy-load` — a heat-dissipating test load: radiates nothing, used for testing
  and as a safe place to park transmit power.
- `ant/k9ay-loop` — a receive-only directional wire loop (a separate controller slot
  `hf/rx-loop-ctrl` is modeled but unimplemented — see §7 item 9).
- The antenna switch's `off` position **shorts the open ports to ground** — this is the
  station's lightning/idle protection, and "grounded" is the safe idle state.

### 2.3 UHF station (partially automated)

| Device | What it is | Slot address |
|---|---|---|
| **PTS-303Z/3050DZ pan/tilt head** | The UHF antenna rotator — a pan/tilt motor head (as used for CCTV cameras) steered over an RS-485 serial line (RS-485: a multi-drop serial electrical standard; here connected to the host via a USB adapter) using the Pelco-D/Pelco-P protocols; driven by an interactive terminal application on the shack PC, deliberately not a headless service. | `muehle/uhf/rotator` |
| **IC-9700 radio, X-Quad antennas, masthead preamps** | The UHF radio and antennas — **not automated** and **not planned** (see §5); the polarization relay controller for the X-Quad antennas is a documented gap. | `muehle/uhf/pol-ctrl` (unimplemented) |

The UHF rotator console exposes exactly one *remote* motion interface: a
**rotctld-compatible TCP server** — rotctld is the text protocol of Hamlib (an
open-source radio/rotator control library), spoken by tracking and control clients such
as gpredict (a satellite-tracking program). **Arming** is the safety gate for precisely
this path: only after the operator has armed the head from the interactive console may
remote rotctld clients move it, and arming is never possible remotely. The MQTT surface
exposes only a `stop` command and can never move the head at all.

### 2.4 Compute and infrastructure

| Thing | What it is | Notes |
|---|---|---|
| **shari** | A Raspberry Pi (single-board Linux computer) at `192.168.1.139` — the host running all unattended services, each as a hardened systemd service (a Linux service-manager unit with restricted privileges). | See `05-deployment-ops.md` |
| **shack-pc** | The shack's Windows PC — hosts only the interactive UHF rotator console (a human starts it by hand; arming the rotator for remote control is a deliberate manual keyboard act). | |
| **MQTT broker** | The message-relay server the whole system publishes through. The normative requirement is **any MQTT broker with a persistent store** (retained messages survive broker restarts). Deployed reference implementation: Mosquitto at `tcp://192.168.1.50:1883`. | Broker topology is an open decision, §7 |
| **Android tablet** | A fixed-mount tablet on the operating desk running the operator console app — the only human interface to the HF chain. | See `04-console.md` |
| **horstreporter** | The owner's own external web service that aggregates **DX spots** (reports that a distant — "DX" — station was heard, with location and signal strength) and serves them over HTTPS Server-Sent Events; the console plots them on its maps. | External dependency, read-only |

---

## 3. What an operator does with the system

A day-in-the-life narrative. The exact UI detail is specified in `04-console.md`; the
sequences and timings behind it in `02-interface-spec.md` §5 and `06-safety.md`.

**Powering up.** The station is cold: mains off, antennas grounded. The operator walks to
the tablet and taps **START STATION**. A logic component called the **sequencer**
(`powerseq`, slot `muehle/hf/power-seq`) executes an ordered startup with real
confirmations at each step, and the automation SHALL follow this order exactly:

1. Master mains plug on.
2. Wait 30 s for network equipment (the wifi smart plugs and the broker) to boot —
   the shipped default network delay (`network_delay_s`), configurable per site.
3. 13.8 V PSU plug on.
4. Wait until the controllers that boot from that supply (antenna switch, both relay
   channels, PA arm) are confirmed alive on the bus.
5. Radio remote-on relay on; wait until the radio bridge is confirmed alive.
6. PA remote-on relay on; wait until the PA's own telemetry confirms it is powered.
7. PA arm permit enabled (the arm permit only translates into "armed" when the hardware
   conditions — radio alive and online, not tuning, antenna connected, band safe — are
   also true; the arm relay is computed by the relay controller's firmware and is never
   commanded directly).

Each wait step has a deadline (default 120 s); a timeout stops the sequence, records a
fault naming the failed step, and does **not** roll back already-completed steps. The
operator watches the sequence phase (`starting` → `running`) on the console.

**Picking a band.** The operator taps a band button (e.g. 20 m). Two things happen
automatically, driven by the **antenna reconciler** (`antennaselect`, slot
`muehle/hf/antenna-select`) — the policy component that is the single writer of antenna
routing intent:

- Band policy resolves the band to an antenna resource (20 m → the Ultrabeam beam;
  80/40 m → the fan-dipole; anything else → the fan-dipole via the tuner) and routes the
  antenna switch there — **without hot-switching**: a port change SHALL be withheld
  while any transmit power is detected, and RF is inhibited and receive confirmed before
  the relay moves.
- **Band-follow**: because the selected resource is the Ultrabeam, the reconciler also
  forwards the operating frequency to the antenna controller so the beam's elements
  motor-tune to resonance; and **PA-follow** pre-positions the amplifier's band setting
  so it does not trip on the first transmission on a new band; and **tuner-follow**
  engages or bypasses the antenna tuner per band. The deployed rule: when the fan-dipole
  is selected, the ATU is switched in line on the **30 m, 60 m, 80 m, and 160 m**
  bands (the configurable `atu_bands` list) and bypassed otherwise. Note the tension:
  the fan-dipole is described as naturally resonant on 80 m, yet the deployed
  configuration engages the ATU there too — recorded as an open decision in §7
  item 13.

The operator can override any of this: manual antenna selection, forcing the antenna to
the dummy load, or switching the reconciler to manual mode. An operator hold is itself
overridden by the idle rule (below) — deliberately, so a forgotten hold cannot defeat
walk-away safety.

**Transmitting.** The operator speaks or **keys** — "keying" is ham shorthand for
starting a transmission (from the Morse-code key); the moment a transmission begins is
called **key-up**. Transmit permitting is layered: the fast
hardware key line (a physical wire that inhibits transmit within the radio) runs in
series through the interlock chain, and the software arm permit is ANDed into it
electrically. Software never sits in the per-transmission path — it only arms or
disarms. On the console the operator watches forward power, SWR, PA temperature, tuner
state, and any faults; every control is disabled while its owning component is not
confirmed alive.

**Watching DX spots.** The console's map shows FT8/FT4 digital-mode spots from
horstreporter as a live overlay on an azimuthal (true-bearing-and-distance) compass and
a world map, so the operator can see where activity is and turn the beam toward it
(tap-to-aim) or pick a more promising band. (FT8 and FT4 are specific narrow-bandwidth
digital modes in which stations exchange short, structured, computer-encoded messages on
fixed timed slots; contacts made with them are reported to spot aggregators as automated
spots, which is where horstreporter gets most of its data.)

**Walking away.** If the operator stops touching the radio for 30 minutes (no dial
change, no transmit), the reconciler infers the station inactive and routes the antenna
switch to `off` — grounding the antennas (lightning protection). This idle rule outranks
operator holds by design.

**Shutting down.** The operator taps **STOP STATION** and the sequencer runs the exact
reverse order with a 2-second stagger between steps to limit electrical inrush: disarm
PA → PA off → radio off → PSU off → mains off. One button, ordered, confirmed.

---

## 4. System qualities — what "correct" means

The reconstruction SHALL satisfy these; the full normative statements live in
`02-interface-spec.md` §8 and `06-safety.md`.

1. **Never hot-switch antennas.** The antenna switch declares `hot_switch: false`; a
   port change SHALL happen only with RF inhibited and receive confirmed first. The
   reconciler withholds changes during transmit; the console refuses direct-drive port
   changes unless it can positively confirm the radio is receiving (unknown state
   blocks — fail closed).
2. **Never transmit the amplifier into a bad load.** The PA arm permit SHALL require:
   an explicit enable command, the radio bridge alive and its state fresh (heartbeat
   within 10 s), the radio not tuning, the antenna on a valid port (not grounded), and a
   safe band. The arm relay is fail-safe **open** — any loss of power, software, or
   heartbeat de-energizes it and removes transmit permission.
3. **Antenna grounded when idle.** After 30 minutes without dial or transmit activity
   the system SHALL ground the antennas, and this idle rule SHALL outrank operator holds
   (walk-away safety).
4. **One command to start and stop.** A single non-retained `/cmd` message (`start` /
   `stop` on the sequencer slot) drives the entire ordered, confirmation-based power
   sequence; no operator multi-step power-up is ever required.
5. **Safety lives on hardware, software only mirrors it.** The transmit-inhibit
   interlock is a physical line in series through the equipment; the messaging plane
   SHALL never be in the enforcement path. Loss of any or all software degrades the
   station to inaction (manual operation), never to hazard.
6. **Two-layer liveness, and both must be checked.** A slot is "alive" only if BOTH
   (a) its `/status` topic — the bridge process liveness, plain string `online`/`offline`
   maintained via MQTT last-will — says `online`, AND (b) its `/state` document's
   `device_online` boolean — the hardware behind the bridge — is true. Consumers acting
   on retained state SHALL check both first; keying on `/status` alone caused real
   failures (§6). Known actual behavior: on a *clean* process shutdown no last-will
   fires, so a retained `/status` of `online` can persist for a stopped service —
   consumers must not trust `/status` alone for anything safety-relevant.
7. **Observable state.** Every component publishes its full state as one retained JSON
   snapshot on change, with a UTC timestamp; the bus is the single source of truth, and
   a late joiner reconstructs the whole station picture from retained messages alone.
   Stale or missing data must present as loud failure, not as plausible health (the
   console's link-down banner, offline rows with when-it-went-dark timestamps, and
   "unknown ≠ off" presentation rules exist for exactly this).
8. **Fail-safe defaults everywhere.** Power plugs power up off; the arm relay
   de-energizes to the safe state; the switch's `off` position grounds the antennas.
9. **Commands are fire-and-observe.** A command publisher never assumes success; every
   consumer reacts to observed state, never to intent. Commands are normally not
   retained; the only retained commands are idempotent actuator setpoints that a
   component may safely replay on restart (power, relay positions, arm enable,
   antenna selection), and one-shot commands that are retained SHALL be cleared
   immediately after execution.
10. **One arbiter per contested actuator.** Antenna routing has exactly one policy
    writer (the reconciler) resolving an explicit priority ladder — idle > operator >
    auto — and it publishes *why* the actuator is where it is (`source: idle |
    operator | auto`), so the bus explains itself.

---

## 5. Scope: what this PRD specifies vs. what is out

**In scope (specified by this PRD):**

- The HF chain automation: radio, PA, tuner, antenna switch, antenna controller,
  rotator, relay controller (power-on channels + PA arm), smart plugs, sequencer,
  antenna reconciler, discovery renderer — and the operator console.
- The UHF rotator console (Pelco pan/tilt head) with its deliberate manual-arming
  safety model.
- The bus contract itself (topics, payloads, liveness, timings) and the deployment/
  operations model.

**In scope as specification, but specified-not-implemented in the current system:**

- **The logging layer.** A documented extension to the bus — a `qso-log` role keeping
  the station's contact logbook and publishing per-QSO and DX-spot *events*
  (non-retained, one JSON object per contact) — exists as a written model
  (`docs/logging-integration-model.md`) with **no implementing component today**. The
  reconstruction treats it as future work; see `07-priorities-milestones.md`. (ADIF —
  the standard amateur-radio logbook interchange format — is the event vocabulary.)
- **Host liveness nodes** (`muehle/host/shari`, `muehle/host/shack-pc` — publishing
  online/temperature/load for the compute hosts) appear in the system model but no
  current component publishes them. Model-only; host liveness matters because shari is
  a single point (§6) — a reconstruction SHOULD implement them.

**Out of scope / known gaps, honestly:**

- **PLC #2 polarization firmware does not exist.** The slot table attributes
  `muehle/uhf/pol-ctrl` (X-Quad antenna polarization relays) to the M5 Stamp project,
  but no firmware for that second controller is in the repository. This is a documented
  gap — a component to be built, not a component to be reproduced.
- **The UHF radio (IC-9700) and UHF receive chain are unplanned.** No bridge, no slot,
  no automation; only the UHF rotator is automated.
- **HVAC/climate control** is not wired anywhere; the console's climate panel is a
  static placeholder.
- **Auxiliary tooling** (a passive bus traffic recorder, a browser-based bus
  inspection/stimulation tool, a bench tool that reverse-engineered the UHF rotator's
  serial protocol, and design mockups for the console) are specified in
  `03-components/aux-projects.md` as tools, not station components. The bench tool's
  *findings* — the pan/tilt head ignores absolute-position commands, its tilt readback
  is meaningless, and it emits checksum-valid garbage while moving — are load-bearing
  contract for any UHF rotator implementation.
- High-rate data streams (audio, spectrum, signal-strength samples) are explicitly
  **not** on the bus, by design.

---

## 6. Documented gaps and known incidents

An honest summary of what is known to be fragile or wrong in the current system. A
reconstruction must not blindly copy these; where the research notes state the intended
fix, it is included. Detail lives in the per-component documents.

**Live incidents that shaped hard requirements:**

- **PA-arm heartbeat starvation (live).** The PA arm relay de-energizes if the radio's
  state is not refreshed within 10 s — but the radio bridge publishes state *only on
  change*, so a healthy but idle radio starves the heartbeat and silently drops the
  arm. **Hard requirement derived from this incident:** the radio state path SHALL
  include a periodic republish/heartbeat (at most every 5 s) or an equivalent
  liveness mechanism. (Project memory: "antenna grounding recovery gaps".)
- **Blocking MQTT handlers deadlock the client (live, twice).** In the reference MQTT
  library, incoming-message handlers run on the connection's dispatch thread; a handler
  that publishes synchronously deadlocks the whole client (hit the discovery component
  live) and a blocking connect ignores shutdown signals (hung a bridge at shutdown
  until the service manager force-killed it). **Library-independent constraint:**
  handler work SHALL be isolated from the receive path (queue + single worker; drop on
  overflow rather than block), and shutdown SHALL complete promptly even during a broker
  outage.
- **Consumer keyed on bridge liveness alone (live).** The reconciler originally watched
  only `/status`; when a device link died while its bridge stayed up, it chattered on
  stale retained state. Hence the two-layer AND rule (§4.6) as a hard requirement.
- **Command-payload convention violated (live).** A bridge parsed its command argument
  from a key named after the action instead of the universal `value` key and the
  command was silently wrong on the air. Hence the exact `/cmd` payload contract
  (`02-interface-spec.md` §4).
- **First key-up after grounding can hit the short.** Recovery from auto-grounding is
  fragile: re-activation can race, and a transmit can begin before the antenna switch
  has settled on the new port. The cold-switch ordering contract (§4.1) and the
  switch's `settled` state exist to close this.
- **Serial self-heal (live-proven).** USB-serial adapters drop and re-enumerate under
  a new device file; the antenna-controller bridge must reopen by stable device path
  on I/O error rather than dying — a reconstruction on the same hardware needs the
  same self-heal.

**Structural fragilities (accepted, documented):**

- **The reconciler is a coordination single point.** If it dies, all soft automation
  (antenna routing, band-follow, tuner-follow, PA-follow) stops together; the station
  degrades to manual — acceptable only because safety is hardware. A reconstruction
  should add supervision/restart and an explicit "reconciler offline" indication.
- **shari is a single point.** It fronts most bridges and hosts all logic slots; its
  loss takes the automation cluster offline simultaneously (correctly, via last-wills,
  but completely). Host liveness is load-bearing yet currently unimplemented (§5).
- **Retained state can be stale.** Retained messages survive crashes; every consumer
  must check liveness before acting on them, and nothing safety-relevant may trust
  retained state. Relatedly: after a *clean* shutdown, retained `/status` stays
  `online` because the broker never fires the last-will (§4.6).
- **`device_online` convention drift.** The model says the field is "omitted when
  true"; the deployed bridges publish `device_online: true` explicitly. Consumers must
  treat both forms; see §7.
- **160 m has no resonant antenna.** It routes to the fan-dipole through the tuner at
  high SWR — a known operating condition, not a fault.
- **UHF rotator device quirks** (measured, contract): absolute-position commands are
  ignored (open-loop jog/stop only), tilt readback is meaningless, readback is garbage
  while the motor runs, and the head re-homes itself unless a self-check-disable
  command is sent at start — and in the current code that disable is sent once per
  process start, not once per reconnect (a known defect; a reconstruction should send
  it after every successful link reopen).
- **UHF rotator auto-heal gives up.** After a serial failure, if the automatic reopen
  fails once, healing stops permanently until a human intervenes — known defect.
- **Console-side known issues** (fully enumerated in `04-console.md` §9): the web
  channel stores the MQTT password in plain browser storage; broker credentials cannot
  be changed in-app once set; the START button is enabled while the shutdown sequence
  is still in flight; the climate panel is hard-coded mock data.
- **Tooling hygiene**: a real-looking MQTT password is committed in a checked-in bench
  config of the bus-inspection tool — a leaked credential to be rotated and not
  reproduced; the bus-inspection tool's publish endpoint has no authentication on the
  LAN.
- **Plaintext secrets in 0600 config files** are the accepted trade-off in the current
  system (file permissions instead of a secrets manager); the stronger option is noted
  in `05-deployment-ops.md`.

---

## 7. Open decisions & unresolved facts

Each item below has more than one defensible answer or an unknown fact. The evidence is
given for every variant; the reconstruction team must resolve them (or, where marked,
confirm on the physical device) rather than silently picking one.

1. **Which antenna-switch port carries the Ultrabeam — 3 or 4?** The integration
   documentation and repo-level notes say the Ultrabeam is on switch **port 3**; the
   reconciler's example config, its deploy seed script, and the console's antenna map
   say **port 4**. (Consistent everywhere: port 6 = fan-dipole, port 1 = dummy load;
   the docs' own passive-resource list even mentions fan-dipole on port 2 in one
   place.) The live config file on the Raspberry Pi is authoritative but was **not
   readable from the workstation when this PRD was written**. Decision required on the
   device. Port numbers are per-site configuration in any case; a reconstruction must
   never hard-code them, and every wiring-map document must present both variants until
   confirmed. The console's UI port map (off, port1 = dummy load, port4 = Ultrabeam,
   port5, port6 = fan-dipole) reflects the port-4 variant and omits unwired ports.
2. **Broker topology.** All deployed defaults point at the MQTT broker
   `192.168.1.50:1883`; this PRD treats that as production. However, work exists
   (committed, on an unmerged feature branch, **not deployed** as of 2026-08-29) to
   migrate components to a broker on shari (`192.168.1.139`), bridged to the house
   broker. The console's first-launch setup form defaults to `192.168.1.50`. Decision:
   single broker vs. two-broker topology for the reconstruction, and when. Note that
   the console's broker credentials are not editable in-app after first setup, so a
   migration today forces a tablet app-data reset.
3. **`device_online` wire form.** The model says the field is "omitted when true";
   deployed bridges publish `device_online: true` explicitly. Either the contract
   should mandate explicit `true` (simpler for consumers) or mandate
   absence-means-true with consumers required to treat both forms as equivalent
   (compatibility-friendlier). Unresolved; `02-interface-spec.md` states the
   consumer-side rule that is safe under both.
4. **Value-key exception policy.** The universal `/cmd` shape is
   `{"action":"<name>","value":"<string>"}` (arguments ALWAYS under `value`, as
   strings, booleans as `"true"`/`"false"`). Two documented exceptions exist in the
   field: the antenna controller's frequency command uses an integer `freq_hz` key
   (declared by its `expose` descriptor), and several console-driven payloads
   (`{"select":…}`, `{"request":…}`, rotator `{"action":"set_az","az":…}`, tuner's
   real-boolean `set_inline`) deviate. Open decision: reproduce the deployed byte
   shapes exactly (deployed bridges parse them as-is) or clean them up with a
   coordinated migration. The per-component documents (`03-components/`) and
   `04-console.md` §4 list every exact shape.
5. **pa-arm heartbeat fix shape.** The 10 s arm heartbeat is a hard requirement (§6);
   whether the fix is a ≤5 s periodic state republish from the radio bridge, or a
   dedicated liveness topic, or an arm-controller-side grace change is open — the
   requirement is the outcome (arm SHALL NOT drop while the radio is healthy but
   idle), not the mechanism.
6. **UHF rotator self-check disable cadence.** The current code sends the head's
   self-check-disable command once per process start; the correct behavior is arguably
   once per *link* (re)connect. Changing it requires confirmation against the head's
   firmware behavior; flagged as an improvement, not verified.
7. **Logging layer implementation.** Specified (events, QSO log role, spots plane) but
   unimplemented; whether the reconstruction builds it in the first pass or defers it
   is a scoping decision recorded in `07-priorities-milestones.md`.
8. **Host liveness.** Whether `muehle/host/*` nodes get an implementing component (and
   what "load" means on a Pi) is undecided; the model specifies the shape only.
9. **K9AY receive-loop controller.** The model describes a `hf/rx-loop-ctrl` slot
   (receive-loop direction control, preamp dropped by a hardware transmit-detect
   line), but no bridge exists in the repo and no research file covers it. Fact
   unknown: whether hardware for it exists at the site. Out of scope until confirmed.
10. **Pelco-P addressing assumption.** The UHF head is assumed to use the same address
    byte in both serial protocols; if it is actually zero-indexed under Pelco-P, every
    P frame is silently ignored. Bench-unverified; irrelevant while Pelco-D (the
    default) is used, but any P-mode use must re-verify.
11. **`m5stamp` PLC #2.** No firmware exists (§5); the reconstruction either writes it
    or drops the `pol-ctrl` slot. Unknown: whether PLC #2 hardware is physically
    installed at the site.
12. **Legacy naming.** Two deployed bridge names violate the project's own naming
    convention (`flexbridge`, `ultrabridge`); renaming is deferred because it touches
    live service units. A reconstruction with no legacy can adopt the target names
    (`flex-radio-bridge`, `ultrabeam-ant-ctrl-bridge`) — free to decide, provided the
    *slot addresses* (the actual contract) stay unchanged.

---

*Cross-references: the exact bus contract in `02-interface-spec.md`; the safety model in
full in `06-safety.md`; per-component behavior in `03-components/`; the console in
`04-console.md`; hosts and deployment in `05-deployment-ops.md`; build order in
`07-priorities-milestones.md`; raw research notes in `_research/`.*