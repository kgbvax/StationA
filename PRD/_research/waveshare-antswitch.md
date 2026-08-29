# Research spec: waveshare_relay-antswitch-bridge (slot `muehle/hf/ant-switch`)

Source of truth read for this document: `waveshare_relay-antswitch-bridge/CLAUDE.md`,
`waveshare_relay-antswitch-bridge/esphome/station-at1.yaml` (the entire firmware; it is the
only YAML under `esphome/`), `waveshare_relay-antswitch-bridge/docs/ant-switch-mqtt-api.md`,
plus cross-checks in `docs/station-integration-model.md` and `antennaselect/` (the commander
of this slot).

---

## 1. Purpose & role

Amateur-radio ("ham") stations transmit and receive radio signals through **antennas** —
wired structures (here: an "Ultrabeam" motorized beam antenna, an 80/40 m "fan dipole" wire
antenna, and a "dummy load" — a heat-dissipating resistor used as a safe, non-radiating test
load). This station has several antennas but one coaxial feed line to the radio, so a
**coaxial antenna switch** — a electromechanical relay matrix — must connect exactly one of
six physical ports to the feed line at a time.

`waveshare_relay-antswitch-bridge` is the firmware for that 1:6 switch. It is **not** a Go
service like most of this repo's components: it is an **ESPHome** configuration (a
declarative YAML compiled to C++ firmware for an Espressif ESP32 microcontroller). The
compiled firmware *is* the bridge — there is no separate daemon.

Its role in the station:

- It is a **dumb actuator**: it selects one of six ports (or none — "off", meaning the feed
  line is left unconnected to any antenna) and reports what is selected plus whether the
  relay has finished moving. It holds **zero policy** — it does not decide which antenna is
  appropriate for the current radio frequency.
- *Which* port to select is decided by the `antennaselect` reconciler (a separate service,
  slot `muehle/hf/antenna-select`), which publishes commands to this switch.
- The physical antennas are **passive resources** (no electronics, no MQTT presence); they
  exist only in the reconciler's wiring map. The mapping at this station (per the meta-repo
  CLAUDE.md and `antennaselect` reconciler tests): **port 1 = dummy load, port 3 = Ultrabeam,
  port 6 = 80/40 fan dipole; ports 2, 4, 5 unused**. (See §9 for a contradiction found in
  `antennaselect/config.example.toml`, which says `port4 = "ultrabeam"`.)
- The switch deliberately never refuses a command based on whether the station is
  transmitting ("TX"). Sequencing port changes to happen only while receiving ("cold
  switching") is the **commander's** responsibility, because the switch has no RF sensing.

## 2. Upstream interface — the physical device

There is no upstream *service*; the firmware runs on the switching hardware itself.

**Board**: WaveShare ESP32-S3-POE-ETH-8DI-8DO — an ESP32-S3-WROOM-1U-N16R8 module
(ESP32-S3 variant, 16 MB flash, 8 MB PSRAM) on a carrier board with 8 relay outputs,
8 opto-isolated digital inputs, an RS-485 UART, an RTC chip, a WS2812 RGB LED, and a buzzer.
Framework: ESP-IDF (not Arduino). Network: Wi-Fi (an Ethernet W5500 block is present but
commented out).

**Relay drive**: the 8 relay coils are driven through a **PCA9554** I²C GPIO expander at
address `0x20` on the I²C bus (SCL = GPIO41, SDA = GPIO42). Expander pin → board relay →
logical port:

| Logical port | MQTT value | Board relay | PCA9554 pin |
|---|---|---|---|
| none (feed line unconnected) | `"off"` | — (all off) | — |
| port 1 (dummy load) | `"port1"` | Relay 3 | 2 |
| port 2 (unused) | `"port2"` | Relay 4 | 3 |
| port 3 (Ultrabeam) | `"port3"` | Relay 5 | 4 |
| port 4 (unused) | `"port4"` | Relay 6 | 5 |
| port 5 (unused) | `"port5"` | Relay 7 | 6 |
| port 6 (fan dipole) | `"port6"` | Relay 8 | 7 |
| (not part of the switch) | — | Relay 1 | 0 |
| (not part of the switch) | — | Relay 2 | 1 |

**Critical hardware fact**: the PCA9554 drives the relay *coils* but there is **no
contact-feedback input** wired on this board. The firmware therefore cannot verify that a
relay's contacts actually closed — `selected` reports the *driven coil state* (what it
commanded the expander to output), not verified contact closure. This is a behavior
contract point, not an incidental detail.

**Other hardware configured (unused by the switch function, kept as-is):** Relays 1–2
(expander pins 0–1) as plain toggles; buzzer on GPIO48 (`inverted: true`); WS2812 RGB LED on
GPIO38 (1 LED, GRB order); 8 digital input binary sensors with pull-ups on GPIO47, GPIO40,
GPIO39, GPIO5, GPIO6, GPIO7, GPIO1, GPIO2; RS-485 UART on TX GPIO17 / RX GPIO18 at 9600 baud
(no protocol attached); PCF85063 hardware RTC on the same I²C bus. Ethernet option pins (if
ever enabled): W5500 with clk GPIO15, mosi GPIO13, miso GPIO14, cs GPIO16, interrupt GPIO12.

## 3. MQTT presence

Connects to the station MQTT broker at `192.168.1.50:1883`, username `hf`, password from
`!secret mqtt_password`, client id `ant-switch` (fixed string, not suffixed with MAC),
`clean_session: false` (persistent session), MQTT-over-plain-TCP. ESPHome's automatic
per-entity topics are fully suppressed (`topic_prefix: null`) and HA discovery is off
(`discovery: false`) — the bus carries **exactly four topics**, all under
`<site>/<station>/<slot>` = `muehle/hf/ant-switch`:

| Topic (exact) | Direction | Retained | QoS | Payload |
|---|---|---|---|---|
| `muehle/hf/ant-switch/meta` | bridge → bus | yes | 1 | JSON birth certificate |
| `muehle/hf/ant-switch/state` | bridge → bus | yes | 1 | JSON snapshot |
| `muehle/hf/ant-switch/status` | bridge → bus | yes | 1 | plain string `online` / `offline` |
| `muehle/hf/ant-switch/cmd` | bus → bridge (bridge subscribes, QoS 1) | retained *by the commander* | 1 | JSON `{"select":"<value>"}` |

### `/status` — liveness (LWT)

- **Birth message** (published by the firmware itself on every MQTT (re)connect): topic
  `muehle/hf/ant-switch/status`, payload `online`, QoS 1, retained.
- **Last Will and Testament** (registered with the broker at connect; broker publishes it if
  the client disappears without a clean disconnect — power loss, crash, Wi-Fi drop):
  same topic, payload `offline`, QoS 1, retained.
- **Shutdown message** (published by the firmware on an orderly shutdown): same topic,
  payload `offline`, QoS 1, retained.

There is no periodic heartbeat topic. Liveness is conveyed solely by `/status` plus the
publish-on-change of `/state`. Note this is one of the station's "two-layer liveness"
slots: `/status` = network/firmware alive, while other slots carry a separate
`device_online` in `/state` for device-link health — this slot has **no** `device_online`
field because the firmware and the device are the same unit.

### `/meta` — birth certificate

Published once per connect cycle, after a deliberate **100 ms delay** from
`on_connect` (a jitter guard for the connect window), retained, QoS 1. Exact payload:

```json
{"schema":"1.0","role":"ant-switch","device":{"model":"WaveShare ESP32-S3-POE-ETH-8DI-8DO (1:6 relay switch)"},"link":"wifi","location":"bauwagen","capabilities":{"ports":[1,2,3,4,5,6],"off":true,"exclusive":true,"hot_switch":false},"expose":{"device":{"name":"Antenna switch","model":"WaveShare ESP32-S3-POE-ETH-8DI-8DO (1:6 relay switch)"},"fields":[{"key":"selected","name":"Selected port","type":"enum","options":["off","port1","port2","port3","port4","port5","port6"],"writable":true,"command":{"value_key":"select","value_type":"string"}},{"key":"settled","name":"Settled","type":"boolean"}]}}
```

Semantics of the fields a reimplementation must reproduce:

- `schema`: version tag of the station bus schema, `"1.0"`.
- `role`: the station-bus role name, `"ant-switch"`.
- `device.model`: free-text hardware description.
- `link`: `"wifi"` (network transport of this slot).
- `location`: `"bauwagen"` (German: the trailer/wagon building housing the station).
- `capabilities.ports`: `[1,2,3,4,5,6]` — integers, the selectable port numbers.
- `capabilities.off: true` — an explicit no-antenna position exists (all relays open).
- `capabilities.exclusive: true` — exactly one port may be connected at any time; the
  firmware enforces this by construction (see §5).
- `capabilities.hot_switch: false` — **load-bearing**: declares that RF (radio transmit
  power) must NOT be flowing while the port changes; the commander must sequence changes
  around receive-only periods.
- `expose`: a consumer-neutral field surface used by the station's `hadiscovery` service to
  render Home Assistant UI entries. Two fields: `selected` (writable enum; a command
  publishes the chosen value under the key named by `command.value_key`, i.e. `"select"`,
  as a string) and `settled` (read-only boolean). The enum option strings are inlined here
  rather than referencing `capabilities.ports` because the shapes differ (ints vs strings).

### `/state` — live state snapshot

Retained, QoS 1, JSON, published **only when `selected` or `settled` changes** (no
cadence). Shape:

```json
{ "ts": "2026-07-06T12:34:56Z", "selected": "port3", "settled": true }
```

- `ts`: string, `YYYY-MM-DDTHH:MM:SSZ` UTC. **Implementation detail/fragility**: the
  timestamp comes from the ESPHome `homeassistant` time platform (`ha_time`), i.e. it is set
  over the ESPHome native API by a Home Assistant server — *not* from the board's PCF85063
  RTC and not from an NTP source. If no Home Assistant is connected when the publish runs,
  the clock is unsynced and `ts` is wrong (epoch-era). (The `on_boot` hook at priority
  `-100` does `pcf85063.read_time` into a *separate* time entity `pcf85063_time`, which is
  never used for `ts`.)
- `selected`: string, one of `off`, `port1`, `port2`, `port3`, `port4`, `port5`, `port6`.
  Derived by reading the six relay switch states in priority order (relay_3 checked first,
  then 4, 5, 6, 7, 8; first one whose state is true wins, else `off`) — it is **never echoed
  from the last received command**. On this hardware that means it reflects the driven coil
  state (best-effort readback, §2).
- `settled`: boolean. `false` from the moment a relay move is commanded until
  `relay_settle_ms` (= **200 ms**, tunable substitution) has elapsed since the most recent
  commanded change; then `true`. It is a **conservative timed guard**, not a hardware
  signal — the design rule is: never publish `settled: true` optimistically; always hold
  `false` at least for the relay's worst-case mechanical travel time after a change.

## 4. Command surface

Exactly one command, over MQTT topic `muehle/hf/ant-switch/cmd` (JSON, QoS 1, retained by
the *commander* — this bridge never publishes to `/cmd`):

```json
{ "select": "port3" }
```

Valid values of the `select` string: `"off"`, `"port1"`, `"port2"`, `"port3"`, `"port4"`,
`"port5"`, `"port6"`.

Behavior on receipt:

1. The string is mapped to a relay number: `off`→0, `port1`→3, `port2`→4, `port3`→5,
   `port4`→6, `port5`→7, `port6`→8.
2. An unknown value (anything else, or a missing/non-string `select` key) is **silently
   ignored** — no relay moves, no state publish, no error message anywhere. The firmware
   just returns from the handler.
3. The shared `select_relay` script runs (see §5): idempotent if the target equals the
   current position; otherwise an exclusive move + `settled:false` publish, with
   `settled:true` following 200 ms later.

Secondary command surface (manual bring-up only, not on the MQTT bus): an ESPHome template
**select entity** named "Antenna Port" with options `off`/`port1`…`port6`, reachable via
Home Assistant over the ESPHome native API (encrypted with key
`5jh059h77vfI5eBHJ8Qa1X06biRAhYxZEjLhotyG5tY=`) or the device's built-in web server on
port 80. It drives the *same* `select_relay` script as `/cmd` — identical behavior — and it
never touches MQTT. Its displayed value is computed from relay states (not optimistic).

There are no other commands: no reboot command topic, no relay-pulse commands, no
per-relay raw control on the bus (the eight individual GPIO switches are `internal: true`
for relays 3–8, i.e. hidden from the HA/native-API surface; relays 1–2 and the buzzer are
exposed natively but are not part of the switch function).

## 5. Behavior & state machine

### Startup / boot

1. ESPHome boots the ESP32-S3 (ESP-IDF). `on_boot` at priority `-100` (runs at the end of
   the boot sequence — the YAML comment claims "early", which is wrong; ESPHome boot
   priorities are descending) reads the hardware RTC (PCF85063) into a time entity.
2. Wi-Fi associates (SSID/password from secrets). MQTT connects to `192.168.1.50:1883` with
   client id `ant-switch`, persistent session, and registers the LWT.
3. All six port relays are **off** at boot: ESPHome GPIO switches default to off with no
   restore, and the PCA9554 expander also powers up with all pins as inputs. **After any
   power loss the switch comes up disconnected (`selected` = `off`)** — the relays do not
   "hold last position" across power loss.
4. The in-memory global `g_settled` starts as `true` (nothing is moving).
5. On MQTT connect, in order: `publish_meta` script executes (100 ms delay, then retained
   `/meta`), and `publish_state` executes (retained `/state` with
   `selected:"off"`, `settled:true` — unless a relay is somehow already driven).
6. Because the session is persistent (`clean_session: false`) and `/cmd` is retained by the
   commander, a retained `/cmd` is redelivered when the firmware (re)subscribes after
   connect. **Self-heal**: if the retained command names a different port than the
   hardware's current (off) position, the firmware executes the select — the switch
   re-converges to the last commanded port after a power cycle, unprompted by the
   reconciler. If the retained command matches the current position, the idempotent path
   runs: a `/state` publish with no relay movement ("no relay chatter").

### The select operation (`select_relay` script, mode `single`)

Given a target relay number (0 = off, 3–8 = ports 1–6):

1. Compute the target port name; read the current port by scanning relay_3…relay_8 states.
2. **Idempotence**: if current == target, publish `/state` and return. No relay toggles,
   `settled` stays `true` (no false-settle flap).
3. Otherwise: turn **all six** relays off, then turn exactly the target relay on (for
   `off`, the target step turns none on). This ordering guarantees the `exclusive`
   invariant by construction — there is never a moment with two relays on. Note there is
   also a brief moment with **zero** relays on during every change (a make-before-break is
   impossible with this firmware — deliberate, since hot switching is forbidden anyway).
4. Set `g_settled = false` and immediately publish `/state` (new `selected`,
   `settled:false`).
5. Start the `settle_timer` script (mode `restart`): wait `relay_settle_ms` = 200 ms, then
   set `g_settled = true` and publish `/state` again (`settled:true`). `mode: restart`
   means a new command arriving inside the window cancels and re-arms the timer — settle
   is measured from the *latest* commanded change.

So a port change produces exactly two `/state` publishes: (`selected` = new,
`settled` = false) at T+0, then (`settled` = true) at T+200 ms.

### Reconnection

- MQTT drop: LWT fires (`/status` = `offline`, retained). Relays **hold their current
  position** through an MQTT outage — only a reboot resets them.
- On reconnect: birth message (`online`), then `/meta` (after 100 ms) and `/state` (current
  position, `settled` true — `g_settled` survives the outage since the firmware didn't
  reboot), then any queued/retained `/cmd` redelivery runs the idempotent path (position
  unchanged → publish only; changed → real move with the 200 ms settle).
- Wi-Fi loss behaves like MQTT loss; ESPHome auto-reconnects both (no custom backoff is
  configured — ESPHome defaults apply).

### Error paths

- Invalid `/cmd` payload: silently ignored (§4).
- There is **no fault-reporting path**: no explicit "hardware unreachable" state is
  implemented. The docs (`ant-switch-mqtt-api.md` §8) describe an optional stale-marking
  behavior ("hold last `selected` with `settled:false`") but the firmware has no such
  logic — partly because there is no separate device to lose (firmware = device). I²C
  expander communication failures would surface only in logs (`logger: level: WARN`).

## 6. Configuration

There is **no TOML config file** — this component is ESPHome YAML, not a Go service, and it
explicitly does **not** follow the meta-repo's systemd/TOML conventions. All knobs are
YAML `substitutions` at the top of `esphome/station-at1.yaml`:

| Key | Value | Meaning |
|---|---|---|
| `device_name` | `station-at1` | ESPHome node name (mDNS/OTA identity) |
| `friendly_name` | `Antenna Select` | UI label |
| `mqtt_broker` | `192.168.1.50` | broker host |
| `mqtt_port` | `1883` | broker port (plain TCP) |
| `mqtt_user` | `hf` | broker username |
| `site` / `station` / `slot` | `muehle` / `hf` / `ant-switch` | topic prefix — all four topics derive from these |
| `relay_settle_ms` | `"200"` | conservative settle guard, milliseconds |

Secrets — **not** in the YAML, resolved at compile time from a `secrets.yaml` next to the
config (gitignored; never committed):
- `wifi_ssid`, `wifi_password` — Wi-Fi credentials
- `mqtt_password` — broker password

Other configuration surfaces baked into the YAML:
- Native API encryption key (committed in the YAML, see §9), OTA platform `esphome` with
  the password line commented out (i.e. **OTA currently has no password**).
- `web_server` on port 80, no authentication.
- `logger` level `WARN`.
- `name_add_mac_suffix: false`.

## 7. Deployment

- No systemd unit, no `deploy.sh`, no shari involvement. The firmware is flashed to the
  board with the standard ESPHome toolchain: `esphome run esphome/station-at1.yaml`
  (first flash over USB; subsequent updates over-the-air via the `ota: esphome` platform or
  the ESPHome dashboard). A local validation check is
  `esphome config esphome/station-at1.yaml`, which requires a throwaway `secrets.yaml`
  to exist.
- The device runs standalone, powered from the station's 13.8 V PSU rail (per the
  integration model, the ant-switch boots when that supply turns on — relevant to station
  power sequencing: `powerseq` waits for this slot's `/status` to go `online` after
  power-on).
- Physical location: "bauwagen" (trailer building at the site).
- Dependencies: the station MQTT broker at `192.168.1.50:1883` (with `hf`-user credentials
  in `secrets.yaml`); optionally a Home Assistant server for time sync (see §3 `ts`) and
  as secondary manual surface.

## 8. Invariants & safety rules (behavior contract)

1. **Exclusive**: at most one port relay is energized at any instant. Enforced by
   turn-all-off-then-one-on in `select_relay`. A reimplementation must preserve this — two
   ports connected at once puts transmitter power into an unexpected antenna path.
2. **Never echo the command**: `selected` must always be derived from (read back from) the
   relay state, never from the last received `/cmd`.
3. **`settled` is conservative**: `false` from the commanded change until the worst-case
   travel time (200 ms here) has elapsed, measured from the most recent change; `true`
   only after. Never optimistic. Downstream RF re-enable gates on this.
4. **Idempotent re-apply**: re-receiving the current position's command must not toggle
   relays and must not flap `settled` (no relay chatter on reconnect/redelivery).
5. **`hot_switch: false` discipline is the commander's job**: this component must not
   invent TX gating; equally, the reconciler must never send a port change during
   transmission. The switch itself executes any valid command immediately.
6. **Power loss → all relays open** (safe default; feed line disconnected), then
   self-healing re-application of the retained `/cmd` on reconnect.
7. **`/cmd` may be retained only because it is idempotent and position-based** — there are
   no one-shot commands on this slot. A reimplementation adding non-idempotent commands
   breaks the retention contract.
8. All four topics retained + QoS 1; `/status` LWT retained so consumers see `offline` after
   a crash.

## 9. Known defects & fragilities

- **Wiring-map contradiction (documentation, not code)**: the meta-repo CLAUDE.md and
  `antennaselect`'s tests both say port 3 = Ultrabeam and port 1 = dummy load, but
  `antennaselect/config.example.toml` maps `port4 = "ultrabeam"` and calls port 3 unused.
  The reconciler's *wiring map* is config-editable, so the physical truth lives in the
  deployed config, not in either file. The example config and the tests/CLAUDE disagree —
  one of them is stale. Any reimplementation must treat the port↔antenna assignment as
  deployment configuration, never hardcode it (this firmware correctly hardcodes nothing
  about antennas).
- **`ts` depends on Home Assistant**: the `/state` timestamp uses the ESPHome
  `homeassistant` time source. With HA down, `ts` is wrong (unsynced epoch). The board's
  hardware RTC is read on boot but only into an unused time entity — the RTC value never
  reaches `ts`. Fragile and arguably a bug.
- **Silent drop of invalid commands**: an invalid `select` value produces no error surface
  at all (no log at WARN level from the handler itself, no MQTT response). Commanders get
  no feedback that their command was rejected.
- **`select_relay` has `mode: single`**: if invoked while a previous invocation is still
  running, the new invocation is *dropped* by ESPHome. The script body is fast
  (synchronous), so the window is tiny, but a rapid /cmd + native-API command collision
  could in principle drop one. The settle timer itself is `mode: restart` (correct).
- **No contact-feedback readback** (inherent to the hardware): a failed/jammed relay is
  invisible; `selected` will still report the commanded coil state and `settled` will go
  `true` after 200 ms regardless. There is no fault state.
- **Security posture**: OTA password commented out (none set); web server on port 80 with
  no auth; the native-API encryption key is committed in the repo (defeats its purpose if
  the repo is shared); broker credentials sit in a gitignored `secrets.yaml` on the build
  host — the ESPHome "secrets" model means they are embedded in the compiled firmware
  image, recoverable from the flash of a physically taken device.
- **Static client id `ant-switch`**: a second flash of the same YAML on another board would
  fight over the MQTT session. Single-device assumption, undocumented.
- **Doc example drift**: `docs/ant-switch-mqtt-api.md` §8 still illustrates "reconciler
  selects the dipole (port 2)" with `{"select":"port2"}` — a leftover from an earlier
  switch generation; at this station port 2 is unused and the dipole is port 6. The wire
  format is right, the example value is misleading. The doc also says "Configured via
  `[mqtt]` in `config.toml`" — leftover prose from the Go-bridge template; this component
  has no config.toml.
- **`on_boot` comment wrong**: says "run this early" but priority `-100` is the latest
  boot stage in ESPHome semantics.
- **Momentary full-disconnect during change**: every port change passes through
  zero-relays-on (break-before-make). Deliberate (no hot switching allowed) but worth
  stating: mid-change there is no antenna and no ground connection on the feed line.

## 10. Re-implementation notes

Must preserve **verbatim** (behavior contract):

- Exact topic strings `muehle/hf/ant-switch/{meta,state,status,cmd}`; retained + QoS 1 on
  all four; `/status` as plain `online`/`offline` with broker-side LWT plus clean-shutdown
  message.
- Exact `/meta` JSON content (all keys above, including `capabilities.hot_switch:false`,
  `exclusive:true`, `off:true`, `ports:[1,2,3,4,5,6]`, and the `expose` block with
  `value_key:"select"`), published on every connect.
- Exact `/state` field set and types: `ts` (string, `YYYY-MM-DDTHH:MM:SSZ` UTC), `selected`
  (enum string, read-back semantics), `settled` (bool).
- Command vocabulary `{"select":"off|port1..port6"}`, silent-ignore of invalid values,
  idempotence, exclusive all-off-then-one-on move, `settled` false immediately on change
  and true 200 ms after the *latest* change (timer restart semantics).
- Boot-to-all-relays-off on power loss + retained-`/cmd` self-heal on reconnect; relays
  hold state through MQTT-only outages.
- Never echo commands into `selected`; no TX gating inside the switch.

Free to change (implementation detail):

- ESPHome, ESP-IDF, the PCA9554/expander specifics, GPIO numbers, the unused peripherals
  (relays 1–2, buzzer, LED, inputs, RS-485, RTC) — provided the port→relay exclusive
  behavior is preserved on the new hardware.
- The 100 ms connect-time delay before `/meta` (it is a jitter guard, not protocol).
- The HA-native-API / web-server secondary surface (manual bring-up only; nothing in the
  station core depends on it).
- The exact `ts` clock source — but a reimplementation should fix the HA-time dependency
  (use NTP or the RTC) while keeping the format.
- The `device.model` free-text string.
- Port↔antenna assignment must remain external configuration (it already is: this
  firmware knows nothing about antennas).

Open questions / not determinable from the repo:

- Whether the *deployed* `antennaselect` wiring map says Ultrabeam is on port 3 (per tests
  and CLAUDE.md) or port 4 (per `config.example.toml`) — the deployed config on shari is
  authoritative and was not inspected here.
- Whether the deployed firmware image matches this YAML (no versioning/version reporting
  exists in the firmware itself — nothing in `/meta` reports a firmware version).
- Whether the ESPHome persistent-session + retained-`/cmd` redelivery actually fires the
  self-heal path on every reconnect in the deployed ESPHome version (the YAML comment
  asserts it; behavior depends on ESPHome's subscription semantics at that version).