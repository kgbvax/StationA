# MQTT schema — m5stamp-pol-ctrl

This document describes the MQTT interface exposed by **m5stamp-pol-ctrl**, the
M5 Stamp PLC #2 firmware (ESPHome). It implements the `pol-ctrl` slot of the
station integration model (`../../docs/station-integration-model.md`): live
X-Quad polarization switching — one **shared** setting for both the 2 m and
70 cm X-Quads (their phase lines are driven by the same relay set).

> **Status: implemented as an ESPHome config.** The firmware **is** the bridge:
> `esphome/xphasectrl.yaml` (moved here from the loose repo-root YAML) publishes
> the canonical `meta`/`state`/`status`/`cmd` topics directly over MQTT — the
> waveshare ant-switch pattern, with that precedent's recorded pitfalls fixed
> (silent invalid-value drops, single-device `client_id`, no firmware version in
> `/meta`). Everything below is the stable, authoritative surface; downstream
> (hf_console, HA via `hadiscovery`) binds to it, not to ESPHome-native entities.

The controller is a **dumb actuator** with honest state (integration model §5):
it sets one of four phases, reports what the relays actually say, and rejects
anything else loudly. Polarization stays **operator-driven** — no component
binds it to band, tracking, or any automatic policy (sat-ops plan R15).

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1 (plain TCP, `192.168.1.139:1883` — the shack broker on shari) |
| Authentication | Username/password (`hf` account) |
| Clean session | **No** — persistent session; retained `/cmd` is re-delivered on every reconnect (self-heal) |
| Auto-reconnect | yes (ESPHome MQTT client) |
| Client ID | `muehle-uhf-pol-ctrl-xctrl` — slot address + device-name suffix, per-device unique |
| Discovery | off; `topic_prefix: null` — the bus carries exactly the four canonical topics |

Client-ID note: the device suffix exists because a bare slot-derived ID
(the ant-switch's `ant-switch`) collides the moment a second device of the same
kind appears — a recorded pitfall this firmware fixes.

---

## 2. Topic addressing

```
<site>/<station>/<slot>/<suffix>
```

Composed from ESPHome `substitutions` (site `muehle`, station `uhf`, slot
`pol-ctrl`) — no site/station/slot constants anywhere in the logic:

```
muehle/uhf/pol-ctrl/meta
muehle/uhf/pol-ctrl/state
muehle/uhf/pol-ctrl/status
muehle/uhf/pol-ctrl/cmd
```

| Suffix | Retained | Direction | Purpose |
|--------|----------|-----------|---------|
| `/meta` | yes | PLC → bus | birth certificate: identity + capabilities + `expose` |
| `/state` | yes | PLC → bus | live phase readback + error (JSON snapshot) |
| `/status` | yes | broker LWT | liveness: `online` / `offline` |
| `/cmd` | **yes** | bus → PLC | desired polarization (self-healing steady state) |

`/cmd` **is retained** — the model §8 actuator exception. `set_pol` is a
steady-state intent, not a one-shot: re-applying `{"action":"set_pol","value":"cl"}`
after a controller reboot or broker reconnection reproduces exactly the same
physical phase, so the retained value is *supposed* to survive and re-apply
(the deliberate contrast with the sat rotators' one-shot `goto`/`stop`, plan
KTD13). The command subscription is QoS 0 (§8 rule 2): a persistent session
must not let the broker queue an offline backlog — retained delivery is
independent of subscription QoS, so the self-heal still works.

---

## 3. `/status` — liveness

Plain string, retained, QoS 1. `online` on every (re)connect; `offline` via
broker Last Will on unclean disconnect and published on clean shutdown.

Two-layer liveness note (model §3): this slot's `/status` is the ESPHome MQTT
client's liveness; the hardware side (AW9523 expander reachability) is reported
as `device_online` in `/state`. Consumers AND both — a connected client with a
failed expander shows `/status: online` + `device_online: false`.

---

## 4. `/meta` — birth certificate

Retained JSON, published once per connect cycle.

```json
{
  "schema": "1.0",
  "role": "pol-ctrl",
  "device": { "model": "M5 Stamp PLC #2 (StamPLC K141)", "serial": "xctrl", "firmware": "1.0.0" },
  "link": "wifi",
  "host": "embedded",
  "location": "bauwagen",
  "capabilities": {
    "polarizations": ["h", "v", "cl", "cr"],
    "exclusive": true,
    "shared": true,
    "vertical": "all_relays_off"
  },
  "expose": {
    "device": { "name": "X-Quad polarization", "model": "M5 Stamp PLC #2 (StamPLC K141)" },
    "fields": [
      { "key": "pol", "name": "Polarization", "type": "enum",
        "options": ["h", "v", "cl", "cr"], "writable": true,
        "command": { "action": "set_pol", "value_key": "value", "value_type": "string" } },
      { "key": "error", "name": "Error", "type": "string" },
      { "key": "device_online", "name": "Device Online", "type": "boolean" }
    ]
  }
}
```

| Field | Notes |
|-------|-------|
| `role` | `pol-ctrl` — canonical role name (model §4, §7.2), never a device name |
| `device` | PLC #2 identity; `serial` is the ESPHome device name; `firmware` is the `fw_version` substitution (bumped per released flash) — the version-in-`/meta` the ant-switch lacks |
| `host` | `embedded` — device, adapter, and host collapse into one node (model §3, like `m5stamp-hf-ctrl`) |
| `polarizations` | canonical vocabulary: `h`, `v`, `cl`, `cr` (horizontal, vertical, circular left, circular right) |
| `exclusive` | one phase relay at a time, structurally (the interlock) |
| `shared` | the one setting drives **both** X-Quads' phase lines together |
| `vertical` | `all_relays_off` — vertical is the de-energized state, so power loss falls back to vertical |
| `expose` | consumer-neutral field surface (model §3.1). `pol` is a setpoint (`writable` + command descriptor with `action` **and** `value_key: "value"` — the stationa `/cmd` value-key convention) |

---

## 5. `/state` — live state

Retained JSON snapshot, QoS 1. Published on every phase change, on every
(re)connect, and every 60 s as a freshness heartbeat (so `device_online` and
`ts` can never go silently stale between changes).

```json
{ "ts": "2026-09-13T12:34:56Z", "pol": "cl", "device_online": true }
```

With a rejected command pending:

```json
{ "ts": "2026-09-13T12:34:56Z", "pol": "cl", "device_online": true, "error": "cmd rejected: invalid pol 'c1' (expected h|v|cl|cr)" }
```

| Field | Type | Notes |
|-------|------|-------|
| `ts` | string | RFC 3339 UTC, synced from the native-API host (ant-switch `homeassistant` time pattern). **Caveat:** if the ESPHome native API link is down, ts stamps epoch — `pol` itself is unaffected. |
| `pol` | string | `h` \| `v` \| `cl` \| `cr` — derived from the **AW9523 relay states**, never echoed from the select or the last `/cmd`. All relays off ⇒ `v`. |
| `device_online` | boolean | false while the AW9523 I²C expander is marked failed (the phase relays' driver); `/status` stays online in that case. |
| `error` | string | present only while set: the last `/cmd` rejection. Cleared by the next **valid** state change (a `set_pol` that reaches the select, even an idempotent one). |

**Readback honesty (KTD15).** `pol` is derived from the relay *driven-latch*
state — the AW9523B drives relay coils over I²C with no contact-feedback
input wired, so this is the best available signal on this hardware (exactly
the ant-switch's documented readback caveat). It is never the optimistic select
echo: at power-on the relays boot `ALWAYS_OFF`, so `/state` boots `v` honestly
even if the operator's last phase was something else — the retained `/cmd`
then re-applies the last intent once MQTT reconnects, and the snapshot converges.

**Boot-lie fix.** The original loose YAML restored the select's last option
while the relays booted all-off — the device *claimed* the old phase while
physically sitting at vertical. This firmware makes the select boot
`Vertical` (matching the relays) and keeps `/state` relay-derived, so both the
bus and the panel tell the truth from the first publish.

---

## 6. `/cmd` — desired polarization

Retained, JSON. Published by hf_console (Tier 2), an operator, or HA via
`hadiscovery`.

```json
{ "action": "set_pol", "value": "cl" }
```

- The argument rides under the stationa **`value`** key (convention 9,
  `shared/schema`), never under a key named after the action.
- Valid `value`: `"h"`, `"v"`, `"cl"`, `"cr"` — canonical vocabulary, mapped
  internally to the select's `Horizontal` / `Vertical` / `Circular Left` /
  `Circular Right`.
- The command entry point is **idempotent**: a `set_pol` naming the phase the
  relays already report does not touch the relays (a retained re-delivery on
  reconnect is a physical no-op that only refreshes `/state`).

### Rejection semantics — loud, never silent

Every rejection is published into the retained `/state` `error` field (so the
console faults bar and any bus consumer sees it), logged at WARN level, and
shown on the device display as a red error line until the next valid state
change. There is **no silent drop path** — the ant-switch's recorded pitfall
(and the m5stamp firmware's JSON-boolean trap) is deliberately closed:

| Input | Result |
|-------|--------|
| `{"action":"set_pol","value":"cl"}` | phase → Circular Left; `/state` republished |
| `{"action":"set_pol","value":"c1"}` (typo) | `error: "cmd rejected: invalid pol 'c1' (expected h|v|cl|cr)"` |
| `{"action":"set_pol","value":true}` (JSON boolean) | `error: "cmd rejected: set_pol value must be a string h|v|cl|cr"` — type-checked, rejected **loudly** |
| `{"action":"set_pol"}` (no value) | `error: "cmd rejected: set_pol missing value (expected h|v|cl|cr)"` |
| `{"action":"set_az","value":"180"}` | `error: "cmd rejected: action 'set_az' is not set_pol"` |

A rejected command does **not** change `pol` and does **not** clear a
previously set `error` (only a valid state change clears it).

### Local control converges to the bus

All control paths — `/cmd`, Button A (local cycle), a manual selection in the
native API or the auth-protected web UI — funnel through the same select
entity, whose interlock is the only relay writer. Every path ends in a fresh
`/state` publish, so a Button A press on the mast is visible on the bus the
same way a bus command is.

### Interlock (unchanged from the original hardware bring-up config)

A phase change turns **all three** phase relays off first, then energizes the
one relay for the chosen phase — break-before-make, never two at once.
`v` (vertical) is all relays off, which is also the power-loss state: the
phase lines fail to vertical, not to an arbitrary stuck phase.

---

## 7. The FlexRadio relay is NOT part of this slot

AW9523 pin 3 drives an independent **FlexRadio power** relay. It is:

- **never touched** by any `/cmd` path (no action reaches it),
- **excluded from every LAN-reachable UI** — `internal: true` removes it from
  the web UI *and* the native API; only **Button C** on the physical device
  toggles it, with local display feedback (`PWR` dot).

This deliberately narrows the original loose config, which exposed the switch
to Home Assistant by name. The station model carries no `pol-ctrl`-adjacent
FlexRadio capability; keeping the relay local-only is the exposure decision
recorded below.

---

## 8. Pending exposure-register extension (KTD4, sixth vector) — for U9 to fold into `docs/known-issues.md`

> **This section is the drafted sixth enumerated vector of the sat-ops
> exposure review (plan KTD4).** The Tier-1 review (U9) recorded five
> rotator-scoped vectors in `docs/known-issues.md`; this vector covers the
> phase controller's own device surfaces and must land in the register
> **before the PLC #2 flash**. U9's owner: fold this into the register
> verbatim (or adjusted), then this section reduces to a pointer.

**Vector 6 — M5 Stamp PLC #2 (`m5stamp-pol-ctrl`) device surfaces.** Unlike
the rotator listeners, this controller's motion-authority analogue is modest
(relay phase switching, no RF relay sequencing), but the surfaces are the same
class — an unauthenticated or weakly authenticated LAN endpoint can actuate
real hardware:

1. **web_server (HTTP, port 80, LAN-reachable)** — *mitigated*: **digest**
   auth (username/password via gitignored secrets; a bare `web_server:`
   with no auth is exactly the ant-switch precedent this component declines
   to inherit). Digest is chosen over basic so the password never crosses the
   LAN reversibly (ESPHome's own default flips to digest in 2027.1; this
   firmware adopts it now). The FlexRadio relay is not exposed here at all
   (`internal: true`).
2. **OTA (ESPHome OTA, password-protected)** — *mitigated*: OTA password set
   via secrets; first flash is physical USB anyway (pre-OTA firmware), so the
   password gate exists before OTA is ever reachable.
3. **Native API (ESPHome, encrypted, `api_key`)** — *mitigated*: encryption
   key required; adoption requires deliberate pairing. Manual bring-up
   surface only; nothing in the station depends on it.
4. **MQTT `/cmd` via the shack broker (and the HA bridge's inbound
   `muehle/+/+/cmd` forwarding)** — *accepted as-is* (same posture as the
   rotator slots' KTD4 vector 3): any broker account with `hf`-class write
   rights can set polarization. Accepted because polarization switching is
   low-consequence (relay phase, no mechanical motion, no RF-relay sequencing)
   and operator-driven by design; the interlock bounds any command's physical
   effect to one phase relay.
5. **captive_portal (fallback AP)** — *accepted*: engages only when the
   configured WiFi fails (provisioning convenience). The fallback hotspot's
   SSID and password are auto-generated by ESPHome from the device MAC and
   shown only at boot on the serial console — no hardcoded credential. On
   the shack LAN this window is transient. Revisit only if the device is
   ever deployed off-site.

**Draft decision:** vectors 1–3 mitigated (auth/encryption in firmware, baked
into the first flash), vector 4 accepted (inherits the reviewed no-arming /
HA-bridge posture; consequence-bounded by the interlock), vector 5 accepted
with revisit note. **FlexRadio power relay: no remote surface at all** — the
strictest posture on the device, chosen because that relay mains-switches a
radio and has no bus-side consumer.

---

## 9. Bench acceptance checklist (pre-deploy, on the real PLC #2)

The interlock and the retained-cmd self-heal must be **shown on the real
device**, not inherited from framework behavior. First flash is physical USB
(pre-OTA firmware — same constraint as PLC #1).

1. **Compile check**: `esphome config esphome/xphasectrl.yaml` clean (requires
   a `secrets.yaml` next to the config — copy `secrets.example.yaml` to
   `esphome/secrets.yaml`).
2. **Boot honesty**: power-on with the relays previously in a non-vertical
   phase → first `/state` after connect reports `pol: "v"` (relays all-off),
   *not* the stale old phase. Display shows `Vertical` with only the `V` dot.
3. **set_pol cl** (AE4): publish retained `{"action":"set_pol","value":"cl"}`
   → `/state` reports `pol: "cl"`, both X-Quads circular-left (scope the
   relays: exactly one phase relay energized).
4. **Interlock scope-verified**: during a phase change, observe on the AW9523
   (scope/logic probe) that no two phase relays are ever energized together —
   all-off precedes one-on (break-before-make).
5. **Retained cmd self-heal**: with `set_pol cr` retained, power-cycle the
   PLC → boots `v` honestly, then re-applies `cr` after MQTT reconnect;
   `/state` converges to `pol: "cr"`.
6. **Local select converges**: press Button A (cycle) → `/state` follows each
   press on the bus (bus consumer sees local changes).
7. **Invalid value**: publish `{"action":"set_pol","value":"xx"}` → retained
   `/state` gains `error: "cmd rejected: invalid pol 'xx' …"`, `pol` unchanged,
   display shows the red error line; a following valid `set_pol` clears it.
8. **JSON-boolean trap**: publish `{"action":"set_pol","value":true}` →
   rejected loudly (typed error in `/state`), relays untouched — not silence.
9. **web_server auth**: `http://xctrl.local/` demands digest-auth credentials;
   the FlexRadio relay does **not** appear in the web UI (nor native API);
   Button C still toggles it locally with `PWR` display feedback.
10. **Liveness**: unplug the device → broker LWT flips `/status` to `offline`;
    clean shutdown (Button A 4 s hold reboot) publishes `offline` itself.
11. **ts sanity**: with the native-API host connected, `ts` is current UTC
    (check the epoch caveat when it is not).

---

## 10. Typical interaction flows

**Read current polarization on startup**: subscribe `muehle/uhf/pol-ctrl/#` —
retained `/meta`, `/state`, `/status` arrive immediately.

**Console sets circular right** (Tier 2, hf_console):
1. Console publishes **retained** `muehle/uhf/pol-ctrl/cmd`:
   `{"action":"set_pol","value":"cr"}`.
2. Firmware validates (action, presence, type, vocabulary), drives the select
   → interlock: all phase relays off, then the Circular-Right relay on.
3. `/state` → `pol: "cr"`, `error` absent; display repaints with the `CR` dot.
4. A later power cycle: relays boot off (honest `v`), MQTT reconnects, the
   broker re-delivers the retained `/cmd`, firmware re-applies `cr`.

**Operator presses Button A on the mast**: select cycles → interlock →
`/state` republishes; the console and any bus consumer see the local change.

---

## 11. Configuration and secrets

- `substitutions` in `esphome/xphasectrl.yaml`: slot addressing
  (`site`/`station`/`slot`), broker address, `fw_version` (bump per released
  flash — it rides in `/meta.device.firmware`).
- `esphome/secrets.yaml` (gitignored; template: `secrets.example.yaml` at the
  component root — copy it next to the config, where ESPHome resolves
  `!secret`): WiFi, `mqtt_password`, `ota_password`, `api_key`,
  `web_server_username` / `web_server_password`.
- Validate locally: `esphome config esphome/xphasectrl.yaml`.

This is the embedded-firmware secrets pattern (like the ant-switch ESPHome
`secrets.yaml` and `m5stamp-hf-ctrl/src/secrets.h`), distinct from the Go
services' systemd EnvironmentFile — see
`../../docs/conventions/config-and-secrets.md`.