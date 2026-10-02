# CLAUDE.md — stationportal

The station **landing page** on scmino, port 80 (`http://scmino/`). One page with:

- **Services** — links to every key UI/endpoint (hf_console web, VHF cam preview,
  testui, HamClock, ultrabridge, device web UIs, Home Assistant, UniFi, the two
  MQTT brokers, rotctld), each **probed server-side** every 30 s (HTTP GET: any
  status = up, so a 401 from a digest-protected ESPHome UI is up; mqtt/tcp/ws =
  TCP connect; `probe = "none"` for external links).
- **Slots & hardware** — every bus address: inventory description + the live
  `/meta` facts (manufacturer, model, serial, firmware) + **two-layer
  liveness** (bridge `/status` LWT AND `/state.device_online`, per the station
  model). Addresses seen on the bus but missing from the inventory still show.
- **Software** — repo components and external programs with host, unit,
  ports; liveness from their slots, else from a named link probe.
- **Hosts** and **passive resources** (antennas, camera).

It is **not a slot**: a passive consumer of `muehle/#` on the live broker
(`.50`, not the scmino mirror) — no publishes, no LWT. Server-rendered HTML,
no JavaScript; `<meta refresh>` every probe interval. `/api/inventory` returns
the same data as JSON, `/healthz` answers `ok`.

## Inventory

`internal/inventory/inventory.toml` is **built into the binary** (go:embed):
versioned with the repo, a redeploy updates it. Decoding is **strict** — a key
in the wrong table fails `go test` (`TestBuiltInInventoryIsValid`), as does a
slot whose component is missing from `[[software]]`. When a service moves host,
gains a port or a device is added, edit this file. A device-side override is
possible via config `inventory = "…"` (replaces it wholesale).

## Commands

```bash
go test -race ./... && go vet ./...
./deploy.sh        # tests, cross-compile, install on scmino (seed-once config + env)
```

The unit gets exactly `CAP_NET_BIND_SERVICE` (port 80 as an unprivileged
user) under the usual hardening. The `hf` password is pulled on-device from
`/etc/testui/testui.env` (or another hf service env) into
`/etc/stationportal/stationportal.env` — it never leaves the host.

## Conventions

Logging per `../docs/conventions/logging.md`; config/secrets per
`../docs/conventions/config-and-secrets.md`; paho handlers only Enqueue
(`shared/mqtt`), connect is ctx-aware with startup backoff — the page keeps
serving the static inventory while the broker is down.
