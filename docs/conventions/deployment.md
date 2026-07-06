# Deployment convention

All stationa services follow the same deploy pattern: cross-compile on the workstation,
copy via SCP, install as a hardened systemd service on the Raspberry Pi (`shari`).

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

---

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

## EnvironmentFile for secrets (flex2mqtt pattern)

`flex2mqtt` stores its MQTT password separately from the TOML config, using a systemd
`EnvironmentFile`:

```ini
EnvironmentFile=/etc/flex2mqtt/flex2mqtt.env
```

```bash
# /etc/flex2mqtt/flex2mqtt.env  (0600, owned by flex2mqtt user)
FLEX2MQTT_MQTT_PASSWORD=<password>
```

The application reads `FLEX2MQTT_MQTT_PASSWORD` and overrides the config value. This
lets the TOML config be less sensitive while the password stays confined to its own
`0600` file.

---

## Service management on shari

```bash
# Deploy
cd /path/to/project && ./deploy.sh

# Check logs
ssh io@192.168.1.139 'journalctl -u flex2mqtt -f'
ssh io@192.168.1.139 'journalctl -u ubctrl -f'

# Restart a service
ssh io@192.168.1.139 'sudo systemctl restart flex2mqtt'

# Check status
ssh io@192.168.1.139 'sudo systemctl status flex2mqtt'

# Edit config on device (seed-once, safe to change here)
ssh io@192.168.1.139 'sudo -e /etc/ubctrl/config.toml'
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
