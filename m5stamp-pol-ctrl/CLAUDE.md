# CLAUDE.md — m5stamp-pol-ctrl

`m5stamp-pol-ctrl` is the firmware for **M5 Stamp PLC #2** (StamPLC K141:
ESP32-S3 + AW9523B I²C expander @0x59, 4 relays; PI4IOE5V6408 button expander;
1.14" ST7789V panel). It publishes the station integration-model slot
`muehle/uhf/pol-ctrl` — **live X-Quad polarization switching**, one shared
setting for both the 2 m and 70 cm X-Quads (their phase lines are driven by
the same relay set):

| Phase relay | AW9523 pin | Phase |
|-------------|-----------|-------|
| `r_horizontal` | 0 | `h` |
| `r_circ_left` | 1 | `cl` |
| `r_circ_right` | 2 | `cr` |
| — (all off) | — | `v` — vertical is the de-energized state |

Pin 3 is the independent **FlexRadio power** relay: local-only (Button C on
the device), `internal: true` — excluded from web_server and native API, never
touched by slot logic.

This is **ESPHome config, not PlatformIO/Arduino** (unlike `m5stamp-hf-ctrl`
on PLC #1) and not a Go service — embedded firmware is not a `-bridge` per
`../docs/conventions/naming.md`. The ESPHome config **is** the slot
implementation: it publishes the canonical four-plane MQTT surface directly
over the station bus (the waveshare ant-switch pattern — firmware is the
bridge). The authoritative wire contract is
`docs/m5stamp-pol-ctrl-mqtt-api.md`.

---

## Commands

```bash
cp secrets.example.yaml esphome/secrets.yaml   # then edit: wifi, mqtt, ota, api_key, web auth
esphome config esphome/xphasectrl.yaml   # compile check (secrets resolve NEXT TO the config)
esphome run esphome/xphasectrl.yaml       # build + flash (first flash must be USB)
esphome logs esphome/xphasectrl.yaml      # serial/OTA logs
```

ESPHome is not installed globally on the workstation — use a throwaway venv:

```bash
python3 -m venv /tmp/esphome-venv
/tmp/esphome-venv/bin/pip install esphome
/tmp/esphome-venv/bin/esphome config esphome/xphasectrl.yaml
```

PLC #2 runs pre-OTA field firmware: the **first flash must be physical USB**;
after that, `esphome run` can go OTA (password via secrets). `secrets.yaml`
lives at `esphome/secrets.yaml` (ESPHome resolves `!secret` next to the config
file, not from the component root) and is gitignored;
`secrets.example.yaml` is the template.

---

## Architecture

**Data flow:** station bus `/cmd` → `mqtt.on_json_message` (loud validation)
→ `polarization` select (single entry point; also Button A / manual UI) →
interlock (all-off-then-one-on) → AW9523 relays; and relay readback →
`publish_state` → `/state`.

1. `esphome/xphasectrl.yaml` — the whole component: hardware bring-up
   (AW9523 external component, display, buttons), the four-plane MQTT layer,
   and the interlock.
2. The **`polarization` select** is the only relay writer (`on_value`):
   idempotent (derived-from-readback guard), break-before-make, vertical =
   all relays off. Every control path funnels through it, so local changes
   converge to the bus and two-active-at-once is structurally impossible.
3. `publish_state` derives `pol` from the **relay driven-latch readback** —
   never the optimistic select or the last `/cmd` (KTD15: relays boot
   all-off ⇒ state boots `v` honestly; the retained `/cmd` re-applies the
   operator's last intent after reconnect — self-healing steady state, model
   §8 actuator exception).
4. Invalid `/cmd` is rejected **loudly**: the rejection lands in the retained
   `/state` `error` field (cleared by the next valid state change), a WARN
   log line, and a red error line on the display. No silent drops — the
   ant-switch pitfall and the m5stamp JSON-boolean trap are closed by
   construction.

**Display** (`update_interval: never`, repainted from the select's
`on_value`): big phase name, per-relay dot row (H/CL/CR/V + PWR for the
FlexRadio relay), error line when a command was rejected. **Buttons**: A =
cycle polarization (hold 4 s = reboot), B = unused, C = toggle FlexRadio
power (local only).

**Polarization stays operator-driven** (plan R15): nothing anywhere binds it
to band, tracking, or any automatic policy.

---

## MQTT topics

```
muehle/uhf/pol-ctrl/meta     retained  birth certificate (capabilities + expose)
muehle/uhf/pol-ctrl/state   retained  { ts, pol, device_online, error? }
muehle/uhf/pol-ctrl/status  retained  online | offline (LWT — this PLC)
muehle/uhf/pol-ctrl/cmd      retained  set_pol intent (self-healing steady state, §8)
```

`/cmd` payload: `{"action":"set_pol","value":"h|v|cl|cr"}` — the argument
rides under the stationa `value` key. `/cmd` is subscribed at QoS 0 (§8 rule
2) and deliberately never cleared: it is desired steady state, re-applied on
every reconnect (the contrast with the sat rotators' one-shot `goto`/`stop`).
See `docs/m5stamp-pol-ctrl-mqtt-api.md` for the full on-the-wire contract,
the KTD4 exposure-register extension (sixth vector — pending fold into
`../docs/known-issues.md` by the docs unit), and the bench acceptance
checklist.

---

## Station model and shared conventions

Shared documentation lives in `../docs/` (this component is a subdirectory of
the stationa monorepo).

| Document | Path |
|---|---|
| Station integration model (§5 dumb actuator, §7.2 `uhf/pol-ctrl`, §8 transport rules, §8.1 checklist) | `../docs/station-integration-model.md` |
| MQTT API (authoritative wire contract + bench checklist) | `docs/m5stamp-pol-ctrl-mqtt-api.md` |
| Config and secrets convention (embedded-firmware pattern) | `../docs/conventions/config-and-secrets.md` |
| Bridge-naming convention (embedded firmware is not a `-bridge`) | `../docs/conventions/naming.md` |
| MQTT broker topology | `../docs/conventions/mqtt-topology.md` |

This firmware implements the `muehle/uhf/pol-ctrl` slot and must conform to
those conventions. The precedent it follows is
`../waveshare_relay-antswitch-bridge/esphome/station-at1.yaml` (with its
recorded pitfalls fixed: loud rejections, suffixed client ID, firmware
version in `/meta`).