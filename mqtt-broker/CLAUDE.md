# CLAUDE.md — mqtt-broker

The **station Mosquitto broker**, running on **scmino** (`192.168.1.178`,
since 2026-10-01; originally planned for shari) and bridged to the Home
Assistant broker at `192.168.1.50:1883`.

**Current state: transitional mirror, one client.** `.50` is still the live
station broker. First client moved 2026-10-03: the M5 Stamp PLC #1
(`m5stamp-hf-ctrl`, `hf/switch` + `hf/pa-arm`; `MQTT_HOST` in its gitignored
`src/secrets.h`) — it reads `hf/radio/state` and `hf/ant-switch/state` through
the bridge. The bridge mirrors `muehle/#` both ways (login `hf` on `.50`),
so clients can move here one at a time; narrow it to the split mapping in
`mosquitto.conf.example` once all have moved. Local accounts: `hf` only.
Never add a `bwbroker` hosts alias anywhere — live services resolve it to
`.50` (2026-09-26 incident). It exists so the station keeps a working `muehle/#` bus
even when the shack↔house link is down; HA is a consumer that catches up when the
link returns.

This is **not a Go component** — no `go.mod`, not in `go.work`. It is plain
config + a deploy script, like the ESPHome/PlatformIO projects:

| File | Purpose |
|------|---------|
| `mosquitto.conf.example` | listener, persistence, password/acl files, the `connection bridge-to-ha` block with split topic directions |
| `acl.conf.example` | `hf` / `bridge` / `console` / `dial` user ACLs |
| `deploy.sh` | seed-once install to scmino (apt, config, password db, systemd) |
| `README.md` | full topology, topic-direction table, ACLs, HA-side setup, operational behavior, verification |

Read [`README.md`](README.md) first — it is the reference. The full broker
topology is also documented in
[`../docs/conventions/mqtt-topology.md`](../docs/conventions/mqtt-topology.md).

## Deploy

```bash
HF_MQTT_PASSWORD=... BRIDGE_MQTT_PASSWORD=... CONSOLE_MQTT_PASSWORD=... DIAL_MQTT_PASSWORD=... ./deploy.sh
```

Then set `remote_password` under `connection bridge-to-ha` in
`/etc/mosquitto/mosquitto.conf` on scmino and `sudo systemctl restart mosquitto`.
(The 2026-10-01 deploy ran with `HA_REMOTE_USER=hf` and no password env vars;
the `hf` password was then seeded on scmino from the local vhfcam-restream
config into `passwd` (via `mosquitto_passwd -U`) and `remote_password`.)
See README.md "HA-side setup" for the matching `stationa-bridge` account on the
HA Mosquitto add-on.

## Conventions

Config/secrets follow [`../docs/conventions/config-and-secrets.md`](../docs/conventions/config-and-secrets.md):
0600 files, seed-once, secrets never in the repo. Deployment follows
[`../docs/conventions/deployment.md`](../docs/conventions/deployment.md). The
`mosquitto` unit from apt is already hardened by upstream; the broker needs
outbound TCP to `.50:1883` (the bridge) and inbound `1883` on the shack LAN.