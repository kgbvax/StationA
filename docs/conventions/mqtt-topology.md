# MQTT broker topology

The station runs **two Mosquitto brokers** joined by a mosquitto `bridge`: a
bauwagen-local broker on **scmino** (`192.168.1.178`, DNS alias `bwbroker`;
authoritative for `muehle/#`)
and the Home Assistant broker at `192.168.1.50:1883` (HA's own Mosquitto add-on,
untouched). The bauwagen broker gives the station a working bus even when the
shack↔house link is down; HA is a consumer that catches up when the link
returns.

The broker itself lives at [`mqtt-broker/`](../../mqtt-broker/) (config, ACL,
seed-once deploy script). This document is the canonical reference for the
topology, addressing, topic directions, and ACLs.

## Topology

```
   scmino (192.168.1.178, alias bwbroker)        HA box (192.168.1.50)
   ┌─────────────────────────────┐               ┌──────────────────┐
   │ Mosquitto (bw, primary)     │── bridge ───▶ │ Mosquitto (HA)   │
   │ 0.0.0.0:1883                │   connection  │ :1883  (untouched)│
   │  - muehle/# authoritative   │               │  - HA MQTT integ.│
   │  - homeassistant/# discovery│               │  - HA birth topic│
   └──────┬──────────────────────┘               └────────▲─────────┘
          │ 192.168.1.178:1883 over the shack LAN          │
   ┌──────┴──────────────────────────────────────┐         │
   │ all station Go services on shari            │  HA UI reads state & sends cmd
   │ (flexbridge, ultra, antennaselect,          │  via its MQTT integration on .50
   │ powerseq, hadiscovery, …)                    │
   └─────────────────────────────────────────────┘
   Remote MQTT clients (Shelly plugs, M5 PLC, ant-switch ESP, console tablet)
   connect to scmino:1883 over the shack LAN.
```

**Authority:** `muehle/#` is primary on the bw broker; HA is a consumer via
the bridge. `homeassistant/status` (HA birth) originates on `.50` and is
forwarded in. `hadiscovery` stays on shari and renders discovery **out** to HA
— consistent with the integration-model invariant "HA is the reference
consumer, not a privileged one" (§9).

## Addressing

| Client | Broker address | Why |
|--------|---------------|-----|
| Go services on shari | `tcp://192.168.1.178:1883` | the broker lives on scmino — nothing is co-located with it anymore; plain `tcp://` over the shack LAN. **Alternative:** `tcp://bwbroker:1883` (see the bwbroker note below) |
| Shelly plugs, M5 PLC, ant-switch ESP, console tablet | `192.168.1.178:1883` | remote devices; use the broker's LAN address |
| Workstation (dev `go run`, `mosquitto_sub`) | `192.168.1.178:1883` | LAN-reachable bw broker |
| HA's MQTT integration | `192.168.1.50:1883` | HA's own broker, unchanged |

Since the 2026-09 move off shari the broker is **not co-located with any
station service**, so `192.168.1.178` (or the `bwbroker` name) is the address
from everywhere — shari services included. Only on-scmino processes use
`127.0.0.1`. Config defaults (`config.example.toml`, `deploy.sh`, Go
`Default()`) carry `192.168.1.178`; devices own their seeded values, so a
device repointed at an older broker address is edited in place at repoint
time.

**The `bwbroker` name.** `bwbroker` is a DNS alias for the bauwagen broker host
(resolving to scmino, `192.168.1.178`, since the move off shari). Any station
component may use `tcp://bwbroker:1883` — `icom9700-radio-bridge` does by
default (plan KTD-7) — so it follows whichever broker is active for bauwagen
business without a config change. The constraint that makes it safe: **`bwbroker`
may resolve only to the `muehle/#`-authoritative (bauwagen) broker.**
Replication is split-direction (state/meta/status bw→HA, `/cmd` HA→bw only), so
a resolution to the HA consumer broker (`.50`) would strand the slot from
station consumers and deafen it to console cmds — a silently half-connected
slot. Deploy verifies resolution (`getent hosts bwbroker`) before config is
seeded. The hop is plaintext `tcp://` on the shack LAN — reviewed and accepted
in `docs/known-issues.md` (the IC-9700 exposure review, vector 7).

## Bridge topic directions

The `connection bridge-to-ha` block (in `mqtt-broker/mosquitto.conf.example`)
connects out from scmino to `.50`. Direction is split (no `both`) so message
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

- **`hf`** is the shared account all station Go services (over the shack LAN) and
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

**Sat-ops rotator slots** (`muehle/uhf/az-rotator`, `muehle/uhf/el-rotator`,
`spid-ercm-rotator-bridge`). Their `/cmd` acceptance follows the same two
accounts as every other slot — and that is the reviewed no-arming free-motion
posture made concrete, not an oversight (the per-vector decisions are in
`docs/known-issues.md`, "Sat-ops rotators: pre-deploy exposure review"):

- the `console` account's narrow `muehle/+/+/cmd` write pattern necessarily
  includes both rotator slots' `/cmd` — the tablet's steering widgets and its
  designated e-stop (STOP publishes `stop` to both rotator slots) ride the same
  authority, so narrowing the account would also remove the e-stop;
- the **HA bridge's inbound `muehle/+/+/cmd`** forwarding delivers motion
  commands to the rotator slots like any other slot — any house-network MQTT
  client can move the sat rotators. The slots publish read-only `expose` blocks
  (no HA motion widgets), but the forwarding path itself is accepted as-is.

Topic directions for the two slots are the standard per-slot planes: `/state`,
`/meta`, `/status` flow out over the bridge to HA (retained); `/cmd` flows in
(bridged from HA, published locally by the console) and is **one-shot** —
published non-retained and cleared with an empty retained publish after
execute-or-reject, so no stale motion intent can replay on a reconnect
(integration model §8 rules 1–3; the pol-ctrl slot next to them is the
contrast: its `set_pol` is retained self-healing steady state).

HA broker (`.50`, configured in the HA Mosquitto add-on — **outside the repo**):
a `stationa-bridge` account with read `homeassistant/status` +
`muehle/+/+/cmd` and write `muehle/+/+/state|meta|status` +
`homeassistant/+/+/+/config`.

## Operational behavior

- **Shack↔house link down:** the station keeps its full local bus (broker on
  scmino; shari services and remote devices reach it over the shack LAN);
  `antennaselect`, `powerseq`, and the console keep working. HA goes stale and
  **cannot command the station** until the link returns — this is the intended
  trade-off (shack autonomy). On link restore, retained `state`/`meta`/`status`
  re-sync to HA; an HA birth (if HA rebooted) re-triggers discovery.
- **scmino reboot:** `mosquitto` is a systemd service with persistence, so
  retained `meta`/`state` survive and the bus re-seeds on boot before the
  bridges reconnect; the shari services reconnect over the LAN.
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