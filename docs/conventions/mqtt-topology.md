# MQTT broker topology

The station runs **two Mosquitto brokers** joined by a mosquitto `bridge`: the
station broker on **scmino** (`192.168.1.178:1883`, `mqtt-broker/`) and the Home
Assistant broker at `192.168.1.50:1883` (HA's own Mosquitto add-on, untouched).
The scmino broker is **the station broker**: every station client connects to it,
and it is bridged both ways to `.50` as a `muehle/#` mirror. The station keeps a
working bus when the shack↔house link is down, and HA is a consumer that catches
up when the link returns. The original design (2026-08) put this broker on
shari (`192.168.1.139`). It was never deployed there, and the broker went live on
scmino on 2026-10-01.

The broker itself lives at [`mqtt-broker/`](../../mqtt-broker/) (config, ACL,
seed-once deploy script). This document is the canonical reference for the
topology, addressing, topic directions, and ACLs.

> **Current state.** The station broker runs on **scmino**
> (`192.168.1.178:1883`); shari runs no mosquitto. All station clients connect
> to it. It is bridged to `.50` as a `muehle/#` mirror in both directions,
> `homeassistant/status` in, `homeassistant/+/+/+/config` out (see
> `mqtt-broker/mosquitto.conf.example`). A Shelly's native topics are not under
> `muehle/#` and are not bridged, so a plug must use the same broker as
> shelly-power-bridge. Per-service broker settings are seed-once on-device
> configs, so the live value is whatever each config names; the repo defaults
> below point at scmino. The bridge logs in to `.50` as `hf`; the only local
> account is `hf`. The bridge may later be narrowed to the split mapping below.
> Never put a `bwbroker` alias in any `/etc/hosts` that resolves it to anything
> but the station broker (2026-09-26 incident, when it resolved to `.50`).

## Topology

Station broker with the HA bridge:

```
   scmino (192.168.1.178)                        HA box (192.168.1.50)
   ┌─────────────────────────────┐               ┌──────────────────┐
   │ Mosquitto (station, primary)│── bridge ───▶ │ Mosquitto (HA)   │
   │ 0.0.0.0:1883                │   connection  │ :1883  (untouched)│
   │  - muehle/# authoritative   │               │  - HA MQTT integ.│
   │  - homeassistant/# discovery│               │  - HA birth topic│
   └──────┬──────────────────────┘               └────────▲─────────┘
          │ 127.0.0.1                                      │
   ┌──────┴──────────────────────────────────────┐         │
   │ station Go services on scmino (flexbridge,  │  HA UI reads state & sends cmd
   │ ultrabridge, antennaselect, antmode,        │  via its MQTT integration on .50
   │ powerseq, hadiscovery, …) via 127.0.0.1;    │
   │ spid-ercm-rotator-bridge, hf-mqtt-capture   │
   │ on shari via the LAN                        │
   └─────────────────────────────────────────────┘
   Remote MQTT clients (Shelly plugs, M5 Stamp PLC #1 and #2, ant-switch ESP,
   console tablet, M5 Dial, M5 DualKey, winpc-power-bridge on bwpc) connect to
   192.168.1.178:1883 over the shack LAN.
```

**Authority:** `muehle/#` is primary on the station broker; HA is a
consumer via the bridge. `homeassistant/status` (HA birth) originates on `.50`
and is forwarded in. `hadiscovery` (on scmino since 2026-10-12) renders discovery **out** to HA — consistent with the
integration-model invariant "HA is the reference consumer, not a privileged
one" (§9).

## Addressing

| Client | Broker address | Why |
|--------|---------------|-----|
| Go services on scmino | `tcp://127.0.0.1:1883` or `tcp://192.168.1.178:1883` | co-located with the station broker; loopback or the LAN address |
| Go services on shari (spid-ercm-rotator-bridge, hf-mqtt-capture) | not recorded in the repo | defaults are `tcp://127.0.0.1:1883` but shari runs no broker; the live seed-once config must name the scmino broker (`192.168.1.178:1883`) |
| Shelly plugs, M5 Stamp PLC #1 and #2, M5 DualKey, M5 Dial, hf_console tablet (setup default), winpc-power-bridge (bwpc) | `192.168.1.178:1883` | the station broker |
| ant-switch ESP | `waveshare_relay-antswitch-bridge/esphome/station-at1.yaml` and its `docs/known-issues.md` | verify on the device that it names `192.168.1.178` |
| Workstation (dev `go run`, `mosquitto_sub`) | `192.168.1.178:1883` (station broker); `192.168.1.50:1883` also shows every `muehle/#` topic through the mirror | shari (`.139`) runs no broker |
| HA's MQTT integration | `192.168.1.50:1883` | HA's own broker, unchanged |

`127.0.0.1` reaches the station broker only on scmino; shari runs no broker.
Repo defaults point at the station broker: `deploy.sh`, Go `Default()` and
`config.example.toml` use `tcp://192.168.1.178:1883` or `tcp://127.0.0.1:1883` (on
scmino). Ultrabridge has no
example file and its `Default()` leaves the broker empty (MQTT disabled).
Seed-once on-device configs own the live values.

**`bwbroker` (historical).** `icom9700-radio-bridge` used to address the broker
through a DNS name, `tcp://bwbroker:1883`. Its default is now
`tcp://192.168.1.178:1883` like every other service and `deploy.sh` no longer gates
on that name. Any remaining `bwbroker` alias must resolve only to the station broker;
in the 2026-09-26 incident it resolved to `.50`.

## Bridge topic directions

The `connection bridge-to-ha` block (in `mqtt-broker/mosquitto.conf.example`)
connects out from scmino to `.50`. It carries `topic muehle/# both 1` (the
mirror), plus `homeassistant/status in 1` and
`homeassistant/+/+/+/config out 1`; `try_private true` (default) is the loop
guard. The table below is the split mapping the bridge may be narrowed to. Its four `muehle/+/+/…` lines
are commented out in the example under END STATE, and its two `homeassistant/…`
lines are already active. It is applied when the full mirror is no longer wanted, and its
split directions make message loops structurally impossible.

| `topic` directive                        | dir | why                                                                       |
|-----------------------------------------|-----|---------------------------------------------------------------------------|
| `topic muehle/+/+/state out 1`           | out | HA needs slot state (sensors) — retained                                  |
| `topic muehle/+/+/meta out 1`           | out | HA needs device/class metadata — retained                                 |
| `topic muehle/+/+/status out 1`         | out | HA `availability_topic` (LWT online/offline) — retained                    |
| `topic homeassistant/+/+/+/config out 1`| out | `hadiscovery`-rendered discovery objects → HA ingests                     |
| `topic homeassistant/status in 1`       | in  | HA birth triggers `hadiscovery` to re-publish discovery                   |
| `topic muehle/+/+/cmd in 1`              | in  | **required** — `hadiscovery` renders `command_topic=<addr>/cmd` for writable components; HA publishes cmds on `.50` |

With the split mapping, local command origins (the console tablet) publish `cmd` on
the station broker directly and are **not** bridged out — HA does not subscribe
to `/cmd`. With the current full mirror, `/cmd` is carried both ways.

## Accounts and ACLs

Deployed state (per `mqtt-broker/CLAUDE.md`, not re-checked live): the scmino
`passwd` holds only `hf`. The `bridge`, `console` and `dial` ACL blocks are
seeded but have no password entries, and the bridge logs in to `.50` as `hf`,
not `stationa-bridge`. The tablet's setup default is the station broker
(`192.168.1.178`). The table
below and the HA-side `stationa-bridge` account are the split-mapping design.

Station broker (`mqtt-broker/acl.conf.example`):

| user     | read                                                  | write                                                |
|----------|-------------------------------------------------------|------------------------------------------------------|
| `hf`     | `#` (full)                                            | `#` (full) — trusted station-internal account        |
| `bridge` | `muehle/+/+/state`, `/meta`, `/status`, `homeassistant/+/+/+/config` | `homeassistant/status`, `muehle/+/+/cmd`, `local/bridge-to-ha` |
| `console`| `muehle/+/+/state`, `/meta`, `/status`                | `muehle/+/+/cmd` (narrow)                            |
| `dial`   | `muehle/hf/rotator/state`, `/status`                  | `muehle/hf/rotator/cmd` (one slot only)              |

- **`hf`** is the shared station account: the station Go services (via
  `127.0.0.1` or the LAN address), M5 Stamp PLC #1 and the **Shelly smart plugs** connect as `hf`. The
  plugs publish on their native prefixes, not under `muehle/#`: Gen1
  `shellies/<id>/…` (the TRX PSU Plug S, `shellyplug-s-A71704`) and Gen2+/Gen3
  `<shelly_id>/…` (the two Plug S Gen3, `shellyplugsg3-<mac>`). Because those
  prefixes are per-device, `hf` gets full `readwrite #`. Narrow it to `muehle/#`
  plus the concrete Shelly prefixes if the plug list is fixed.
  `hadiscovery` also runs as `hf` (needs to read `muehle/+/+/meta` +
  `homeassistant/status` and write `homeassistant/#`).
- **`bridge`** is reserved for the local side of the bridge connection under the
  split mapping. The example `connection bridge-to-ha` block sets no
  `local_username`, so nothing connects as `bridge` (it has no password on
  scmino) and its ACL does not restrict the bridge. Under the
  `muehle/# both` mirror the narrow `bridge` ACL would block the mirror (if
  mosquitto applies it to the bridge context), so add `local_username bridge`
  (and a `local_password` set on the device) only together with the
  split mapping, and check on the deployed mosquitto version that ACLs apply to
  the bridge context.
- **`console`** is the `hf_console` tablet. Configure the tablet with this
  account, not `hf` (see `hf_console/CLAUDE.md`).
- **`dial`** is the M5Stack Dial rotator head (`m5dial-hf-rotctrl/`), the
  narrowest account: it touches only `muehle/hf/rotator` (read state/status,
  write cmd). Configure the Dial's `src/secrets.h` with this account, not `hf`.

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

Under the split mapping, topic directions for the two slots are the standard per-slot
planes: `/state`, `/meta`, `/status` flow out over the bridge to HA (retained);
`/cmd` flows in (bridged from HA, published locally by the console) and is
**one-shot** — published non-retained and cleared with an empty retained publish
after execute-or-reject, so no stale motion intent can replay on a reconnect
(integration model §8 rules 1–3; the pol-ctrl slot next to them is the
contrast: its `set_pol` is retained self-healing steady state). With the
current full mirror all four planes go both ways.

HA broker (`.50`, split-mapping design, configured in the HA Mosquitto add-on — **outside
the repo**):
a `stationa-bridge` account with read `homeassistant/status` +
`muehle/+/+/cmd` and write `muehle/+/+/state|meta|status` +
`homeassistant/+/+/+/config`.

## Operational behavior

- **Shack↔house link down:** the station keeps its full local bus
  on scmino (Go services via `127.0.0.1`, remote devices via `192.168.1.178`);
  `antennaselect`, `powerseq`, and the console keep working. HA goes stale and
  **cannot command the station** until the link returns — this is the intended
  trade-off (shack autonomy). On link restore, retained `state`/`meta`/`status`
  re-sync to HA; an HA birth (if HA rebooted) re-triggers discovery.
  Every station client is on the scmino broker, so none loses the bus.
- **scmino reboot:** `mosquitto` is a systemd service with persistence
  (`autosave_interval 1800`), so retained `meta`/`state` survive and the bus
  re-seeds on boot before the bridges reconnect.
- **HA reboot:** HA republishes `homeassistant/status=online` → the bridge
  forwards it in → `hadiscovery` republishes discovery out → HA ingests. This
  is the standard HA-rebirth flow, unchanged from the single-broker setup.
- **Cold start, HA down:** `hadiscovery` publishes retained discovery configs as
  soon as it receives each slot's `/meta`; it does not wait for an HA birth. HA
  reads the retained configs when it comes up (via the
  bridge's `homeassistant/+/+/+/config out` line once the link is up), and its
  birth (`homeassistant/status=online`) triggers a full re-publish.

## Deploy and verification

See [`mqtt-broker/README.md`](../../mqtt-broker/README.md) for the seed-once
deploy, HA-side account setup, the component checklist, and the
end-to-end verification procedure (bridge loop test, canary service, outage
drill).