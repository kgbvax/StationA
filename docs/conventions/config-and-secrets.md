# Persistent config and secret handling for systemd-deployed services

## Context

Services originally had no persistent configuration on the target machine. Every
setting — HTTP address, serial port, baud, and MQTT broker/credentials — was passed as
a **CLI flag**. `deploy.sh` read environment variables on the developer's workstation
and baked them directly into the systemd unit's `ExecStart=` line.

Two problems followed:

1. **The MQTT password leaked.** Baking `-mqtt-password <value>` into
   `ExecStart=` put the secret in `/etc/systemd/system/<service>.service` (readable
   via `systemctl cat`), in the process command line (`ps aux`, `/proc/<pid>/cmdline`),
   and anywhere journald echoed the unit.
2. **The device could not own its settings.** Each deploy regenerated the unit from
   the developer's shell environment, so there was no way to change a setting on the
   Pi and have it survive the next deploy.

This document records the convention adopted to fix both.

---

## Convention

### 1. The application owns a single config file; flags override it

A `internal/config` package defines a `Config` struct, its defaults and a loader. Two
shapes are in use:

**(a) `Default()` + `Load(path)`** (TOML via `github.com/pelletier/go-toml/v2`;
vhfcam-restream uses `github.com/BurntSushi/toml`). `main` resolves settings with the
precedence **explicit flag > config-file value > built-in default**:

```go
def := config.Default()
configPath := flag.String("config", "/etc/SERVICE/config.toml", "...")
// ...define flags with def.* as their defaults...
flag.Parse()

cfg := loadConfig(*configPath, isFlagSet("config")) // defaults <- file
applyFlagOverrides(&cfg, ...)                        // <- explicit flags only
```

"Explicit flag" is detected with `flag.Visit` (it visits only flags that were actually
set), **not** by comparing against the default value — so a flag set to the same value
as its default still deterministically wins over the file.

The flag set varies: ultrabridge and testui have one flag per main setting;
antennaselect, antmode, beamsteer and hadiscovery have `-config` plus `-broker`;
stationportal has `-config` plus `-http` (applied when non-empty, not via `flag.Visit`);
hf-mqtt-capture and vhfcam-restream have only `-config`.
stationportal and testui read the MQTT password from the EnvironmentFile of §4;
vhfcam-restream keeps it in the config (`[overlay] mqtt_password`), and
`VHFCAM_MQTT_PASSWORD` only overrides it when set. Used by ultrabridge, antennaselect, antmode, beamsteer,
hadiscovery, hf-mqtt-capture, stationportal, testui and vhfcam-restream.

**(b) `Defaults()` + `RegisterFlags(fs)` + `Load(*Flags)`** (TOML via
`github.com/BurntSushi/toml`). The only flags are `-config` and `-log.level` (plus
`-debug` or `-check` in a few), and `applyEnv` applies `<PREFIX>_MQTT_*` environment
overrides, so the precedence is **explicit flag > environment > config-file value >
built-in default**. Used by flexbridge, acom1200s-pa-bridge, atr1k-tuner-bridge,
icom9700-radio-bridge, logger-spot-bridge, oscarwatch-sattrack-bridge, powerseq,
shelly-power-bridge, spid-ercm-rotator-bridge, winpc-power-bridge and
wrc-rotator-bridge.

### 2. Missing-file semantics depend on whether `-config` was explicit

- Default path absent → run on defaults + flags (keeps `go run ./cmd/... -port ...`
  and the mock device working locally).
- `-config` set explicitly to a missing/unreadable path → fatal.
- Present-but-malformed file → fatal with the parse error.

`Load` preserves `fs.ErrNotExist` so the caller can tell "no file" from "bad file"
with `errors.Is(err, fs.ErrNotExist)`.

Not every component implements this yet: acom1200s-pa-bridge, atr1k-tuner-bridge,
flexbridge, hf-mqtt-capture, powerseq, shelly-power-bridge, stationportal and
wrc-rotator-bridge run on defaults when the file is missing even if `-config` was set
explicitly (only another read error or a parse error is fatal).

### 3. The secret never appears on the command line

Because the app reads the password from a file (the config file, or the
`EnvironmentFile` of §4), `deploy.sh` stops emitting any `-mqtt-*` flags. `ExecStart` becomes just:

```ini
ExecStart=/opt/SERVICE/SERVICE -config /etc/SERVICE/config.toml
```

The config file (and, where used, the `EnvironmentFile` that holds the password, see
§4) is installed `0600`, owned by the service user. `ConfigurationDirectory=SERVICE`
lets systemd manage `/etc/SERVICE`.

### 4. EnvironmentFile for secrets (default for most bridges)

For services where the config TOML itself is not secret but the password is, use
`EnvironmentFile` in the unit and override the password via an env var:

```ini
EnvironmentFile=/etc/SERVICE/SERVICE.env
```

```bash
# /etc/SERVICE/SERVICE.env  (0600, owned by service user)
SERVICE_MQTT_PASSWORD=s3cr3t
```

The application reads `SERVICE_MQTT_PASSWORD` and overrides the config value.
Used by acom1200s-pa-bridge, atr1k-tuner-bridge, flexbridge
(`FLEXBRIDGE_MQTT_PASSWORD`), icom9700-radio-bridge (`ICOM9700_MQTT_PASSWORD`,
`ICOM9700_CIV_PASSWORD`), logger-spot-bridge (`LOGGER_SPOT_BRIDGE_MQTT_PASSWORD`;
`LOGGER_SPOT_BRIDGE_QRZ_PASSWORD` is env-only), oscarwatch-sattrack-bridge, powerseq,
shelly-power-bridge, spid-ercm-rotator-bridge, stationportal, testui and
wrc-rotator-bridge. spid-ercm-rotator-bridge and icom9700-radio-bridge reject a
`password` key in the TOML; the others accept it as a fallback, and the env var wins
when set.

### 5. Deploy seeds the config once, then never overwrites it

The Pi becomes the source of truth. `deploy.sh` generates a config from its env vars,
transfers it to a `0600` temp path, and installs it **only if no config exists yet**; an
existing file is left untouched and the transferred copy removed:

```bash
if [ -e "$CONFIG_FILE" ]; then
  echo "config exists -- leaving it untouched (seed-once)."
else
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$SEED" "$CONFIG_FILE"
fi
```

The `EnvironmentFile` (`/etc/SERVICE/SERVICE.env`, `0600`), where used, is seeded once in
the same way. Some deploy scripts copy the shared `hf` MQTT password from an existing
station service env file on the device, so the password never leaves the Pi. This
happens only when they seed a fresh file. antennaselect, antmode, beamsteer and
hf-mqtt-capture inject it into the newly installed config when `MQTT_PASSWORD` was
empty. stationportal always seeds its `EnvironmentFile` this way, and testui does so
whenever such an env file is found.

**Exception — config migrations:** when a release needs a new config section, a
component may ship a `migrate-config.sh` that `deploy.sh` runs against the existing
file (today only powerseq). It is idempotent, backs up to
`config.toml.pre-migrate-<timestamp>`, validates the result with the new binary
(`powerseq -check`) before the service is stopped, and restores the backup and aborts
the deploy on failure. Everything else in the file stays device-owned.

### 6. Generate the seed safely

Create local/remote temp copies with `umask 077` / `install -m 600` so the secret is
never world-readable in `/tmp`, remove them on exit, and escape values written into TOML
basic strings (backslash and double-quote) so a password with special characters
round-trips.

---

## Why this matters

- **Secret hygiene:** secrets in `ExecStart`/`ps` are visible to any local user and to
  log scrapers. Moving the secret into a `0600` file owned by the service user removes
  it from every command-line and unit-file surface.
- **Operability:** seed-once means an operator can edit the config file on the device
  and redeploy the binary freely without losing settings — the device owns its
  configuration.
- **Local-dev ergonomics preserved:** flag-over-file precedence plus tolerant
  default-path handling keeps the mock/`go run` workflow intact while production runs
  entirely from the file.

---

## When to apply

- **Adding a new runtime setting** → extend `internal/config.Config`, the defaults
  (`Default()`/`Defaults()`), `config.example.toml` and the `deploy.sh` seed generator;
  optionally add a matching flag for local overrides. Deployed devices keep their
  existing file: a new optional key falls back to its default, while a new required key
  or section needs a hand edit on the device or a migration script (see §5, powerseq
  `migrate-config.sh`).
- **Introducing a new secret** → it goes in the `0600` config file (or env file), never
  as a flag or unit-file value.
- **Changing a setting on a deployed device** → edit the file on the Pi (seed-once will
  not overwrite it), or delete it and redeploy to re-seed.

---

## Before/after example

Before — secret on the command line (visible in `systemctl cat` and `ps`):

```ini
ExecStart=/opt/ultrabridge/ultrabridge -http 0.0.0.0:8080 -mqtt-broker tcp://h:1883 -mqtt-password s3cr3t
```

After — secret confined to a `0600` file the app reads:

```ini
ExecStart=/opt/ultrabridge/ultrabridge -config /etc/ultrabridge/config.toml
ConfigurationDirectory=ultrabridge
```

```toml
# /etc/ultrabridge/config.toml  (0600, owned by the service user)
http_addr = "0.0.0.0:8080"
baud      = 19200

[mqtt]
broker   = "tcp://h:1883"
password = "s3cr3t"   # never reaches ExecStart or ps
```

---

## Trade-off accepted

The password is plaintext at `0600`. Encrypted-at-rest secrets (`systemd-creds` /
`LoadCredential`, TPM-bound) are the stronger option if this threat model tightens, at
the cost of systemd ≥ 250 and more moving parts.

---

## Embedded-firmware secrets pattern

Everything above governs the systemd-deployed Go services on the station Pis (scmino,
shari). The Windows agent `winpc-power-bridge` (bwpc) keeps a seed-once `config.toml` in
its install directory, but it has no systemd `EnvironmentFile`. Its MQTT password is the
user environment variable `WINPC_POWER_BRIDGE_MQTT_PASSWORD` (set in the GUI, inherited
by the logon task), with the TOML `[mqtt] password` as fallback (see
`winpc-power-bridge/CLAUDE.md`).

The station's embedded firmware — flashed onto a device, not systemd-managed — uses a
different pattern with the same goal (the real secret never sits in a checked-in file
or on a command line):

- **Secrets live in a gitignored file next to the firmware config**, resolved at
  compile/flash time. ESPHome resolves `!secret` substitutions from a `secrets.yaml`
  sitting next to the device YAML; PlatformIO firmware includes a `secrets.h` from
  `src/` (`include/` in `m5dualkey-hf-antctrl`).
- **A checked-in example template documents the required keys** —
  `secrets.example.yaml` (ESPHome) / `secrets.example.h` or `secrets.example.h.x`
  (PlatformIO): copy it to the gitignored location, fill in real values, never commit
  the filled copy. Exception: `waveshare_relay-antswitch-bridge` has no example file;
  its keys are listed in its `CLAUDE.md`.
- **No `0600` TOML and no systemd `EnvironmentFile`** — the baked firmware carries
  the values, so rotating a secret means a reflash, not a file edit on the device.

Components using this pattern: `waveshare_relay-antswitch-bridge` (ESPHome),
`m5stamp-pol-ctrl` (ESPHome), `m5stamp-hf-ctrl` (PlatformIO),
`m5dial-hf-rotctrl` (PlatformIO — its template is `secrets.example.h.x`, so the
copy to the gitignored `src/secrets.h` is always a rename), and
`m5dualkey-hf-antctrl` (PlatformIO — template `include/secrets.example.h.x`, copied to
the gitignored `include/secrets.h`).
