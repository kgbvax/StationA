# 03 — Component spec: 1:6 coaxial antenna switch (slot `muehle/hf/ant-switch`)

This document specifies the station's coaxial antenna switch. It is an embedded-firmware
component. It connects exactly one of six physical antenna ports (or none) to the single
coaxial feed line ("coax" — the shielded cable carrying radio-frequency signals) that runs
to the transceiver. The reader needs no amateur-radio background: **amateur radio** is the
licensed hobby of two-way radio communication. A **transceiver** ("TRX") is the radio device
used to transmit and receive. **RF** means radio-frequency energy. **TX** means transmitting.
**RX** means receiving. The switch is a deliberately *dumb actuator*. It selects a port when
told to. It reports the selected port. It applies **zero policy** about which antenna is
correct for the current frequency. That decision belongs to the antenna-selection reconciler
(a separate component, slot `muehle/hf/antenna-select` — see
`03-components/antennaselect.md`). This document is the normative behavior contract for any
re-implementation, independent of the reference technology. The reference technology is
ESPHome firmware on an ESP32-S3 relay board (§13 describes it non-normatively).
`02-interface-spec.md` §1–§3 defines the terms from the bus architecture (**MQTT**,
**broker**, **retained message**, **slot**, **plane**, **LWT**). This document uses those
terms as that document gives them. Brief reminders also come at first use.

---

## 1. Purpose and role in the station

The station has three antennas but one coaxial feed line to the radio:

- **Ultrabeam** — a motorized beam (directional) antenna, tuned by a separate controller
  (slot `muehle/hf/ant-ctrl`, see `03-components/ultrabridge.md`).
- **80/40 m fan dipole** — a passive wire antenna resonant on the 80 m and 40 m bands
  (frequency ranges named for their wavelength — about 3.5 MHz and 7 MHz).
- **Dummy load** — a heat-dissipating resistor used as a safe, non-radiating test load
  (transmitting into it radiates nothing, so it is used for testing).

A **coaxial antenna switch** is an electromechanical relay matrix. It connects exactly one
of its six physical ports to the feed line at a time. Each port goes to one antenna (or the
dummy load). The component specified here is the firmware that runs on the switching
hardware itself. There is no separate daemon process and no upstream device behind a serial
or network link. The firmware *is* the bridge and the device in one unit.

Responsibilities of the component:

1. Accept one command over the station MQTT bus: select a port (one of six, or "off").
2. Enforce **exclusive selection**: energize at most one port relay at any instant.
3. Report the selected port as retained MQTT state. Derive it by reading the relay state
   back, never by echoing the command. Also report whether the relay move has **settled**
   (finished moving, conservatively timed — see §6.3).
4. Come up in the all-off position after any power loss. Self-heal to the last commanded
   port by re-executing the retained command on reconnect (§6.1, §6.2).

Explicitly **not** its responsibilities:

- **No antenna knowledge.** The firmware knows nothing about which antenna goes to which
  port. The port↔antenna mapping lives entirely in the reconciler's configuration
  (§9.1). A re-implementation must not hardcode any port↔antenna assignment.
- **No transmit (TX) gating.** The switch has no RF sensing and never refuses a command
  because the station transmits. The **commander** must sequence port changes to happen
  only while the station receives ("**cold switching**" — changing the relay matrix while
  no transmit RF flows, so the relay contacts do not arc). This is the commander's
  responsibility. The switch declares this in its capability block through
  `hot_switch: false` (§4.2) and executes any valid command immediately. See
  `06-safety.md` for the station-wide cold-switch sequencing rules.

The station's power sequencer (`powerseq`, slot `muehle/hf/power-seq`,
see `03-components/powerseq.md`) depends on this slot's liveness during station start-up.
The switch gets power from the station's 13.8 V DC supply rail. The sequencer waits for
this slot's `/status` to go `online` after power-on before it continues. The `/status`
birth timing is therefore load-bearing beyond this component. The relative order of
`/meta` and `/state` after that birth is not load-bearing (§5.1).

---

## 2. Hardware interface (what the firmware drives)

This section describes the deployed hardware. It is normative only where marked. A
re-implementation on different hardware must preserve the *behavior* (§4–§8). But the
board, the pin numbers, and the expander chip can change (§13).

**Board**: WaveShare ESP32-S3-POE-ETH-8DI-8DO — an ESP32-S3 microcontroller module
(16 MB flash, 8 MB PSRAM — PSRAM is an extra external RAM chip) on a carrier board. The
carrier board has 8 relay outputs
(a relay is an electrically operated electromechanical switch) and 8 opto-isolated digital
inputs. It also has an RS-485 UART port (RS-485 is a differential multi-drop serial-bus
standard), a hardware real-time clock chip, one RGB LED, and
a buzzer. A logic pin drives each relay coil. The relay contacts switch the coax line.
Network transport is Wi-Fi. The board also carries a W5500 wired-Ethernet controller
chip. The reference firmware disables that option: its Ethernet block exists only as
comments (`clk_pin: GPIO15`, `mosi_pin: GPIO13`, `miso_pin: GPIO14`, `cs_pin: GPIO16`,
`interrupt_pin: GPIO12`). A re-implementation can use wired Ethernet instead of Wi-Fi.
Then it must set the `/meta` `link` value accordingly (§3.2). The `link` field is an
informational transport descriptor, not a behavioral contract.

**Relay drive — I/O mapping (normative for a re-implementation of this hardware, or
wherever a design uses a port-multiplexing expander)**: the microcontroller does not drive
the 8 relay coils with its own pins. A **PCA9554** I²C GPIO expander (a small chip that
adds remotely-readable/writable I/O pins over a two-wire bus. GPIO = general-purpose
input/output pin) at address `0x20` (SCL = the bus clock line, on GPIO41. SDA = the bus
data line, on GPIO42) drives them. Expander pins 0 and 1 drive board relays 1 and 2,
which are **not part of the antenna switch function** (they exist as spare toggles). The
six switch positions use expander pins 2–7 → board relays 3–8 → logical ports 1–6:

| Logical port | MQTT value string | Board relay | PCA9554 expander pin |
|---|---|---|---|
| none (feed line unconnected) | `off` | — (all relays off) | —. |
| port 1 | `port1` | Relay 3 | 2. |
| port 2 | `port2` | Relay 4 | 3. |
| port 3 | `port3` | Relay 5 | 4. |
| port 4 | `port4` | Relay 6 | 5. |
| port 5 | `port5` | Relay 7 | 6. |
| port 6 | `port6` | Relay 8 | 7. |
| (spare, not part of the switch) | — | Relay 1 | 0. |
| (spare, not part of the switch) | — | Relay 2 | 1. |

The contract gives the command value→relay mapping as fixed: `"off"` → no relay,
`"port1"`→relay 3, …
`"port6"`→relay 8 (§5).

**No contact feedback (normative)**: the expander drives the relay *coils* and there is no
input wired back from the relay *contacts*. The firmware therefore cannot check that a
relay's contacts actually closed or stayed closed. The behavior contract must state these
consequences honestly (§6.4, §8). A jammed or failed relay is invisible on the bus.
`selected` reports the driven coil state (a best-effort readback), not confirmed contact
closure. `settled` becomes `true` on the 200 ms timer, no matter what the contacts
physically did. A re-implementation must treat `selected` as *commanded-coil readback*. It
must not present it as confirmed contact state. If new hardware provides contact feedback,
that is an improvement the consumers can benefit from, but the wire format stays the same.

**Other on-board hardware** (configured in the reference firmware but unused by the switch
function):

- spare relays 1–2 as plain toggles.
- buzzer on GPIO48.
- WS2812 RGB LED on GPIO38.
- 8 digital inputs with pull-ups on GPIOs 47, 40, 39, 5, 6, 7, 1, 2.
- RS-485 UART (TX GPIO17 / RX GPIO18, 9600 baud, no protocol attached).
- PCF85063 hardware RTC on the same I²C bus.

A re-implementation can drop these or keep them.

---

## 3. MQTT presence — topics

The firmware connects to the station MQTT broker (a message server that fans published
messages out to subscribers) at `192.168.1.50:1883` (plain TCP, user `hf` — see §11 for the
planned broker migration). The MQTT client id is `ant-switch` (a fixed string, not suffixed
by MAC address). §12.2 lists the single-device assumption that this encodes as a
constraint. The session is **persistent** (`clean_session: false`). The broker therefore
keeps the subscription and the queued messages across reconnects.

The reference framework disables all per-entity automatic topics. The bus then carries
**exactly four topics**, all under the slot address `muehle/hf/ant-switch`. A "slot" is one
component's address on the bus. `02-interface-spec.md` §1.2 defines the four "planes"
(`meta`, `state`, `status`, `cmd`). **QoS 1** is an MQTT quality-of-service level: the
broker re-transmits a message until the receiver acknowledges it. Every QoS 1 message
therefore arrives at least once.

| Topic (exact) | Direction | Retained | QoS | Payload |
|---|---|---|---|---|
| `muehle/hf/ant-switch/meta` | switch → bus | yes | 1 | JSON identity/capability block (§4.2). |
| `muehle/hf/ant-switch/state` | switch → bus | yes | 1 | JSON state snapshot (§4.3). |
| `muehle/hf/ant-switch/status` | switch → bus | yes | 1 | plain string `online` / `offline` (§4.1). |
| `muehle/hf/ant-switch/cmd` | bus → switch (subscribed, QoS 1) | retained **by the commander**, not by the switch | 1 | JSON `{"select":"<value>"}` (§5). |

### 3.1 `/status` — liveness

Liveness uses MQTT's **Last Will and Testament** (LWT). The client registers this message
with the broker at connect time. The broker publishes it automatically if the client
disappears without a clean disconnect (power loss, crash, Wi-Fi drop).

- On every MQTT (re)connect the switch publishes: topic `muehle/hf/ant-switch/status`,
  payload `online`, QoS 1, retained ("birth message").
- LWT registered at connect: same topic, payload `offline`, QoS 1, retained.
- On an orderly shutdown the firmware itself publishes `offline` (retained, QoS 1) before
  disconnecting.

There is no periodic heartbeat topic. State changes (§4.3) are the only other traffic.
**This slot has no `device_online` field** (the second liveness layer most other slots
carry in `/state` for the health of a *device behind* the bridge). The firmware and the
device are the same unit. `/status` alone is therefore the liveness signal for this slot.
Consumers apply the station's two-layer liveness rule (`02-interface-spec.md` §5) by
treating this slot as single-layer. `/status` = `online` is the only availability check
they can do.

Station-wide caveat (from `02-interface-spec.md` §5, repeated because it applies here): on
a *clean* process shutdown with no explicit offline publish, the broker does not fire the
LWT. A retained `/status` of `online` can then persist for a stopped component. The
reference firmware does publish `offline` on orderly shutdown. The stale-online risk here
is therefore limited to firmware that halts before it reaches its shutdown path (a hard
fault, for example). Consumers must not treat a retained `online` alone as proof of
current availability.

### 3.2 `/meta` — identity and capability block

The switch publishes `/meta` once per connect cycle, retained, QoS 1, after a deliberate
**100 ms delay from connect** (a connect-window jitter guard). The delay value is a
default, not protocol. A re-implementation can choose a different guard if `/meta` still
appears promptly after connect and before any consumer needs it. Exact payload follows
(field names and values normative, except that the `device.model` free-text string can
differ for different hardware):

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
- `capabilities.exclusive: true` — exactly one port can connect at any time. The method of
  §6.2 enforces this by construction.
- `capabilities.hot_switch: false` — **load-bearing**: transmit RF must not flow while the
  port changes. The commander must sequence changes into receive-only periods. The switch
  itself does not enforce this (§1).
- `expose`: the consumer-neutral field surface (used by the station's Home Assistant
  discovery renderer, slot `muehle/hf/discovery`) with two fields:
  - `selected` — writable enum with option strings `off`, `port1` … `port6`. Its
    `command.value_key` is `"select"`. A command payload puts the chosen value under the
    JSON key `"select"` as a string (§5). The descriptor inlines the enum option *strings*
    here, instead of deriving them from the `capabilities.ports` integers, because the
    shapes differ.
  - `settled` — read-only boolean.

Note on the station-wide command-payload convention (`02-interface-spec.md` §1.4): commands
normally look like `{"action":"<name>","value":"<string>"}`. This slot uses the
**value-key-only** form documented there. The expose descriptor names `value_key:"select"`
and carries no `action`. The payload is therefore `{"select":"<value>"}`, with the
argument as a string under that key. This is conformant, not an exception.

### 3.3 `/state` — live state snapshot

The switch publishes `/state` retained, QoS 1, JSON, **only when `selected` or `settled`
changes**. There is no periodic republication (consumers must not expect a heartbeat on
this topic — see the pa-arm heartbeat requirement in `06-safety.md` for why other slots
differ). Exact shape:

```json
{ "ts": "2026-07-06T12:34:56Z", "selected": "port3", "settled": true }
```

- `ts`: string, `YYYY-MM-DDTHH:MM:SSZ`, UTC. In the reference implementation this clock
  comes from a Home Assistant time sync over the vendor's native API (a fragility — §12.1
  lists it). A re-implementation must use an independent source (NTP — network time
  protocol — or the on-board RTC) and must keep the exact format.
- `selected`: string, one of `off`, `port1`, `port2`, `port3`, `port4`, `port5`, `port6`.
  **Normative semantics**: the firmware derives it by reading the six relay states back in
  fixed priority order (port 1's relay first, then port 2's, up to port 6's — the first
  energized relay wins — no energized relay → `off`). It is **never** an echo of the last
  received command. The hardware has no contact feedback (§2). This value is therefore a
  best-effort readback of driven coil state.
- `settled`: boolean. §6.3 specifies the conservative timed guard.

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

1. Map the value to a relay: `"off"` → none, `"port1"`→relay 3, `"port2"`→relay 4,
   `"port3"`→relay 5, `"port4"`→relay 6, `"port5"`→relay 7, `"port6"`→relay 8 (§2 table).
2. The firmware **silently ignores** an invalid value (anything not in the list, or a
   missing / non-string `select` key). No relay moves. No state publish appears. No error
   message appears anywhere. The reference handler just returns. (This is normative for
   wire compatibility. The absence of any rejection feedback is a known defect — see §8.)
3. The selection procedure of §6.2 then runs. If the target equals the current position,
   the run is idempotent. Otherwise the firmware makes an exclusive move. It publishes
   `settled:false` immediately and `settled:true` 200 ms after the change.

**Retention of `/cmd` (normative and deliberate)**: this slot's command is an idempotent,
position-based setpoint. The **commander** (the antennaselect reconciler) therefore
publishes it **retained**. The switch never publishes to `/cmd` and never clears the
retained message. Retention makes power-loss self-heal possible (§6.1). It is only safe
because every command is idempotent. A re-implementation must not add non-idempotent
(one-shot) commands to this slot without breaking that contract. This is the documented
exception to the station-wide "commands are not retained" default
(`02-interface-spec.md` §1.5), alongside the other retained actuator setpoints (power
switching, for example). Generic bus-inspection tools can reject retained publishes to
`/cmd` as a blanket safety rule (the station's `testui` tool does). Such a tool therefore
does not retain operator-issued selections, and the broker does not replay them on a later
power loss. Only the reconciler's commands participate in self-heal.

**Secondary (non-bus) manual surface — non-normative**: the reference firmware also
exposes the same selection as a vendor-platform "select entity" with options
`off`/`port1`…`port6` over its native API. It also runs a built-in web server on port 80
(bring-up/debug only). Both drive the same selection procedure with the same semantics and
never touch MQTT. Nothing in the station core depends on them. There are no other
commands: no reboot command topic, no relay-pulse commands, no raw per-relay control on
the bus. The external surface hides the six individual relay toggles. The firmware exposes
the spare relays 1–2 and the buzzer natively, but they are not part of the switch function.

---

## 5. Behavior contract

### 5.1 Boot / power-on (normative)

1. All six port relays are **off** at boot. The reference firmware's GPIO switches default
   to off with no position restore. The I/O expander chip itself powers up with all pins
   as inputs (de-energized). **After any power loss the switch comes up disconnected
   (`selected` = `off`)** — relays never "hold last position" across a power loss.
2. The internal settled flag starts `true` (nothing moves).
3. Wi-Fiassociates. MQTT connects (persistent session, LWT registered).
4. On MQTT connect: birth message `/status` = `online` (the framework publishes it on
   CONNACK). Then the switch publishes retained `/state` (`selected:"off"`,
   `settled:true` — or the readback value if a relay is somehow already driven). Then it
   publishes retained `/meta`, about 100 ms after connect (the guard of §3.2).
   **Reference behavior, not a normative order**: the reference firmware starts both
   publish tasks at connect, one after the other. Its `/state` task contains no delay and
   finishes first. Its `/meta` task contains the 100 ms delay. The wire order is therefore
   `/status`, then `/state`, then `/meta`. The comment in the reference YAML claims the
   opposite order. That comment is wrong. The relative order of `/meta` and `/state` is
   **not normative**: consumers read retained planes, so they must not depend on it. Only
   the `/status` birth is load-bearing (§1).
5. Because the session is persistent and the commander retains `/cmd`, the broker
   redelivers the retained command to the freshly subscribed switch. **Self-heal
   (normative)**: if the retained command names a different port than the hardware's
   current (off) position, the switch executes the selection. It re-converges to the last
   commanded port after a power cycle, with no action from the commander. If the retained
   command matches the current position, the idempotent path runs: a `/state` publish with
   **no relay movement** (no relay chatter).

### 5.2 The selection procedure (normative)

Given a target (port or off):

1. Get the current position by reading back relay states (§3.3).
2. **Idempotence**: if current == target, publish `/state` and return. No relay toggles.
   `settled` stays `true` — a repeated command must not produce a `settled:false`→`true`
   flap.
3. Otherwise: turn **all six** relays off. Then turn exactly the target relay on (for
   `off`, the second step turns none on). This all-off-then-one-on ordering guarantees the
   exclusive invariant by construction — no instant ever has two ports connected. It also
   means every change passes through a brief moment with **zero** relays on
   (**break-before-make**): mid-change no antenna connects to the feed line (and, per
   §9.2's open grounding question, possibly no ground either). This is deliberate — hot
   switching is forbidden anyway, so the firmware does not try make-before-break.
4. Set the settled flag `false` and immediately publish `/state` (new `selected`,
   `settled:false`).
5. Arm a restartable settle timer: wait `relay_settle_ms` = **200 ms** (a conservative
   default that covers worst-case relay mechanical travel time, tunable in configuration —
   §10). Measure the time from the **most recent** commanded change. A new command inside
   the window cancels the timer and re-arms it. Then set the settled flag `true` and
   publish `/state` again (`settled:true`).

A port change therefore produces exactly two `/state` publishes: (`selected` = new,
`settled` = false) at T+0, then (`settled` = true) at T+200 ms.

### 5.3 The settle guard — why 200 ms is load-bearing (normative)

`settled` is a **conservative timed guard**, not a hardware signal. The rule: publish
`settled:false` from the moment the firmware commands a relay move until the guard time
has elapsed since the most recent change. Never publish `settled:true` optimistically.
Downstream consumers gate RF re-enable on it — the station's cold-switch sequencing
(antenna change, then wait for `settled:true`, then allow transmit again — see
`06-safety.md`) depends on `settled` never lying in the *optimistic* direction. A jammed
relay can make it lie in the *pessimistic-optimistic* direction (it goes `true` at 200 ms
even if the contacts never moved — §8). A re-implementation must not shorten the guard
below the relay's worst-case specified travel time. It must also keep the timer-restart
semantics (settle measured from the latest change, not the first of a burst).

### 5.4 Reconnection and outage behavior (normative)

- **MQTT outage, firmware alive**: the LWT fires (`/status` = `offline`, retained).
  Relays **hold their current position** through an MQTT outage — only a reboot/power
  loss resets them. The settled flag also survives (the firmware did not reboot).
- **Reconnect**: birth message `online`. Then `/state` (current readback position,
  `settled` true) and `/meta` (about 100 ms after connect — §5.1 step 4). The relative
  order of these two is not normative. Then the retained-`/cmd` redelivery runs the
  idempotent path if the position matches, or a real move if the retained command names a
  different port (someone moved the switch manually through the native surface during the
  outage, for example).
- Wi-Fi loss behaves like MQTT loss. The firmware auto-reconnects both.

---

## 6. Invariants and safety rules (normative summary)

The following are the testable requirements extracted from the sections above.
A re-implementation must satisfy each:

1. **Exclusive selection**: the firmware energizes at most one port relay at any instant.
   The all-off-then-one-on procedure (§5.2 step 3) enforces this. Two ports connected at
   once puts transmitter power into an unexpected antenna path.
2. **Never echo the command**: the firmware always derives `selected` from the relay state
   readback, never from the last received `/cmd`.
3. **Conservative settle**: `settled:false` from the commanded change until 200 ms (the
   configured default) after the *most recent* change. `true` only after that. Timer
   restart semantics apply on repeated commands. Never optimistic (§5.3).
4. **Idempotent re-apply**: re-receiving the current position's command must not toggle
   relays and must not flap `settled` (§5.2 step 2). This is what makes safe
   reconnect/redelivery and the retained-`/cmd` contract possible.
5. **Power loss → all relays open** (the safe default position — the feed line
   disconnects). The switch then re-applies the retained `/cmd` on reconnect (§5.1).
   Relays hold state through MQTT-only outages (§5.4).
6. **No TX gating inside the switch**: it executes any valid command immediately.
   Cold-switch sequencing is the commander's job, declared through `hot_switch:false` in
   `/meta` (§1, §3.2).
7. **Topic and QoS contract**: exactly the four topics of §3, all retained where
   specified, QoS 1. `/status` is plain `online`/`offline`, with broker-side LWT plus an
   explicit clean-shutdown `offline`. The switch republishes `/meta` on every connect. It
   publishes `/state` only on a change of `selected` or `settled`.
8. **`/cmd` retained only because it is idempotent and position-based** — nobody can add
   one-shot commands to this slot without breaking the retention contract (§4).
9. **No port↔antenna knowledge** in the switch (§1). The wiring map is external
   configuration (§9.1).

---

## 7. Fault reporting — absent (documented gap)

There is **no fault-reporting path** on this slot. A re-implementation must decide
consciously what to keep of that:

- No "hardware unreachable" state exists. Partly inherent — there is no separate device
  behind the bridge to lose. If the firmware is dead, the LWT says so, and nothing else
  can go out on the bus anyway. But the reference component's own doc describes a
  stale-marking behavior that is not necessary ("hold last `selected` with
  `settled:false`" when the hardware looks stale). The firmware simply does not have it.
- **I²C expander communication failures surface only in the device's local logs**
  (reference log level: WARN) — never on the bus. A bus consumer cannot distinguish
  "expander write failed" from normal operation.
- **No contact feedback** (§2): a jammed or failed relay is invisible. `selected` reports
  the driven coil state and `settled` goes `true` after 200 ms regardless. There is no
  fault state for "relay did not move".
- **Silent drop of invalid commands** (§4 step 2): a commander publishing a malformed
  payload gets no error feedback whatsoever.

A re-implementation must keep the wire format the same. It can add fault visibility (a
read-only fault field in `/state`, for example — additive, so consumers that ignore it see
no change). The station's specified-but-unbuilt logging layer (`00-system-overview.md`)
is the natural place to route device-level faults. Nothing in the deployed system expects
them today.

---

## 8. Liveness within station start-up

The station's 13.8 V DC supply rail powers the switch. It therefore boots when that supply
turns on during station start-up. The power sequencer (`powerseq`) waits for
`muehle/hf/ant-switch/status` to be `online` before it continues to later steps. The
antenna-selection reconciler acts on this slot's `/state` (readback position) as ground
truth for the station's antenna-safety interlocks (PA = power amplifier, the station's
high-power transmit amplifier. The PA arm path drops when the antenna is `off`).
Requirements:

- The switch must connect and publish `/status` = `online`, and retained `/meta` and
  retained `/state` must be present, within the liveness-wait window that
  `03-components/powerseq.md` defines for this slot. That window is the deadline of the
  sequencer's `wait-controllers-online` step: 120 s after the 13.8 V supply turns on (the
  default `step_timeout_s` of the sequencer's step timing). The reference implementation
  has no bound of its own. The sequencer deadline is the operative acceptance bound. The
  acceptance test must show all three planes present inside that window on the reference
  hardware.
- Because `/state` is publish-on-change (§3.3), a consumer that misses the boot-time
  publish still gets the current position from the retained message. Consumers must read
  retained `/state` instead of waiting for a fresh publish.

---

## 9. Open decisions encoded as configuration or hardware wiring

### 9.1 Port↔antenna wiring map — OPEN DECISION (on-site confirmation needed)

The switch itself carries no port↔antenna knowledge (§6 item 9). But the station's
port↔antenna assignment **contradicts itself between sources**. You must treat it as
unresolved deployment configuration:

- **Variant A — Ultrabeam on port 3**: repo-root component documentation, the station
  integration model, and the antennaselect unit tests agree. They say port 1 = dummy
  load, **port 3 = Ultrabeam**, and port 6 = 80/40 fan dipole. Ports 2, 4, 5 stay unused.
  (`docs/station-integration-model.md` shows
  `wiring_map { port1: dummy-load, port3: ultrabeam, port6: fan-dipole }`.)
- **Variant B — Ultrabeam on port 4**: the antennaselect example configuration
  (`antennaselect/config.example.toml`, which the deploy script seeds) maps
  `port4 = "ultrabeam"` and calls port 3 unused. The operator console's antenna map
  agrees with port 4.

The fan dipole's port is also contradicted across sources. The station integration model
gives it port 6 in its `wiring_map` but port 2 in its "Passive resources" list, a few lines
later. The port↔antenna assignment is therefore contested **as a whole**, not only for the
Ultrabeam. The authoritative source is the **deployed reconciler configuration on the
station's Raspberry Pi "shari"** (`/etc/antennaselect/config.toml`). It was not readable
from the workstation when the PRD authors wrote this document. A re-implementation must
(a) keep the port↔antenna mapping as external, editable configuration, never code. It
must (b) resolve the full wiring map before first use by reading the deployed config (or
the physical cabling). Until then, treat every port↔antenna assignment as unknown. See
also `03-components/antennaselect.md` (open decisions) and `06-safety.md`.

### 9.2 What the "off" position physically is — OPEN DECISION (grounding)

Two readings of the `off` position exist in the sources:

- **Variant A — off = grounded / shorted**: the antenna-selection reconciler's
  documentation and configuration comments say that the switch's "off" position "shorts
  the open ports to ground". That gives lightning protection. The station integration
  model
  relies on a "fail-safe-to-ground default" at power loss. The live incident record (first
  key-up after an idle-grounding transmits into a "grounded/short switch port", documented
  in the antennaselect research) also agrees. On the real hardware, something shorts the
  feed in the off position.
- **Variant B — off = open / unconnected**: the switch firmware's own contract describes
  `off` as *all relays open, feed line unconnected*. The PRD research inferred that, with
  zero relays energized (the break-before-make moment of §5.2 step 3), no antenna and no
  ground connect to the feed line in the off position. That sentence is the research's
  own analysis. No firmware text or firmware doc states it. Nothing in the firmware
  drives any grounding relay (the switch function does not use the spare relays 1–2). The
  firmware's capability block says only `off:true` (an explicit no-port position exists).
  It says nothing about grounding. But the component's own API doc
  (`waveshare_relay-antswitch-bridge/docs/ant-switch-mqtt-api.md`, capabilities table for
  `off`) says "an explicit no-port / grounded position exists (all relays off)" — a
  grounding claim inside the firmware component's own documentation. That doc carries
  wording for both variants. This makes the contradiction deeper, not weaker.

These cannot both be true as stated. Possibly the physical relay wiring (SPDT relay
contacts — single-pole double-throw, a changeover contact with one common terminal and
two selectable outputs — with their normally-closed terminals tied to ground, or an
external grounding relay outside the firmware's view) shorts the feed when all six port
relays are open.
Then variant A is true *in hardware*, even though the firmware only knows "all coils
off". The repo does not settle the question. Someone must check it on the physical
hardware. The safety consequence is significant: whether an idle, unattended station's
antenna feed is actually grounded against lightning depends on the answer. The
reconciler's auto-grounding feature (idle timeout → select `off`, see
`03-components/antennaselect.md`) presents itself as lightning protection on variant A's
assumption. A re-implementation must not claim grounding in any documentation before
someone checks the physical behavior. If the design needs grounding and the hardware does
not have it, add it explicitly. For example: drive one of the spare relays as an external
grounding relay with its own exposed capability.

---

## 10. Configuration

This component is firmware that runs embedded in the switching hardware, not a hosted
service. It deliberately does **not**
follow the station's TOML-config / systemd conventions (those apply to the hosted bridges
— see `05-deployment-ops.md`). All build-time knobs are substitution variables at the top
of the reference firmware configuration:

| Key | Default value | Meaning |
|---|---|---|
| `device_name` | `station-at1` | firmware node name (network/OTA identity). |
| `friendly_name` | `Antenna Select` | UI label. |
| `mqtt_broker` | `192.168.1.50` | broker host. |
| `mqtt_port` | `1883` | broker port (plain TCP). |
| `mqtt_user` | `hf` | broker username. |
| `site` / `station` / `slot` | `muehle` / `hf` / `ant-switch` | topic prefix — all four topic strings derive from these. |
| `relay_settle_ms` | `"200"` | conservative settle guard, milliseconds (§5.3). |

The firmware resolves secrets (broker password, Wi-Fi SSID and password) at **compile
time** from a `secrets.yaml` next to the firmware config — gitignored, never committed.
Note the inherent limitation of the embedded-secrets model: the credentials end up inside
the compiled firmware image. Someone who physically takes the device can recover them from
its flash. The project accepts this for hardware on a private LAN. A re-implementation must
not move credentials into any committed file.

Other configuration baked into the reference firmware, with its security posture (see
§12.2 for assessment):

- native-API encryption key (committed in the repo — this defeats its purpose if someone
  shares the repo. A re-implementation must generate a fresh secret and keep it out of the
  repo).
- OTA (over-the-air update) platform with the password line commented out — **there is now
  no OTA password**.
- built-in web server on port 80 with **no authentication**.
- log level WARN.
- client-id suffixing disabled

---

## 11. Deployment

No systemd unit, no deploy to shari — the device runs standalone in the "bauwagen"
(trailer building). The station's 13.8 V rail powers it (§8). It is reachable over Wi-Fi.

- **First flash**: over USB from a workstation with the vendor toolchain (`esphome run
  esphome/station-at1.yaml` in the reference). The mechanism is tool-specific. The
  sequence is not: compile → flash over USB → device joins Wi-Fi → connects to the broker.
- **Subsequent updates**: over-the-air (OTA) over Wi-Fi. The update channel has no
  password now (§10).
- **Local validation** before flashing: `esphome config esphome/station-at1.yaml` (this
  needs a throwaway `secrets.yaml` on the build host).
- Dependencies: the station MQTT broker at `192.168.1.50:1883` with `hf`-user credentials
  (in `secrets.yaml`). Optionally a Home Assistant server for the reference
  implementation's clock source (§12.1) and as the secondary manual surface (§4).
- **Broker topology decision point**: deployed components point at the broker at
  `192.168.1.50:1883`. A migration of station components to a broker running on shari
  (`192.168.1.139`) exists on an unmerged feature branch. Nobody had deployed it as of
  2026-08-29. This PRD treats `192.168.1.50` as production. For embedded devices the
  broker address is compile-time configuration (§10), so a migration needs a
  re-flash/reconfigure of this device — see `05-deployment-ops.md` §2 for the migration
  plan.

---

## 12. Reference-implementation notes (non-normative)

### 12.1 Known defects and fragilities of the deployed implementation

This list exists so the re-implementation can fix the defects deliberately instead of
reproducing them:

- **`ts` clock source is Home Assistant**: the vendor platform's HA time sync sets the
  `/state` timestamp — not NTP and not the on-board PCF85063 RTC (the firmware reads the
  RTC on boot into a time entity that it never uses). With no HA connected, `ts` is wrong
  (epoch-era). Fix: use NTP or the RTC. Keep the format.
- **Silent drop of invalid commands** (§7) — no log even at WARN from the handler, no
  MQTT response.
- **`mode: single` on the selection procedure**: in the reference framework, the framework
  drops a second invocation while a previous one still runs. The procedure body is
  synchronous and fast, so the window is tiny. But a rapid bus + manual-surface command
  collision can in principle drop one command. The settle timer itself uses restart
  semantics (correct, §5.2).
- **No contact-feedback readback** — inherent to the hardware (§2, §7).
- **Static client id `ant-switch`**: a second board flashed with the same configuration
  can fight over the MQTT session. Single-device assumption, undocumented.
- **Documentation drift in the component's own API doc**: one example still shows
  `{"select":"port2"}` for "reconciler selects the dipole" — a leftover from an earlier
  switch generation. Sources contest the dipole's own port (6 or 2 — §13). Port 6 is the
  majority reading, so the example value gives the wrong impression. The
  wire format shown is right. The same doc
  also mentions a `config.toml` that does not exist for this component.
- **Security posture**: no OTA password. Unauthenticated web server on port 80. Committed
  native-API encryption key. Embedded compile-time secrets (§10).
- **Boot-hook comment error**: the reference YAML's boot hook comment claims an "early"
  stage but uses the vendor's latest boot priority. The error is cosmetic. But it is a
  caution that comments there are not authoritative.

### 12.2 What is free to change in a re-implementation

- The firmware framework, microcontroller, expander chip, GPIO numbers, and the unused
  peripherals are free to change. This assumes the new hardware preserves the exclusive
  all-off-then-one-on behavior, the readback semantics, and the settle guard.
- The 100 ms connect-time delay before `/meta` (a jitter guard, not protocol).
- The manual/secondary surface (native API, web server) — nothing in the station core
  depends on it.
- The `ts` clock source (recommended: fix the HA dependency, and keep the format).
- The `device.model` free-text string.
- Fault visibility, added additively (§7).

---

## 13. Open decisions and unresolved facts

1. **Ultrabeam port: 3 or 4** (§9.1). Variant A (port 3): repo docs, integration model,
   antennaselect tests. Variant B (port 4): `antennaselect/config.example.toml` + deploy
   seed + console antenna map. The fan dipole's port is also contested (port 6 vs port 2
   inside the integration model itself — §9.1). No source confirms any port↔antenna
   assignment.
   Authoritative: the deployed
   `/etc/antennaselect/config.toml` on shari, unreadable from the workstation at PRD
   time. Resolution needs on-device confirmation of the whole wiring map. Until then the
   switch spec stays port-agnostic (as it must be).
2. **What `off` physically is — grounded/short vs open** (§9.2). Variant A (shorted to
   ground, lightning-safe): reconciler docs/config comments, integration model
   "fail-safe-to-ground", live post-grounding key-up incident wording. Variant B (all
   relays open, no ground): the switch firmware's own contract and its unused spare
   relays. The component's own API doc carries wording for both variants ("an explicit
   no-port / grounded position exists"). Someone must check this on the physical hardware.
   If the design needs grounding, add it explicitly (external grounding relay) instead of
   assuming it.
3. **Deployed firmware image version**: nothing in `/meta` reports a firmware version. So
   you cannot check from the bus whether the flashed device matches the source in the
   repo. A re-implementation must add a version field to `/meta` (additive).
4. **Retained-`/cmd` redelivery actually firing self-heal**: the mechanism (§5.1 step 5)
   depends on the reference framework's persistent-session subscription semantics at the
   deployed version. The source asserts it, but nobody independently checked it on the
   deployed device. The behavior contract (self-heal after power loss) is normative
   regardless — a re-implementation must show it in acceptance tests instead of inheriting
   it from library behavior.
5. **Invalid-command handling**: the wire-compatible contract is silent-ignore (§4). A
   re-implementation can justifiably add a rejection log or an additive error field. The
   PRD leaves that as a deliberate deviation to record, not a silent change.
6. **Broker migration** (§11): production is `192.168.1.50:1883`. The shari-broker
   migration has a plan, but nobody has deployed it. For this embedded device it needs a
   reconfigure/re-flash.
7. **No heartbeat on `/state`** (§3.3) is a deliberate contract here. But consumers with
   staleness expectations (and any future RF-safety logic that keys on this slot) must
   rely on retained `/state` + `/status`, not fresh-publish cadence. The reason: the
   station's pa-arm heartbeat incident (see `06-safety.md`) shows how publish-on-change
   starves liveness consumers in general.