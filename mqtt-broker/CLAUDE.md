# CLAUDE.md — mqtt-broker

The **shack-local Mosquitto broker**, running on
[shari](../CLAUDE.md) (`192.168.1.139`) and bridged to the Home Assistant broker
at `192.168.1.50:1883`. It exists so the station keeps a working `muehle/#` bus
even when the shack↔house link is down; HA is a consumer that catches up when the
link returns.

This is **not a Go component** — no `go.mod`, not in `go.work`. It is plain
config + a deploy script, like the ESPHome/PlatformIO projects:

| File | Purpose |
|------|---------|
| `mosquitto.conf.example` | listener, persistence, password/acl files, the `connection bridge-to-ha` block with split topic directions |
| `acl.conf.example` | `hf` / `bridge` / `console` / `dial` user ACLs |
| `deploy.sh` | seed-once install to shari (apt, config, password db, systemd) |
| `README.md` | full topology, topic-direction table, ACLs, HA-side setup, operational behavior, verification |

Read [`README.md`](README.md) first — it is the reference. The full broker
topology is also documented in
[`../docs/conventions/mqtt-topology.md`](../docs/conventions/mqtt-topology.md).

## Deploy

```bash
HF_MQTT_PASSWORD=... BRIDGE_MQTT_PASSWORD=... CONSOLE_MQTT_PASSWORD=... DIAL_MQTT_PASSWORD=... ./deploy.sh
```

Then set `remote_password` under `connection bridge-to-ha` in
`/etc/mosquitto/mosquitto.conf` on shari and `sudo systemctl restart mosquitto`.
See README.md "HA-side setup" for the matching `stationa-bridge` account on the
HA Mosquitto add-on.

## Conventions

Config/secrets follow [`../docs/conventions/config-and-secrets.md`](../docs/conventions/config-and-secrets.md):
0600 files, seed-once, secrets never in the repo. Deployment follows
[`../docs/conventions/deployment.md`](../docs/conventions/deployment.md). The
`mosquitto` unit from apt is already hardened by upstream; the broker needs
outbound TCP to `.50:1883` (the bridge) and inbound `1883` on the shack LAN.