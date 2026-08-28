# Deployment convention

All stationa services follow the same deploy pattern: cross-compile on the workstation,
copy via SCP, install as a hardened systemd service on the Raspberry Pi (`shari`).

> **Deviation — interactive components:** a component that is an *operator TUI* is not
> a service. `pelcobridge2` (UHF rotator, host `shack-pc`, Windows) deploys as a bare
> binary (`deploy.sh`: cross-compile + scp) and is started interactively by the
> operator. No systemd unit, no auto-start, no hardened unit — arming the rotator is a
> keyboard act, so a headless always-on process would contradict its safety model.
> The seed-once config and 0600 rules still apply.

---

## Build

Cross-compile for the Raspberry Pi (Linux arm64):

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/SERVICE-linux-arm64 ./cmd/SERVICE
```

- `CGO_ENABLED=0` — fully static binary; no shared library dependencies on the target.
- `-trimpath` — removes build-machine paths from the binary.
- `-ldflags="-s -w"` — strips symbol table and DWARF info; reduces binary size.

Each project's `deploy.sh` wraps this and copies the result to the Pi.

> **Monorepo + go.work (dev-time only).** Every Go component is a subdirectory of
> this repo and a member of the root `go.work` workspace, and each imports the
> shared module `codeberg.org/kgbvax/stationa/shared` (resolved by a
> `replace … => ../shared` in its own `go.mod`). `go.work` is a **development-time
> convenience** — `go build ./...` at the repo root builds them all. `deploy.sh`,
> however, runs a plain per-module `go build ./cmd/SERVICE` from inside the
> component directory, which resolves `shared/` through the `replace` **without**
> needing `go.work`. So the Pi does not need `go.work` or the sibling modules
> checked out — only the deployed binary and its `0600` config. Keep the `replace`
> in every `go.mod`; never rely on `go.work` for a deploy build (`go mod tidy`
> ignores `go.work`).

## Seed-once config

Configuration (including the MQTT password) lives in a `0600` TOML file on the device,
not in the systemd unit. `deploy.sh` seeds this file on first deploy; subsequent deploys
leave it untouched.

See `conventions/config-and-secrets.md` for the full pattern and rationale.

---

## systemd unit hardening

Every service unit includes these hardening directives:

```ini
[Service]
User=SERVICE                  # dedicated unprivileged system user
Group=SERVICE
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
ConfigurationDirectory=SERVICE   # systemd manages /etc/SERVICE
```

For services that open serial/USB-serial devices, add:

```ini
SupplementaryGroups=dialout    # service user must be in the serial group
DeviceAllow=char-ttyUSB rw
DeviceAllow=char-ttyACM rw
DeviceAllow=char-tty rw
```

The `ExecStart` line contains only the binary path and `-config` flag — no secrets.

---

## udev rule for USB-serial devices

To ensure the service user can always open FTDI USB-serial adapters regardless of
distribution defaults (some assign to `plugdev`, not `dialout`), install a udev rule:

```
SUBSYSTEM=="tty", SUBSYSTEMS=="usb", ATTRS{idVendor}=="0403", GROUP="dialout", MODE="0660"
```

Install to `/etc/udev/rules.d/99-SERVICE-serial.rules` and reload:

```bash
sudo udevadm control --reload-rules
sudo udevadm trigger --subsystem-match=tty
```

`deploy.sh` handles this automatically when `SERIAL_USB_VENDOR` is set (default: `0403`
for FTDI). Set `SERIAL_USB_VENDOR=""` to skip.

---

## EnvironmentFile for secrets (flexbridge pattern)

`flexbridge` stores its MQTT password separately from the TOML config, using a systemd
`EnvironmentFile`:

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

## Service management on shari

```bash
# Deploy
cd /path/to/project && ./deploy.sh

# Check logs
ssh io@192.168.1.139 'journalctl -u flexbridge -f'
ssh io@192.168.1.139 'journalctl -u ultrabridge -f'

# Restart a service
ssh io@192.168.1.139 'sudo systemctl restart flexbridge'

# Check status
ssh io@192.168.1.139 'sudo systemctl status flexbridge'

# Edit config on device (seed-once, safe to change here)
ssh io@192.168.1.139 'sudo -e /etc/ultrabridge/config.toml'
```

---

## Adding a new service

1. Follow the `internal/config` package pattern (see `config-and-secrets.md`).
2. Copy a `deploy.sh` from an existing service and update the service-specific
   variables at the top.
3. Write the systemd unit with the hardening directives above.
4. Add the slot address to `stationa/README.md`.
5. Create `CLAUDE.md` for the new project (use `docs/templates/mqtt-schema.md` for the
   MQTT section).
