# Salvage: antenna-switch.md
> Extracted from PRD/03-components/antenna-switch.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [decision] Port↔antenna wiring map — OPEN DECISION (on-site confirmation needed)

The switch itself carries no port↔antenna knowledge. But the station's port↔antenna
assignment **contradicts itself between sources**. You must treat it as unresolved
deployment configuration:

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
the physical cabling). Until then, treat every port↔antenna assignment as unknown.

Code-check verdict (2026-09-03): **still open.** The repo still carries both variants —
`docs/station-integration-model.md` line 564 shows `port3: ultrabeam` while
`antennaselect/config.example.toml` line 27 maps `port4 = "ultrabeam"`. The deployed
`/etc/antennaselect/config.toml` on shari (authoritative) remains unread; an ssh attempt
to read it during salvage failed on an infrastructure error, not on the device.

## [decision] What the "off" position physically is — OPEN DECISION (grounding)

Two readings of the `off` position exist in the sources:

- **Variant A — off = grounded / shorted**: the antenna-selection reconciler's
  documentation and configuration comments say that the switch's "off" position "shorts
  the open ports to ground". That gives lightning protection. The station integration
  model relies on a "fail-safe-to-ground default" at power loss. The live incident record
  (first key-up after an idle-grounding transmits into a "grounded/short switch port",
  documented in the antennaselect research) also agrees. On the real hardware, something
  shorts the feed in the off position.
- **Variant B — off = open / unconnected**: the switch firmware's own contract describes
  `off` as *all relays open, feed line unconnected*. The PRD research inferred that, with
  zero relays energized (the break-before-make moment of the selection procedure), no
  antenna and no ground connect to the feed line in the off position. That sentence is the
  research's own analysis. No firmware text or firmware doc states it. Nothing in the
  firmware drives any grounding relay (the switch function does not use the spare relays
  1–2). The firmware's capability block says only `off:true` (an explicit no-port position
  exists). It says nothing about grounding. But the component's own API doc
  (`waveshare_relay-antswitch-bridge/docs/ant-switch-mqtt-api.md`, capabilities table for
  `off`) says "an explicit no-port / grounded position exists (all relays off)" — a
  grounding claim inside the firmware component's own documentation. That doc carries
  wording for both variants. This makes the contradiction deeper, not weaker.

These cannot both be true as stated. Possibly the physical relay wiring (SPDT relay
contacts with their normally-closed terminals tied to ground, or an external grounding
relay outside the firmware's view) shorts the feed when all six port relays are open.
Then variant A is true *in hardware*, even though the firmware only knows "all coils
off". The repo does not settle the question. Someone must check it on the physical
hardware. The safety consequence is significant: whether an idle, unattended station's
antenna feed is actually grounded against lightning depends on the answer. The
reconciler's auto-grounding feature (idle timeout → select `off`) presents itself as
lightning protection on variant A's assumption. A re-implementation must not claim
grounding in any documentation before someone checks the physical behavior. If the design
needs grounding and the hardware does not have it, add it explicitly. For example: drive
one of the spare relays as an external grounding relay with its own exposed capability.

Code-check verdict (2026-09-03): **still open.** `docs/station-integration-model.md` line
564 now asserts `off: grounded` in the wiring_map, while the firmware
(`waveshare_relay-antswitch-bridge/esphome/station-at1.yaml`) drives no grounding relay
and the API doc line 106 still carries the dual-variant wording "an explicit no-port /
grounded position exists (all relays off)".

## [defect] `ts` clock source is Home Assistant

The vendor platform's HA time sync sets the `/state` timestamp — not NTP and not the
on-board PCF85063 RTC (the firmware reads the RTC on boot into a time entity that it
never uses). With no HA connected, `ts` is wrong (epoch-era). Fix: use NTP or the RTC.
Keep the format.

Code-check verdict (2026-09-03): **still open** — `esphome/station-at1.yaml` `time:` still
uses `platform: homeassistant` (with `pcf85063.write_time` on sync), and the `pcf85063`
time entity is still read but unused for `/state` timestamps.

## [defect] Security posture of the deployed firmware

- native-API encryption key committed in the repo — this defeats its purpose if someone
  shares the repo. A re-implementation must generate a fresh secret and keep it out of
  the repo.
- OTA (over-the-air update) platform with the password line commented out — **there is
  now no OTA password**.
- built-in web server on port 80 with **no authentication**.
- compile-time secrets: the firmware resolves secrets (broker password, Wi-Fi SSID and
  password) at compile time from a gitignored `secrets.yaml`; the credentials end up
  inside the compiled firmware image. Someone who physically takes the device can recover
  them from its flash. The project accepts this for hardware on a private LAN.

Code-check verdict (2026-09-03): **still open** — `station-at1.yaml` carries the
committed `api: encryption: key`, `ota:` password still commented out,
`web_server: port: 80` with no auth.

## [defect] Silent drop of invalid commands

The firmware **silently ignores** an invalid `/cmd` value (anything not in the valid
list, or a missing / non-string `select` key). No relay moves. No state publish appears.
No error message appears anywhere — no log even at WARN from the handler, no MQTT
response. A commander publishing a malformed payload gets no error feedback whatsoever.
This is normative for wire compatibility; its absence of feedback is the defect.

Code-check verdict (2026-09-03): **still open** — the `on_json_message` lambda in
`station-at1.yaml` does `if (r < 0) return;` with no log or publish.

## [defect] `mode: single` on the selection procedure

In the reference framework, the framework drops a second invocation while a previous one
still runs. The procedure body is synchronous and fast, so the window is tiny. But a
rapid bus + manual-surface command collision can in principle drop one command. The
settle timer itself uses restart semantics (correct).

Code-check verdict (2026-09-03): **still open** — `select_relay` script is `mode: single`.

## [defect] Static client id `ant-switch` — single-device assumption

A second board flashed with the same configuration can fight over the MQTT session.
Single-device assumption, undocumented.

Code-check verdict (2026-09-03): **still open** — `mqtt: client_id: ant-switch`, suffixing
disabled.

## [defect] No firmware version reported on the bus

Nothing in `/meta` reports a firmware version. So you cannot check from the bus whether
the flashed device matches the source in the repo. A re-implementation must add a version
field to `/meta` (additive).

Code-check verdict (2026-09-03): **still open** — the `publish_meta` payload in
`station-at1.yaml` has no version field.

## [defect] Documentation drift in the component's own API doc

- One example still shows `{"select":"port2"}` for "reconciler selects the dipole" — a
  leftover from an earlier switch generation. Sources contest the dipole's own port (6 or
  2). Port 6 is the majority reading, so the example value gives the wrong impression.
  The wire format shown is right.
- The same doc mentions a `config.toml` that does not exist for this component.

Code-check verdict (2026-09-03): **still open** —
`waveshare_relay-antswitch-bridge/docs/ant-switch-mqtt-api.md` lines 138/172/208–210 still
use the `port2` dipole example; line 40 still points at `[mqtt]` in a `config.toml`.

## [defect] Boot-hook comment error (cosmetic, but a caution)

The reference YAML's boot hook comment claims an "early" stage but uses the vendor's
latest boot priority. The error is cosmetic. But it is a caution that comments there are
not authoritative.

## [decision] Retained-`/cmd` redelivery firing self-heal — unverified on the deployed device

The self-heal mechanism (broker redelivers the retained `/cmd` to the freshly subscribed
switch after a power cycle, so the switch re-converges to the last commanded port) depends
on the reference framework's persistent-session subscription semantics at the deployed
version. The source asserts it, but nobody independently checked it on the deployed
device. The behavior contract (self-heal after power loss) is normative regardless — a
re-implementation must show it in acceptance tests instead of inheriting it from library
behavior.

## [decision] Broker address is compile-time for this device

For embedded devices the broker address is compile-time configuration
(`mqtt_broker` substitution variable, default `192.168.1.50`). A broker migration to
shari therefore needs a re-flash/reconfigure of this device — unlike the hosted bridges,
which only change a config file.