# mqtt-broker — shack-local Mosquitto, bridged to the Home Assistant broker

This is the **shack-local MQTT broker**: a Mosquitto instance on
[shari](../CLAUDE.md) (`192.168.1.139`) that all station components talk to
locally, with a mosquitto `bridge` connection replicating the `muehle/#`
namespace to the Home Assistant broker at `192.168.1.50:1883` (HA's own
Mosquitto add-on).

**Why.** The HA broker is in the house, not the shack. Every station component
on shari reached it over the house LAN, so if the shack↔house link drops the
station loses its entire bus — `antennaselect`, `powerseq`, and the `hf_console`
tablet can no longer coordinate even though all the radios/PAs/rotators are
physically fine and locally reachable. A shack-local broker gives the station a
working bus independent of the house link; HA is a consumer that catches up
when the link returns.

This is **not a Go component** (no `go.mod`, not in `go.work`) — it is plain
config + a deploy script, like the ESPHome/PlatformIO projects. **No station
component code changes were needed**: every component already configures a
single broker address, so each is simply repointed at the shack broker.

## Topology

```
   shari (192.168.1.139)                         HA box (192.168.1.50)
   ┌─────────────────────────────┐               ┌──────────────────┐
   │ Mosquitto (shack, primary)  │── bridge ───▶ │ Mosquitto (HA)   │
   │ 127.0.0.1:1883              │   connection  │ :1883  (untouched)│
   │  - muehle/# authoritative   │               │  - HA MQTT integ.│
   │  - homeassistant/# discovery│               │  - HA birth topic│
   └──────┬──────────────────────┘               └────────▲─────────┘
          │ 127.0.0.1                                      │
   ┌──────┴──────────────────────────────────────┐         │
   │ all station Go services (flexbridge, ultra,  │  HA UI reads state & sends cmd
   │ antennaselect, powerseq, hadiscovery, …)     │  via its MQTT integration on .50
   └─────────────────────────────────────────────┘
   Remote MQTT clients (Shelly plugs, M5 PLC, ant-switch ESP, console tablet)
   connect to shari:1883 over the shack LAN.
```

**Authority:** `muehle/#` is primary on the shack broker; HA is a consumer via
the bridge. `homeassistant/status` (HA birth) originates on `.50` and is
forwarded in. `hadiscovery` stays on shari and renders discovery **out** to HA —
consistent with the integration-model invariant "HA is the reference consumer,
not a privileged one."

## The bridge (topic directions)

Lives in `mosquitto.conf.example` under the `connection bridge-to-ha` block.
Direction is split (no `both`) so message loops are structurally impossible;
`try_private true` (default) is an extra loop guard.

| `topic` directive                        | dir | why                                                                       |
|-----------------------------------------|-----|---------------------------------------------------------------------------|
| `topic muehle/+/+/state out 1`           | out | HA needs slot state (sensors) — retained                                  |
| `topic muehle/+/+/meta out 1`           | out | HA needs device/class metadata — retained                                 |
| `topic muehle/+/+/status out 1`         | out | HA `availability_topic` (LWT online/offline) — retained                    |
| `topic homeassistant/+/+/+/config out 1`| out | hadiscovery-rendered discovery objects → HA ingests                       |
| `topic homeassistant/status in 1`       | in  | HA birth triggers hadiscovery to re-publish discovery                     |
| `topic muehle/+/+/cmd in 1`              | in  | **required** — hadiscovery renders `command_topic=<addr>/cmd` for writable components; HA publishes cmds on `.50` |

The `homeassistant/status` `in` line is specific (no broader `homeassistant/#`
`in` exists), so there is no overlap with the `homeassistant/+/+/+/config`
`out` line. Local command origins (the console tablet) publish `cmd` on the
shack broker directly and are **not** bridged out — HA does not subscribe to
`/cmd`.

## Accounts and ACLs

Shack broker (`acl.conf.example`):

| user     | read                                                  | write                                                |
|----------|-------------------------------------------------------|------------------------------------------------------|
| `hf`     | `#` (full)                                            | `#` (full) — trusted station-internal account        |
| `bridge` | `muehle/+/+/state`, `/meta`, `/status`, `homeassistant/+/+/+/config` | `homeassistant/status`, `muehle/+/+/cmd`, `local/bridge-to-ha` |
| `console`| `muehle/+/+/state`, `/meta`, `/status`                | `muehle/+/+/cmd` (narrow)                            |

- **`hf`** is the shared account all station Go services (via `127.0.0.1`) and
  the **Shelly smart plugs** connect as. The Shelly plugs publish on their own
  Gen2+ prefix (`shellyplus1pm-<id>/...`), not under `muehle/#`; because that
  prefix is per-device and varies by model, `hf` gets full `readwrite #` rather
  than a narrow pattern. Narrow it to `muehle/#` + concrete Shelly prefixes if
  your model list is fixed. `hadiscovery` also runs as `hf` (needs to read
  `muehle/+/+/meta` + `homeassistant/status` and write `homeassistant/#`).
- **`bridge`** is the local side of the bridge connection — kept narrow so it
  can never originate station state or consume HA commands.
- **`console`** is the `hf_console` tablet (`hf_console/CLAUDE.md` already
  recommends a dedicated console user). Configure the tablet with this account,
  not `hf`.

HA broker (`.50`, configured in the HA Mosquitto add-on — **outside this
repo**): a `stationa-bridge` account with read `homeassistant/status` +
`muehle/+/+/cmd` and write `muehle/+/+/state|meta|status` +
`homeassistant/+/+/+/config`.

## Deploy

```bash
# From this directory — seeds config, ACL, and password db once on shari.
HF_MQTT_PASSWORD=... BRIDGE_MQTT_PASSWORD=... CONSOLE_MQTT_PASSWORD=... ./deploy.sh
```

`deploy.sh` is seed-once (see `../docs/conventions/config-and-secrets.md`):
- installs `mosquitto`/`mosquitto-clients` via apt if missing;
- seeds `/etc/mosquitto/mosquitto.conf` (0600) and `acl.conf` only if absent;
- seeds the password db `/etc/mosquitto/passwd` (0600) with `hf`/`bridge`/`console`
  from the `*_MQTT_PASSWORD` env vars, only if absent;
- enables + restarts the `mosquitto` systemd unit.

After the first deploy, edit on the device — **do not** redeploy to change
settings (it would no-op):

1. Set the bridge secret: in `/etc/mosquitto/mosquitto.conf`, under
   `connection bridge-to-ha`, uncomment `remote_password <value>` with the
   HA-side `stationa-bridge` account password. Restart mosquitto.
2. If any password was skipped (env var empty), add it on the device:
   `sudo mosquitto_passwd /etc/mosquitto/passwd hf` (repeat for `bridge`,
   `console`).

### HA-side setup (outside the repo)

1. In the Home Assistant Mosquitto add-on config, add a `stationa-bridge` user
   with a password, and an ACL granting read `homeassistant/status` +
   `muehle/+/+/cmd` and write `muehle/+/+/state|meta|status` +
   `homeassistant/+/+/+/config`. Restart the add-on.
2. Confirm HA still publishes `homeassistant/status = online` on birth (it does
   by default). The bridge forwards this in to drive `hadiscovery`.
3. HA's MQTT integration keeps pointing at `.50` — no change there.

## Repointing station components

After the broker is up, repoint every component. There is **no code change** —
only broker addresses in config / defaults / docs. Defaults were updated across
the repo; on the devices, the seeded config files own the values, so edit them
in place where they diverge.

**Go services on shari** → `tcp://127.0.0.1:1883` (broker is co-located):
`flexbridge`, `ultrabridge`, `acom1200s-pa-bridge`, `wrc-rotator-bridge`,
`atr1k-tuner-bridge`, `shelly-power-bridge`, `powerseq`, `antennaselect`,
`hadiscovery`. Edit `/etc/<service>/config.toml` `mqtt.broker` (or the matching
`*_MQTT_BROKER` env override) and the `*_MQTT_PASSWORD` to the shack broker's
`hf` password, then `sudo systemctl restart <service>`.

**Remote MQTT clients** → `192.168.1.139:1883` (the shari LAN address):
- **Shelly plugs** — reconfigure each device's MQTT settings to the shari
  broker, `hf` user. (Device-side, via the Shelly web UI; not in this repo.)
- **M5 PLC** (`m5stamp-hf-ctrl`) — set `MQTT_HOST` in `secrets.h` and reflash.
- **ant-switch ESP** (`waveshare_relay-antswitch-bridge`) — set `mqtt_broker` in
  the ESPHome YAML / `secrets.yaml` and reflash.
- **`hf_console` tablet** — set the broker to `192.168.1.139:1883` on first
  launch or via the console top-bar gear. Use the `console` account.

## Operational behavior

- **Shack↔house link down:** the station keeps its full local bus on shari (Go
  services via `127.0.0.1`, remote devices via the shari IP); `antennaselect`,
  `powerseq`, and the console keep working. HA goes stale and **cannot command
  the station** until the link returns — this is the intended trade-off (shack
  autonomy). On link restore, retained `state`/`meta`/`status` re-sync to HA; an
  HA birth (if HA rebooted) re-triggers discovery.
- **shari reboot:** `mosquitto` is a systemd service with persistence, so
  retained `meta`/`state` survive and the bus re-seeds on boot before the
  bridges reconnect.
- **HA reboot:** HA republishes `homeassistant/status=online` → the bridge
  forwards it in → `hadiscovery` republishes discovery out → HA ingests. This
  is the standard HA-rebirth flow, unchanged from the single-broker setup.
- **Cold start, HA down:** `hadiscovery` publishes nothing until an HA birth
  arrives over the bridge — same as today.

## Verification

1. **Broker up:** `systemctl status mosquitto` and
   `mosquitto_sub -h 127.0.0.1 -t '$SYS/#' -v`.
2. **Bridge both directions + retain:** publish a retained
   `muehle/test/foo/state` on `127.0.0.1`, subscribe to it on `.50` → arrives.
   Publish `muehle/test/foo/cmd` on `.50`, subscribe on `127.0.0.1` → arrives.
3. **Canary service:** repoint one lightweight service first (e.g.
   `antennaselect`) to `127.0.0.1`, restart, confirm its `muehle/.../meta` +
   `/state` appear on `.50` (bridged) and the HA entity shows up. Then roll the
   rest out service-by-service.
4. **hadiscovery:** with HA up, restart `hadiscovery` → on HA birth it
   republishes discovery; confirm HA entities appear. Toggle a writable HA
   entity → confirm the `cmd` arrives at the shack broker
   (`mosquitto_sub -h 127.0.0.1 -t 'muehle/+/+/cmd'`).
5. **Outage drill:** firewall-block `.50` from shari; confirm the station still
   responds to console commands and `antennaselect` still reconciles locally.
   Unblock → confirm re-sync.
6. **Remote devices:** reconfigure each Shelly plug, the M5 PLC, the ant-switch
   ESP, and the tablet; confirm each appears on the shack broker
   (`mosquitto_sub -h 127.0.0.1 -t 'muehle/#' -v`).

## Out of scope

- No TLS (the whole stack is plain `tcp://…:1883` on the LAN). If the
  house↔shack link ever leaves the LAN, revisit TLS on the bridge connection.
- No change to HA's MQTT integration or to any station component's MQTT code.