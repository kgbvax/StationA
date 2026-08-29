# 03 — Component spec: 1:6 coaxial antenna switch (slot `muehle/hf/ant-switch`)

This document specifies the station's coaxial antenna switch: an embedded-firmware component
that connects exactly one of six physical antenna ports (or none) to the single coaxial feed
line ("coax" — the shielded cable carrying radio-frequency signals) that runs to the
transceiver. The reader needs no amateur-radio background: **amateur radio** is the licensed
hobby of two-way radio communication; a **transceiver** ("TRX") is the radio device used to
transmit and receive; **RF** means radio-frequency energy; **TX** means transmitting, **RX**
means receiving. The switch is a deliberately *dumb actuator*: it selects a port when told to,
reports what is selected, and holds **zero policy** about which antenna is appropriate for the
current frequency — that decision belongs to the antenna-selection reconciler (a separate
component, slot `muehle/hf/antenna-select`; see `03-components/antennaselect.md`). This
document is the normative behavior contract for any re-implementation, independent of the
reference technology (which is ESPHome firmware on an ESP32-S3 relay board, described
non-normatively in §13). Terms from the bus architecture (**MQTT**, **broker**, **retained
message**, **slot**, **plane**, **LWT**) are defined in `02-interface-spec.md` §1–§3 and are
used here per that document; brief reminders are given at first use anyway.

---

## 1. Purpose and role in the station

The station has three antennas but one coaxial feed line to the radio:

- **Ultrabeam** — a motorized beam (directional) antenna, tuned by a separate controller
  (slot `muehle/hf/ant-ctrl`, see `03-components/ultrabridge.md`).
- **80/40 m fan dipole** — a passive wire antenna resonant on the 80 m and 40 m bands.
- **Dummy load** — a heat-dissipating resistor used as a safe, non-radiating test load
  (transmitting into it radiates nothing, so it is used for testing).

A **coaxial antenna switch** is an electromechanical relay matrix that connects exactly one
of its six physical ports — each wired to one antenna (or the dummy load) — to the feed line
at a time. The component specified here is the firmware that runs on the switching hardware
itself: there is no separate daemon process and no upstream device behind a serial or network
link — the firmware *is* the bridge and the device in one unit.

Responsibilities of the component:

1. Accept one command over the station MQTT bus: select a port (one of six, or "off").
2. Enforce **exclusive selection**: at most one port relay energized at any instant.
3. Report, as retained MQTT state, (a) which port is selected — derived by reading the relay
   state back, never by echoing the command — and (b) whether the relay move has **settled**
   (finished moving, conservatively timed; see §6.3).
4. Come up in the all-off position after any power loss and self-heal to the last commanded
   port by re-executing the retained command on reconnect (§6.1, §6.2).

Explicitly **not** its responsibilities:

- **No antenna knowledge.** The firmware knows nothing about which antenna is wired to which
  port. The port↔antenna mapping lives entirely in the reconciler's configuration
  (§9.1). A re-implementation SHALL NOT hardcode any port↔antenna assignment.
- **No transmit (TX) gating.** The switch has no RF sensing and never refuses a command
  because the station is transmitting. Sequencing port changes to occur only while receiving
  ("**cold switching**" — changing the relay matrix while no transmit RF is flowing, to avoid
  arcing the relay contacts) is the **commander's** responsibility. The switch declares this
  in its capability block via `hot_switch: false` (§4.2) and executes any valid command
  immediately. See `06-safety.md` for the station-wide cold-switch sequencing rules.

The station's power sequencer (`powerseq`, slot `muehle/hf/power-seq`,
see `03-components/powerseq.md`) depends on this slot's liveness during station start-up: the
switch is powered from the station's 13.8 V DC supply rail, and the sequencer waits for this
slot's `/status` to go `online` after power-on before proceeding. The exact boot topic order
below is therefore load-bearing beyond this component.

---

## 2. Hardware interface (what the firmware drives)

This section is descriptive of the deployed hardware and is normative only where marked: a
re-implementation on different hardware must preserve the *behavior* (§4–§8), while the
board, pin numbers, and expander chip may change (§13).

**Board**: WaveShare ESP32-S3-POE-ETH-8DI-8DO — an ESP32-S3 microcontroller module
(16 MB flash, 8 MB PSRAM) on a carrier board with 8 relay outputs (relay = an electrically
operated electromechanical switch; the coil is driven by a logic pin, the contacts switch the
coax line), 8 opto-isolated digital inputs, an RS-485 UART port, a hardware real-time clock
chip, one RGB LED, and a buzzer. Network transport is Wi-Fi.

**Relay drive — I/O mapping (normative for a re-implementation of this hardware, or wherever
a port-multiplexing expander is used)**: the 8 relay coils are not driven by the
microcontroller's own pins but through a **PCA9554** I²C GPIO expander (a small chip that
adds remotely-readable/writable I/O pins over a two-wire bus) at address `0x20`
(SCL = GPIO41, SDA = GPIO42). Expander pins 0 and 1 drive board relays 1 and 2, which are
**not part of the antenna switch function** (they exist as spare toggles). The six switch
positions use expander pins 2–7 → board relays 3–8 → logical ports 1–6:

| Logical port | MQTT value string | Board relay | PCA9554 expander pin |
|---|---|---|---|
| none (feed line unconnected) | `off` | — (all relays off) | — |
| port 1 | `port1` | Relay 3 | 2 |
| port 2 | `port2` | Relay 4 | 3 |
| port 3 | `port3` | Relay 5 | 4 |
| port 4 | `port4` | Relay 6 | 5 |
| port 5 | `port5` | Relay 7 | 6 |
| port 6 | `port6` | Relay 8 | 7 |
| (spare, not part of the switch) | — | Relay 1 | 0 |
| (spare, not part of the switch) | — | Relay 2 | 1 |

The command value→relay mapping is fixed: `"off"` → no relay; `"port1"`→relay 3 …
`"port6"`→relay 8 (§5).

**No contact feedback (normative)**: the expander drives the relay *coils* and there is no
input wired back from the relay *contacts*. The firmware therefore cannot verify that a
relay's contacts actually closed or remained closed. Consequences that the behavior contract
must state honestly (§6.4, §8): a jammed or failed relay is invisible on the bus; `selected`
reports the driven coil state (a best-effort readback), not verified contact closure, and
`settled` becomes `true` on the 200 ms timer regardless of what the contacts physically did.
A re-implementation SHALL treat `selected` as *commanded-coil readback*, and SHALL NOT
represent it as verified contact state. If new hardware provides contact feedback, that is an
improvement the consumers can benefit from, but the wire format stays the same.

**Other on-board hardware** (configured in the reference firmware but unused by the switch
function): spare relays 1–2 as plain toggles; buzzer on GPIO48; WS2812 RGB LED on GPIO38;
8 digital inputs with pull-ups on GPIOs 47, 40, 39, 5, 6, 7, 1, 2; RS-485 UART
(TX GPIO17 / RX GPIO18, 9600 baud, no protocol attached); PCF85063 hardware RTC on the same
I²C bus. These are free to drop or keep in a re-implementation.

---

## 3. MQTT presence — topics

Connects to the station MQTT broker (a message server that fans published messages out to
subscribers) at `192.168.1.50:1883` (plain TCP; user `hf`; see §11 for the planned broker
migration), MQTT client id `ant-switch` (a fixed string, not suffixed by MAC address — the
single-device assumption this encodes is listed as a constraint in §12.2), with a
**persistent session** (`clean_session: false`), so the broker keeps the subscription and
queued messages across reconnects.

All per-entity automatic topics of the reference framework are suppressed; the bus carries
**exactly four topics**, all under the slot address `muehle/hf/ant-switch`. A "slot" is one
component's address on the bus; the four "planes" (`meta`, `state`, `status`, `cmd`) are
defined in `02-interface-spec.md` §2.

| Topic (exact) | Direction | Retained | QoS | Payload |
|---|---|---|---|---|
| `muehle/hf/ant-switch/meta` | switch → bus | yes | 1 | JSON identity/capability block (§4.2) |
| `muehle/hf/ant-switch/state` | switch → bus | yes | 1 | JSON state snapshot (§4.3) |
| `muehle/hf/ant-switch/status` | switch → bus | yes | 1 | plain string `online` / `offline` (§4.1) |
| `muehle/hf/ant-switch/cmd` | bus → switch (subscribed, QoS 1) | retained **by the commander**, not by the switch | 1 | JSON `{"select":"<value>"}` (§5) |

### 3.1 `/status` — liveness

Liveness uses MQTT's **Last Will and Testament** (LWT): a message the client registers with
the broker at connect time, which the broker publishes automatically if the client disappears
without a clean disconnect (power loss, crash, Wi-Fi drop).

- On every MQTT (re)connect the switch publishes: topic `muehle/hf/ant-switch/status`,
  payload `online`, QoS 1, retained ("birth message").
- LWT registered at connect: same topic, payload `offline`, QoS 1, retained.
- On an orderly shutdown the firmware itself publishes `offline` (retained, QoS 1) before
  disconnecting.

There is no periodic heartbeat topic; state changes (§4.3) are the only other traffic.
**This slot has no `device_online` field** (the second liveness layer most other slots carry
in `/state` for the health of a *device behind* the bridge): the firmware and the device are
the same unit, so `/status` alone is the liveness signal for this slot. Consumers apply the
station's two-layer liveness rule (`02-interface-spec.md` §5) by treating this slot as
single-layer: `/status` = `online` is the only availability check available.

Station-wide caveat (from `02-interface-spec.md` §5, repeated because it applies here): on a
*clean* process shutdown with no explicit offline publish, the broker does not fire the LWT
and a retained `/status` of `online` can persist for a stopped component. The reference
firmware does publish `offline` on orderly shutdown, so the stale-online risk here is limited
to firmware that halts without reaching its shutdown path (e.g. a hard fault); consumers
SHALL NOT treat a retained `online` alone as proof of current availability.

### 3.2 `/meta` — identity and capability block

Published once per connect cycle, retained, QoS 1, after a deliberate **100 ms delay from
connect** (a connect-window jitter guard; the value is a default, not protocol — a
re-implementation may choose a different guard as long as `/meta` is published promptly after
connect and before any consumer needs it). Exact payload (field names and values normative;
the `device.model` free-text string may differ for different hardware):

```json
{"schema":"1.0","role":"ant-switch","device":{"model":"WaveShare ESP32-S3-POE-ETH-8DI-8DO (1:6 relay switch)"},"link":"wifi","location":"bauwagen","capabilities":{"ports":[1,2,3,4,5,6],"off":true,"exclusive":true,"hot_switch":false},"expose":{"device":{"name":"Antenna switch","model":"WaveShare ESP32-S3-POE-ETH-8DI-8DO (1:6 relay switch)"},"fields":[{"key":"selected","name":"Selected port","type":"enum","options":["off","port1","port2","port3","port4","port5","port6"],"writable":true,"command":{"value_key":"select","value_type":"string"}},{"key":"settled","name":"Settled","type":"boolean"}]}}
```

Semantics a re-implementation must reproduce:

- `schema`: station bus schema version tag, `"1.0"`.
- `role`: the canonical station-bus role name, `"ant-switch"`.
- `device.model`: free-text hardware description (non-normative content).
- `link`: `"wifi"` — network transport of this slot.
- `location`: `"bauwagen"` — German for the trailer/wagon building that houses the station.
- `capabilities.ports`: array of integers `[1,2,3,4,5,6]` — the selectable port numbers.
- `capabilities.off: true` — an explicit no-antenna position exists (all relays open).
- `capabilities.exclusive: true` — exactly one port may be connected at any time; enforced
  by construction (§6.2).
- `capabilities.hot_switch: false` — **load-bearing**: declares that transmit RF must NOT be
  flowing while the port changes; the commander must sequence changes into receive-only
  periods. The switch itself will not enforce this (§1).
- `expose`: the consumer-neutral field surface (used by the station's Home Assistant
  discovery renderer, slot `muehle/hf/discovery`) with two fields:
  - `selected` — writable enum with option strings `off`, `port1` … `port6`; its
    `command.value_key` is `"select"`, meaning a command payload puts the chosen value under
    the JSON key `"select"` as a string (§5). The enum option *strings* are inlined here
    (rather than derived from `capabilities.ports` integers) because the shapes differ.
  - `settled` — read-only boolean.

Note on the station-wide command-payload convention (`02-interface-spec.md` §4): commands
normally look like `{"action":"<name>","value":"<string>"}`. This slot uses the
**value-key-only** form documented there: the expose descriptor names `value_key:"select"`
and carries no `action`, so the payload is `{"select":"<value>"}` with the argument as a
string under that key. This is conformant, not an exception.

### 3.3 `/state` — live state snapshot

Retained, QoS 1, JSON, published **only when `selected` or `settled` changes** — there is no
periodic republication (consumers must not expect a heartbeat on this topic; see the
pa-arm heartbeat requirement in `06-safety.md` for why other slots differ). Exact shape:

```json
{ "ts": "2026-07-06T12:34:56Z", "selected": "port3", "settled": true }
```

- `ts`: string, `YYYY-MM-DDTHH:MM:SSZ`, UTC. In the reference implementation this clock comes
  from a Home Assistant time sync over the vendor's native API — a fragility listed in
  §12.1; a re-implementation should use an independent source (NTP — network time protocol —
  or the on-board RTC) but SHALL keep the exact format.
- `selected`: string, one of `off`, `port1`, `port2`, `port3`, `port4`, `port5`, `port6`.
  **Normative semantics**: derived by reading the six relay states back in fixed priority
  order (port 1's relay checked first, then port 2's, … port 6's; the first relay found
  energized wins; none energized → `off`). It is **never** an echo of the last received
  command. Because of the no-contact-feedback hardware (§2) this is a readback of driven
  coil state, best-effort.
- `settled`: boolean, conservative timed guard, specified in §6.3.

---

## 4. Command surface

Exactly one command exists, on `muehle/hf/ant-switch/cmd`, JSON, QoS 1. The switch
subscribes to the topic and never publishes to it.

```json
{ "select": "port3" }
```

Valid values of the `select` string: `"off"`, `"port1"`, `"port2"`, `"port3"`, `"port4"`,
`"port5"`, `"port6"`.

Behavior on receipt:

1. Map the value to a relay: `"off"` → none; `"port1"`→relay 3; `"port2"`→relay 4;
   `"port3"`→relay 5; `"port4"`→relay 6; `"port5"`→relay 7; `"port6"`→relay 8 (§2 table).
2. An invalid value — anything not in the list, or a missing / non-string `select` key — is
   **silently ignored**: no relay moves, no state publish, no error message anywhere. The
   reference handler just returns. (Normative for wire compatibility; the absence of any
   rejection feedback is a known defect, see §8.)
3. The selection procedure of §6.2 runs: idempotent if the target equals the current
   position; otherwise an exclusive move with `settled:false` published immediately and
   `settled:true` 200 ms after the change.

**Retention of `/cmd` (normative and deliberate)**: this slot's command is an idempotent,
position-based setpoint, so the **commander** (the antennaselect reconciler) publishes it
**retained**. The switch never publishes to `/cmd` and never clears the retained message.
Retention is what makes power-loss self-heal possible (§6.1) and is only safe because every
command is idempotent; a re-implementation SHALL NOT add non-idempotent (one-shot) commands
to this slot without breaking that contract. This is the documented exception to the
station-wide "commands are not retained" default (`02-interface-spec.md` §4), alongside the
other retained actuator setpoints (e.g. power switching). Note that generic bus-inspection
tools may reject retained publishes to `/cmd` as a blanket safety rule (the station's
`testui` tool does); operator-issued selections made through such a tool are therefore
*not* retained and will not be replayed on a later power loss — only the reconciler's
commands participate in self-heal.

**Secondary (non-bus) manual surface — non-normative**: the reference firmware also exposes
the same selection as a vendor-platform "select entity" with options `off`/`port1`…`port6`
over its native API and a built-in web server on port 80 (bring-up/debug only). It drives the
same selection procedure with identical semantics and never touches MQTT. Nothing in the
station core depends on it. There are no other commands: no reboot command topic, no
relay-pulse commands, no raw per-relay control on the bus (the six individual relay toggles
are hidden from the external surface; the spare relays 1–2 and the buzzer are exposed natively
but are not part of the switch function).

---

## 5. Behavior contract

### 5.1 Boot / power-on (normative)

1. All six port relays are **off** at boot. The reference firmware's GPIO switches default
   to off with no position restore, and the I/O expander chip itself powers up with all
   pins as inputs (de-energized). **After any power loss the switch comes up disconnected
   (`selected` = `off`)** — relays never "hold last position" across a power loss.
2. The internal settled flag starts `true` (nothing is moving).
3. Wi-Fi associates; MQTT connects (persistent session, LWT registered).
4. On MQTT connect, in order: birth message `/status` = `online`; after the 100 ms guard,
   retained `/meta`; retained `/state` (`selected:"off"`, `settled:true` — unless a relay is
   somehow already driven, in which case the readback value).
5. Because the session is persistent and the commander retains `/cmd`, the broker
   redelivers the retained command to the freshly subscribed switch. **Self-heal
   (normative)**: if the retained command names a different port than the hardware's
   current (off) position, the switch executes the selection — it re-converges to the last
   commanded port after a power cycle with no action from the commander. If the retained
   command matches the current position, the idempotent path runs: a `/state` publish with
   **no relay movement** (no relay chatter).

### 5.2 The selection procedure (normative)

Given a target (port or off):

1. Determine the current position by reading back relay states (§3.3).
2. **Idempotence**: if current == target, publish `/state` and return. No relay toggles;
   `settled` stays `true` — a repeated command must not produce a `settled:false`→`true`
   flap.
3. Otherwise: turn **all six** relays off, then turn exactly the target relay on (for
   `off`, the second step turns none on). This all-off-then-one-on ordering guarantees the
   exclusive invariant by construction — there is never an instant with two ports
   connected. Note it also means every change passes through a brief moment with **zero**
   relays on (**break-before-make**): mid-change the feed line is connected to no antenna
   (and, per §9.2's open grounding question, possibly to no ground either). This is
   deliberate — hot switching is forbidden anyway, so no make-before-break is attempted.
4. Set the settled flag `false` and immediately publish `/state` (new `selected`,
   `settled:false`).
5. Arm a restartable settle timer: wait `relay_settle_ms` = **200 ms** (a conservative
   default, chosen to cover worst-case relay mechanical travel time; tunable in
   configuration, §10), measured from the **most recent** commanded change — a new command
   arriving inside the window cancels and re-arms the timer. Then set the settled flag
   `true` and publish `/state` again (`settled:true`).

A port change therefore produces exactly two `/state` publishes: (`selected` = new,
`settled` = false) at T+0, then (`settled` = true) at T+200 ms.

### 5.3 The settle guard — why 200 ms is load-bearing (normative)

`settled` is a **conservative timed guard**, not a hardware signal. The rule: publish
`settled:false` from the moment a relay move is commanded until the guard time has elapsed
since the most recent change; never publish `settled:true` optimistically. Downstream
consumers gate RF re-enable on it — the station's cold-switch sequencing (antenna change →
wait for `settled:true` → allow transmit again; see `06-safety.md`) depends on `settled`
never lying in the *optimistic* direction. A jammed relay can make it lie in the
*pessimistic-optimistic* direction (it goes `true` at 200 ms even if the contacts never
moved — §8), but a re-implementation SHALL NOT shorten the guard below the relay's
worst-case specified travel time, and SHALL keep the timer-restart semantics (settle
measured from the latest change, not the first of a burst).

### 5.4 Reconnection and outage behavior (normative)

- **MQTT outage, firmware alive**: the LWT fires (`/status` = `offline`, retained). Relays
  **hold their current position** through an MQTT outage — only a reboot/power loss resets
  them. The settled flag also survives (the firmware did not reboot).
- **Reconnect**: birth message `online`, then `/meta` (after the 100 ms guard) and
  `/state` (current readback position, `settled` true), then the retained-`/cmd` redelivery
  runs the idempotent path if position matches, or a real move if the retained command
  names a different port (e.g. moved manually via the native surface during the outage).
- Wi-Fi loss behaves like MQTT loss; the firmware auto-reconnects both.

---

## 6. Invariants and safety rules (normative summary)

The following are the testable requirements extracted from the above; a re-implementation
SHALL satisfy each:

1. **Exclusive selection**: at most one port relay is energized at any instant, enforced by
   the all-off-then-one-on procedure (§5.2 step 3). Two ports connected at once would put
   transmitter power into an unexpected antenna path.
2. **Never echo the command**: `selected` SHALL always be derived from the relay state
   readback, never from the last received `/cmd`.
3. **Conservative settle**: `settled:false` from the commanded change until 200 ms (the
   configured default) after the *most recent* change; `true` only after. Timer restart
   semantics on repeated commands. Never optimistic (§5.3).
4. **Idempotent re-apply**: re-receiving the current position's command must not toggle
   relays and must not flap `settled` (§5.2 step 2) — this is what makes safe
   reconnect/redelivery and the retained-`/cmd` contract possible.
5. **Power loss → all relays open** (feed line disconnected; the safe default position),
   then self-healing re-application of the retained `/cmd` on reconnect (§5.1). Relays
   hold state through MQTT-only outages (§5.4).
6. **No TX gating inside the switch**: it executes any valid command immediately; cold-switch
   sequencing is the commander's job, declared via `hot_switch:false` in `/meta` (§1, §3.2).
7. **Topic and QoS contract**: exactly the four topics of §3, all retained where specified,
   QoS 1; `/status` plain `online`/`offline` with broker-side LWT plus explicit
   clean-shutdown `offline`; `/meta` republished on every connect; `/state` published only
   on change of `selected` or `settled`.
8. **`/cmd` retained only because it is idempotent and position-based** — no one-shot
   commands may be added to this slot without breaking the retention contract (§4).
9. **No port↔antenna knowledge** in the switch (§1); the wiring map is external
   configuration (§9.1).

---

## 7. Fault reporting — absent (documented gap)

There is **no fault-reporting path** on this slot, and a re-implementation must decide
consciously what to keep of that:

- No "hardware unreachable" state exists. Partly inherent — there is no separate device
  behind the bridge to lose; if the firmware is dead, the LWT says so and nothing else can
  be published anyway. But the reference component's own doc describes an optional
  stale-marking behavior ("hold last `selected` with `settled:false`" when the hardware is
  suspected stale) that the firmware simply does not implement.
- **I²C expander communication failures surface only in the device's local logs** (reference
  log level: WARN) — never on the bus. A bus consumer cannot distinguish "expander write
  failed" from normal operation.
- **No contact feedback** (§2): a jammed or failed relay is invisible; `selected` reports the
  commanded coil state and `settled` goes `true` after 200 ms regardless. There is no fault
  state for "relay did not move".
- **Silent drop of invalid commands** (§4 step 2): a commander publishing a malformed
  payload gets no error feedback whatsoever.

A re-implementation SHOULD keep the wire format identical but MAY add fault visibility
(e.g. a read-only fault field in `/state` — additive, so consumers that ignore it are
unaffected). The station's specified-but-unimplemented logging layer
(`00-system-overview.md`) would be the natural place to route device-level faults; nothing
in the deployed system expects them today.

---

## 8. Liveness within station start-up

The switch is powered from the station's 13.8 V DC supply rail, so it boots when that
supply turns on during station start-up. The power sequencer (`powerseq`) waits for
`muehle/hf/ant-switch/status` to be `online` before proceeding to later steps, and the
antenna-selection reconciler acts on this slot's `/state` (readback position) as ground
truth for the station's antenna-safety interlocks (the PA arm path drops when the antenna
is `off`). Requirements:

- The switch SHALL connect and publish `/status` = `online`, `/meta`, and `/state`
  promptly after supply power (the reference achieves this within normal Wi-Fi association
  + MQTT connect time; no explicit bound is implemented, and none is specified beyond
  "prompt" — the sequencer's liveness-wait policy in `03-components/powerseq.md` defines
  the acceptable window).
- Because `/state` is publish-on-change (§3.3), a consumer that misses the boot-time
  publish still gets the current position from the retained message; consumers SHALL read
  retained `/state` rather than waiting for a fresh publish.

---

## 9. Open decisions encoded as configuration or hardware wiring

### 9.1 Port↔antenna wiring map — OPEN DECISION (must be confirmed on-site)

The switch itself is agnostic (§6 item 9), but the station's port↔antenna assignment is
**contradicted between sources** and must be treated as unresolved deployment configuration:

- **Variant A — Ultrabeam on port 3**: repo-root component documentation, the station
  integration model (`docs/station-integration-model.md` shows
  `wiring_map { port1: dummy-load, port3: ultrabeam, port6: fan-dipole }`), and the
  antennaselect unit tests all say port 1 = dummy load, **port 3 = Ultrabeam**,
  port 6 = 80/40 fan dipole; ports 2, 4, 5 unused.
- **Variant B — Ultrabeam on port 4**: the antennaselect example configuration
  (`antennaselect/config.example.toml`, which the deploy script seeds) maps
  `port4 = "ultrabeam"` and calls port 3 unused; the operator console's antenna map agrees
  with port 4.

Port 1 = dummy load and port 6 = fan dipole are consistent in both variants. The
authoritative source is the **deployed reconciler configuration on the station's Raspberry
Pi "shari"** (`/etc/antennaselect/config.toml`), which was not readable from the workstation
when this PRD was written. Any re-implementation SHALL (a) keep the port↔antenna mapping as
external, editable configuration, never code, and (b) resolve this conflict by reading the
deployed config (or the physical cabling) before first use. Until resolved, treat the
Ultrabeam's port as *unknown between 3 and 4*. See also
`03-components/antennaselect.md` (open decisions) and `06-safety.md`.

### 9.2 What the "off" position physically is — OPEN DECISION (grounding)

Two readings of the `off` position exist in the sources:

- **Variant A — off = grounded / shorted**: the antenna-selection reconciler's
  documentation and configuration comments state that the switch's "off" position "shorts
  the open ports to ground" for lightning protection, and the station integration model
  relies on a "fail-safe-to-ground default" at power loss. The live incident record
  (first key-up after an idle-grounding transmits into a "grounded/short switch port",
  documented in the antennaselect research) is consistent with the feed being shorted in
  the off position on the real hardware.
- **Variant B — off = open / unconnected**: the switch firmware's own contract describes
  `off` as *all relays open, feed line unconnected*, and notes that during every port
  change (and thus in the off position) "there is no antenna and no ground connection on
  the feed line". Nothing in the firmware drives any grounding relay (the spare relays
  1–2 are unused by the switch function), and the firmware's capability block says only
  `off:true` (an explicit no-port position exists), nothing about grounding.

These cannot both be true as stated. Possibly the physical relay wiring (SPDT relay
contacts with their normally-closed terminals tied to ground, or an external grounding
relay outside the firmware's view) shorts the feed when all six port relays are open —
making variant A true *in hardware* even though the firmware only knows "all coils off".
The repo does not settle it; it must be verified on the physical hardware. The safety
consequence is significant: whether an idle, unattended station's antenna feed is actually
grounded against lightning depends on the answer, and the reconciler's auto-grounding
feature (idle timeout → select `off`, see `03-components/antennaselect.md`) is sold as
lightning protection on variant A's assumption. A re-implementation SHALL: (a) not claim
grounding in any documentation until the physical behavior is verified; (b) if grounding is
required and not present in hardware, add it explicitly (e.g. drive one of the spare
relays as an external grounding relay with its own exposed capability) rather than leave it
ambiguous.

---

## 10. Configuration

This component is embedded firmware, not a hosted service, and deliberately does **not**
follow the station's TOML-config / systemd conventions (those apply to the hosted bridges;
see `05-deployment-ops.md`). All build-time knobs are substitution variables at the top of
the reference firmware configuration:

| Key | Default value | Meaning |
|---|---|---|
| `device_name` | `station-at1` | firmware node name (network/OTA identity) |
| `friendly_name` | `Antenna Select` | UI label |
| `mqtt_broker` | `192.168.1.50` | broker host |
| `mqtt_port` | `1883` | broker port (plain TCP) |
| `mqtt_user` | `hf` | broker username |
| `site` / `station` / `slot` | `muehle` / `hf` / `ant-switch` | topic prefix — all four topic strings derive from these |
| `relay_settle_ms` | `"200"` | conservative settle guard, milliseconds (§5.3) |

Secrets (broker password, Wi-Fi SSID and password) are resolved at **compile time** from a
`secrets.yaml` next to the firmware config — gitignored, never committed. Note the inherent
limitation of the embedded-secrets model: credentials end up embedded in the compiled
firmware image and are recoverable from the flash of a physically taken device. This is
accepted for hardware sited on a private LAN, but a re-implementation SHALL NOT move
credentials into any committed file.

Other configuration baked into the reference firmware, with its security posture (see
§12.2 for assessment): native-API encryption key (committed in the repo — defeats its
purpose if the repo is shared; a re-implementation SHALL generate a fresh secret and keep
it out of the repo); OTA (over-the-air update) platform with the password line commented
out, i.e. **currently no OTA password**; built-in web server on port 80 with **no
authentication**; log level WARN; client-id suffixing disabled.

---

## 11. Deployment

No systemd unit, no deploy to shari — the device runs standalone in the "bauwagen"
(trailer building), powered from the station's 13.8 V rail (§8) and reachable over Wi-Fi.

- **First flash**: over USB from a workstation using the vendor toolchain
  (`esphome run esphome/station-at1.yaml` in the reference; the mechanism is
  tool-specific, the sequence is not: compile → flash over USB → device joins Wi-Fi →
  connects to the broker).
- **Subsequent updates**: over-the-air (OTA) over Wi-Fi. The update channel currently has
  no password (§10).
- **Local validation** before flashing: `esphome config esphome/station-at1.yaml`
  (requires a throwaway `secrets.yaml` to exist on the build host).
- Dependencies: the station MQTT broker at `192.168.1.50:1883` with `hf`-user credentials
  (in `secrets.yaml`); optionally a Home Assistant server for the reference implementation's
  clock source (§12.1) and as the secondary manual surface (§4).
- **Broker topology decision point**: deployed components point at the broker at
  `192.168.1.50:1883`. A migration of station components to a broker running on shari
  (`192.168.1.139`) exists on an unmerged feature branch and was NOT deployed as of
  2026-08-29; this PRD treats `192.168.1.50` as production. For embedded devices the
  broker address is compile-time configuration (§10), so a migration requires a
  re-flash/reconfigure of this device — see `05-deployment-ops.md` §2 for the migration
  plan.

---

## 12. Reference-implementation notes (non-normative)

### 12.1 Known defects and fragilities of the deployed implementation

Carried here so the re-implementation can fix them deliberately rather than reproduce them:

- **`ts` clock source is Home Assistant**: the `/state` timestamp is set via the vendor
  platform's HA time sync, not NTP and not the on-board PCF85063 RTC (which is read on
  boot into a time entity that is never used). With no HA connected, `ts` is wrong
  (epoch-era). Fix: use NTP or the RTC; keep the format.
- **Silent drop of invalid commands** (§7) — no log even at WARN from the handler, no MQTT
  response.
- **`mode: single` on the selection procedure**: in the reference framework, a second
  invocation while a previous one is still running is *dropped*. The procedure body is
  synchronous and fast so the window is tiny, but a rapid bus + manual-surface command
  collision could in principle drop one command. The settle timer itself uses restart
  semantics (correct, §5.2).
- **No contact-feedback readback** — inherent to the hardware (§2, §7).
- **Static client id `ant-switch`**: a second board flashed with the same configuration
  would fight over the MQTT session. Single-device assumption, undocumented.
- **Documentation drift in the component's own API doc**: one example still shows
  `{"select":"port2"}` for "reconciler selects the dipole" — a leftover from an earlier
  switch generation; at this station port 2 is unused and the dipole is port 6. The wire
  format shown is right; the example value is misleading. Same doc also mentions a
  `config.toml` that does not exist for this component.
- **Security posture**: no OTA password; unauthenticated web server on port 80; committed
  native-API encryption key; embedded compile-time secrets (§10).
- **Boot-hook comment error**: the reference YAML's boot hook comment claims an "early"
  stage but uses the vendor's latest boot priority — cosmetic, but a caution that
  comments there are not authoritative.

### 12.2 What is free to change in a re-implementation

- The firmware framework, microcontroller, expander chip, GPIO numbers, and the unused
  peripherals — provided the exclusive all-off-then-one-on behavior, the readback
  semantics, and the settle guard are preserved on the new hardware.
- The 100 ms connect-time delay before `/meta` (a jitter guard, not protocol).
- The manual/secondary surface (native API, web server) — nothing in the station core
  depends on it.
- The `ts` clock source (fixing the HA dependency is recommended; keep the format).
- The `device.model` free-text string.
- Adding fault visibility (additively, §7).

---

## 13. Open decisions & unresolved facts

1. **Ultrabeam port: 3 or 4** (§9.1). Variant A (port 3): repo docs, integration model,
   antennaselect tests. Variant B (port 4): `antennaselect/config.example.toml` + deploy
   seed + console antenna map. Authoritative: the deployed `/etc/antennaselect/config.toml`
   on shari, unreadable from the workstation at PRD time. Resolution requires on-device
   confirmation; until then the switch spec stays port-agnostic (as it must be).
2. **What `off` physically is — grounded/short vs open** (§9.2). Variant A (shorted to
   ground, lightning-safe): reconciler docs/config comments, integration model
   "fail-safe-to-ground", live post-grounding key-up incident wording. Variant B (all
   relays open, no ground): the switch firmware's own contract, its unused spare relays,
   and its change-sequence description. Must be verified on the physical hardware; if
   grounding is required, add it explicitly (external grounding relay) rather than assume.
3. **Deployed firmware image version**: nothing in `/meta` reports a firmware version, so
   whether the flashed device matches the source in the repo is unverifiable from the bus.
   A re-implementation SHOULD add a version field to `/meta` (additive).
4. **Retained-`/cmd` redelivery actually firing self-heal**: the mechanism (§5.1 step 5)
   depends on the reference framework's persistent-session subscription semantics at the
   deployed version; the source asserts it, but it was not independently verified on the
   deployed device. The behavior contract (self-heal after power loss) is normative
   regardless — a re-implementation must demonstrate it in acceptance tests rather than
   inherit it from library behavior.
5. **Invalid-command handling**: the wire-compatible contract is silent-ignore (§4), but a
   re-implementation may justifiably add a rejection log or an additive error field; the
   PRD leaves that as a deliberate deviation to record, not a silent change.
6. **Broker migration** (§11): production is `192.168.1.50:1883`; the shari-broker
   migration is planned but undeployed, and for this embedded device it requires a
   reconfigure/re-flash.
7. **No heartbeat on `/state`** (§3.3) is a deliberate contract here, but consumers with
   staleness expectations (and any future RF-safety logic keying on this slot) must rely on
   retained `/state` + `/status`, not fresh-publish cadence — called out because the
   station's pa-arm heartbeat incident (see `06-safety.md`) shows how publish-on-change
   starves liveness consumers in general.