# Deployment convention

All stationa services follow the same deploy pattern: cross-compile on the workstation,
copy via SCP, install as a hardened systemd service on a station Raspberry Pi. The
default host is in each `deploy.sh` (`SSH_HOST`; `WINPC_HOST` in `winpc-power-bridge`): `scmino` (`192.168.1.178`, Raspberry
Pi CM5) for every bridge and logic service except `spid-ercm-rotator-bridge` and
`hf-mqtt-capture`, which deploy to `shari` (`192.168.1.139`). `winpc-power-bridge` runs
on the Windows shack PC `bwpc` (see the deviation below).

> **Deviation — interactive components:** a component that is an *operator TUI* is not
> a service. It deploys as a bare binary (`deploy.sh`: cross-compile + scp) and is
> started interactively by the operator — no systemd unit, no auto-start, no hardened
> unit. The seed-once config and 0600 rules still apply. (The only such component,
> `pelcobridge2` — the UHF pan/tilt rotator TUI on `shack-pc` — was retired
> 2026-10-10, not in use; the rule stands for any future one.)

> **Deviation — Windows agent:** `winpc-power-bridge` runs on the Windows shack PC
> `bwpc` (`192.168.1.197`). `deploy.sh` cross-compiles
> `CGO_ENABLED=0 GOOS=windows GOARCH=amd64` with `-ldflags "-s -w -H=windowsgui"`,
> copies the exe over OpenSSH (scp) and registers a hidden Task Scheduler logon task.
> There is no systemd unit and no Windows service, because the agent must run in the
> interactive user session. The seed-once `config.toml` (in the install directory)
> still applies. The MQTT password comes preferably from the user environment variable
> `WINPC_POWER_BRIDGE_MQTT_PASSWORD`; the TOML `[mqtt] password` is the fallback (see
> `winpc-power-bridge/CLAUDE.md`).

---

## Build

Cross-compile for the Raspberry Pi (Linux arm64):

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/SERVICE-linux-arm64 ./cmd/SERVICE
```

- `CGO_ENABLED=0` — fully static binary; no shared library dependencies on the target.
- `-trimpath` — removes build-machine paths from the binary.
- `-ldflags="-s -w"` — strips symbol table and DWARF info; reduces binary size.

Each project's `deploy.sh` wraps this and copies the result to the Pi. The one
exception is `winpc-power-bridge`, which builds for windows/amd64, not linux/arm64
(see the Windows deviation above).

> **Monorepo + go.work.** Every Go component (except `hf_console/webbridge`, see
> below) is a subdirectory of this repo and a member of the root `go.work` workspace,
> and each imports the shared module `codeberg.org/kgbvax/stationa/shared` (resolved by
> a `replace … => ../shared` in its own `go.mod`). `go build $(go list -m -f '{{.Dir}}/...')` at the repo root
> builds them all (a plain `go build ./...` fails there: the root is not a module). `deploy.sh` runs a per-module `go build ./cmd/SERVICE` from inside
> the component directory. Inside this repo the go command finds the root `go.work`
> automatically (it searches parent directories), so a deploy build uses the workspace
> unless `GOWORK=off` is set. The `replace … => ../shared` in each `go.mod` is what
> keeps a module buildable without `go.work` (check with
> `GOWORK=off go build ./cmd/SERVICE`). Either way, the Pi does not need `go.work` or
> the sibling modules checked out — only the deployed binary and its `0600` config.
> Keep the `replace` in every `go.mod` (`go mod tidy` ignores `go.work`). Exception:
> `hf_console/webbridge` is a Go module outside `go.work` (no `shared` import);
> `hf_console/deploy.sh` builds it with `GOWORK=off`.

## Seed-once config

Configuration lives in a `0600` TOML file on the device, not in the systemd unit. The
MQTT password is either in that file (antennaselect, antmode, beamsteer, hadiscovery,
hf-mqtt-capture, ultrabridge, vhfcam-restream) or, for the other services, in a
separate `0600` EnvironmentFile `/etc/SERVICE/SERVICE.env` (see below).
`deploy.sh` seeds this file (and the EnvironmentFile, where used) on first deploy;
subsequent deploys leave it untouched. Exception: a component may ship a config
migration that edits the on-device file in place (today only
`powerseq/migrate-config.sh`: idempotent, backup first, validated with the new binary
before the service is stopped).

See [`config-and-secrets.md`](config-and-secrets.md) for the full pattern and rationale.

---

## systemd unit hardening

Service units use this hardening template:

```ini
[Service]
User=SERVICE                     # dedicated unprivileged system user
Group=SERVICE
ConfigurationDirectory=SERVICE   # systemd manages /etc/SERVICE
StateDirectory=SERVICE           # only if the service persists state (/var/lib/SERVICE)
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true              # network-only services; omit for serial services
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
RestrictRealtime=true
RestrictSUIDSGID=true
RemoveIPC=true
CapabilityBoundingSet=           # stationportal: CAP_NET_BIND_SERVICE here and in AmbientCapabilities for :80
AmbientCapabilities=
MemoryMax=256M                   # sized per service (64M-256M in use)
TasksMax=64
```

Minimum for any unit: `User`/`Group`, `NoNewPrivileges`, `ProtectSystem=full` or
stricter, `ProtectHome`, `PrivateTmp`, and `ConfigurationDirectory` when the service has
a config file (`hf-console-web` has none). Five older units (antennaselect, beamsteer,
hadiscovery, hf-mqtt-capture, ultrabridge) still use `ProtectSystem=full`.
(`DynamicUser=yes` is an accepted substitute for `User`/`Group`, as in the third-party
`rigctld-ic9700` unit.)

For services that open serial/USB-serial devices, add:

```ini
SupplementaryGroups=dialout    # service user must be in the serial group
DeviceAllow=char-ttyUSB rw
DeviceAllow=char-ttyACM rw
DeviceAllow=char-tty rw
```

The `ExecStart` line contains only the binary path and `-config` flag — no secrets.
Exceptions, both without a config file and without secrets: `hf-console-web`
(`hf_console/deploy.sh`: `-listen`, `-mqtt-broker`, `-web-root`) and the third-party
`rigctld-ic9700` unit (`icom9700-radio-bridge/rigctld-scmino/`, `DynamicUser=yes`).

---

## udev rule for USB-serial devices

To ensure the service user can always open USB-serial adapters (FTDI `0403`, Prolific
`067b`) regardless of distribution defaults (some assign to `plugdev`, not `dialout`),
install one udev rule per adapter vendor (rule as shown, with the vendor id
substituted):

```
SUBSYSTEM=="tty", SUBSYSTEMS=="usb", ATTRS{idVendor}=="0403", GROUP="dialout", MODE="0660"
```

Install to `/etc/udev/rules.d/99-SERVICE-serial.rules` and reload:

```bash
sudo udevadm control --reload-rules
sudo udevadm trigger --subsystem-match=tty
```

`deploy.sh` handles this automatically. The vendor default is per component:
`SERIAL_USB_VENDOR=0403` in ultrabridge (its rule also sets
`ENV{ID_MM_DEVICE_IGNORE}="1"`) and `067b` in acom1200s-pa-bridge.
spid-ercm-rotator-bridge takes a space-separated `SERIAL_USB_VENDORS` list (default
`0403`) and writes one rule per vendor. The scripts say an empty value skips the rule,
but `${VAR:-default}` puts the default back for an empty value, so today the rule cannot
be skipped without editing `deploy.sh` (it would need `${VAR-default}`).

---

## EnvironmentFile for secrets (default for most bridges)

Most services (acom1200s-pa-bridge, atr1k-tuner-bridge, flexbridge,
icom9700-radio-bridge, logger-spot-bridge, oscarwatch-sattrack-bridge, powerseq,
shelly-power-bridge, spid-ercm-rotator-bridge, stationportal, testui,
wrc-rotator-bridge) keep the MQTT password out of the TOML, in a systemd
`EnvironmentFile` `/etc/SERVICE/SERVICE.env` (`0600`, seeded once).
spid-ercm-rotator-bridge and icom9700-radio-bridge reject a `password` key in the TOML.
`flexbridge` example:

```ini
EnvironmentFile=/etc/flexbridge/flexbridge.env
```

```bash
# /etc/flexbridge/flexbridge.env  (0600, owned by flexbridge user)
FLEXBRIDGE_MQTT_PASSWORD=<password>
```

The application reads `FLEXBRIDGE_MQTT_PASSWORD` and overrides the config value. This
lets the TOML config be less sensitive while the password stays confined to its own
`0600` file.

---

## Service management on the Pis (scmino, shari)

```bash
# Deploy
cd /path/to/project && ./deploy.sh

# Check logs
ssh io@192.168.1.178 'journalctl -u flexbridge -f'
ssh io@192.168.1.178 'journalctl -u ultrabridge -f'
ssh io@192.168.1.139 'journalctl -u spid-ercm-rotator-bridge -f'

# Restart a service
ssh io@192.168.1.178 'sudo systemctl restart flexbridge'

# Check status
ssh io@192.168.1.178 'sudo systemctl status flexbridge'

# Edit config on device (seed-once, safe to change here)
ssh io@192.168.1.178 'sudo -e /etc/ultrabridge/config.toml'
```

---

## Adding a new service

1. Create the module (`go.mod` with
   `replace codeberg.org/kgbvax/stationa/shared => ../shared`) and add it to `go.work`.
2. Follow the `internal/config` package pattern (see `config-and-secrets.md`) and ship
   a `config.example.toml`.
3. Copy a `deploy.sh` from a service that already uses the full template (e.g.
   `wrc-rotator-bridge` for a network-only service, `spid-ercm-rotator-bridge` for a
   serial one, not one of the five `ProtectSystem=full` units) and update the
   service-specific variables at the top; its generated unit then carries the hardening
   directives above.
4. Add the component to the table and bus diagram in the root `README.md` and its slot
   to the slot table in the root `CLAUDE.md`.
5. Add the component's `[[software]]` entry and its `[[slot]]` to
   `stationportal/internal/inventory/inventory.toml`, plus a `[[link]]` if it serves a
   web UI and a `[[host]]` only for a new host (and, for a bridge shown in the station
   schematic, `hf_console/lib/store/station_hosts.dart`), then redeploy stationportal.
6. Create `CLAUDE.md` for the new project and
   `<project>/docs/<component>-mqtt-api.md` from `docs/templates/mqtt-schema.md`.
