# MQTT broker topology

The station runs **two Mosquitto brokers** joined by a mosquitto `bridge`: a
shack-local broker on [shari](../../CLAUDE.md) (authoritative for `muehle/#`)
and the Home Assistant broker at `192.168.1.50:1883` (HA's own Mosquitto add-on,
untouched). The shack broker gives the station a working bus even when the
shack↔house link is down; HA is a consumer that catches up when the link
returns.

The broker itself lives at [`mqtt-broker/`](../../mqtt-broker/) (config, ACL,
seed-once deploy script). This document is the canonical reference for the
topology, addressing, topic directions, and ACLs.

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
   │ all station Go services (flexbridge, ultra, │  HA UI reads state & sends cmd
   │ antennaselect, powerseq, hadiscovery, …)     │  via its MQTT integration on .50
   └─────────────────────────────────────────────┘
   Remote MQTT clients (Shelly plugs, M5 PLC, ant-switch ESP, console tablet)
   connect to shari:1883 over the shack LAN.
```

**Authority:** `muehle/#` is primary on the shack broker; HA is a consumer via
the bridge. `homeassistant/status` (HA birth) originates on `.50` and is
forwarded in. `hadiscovery` stays on shari and renders discovery **out** to HA
— consistent with the integration-model invariant "HA is the reference
consumer, not a privileged one" (§9).

## Addressing

| Client | Broker address | Why |
|--------|---------------|-----|
| Go services on shari | `tcp://127.0.0.1:1883` | co-located with the broker; loopback |
| Shelly plugs, M5 PLC, ant-switch ESP, console tablet | `192.168.1.139:1883` | remote from shari; use its LAN address |
| Workstation (dev `go run`, `mosquitto_sub`) | `192.168.1.139:1883` | LAN-reachable shack broker |
| HA's MQTT integration | `192.168.1.50:1883` | HA's own broker, unchanged |

`127.0.0.1` is only valid on shari; from anywhere else (including a developer's
workstation) use `192.168.1.139:1883`. The on-shari config defaults
(`config.example.toml`, `deploy.sh`, Go `Default()`) use `127.0.0.1`; docs and
remote-device configs use `192.168.1.139`.

## Bridge topic directions

The `connection bridge-to-ha` block (in `mqtt-broker/mosquitto.conf.example`)
connects out from shari to `.50`. Direction is split (no `both`) so message
loops are structurally impossible; `try_private true` (default) is an extra
loop guard.

| `topic` directive                        | dir | why                                                                       |
|-----------------------------------------|-----|---------------------------------------------------------------------------|
| `topic muehle/+/+/state out 1`           | out | HA needs slot state (sensors) — retained                                  |
| `topic muehle/+/+/meta out 1`           | out | HA needs device/class metadata — retained                                 |
| `topic muehle/+/+/status out 1`         | out | HA `availability_topic` (LWT online/offline) — retained                    |
| `topic homeassistant/+/+/+/config out 1`| out | `hadiscovery`-rendered discovery objects → HA ingests                     |
| `topic homeassistant/status in 1`       | in  | HA birth triggers `hadiscovery` to re-publish discovery                   |
| `topic muehle/+/+/cmd in 1`              | in  | **required** — `hadiscovery` renders `command_topic=<addr>/cmd` for writable components; HA publishes cmds on `.50` |

Local command origins (the console tablet) publish `cmd` on the shack broker
directly and are **not** bridged out — HA does not subscribe to `/cmd`.

## Accounts and ACLs

Shack broker (`mqtt-broker/acl.conf.example`):

| user     | read                                                  | write                                                |
|----------|-------------------------------------------------------|------------------------------------------------------|
| `hf`     | `#` (full)                                            | `#` (full) — trusted station-internal account        |
| `bridge` | `muehle/+/+/state`, `/meta`, `/status`, `homeassistant/+/+/+/config` | `homeassistant/status`, `muehle/+/+/cmd`, `local/bridge-to-ha` |
| `console`| `muehle/+/+/state`, `/meta`, `/status`                | `muehle/+/+/cmd` (narrow)                            |

- **`hf`** is the shared account all station Go services (via `127.0.0.1`) and
  the **Shelly smart plugs** connect as. The Shelly plugs publish on their own
  Gen2+ prefix (`shellyplus1pm-<id>/...`), not under `muehle/#`; because that
  prefix is per-device and varies by model, `hf` gets full `readwrite #`. Narrow
  it to `muehle/#` + concrete Shelly prefixes if your model list is fixed.
  `hadiscovery` also runs as `hf` (needs to read `muehle/+/+/meta` +
  `homeassistant/status` and write `homeassistant/#`).
- **`bridge`** is the local side of the bridge connection — kept narrow so it
  can never originate station state or consume HA commands.
- **`console`** is the `hf_console` tablet. Configure the tablet with this
  account, not `hf` (see `hf_console/CLAUDE.md`).

HA broker (`.50`, configured in the HA Mosquitto add-on — **outside the repo**):
a `stationa-bridge` account with read `homeassistant/status` +
`muehle/+/+/cmd` and write `muehle/+/+/state|meta|status` +
`homeassistant/+/+/+/config`.

## Operational behavior

- **Shack↔house link down:** the station keeps its full local bus on shari (Go
  services via `127.0.0.1`, remote devices via the shari IP); `antennaselect`,
  `powerseq`, and the console keep working. HA goes stale and **cannot command
  the station** until the link returns — this is the intended trade-off (shack
  autonomy). On link restore, retained `state`/`meta`/`status` re-sync to HA;
  an HA birth (if HA rebooted) re-triggers discovery.
- **shari reboot:** `mosquitto` is a systemd service with persistence, so
  retained `meta`/`state` survive and the bus re-seeds on boot before the
  bridges reconnect.
- **HA reboot:** HA republishes `homeassistant/status=online` → the bridge
  forwards it in → `hadiscovery` republishes discovery out → HA ingests. This
  is the standard HA-rebirth flow, unchanged from the single-broker setup.
- **Cold start, HA down:** `hadiscovery` publishes nothing until an HA birth
  arrives over the bridge — same as before.

## Deploy and verification

See [`mqtt-broker/README.md`](../../mqtt-broker/README.md) for the seed-once
deploy, HA-side account setup, the component-repointing checklist, and the
end-to-end verification procedure (bridge loop test, canary service, outage
drill).