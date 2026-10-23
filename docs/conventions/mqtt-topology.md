# MQTT broker topology

The station runs **two Mosquitto brokers** joined by a mosquitto `bridge`: the
station broker on **scmino** (`192.168.1.178:1883`, `mqtt-broker/`) and the Home
Assistant broker at `192.168.1.50:1883` (HA's own Mosquitto add-on, untouched).
Today the scmino broker is the station broker, bridged both ways to `.50` as a
transitional `muehle/#` mirror; per root `CLAUDE.md` (2026-10-18) most services
still connect to `.50` (see Current state below). In the target end state,
which the rest of this document describes, the station broker is authoritative for
`muehle/#`, so the station keeps a working bus when the shack↔house link is
down, and HA is a consumer that catches up when the link returns. The
original design (2026-08) put this broker on shari (`192.168.1.139`). It was
never deployed there, and the broker went live on scmino on 2026-10-01.

The broker itself lives at [`mqtt-broker/`](../../mqtt-broker/) (config, ACL,
seed-once deploy script). This document is the canonical reference for the
topology, addressing, topic directions, and ACLs.

> **Current state (2026-10-23).** The station broker runs on **scmino**
> (`192.168.1.178:1883`); shari runs no mosquitto. It is bridged to `.50` as a
> **transitional mirror**: `muehle/#` in both directions, `homeassistant/status`
> in, `homeassistant/+/+/+/config` out (see `mqtt-broker/mosquitto.conf.example`).
> Per root `CLAUDE.md` (2026-10-18) most services still connect to `.50`.
> Clients on the scmino broker: M5 Stamp PLC #1 (`hf/switch` + `hf/pa-arm`,
> since 2026-10-03) and shelly-power-bridge with its three Shelly plugs (`power/*`, since 2026-10-18). A Shelly's native
> topics are not under `muehle/#` and are not bridged, so a plug must use the
> same broker as shelly-power-bridge. All other clients use `.50` (root
> `CLAUDE.md`; per-service configs not verified live). Clients move one at a
> time by changing their broker address to `192.168.1.178:1883`; once every
> client has moved, narrow the bridge to the split mapping below. The bridge
> logs in to `.50` as `hf`; the only local account is `hf`. Never put a
> `bwbroker` alias in any `/etc/hosts` — live services resolve that name to
> `.50` (2026-09-26 incident).

## Topology

Target end state (see Current state above for what runs today):

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

**Authority (target):** `muehle/#` is primary on the station broker; HA is a
consumer via the bridge. `homeassistant/status` (HA birth) originates on `.50`
and is forwarded in. `hadiscovery` (on scmino since 2026-10-12; today connected
straight to `.50`) renders discovery **out** to HA — consistent with the
integration-model invariant "HA is the reference consumer, not a privileged
one" (§9).

## Addressing

| Client | Broker address | Why |
|--------|---------------|-----|
| Go services on scmino (target) | `tcp://127.0.0.1:1883` | co-located with the station broker; loopback. **One exception:** `icom9700-radio-bridge` uses `tcp://bwbroker:1883` — see the bwbroker note below |
| Go services on scmino (today) | `tcp://192.168.1.50:1883` | `deploy.sh` default for flexbridge, acom1200s-pa-bridge, wrc-rotator-bridge, atr1k-tuner-bridge, antennaselect, antmode, beamsteer, hadiscovery, logger-spot-bridge, oscarwatch-sattrack-bridge, testui, stationportal, vhfcam-restream, hf-console-web (flag form `192.168.1.50:1883`, no scheme). Exceptions: shelly-power-bridge `tcp://192.168.1.178:1883`; icom9700-radio-bridge `tcp://bwbroker:1883`; ultrabridge and powerseq `deploy.sh` default to `tcp://127.0.0.1:1883` (the scmino broker) but are documented on `.50`, live config not verified |
| Go services on shari (spid-ercm-rotator-bridge, hf-mqtt-capture) | not recorded in the repo | defaults are `tcp://127.0.0.1:1883` but shari runs no broker; the live seed-once config must name another broker |
| Shelly plugs, M5 Stamp PLC #1 | `192.168.1.178:1883` | on the station broker (PLC since 2026-10-03, plugs since 2026-10-18) |
| M5 DualKey, hf_console tablet (setup default), winpc-power-bridge (bwpc) | `192.168.1.50:1883` | repo default; HA broker, mirrored both ways to the station broker |
| M5 Stamp PLC #2 (pol-ctrl) | repo config `192.168.1.50`, inert until the next USB re-flash | verify on the device |
| ant-switch ESP | not settled: `waveshare_relay-antswitch-bridge/esphome/station-at1.yaml` says `192.168.1.139`, `waveshare_relay-antswitch-bridge/docs/known-issues.md` says `192.168.1.50` | verify on the device |
| M5 Dial | example secrets say `192.168.1.139` (no broker there) | verify on the device |
| Workstation (dev `go run`, `mosquitto_sub`) | `192.168.1.50:1883` (live; sees every `muehle/#` topic through the mirror) or `192.168.1.178:1883` (station broker) | shari (`.139`) runs no broker |
| HA's MQTT integration | `192.168.1.50:1883` | HA's own broker, unchanged |

`127.0.0.1` reaches the station broker only on scmino; shari runs no broker.
Repo defaults are mixed: `deploy.sh` defaults to `tcp://192.168.1.50:1883` for
most scmino services, `tcp://192.168.1.178:1883` for shelly-power-bridge,
`tcp://bwbroker:1883` for icom9700-radio-bridge, and `tcp://127.0.0.1:1883` for
ultrabridge, powerseq, spid-ercm-rotator-bridge and hf-mqtt-capture. Go
`Default()` and `config.example.toml` are mixed too: `127.0.0.1` remains in
flexbridge, acom1200s-pa-bridge, wrc-rotator-bridge, atr1k-tuner-bridge,
powerseq, shelly-power-bridge, spid-ercm-rotator-bridge and hf-mqtt-capture (and
in the antennaselect and hadiscovery examples); ultrabridge has no example
file and its `Default()` leaves the broker empty (MQTT disabled), with
`127.0.0.1` in its README/CLAUDE.md examples; others use `.50`,
`hassio.kgbvax.net` (logger-spot-bridge) or `bwbroker` (icom9700-radio-bridge).
Seed-once on-device configs own the live values.

**The `bwbroker` indirection (`icom9700-radio-bridge`).** This service (on
scmino since 2026-10-18, on shari before) is addressed through a DNS name rather
than an IP: its config default is `tcp://bwbroker:1883` (plan KTD-7), a name the
user maintains so the bridge follows whichever broker is active for bauwagen
business without a config change.
The constraint that makes it safe: **`bwbroker` may resolve only to the
`muehle/#`-authoritative (station) broker.** In the end state, replication is
split-direction (state/meta/status station→HA, `/cmd` HA→station only), so a
resolution to the HA consumer broker (`.50`) would strand the slot from
scmino-local consumers and deafen it to console cmds — a silently
half-connected slot. Deploy verifies resolution (`getent hosts bwbroker`) before
config is seeded. When it ran on shari it was the first shari-hosted service on
a non-loopback plaintext `tcp://` broker path, reviewed and accepted in
`docs/known-issues.md` (the IC-9700 exposure review, vector 7).

While the bridge mirrors `muehle/#` both ways (today), a client on either broker
sees the whole bus, so the split-direction hazard does not apply. The constraint takes effect once
the bridge is narrowed to the split mapping. The repo does not record what
`bwbroker` resolves to on scmino: `logger-spot-bridge/CLAUDE.md` records it as
NXDOMAIN on the UniFi DNS, and the Current-state note above says live services
resolve it to `.50`. Verify with `getent hosts bwbroker` on scmino.

## Bridge topic directions

The `connection bridge-to-ha` block (in `mqtt-broker/mosquitto.conf.example`)
connects out from scmino to `.50`. Today it carries `topic muehle/# both 1` (the
transitional mirror), plus `homeassistant/status in 1` and
`homeassistant/+/+/+/config out 1`; `try_private true` (default) is the loop
guard. The table below is the end-state split mapping. Its four `muehle/+/+/…` lines
are commented out in the example under END STATE, and its two `homeassistant/…`
lines are already active. It is applied once every client has moved, and its
split directions make message loops structurally impossible.

| `topic` directive                        | dir | why                                                                       |
|-----------------------------------------|-----|---------------------------------------------------------------------------|
| `topic muehle/+/+/state out 1`           | out | HA needs slot state (sensors) — retained                                  |
| `topic muehle/+/+/meta out 1`           | out | HA needs device/class metadata — retained                                 |
| `topic muehle/+/+/status out 1`         | out | HA `availability_topic` (LWT online/offline) — retained                    |
| `topic homeassistant/+/+/+/config out 1`| out | `hadiscovery`-rendered discovery objects → HA ingests                     |
| `topic homeassistant/status in 1`       | in  | HA birth triggers `hadiscovery` to re-publish discovery                   |
| `topic muehle/+/+/cmd in 1`              | in  | **required** — `hadiscovery` renders `command_topic=<addr>/cmd` for writable components; HA publishes cmds on `.50` |

In the end state, local command origins (the console tablet) publish `cmd` on
the station broker directly and are **not** bridged out — HA does not subscribe
to `/cmd`. Today the tablet publishes on `.50` and the mirror carries `/cmd`
both ways.

## Accounts and ACLs

Deployed state (per `mqtt-broker/CLAUDE.md`, not re-checked live): the scmino
`passwd` holds only `hf`. The `bridge`, `console` and `dial` ACL blocks are
seeded but have no password entries, and the bridge logs in to `.50` as `hf`,
not `stationa-bridge`. The tablet connects to `.50` today (setup default);
whether `.50` has a `console` account is not recorded in the repo. The table
below and the HA-side `stationa-bridge` account are the end-state design.

Station broker (`mqtt-broker/acl.conf.example`):

| user     | read                                                  | write                                                |
|----------|-------------------------------------------------------|------------------------------------------------------|
| `hf`     | `#` (full)                                            | `#` (full) — trusted station-internal account        |
| `bridge` | `muehle/+/+/state`, `/meta`, `/status`, `homeassistant/+/+/+/config` | `homeassistant/status`, `muehle/+/+/cmd`, `local/bridge-to-ha` |
| `console`| `muehle/+/+/state`, `/meta`, `/status`                | `muehle/+/+/cmd` (narrow)                            |
| `dial`   | `muehle/hf/rotator/state`, `/status`                  | `muehle/hf/rotator/cmd` (one slot only)              |

- **`hf`** is the shared station account: the station Go services (target: via
  `127.0.0.1`; today shelly-power-bridge on the station broker and the rest on
  `.50`), M5 Stamp PLC #1 and the **Shelly smart plugs** connect as `hf`. The
  plugs publish on their native prefixes, not under `muehle/#`: Gen1
  `shellies/<id>/…` (the TRX PSU Plug S, `shellyplug-s-A71704`) and Gen2+/Gen3
  `<shelly_id>/…` (the two Plug S Gen3, `shellyplugsg3-<mac>`). Because those
  prefixes are per-device, `hf` gets full `readwrite #`. Narrow it to `muehle/#`
  plus the concrete Shelly prefixes if the plug list is fixed.
  `hadiscovery` also runs as `hf` (needs to read `muehle/+/+/meta` +
  `homeassistant/status` and write `homeassistant/#`).
- **`bridge`** is reserved for the local side of the bridge connection in the
  end state. The example `connection bridge-to-ha` block sets no
  `local_username`, so today nothing connects as `bridge` (it has no password on
  scmino) and its ACL does not restrict the bridge. Under the transitional
  `muehle/# both` mirror the narrow `bridge` ACL would block the mirror (if
  mosquitto applies it to the bridge context), so add `local_username bridge`
  (and a `local_password` set on the device) only together with the end-state
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

In the end state, topic directions for the two slots are the standard per-slot
planes: `/state`, `/meta`, `/status` flow out over the bridge to HA (retained);
`/cmd` flows in (bridged from HA, published locally by the console) and is
**one-shot** — published non-retained and cleared with an empty retained publish
after execute-or-reject, so no stale motion intent can replay on a reconnect
(integration model §8 rules 1–3; the pol-ctrl slot next to them is the
contrast: its `set_pol` is retained self-healing steady state). Today the
transitional mirror carries all four planes both ways.

HA broker (`.50`, end state, configured in the HA Mosquitto add-on — **outside
the repo**):
a `stationa-bridge` account with read `homeassistant/status` +
`muehle/+/+/cmd` and write `muehle/+/+/state|meta|status` +
`homeassistant/+/+/+/config`.

## Operational behavior

- **Shack↔house link down (end state):** the station keeps its full local bus
  on scmino (Go services via `127.0.0.1`, remote devices via `192.168.1.178`);
  `antennaselect`, `powerseq`, and the console keep working. HA goes stale and
  **cannot command the station** until the link returns — this is the intended
  trade-off (shack autonomy). On link restore, retained `state`/`meta`/`status`
  re-sync to HA; an HA birth (if HA rebooted) re-triggers discovery. **Today**
  only M5 Stamp PLC #1, shelly-power-bridge and the Shelly plugs are on the
  scmino broker. Every client on `.50` (per root `CLAUDE.md`: flexbridge,
  antennaselect, hadiscovery, the console; powerseq and ultrabridge unverified,
  their `deploy.sh` default is the scmino broker) loses the bus while the link
  is down.
- **scmino reboot:** `mosquitto` is a systemd service with persistence
  (`autosave_interval 1800`), so retained `meta`/`state` survive and the bus
  re-seeds on boot before the bridges reconnect.
- **HA reboot:** HA republishes `homeassistant/status=online` → the bridge
  forwards it in → `hadiscovery` republishes discovery out → HA ingests. This
  is the standard HA-rebirth flow, unchanged from the single-broker setup.
  (End state. Today hadiscovery is connected to `.50` and sees the birth
  directly.)
- **Cold start, HA down:** `hadiscovery` publishes retained discovery configs as
  soon as it receives each slot's `/meta`; it does not wait for an HA birth. HA
  reads the retained configs when it comes up (in the end state, via the
  bridge's `homeassistant/+/+/+/config out` line once the link is up), and its
  birth (`homeassistant/status=online`) triggers a full re-publish.

## Deploy and verification

See [`mqtt-broker/README.md`](../../mqtt-broker/README.md) for the seed-once
deploy, HA-side account setup, the component-repointing checklist, and the
end-to-end verification procedure (bridge loop test, canary service, outage
drill).