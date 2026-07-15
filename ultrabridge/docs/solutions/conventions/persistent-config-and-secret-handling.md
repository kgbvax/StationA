---
title: "Persistent config and secret handling for systemd-deployed services"
date: 2026-06-30
category: conventions
module: Configuration & deployment
problem_type: convention
component: tooling
severity: medium
applies_when:
  - "Adding or changing a runtime setting for ubctrl"
  - "Handling a secret (MQTT password, token) for a systemd-deployed service"
  - "Deploying the Go binary to the Raspberry Pi via deploy.sh"
tags: [configuration, secrets, systemd, deployment, toml, go]
---

# Persistent config and secret handling for systemd-deployed services

## Context

`ubctrl` originally had no persistent configuration on the target machine. Every
setting — HTTP address, serial port, baud, and the MQTT broker/credentials — was
passed as a **CLI flag**. `deploy.sh` read environment variables on the
developer's workstation and baked them directly into the systemd unit's
`ExecStart=` line.

Two problems followed:

1. **The MQTT password leaked.** Baking `-mqtt-password <value>` into
   `ExecStart=` put the secret in `/etc/systemd/system/ubctrl.service` (readable
   via `systemctl cat ubctrl`), in the process command line (`ps aux`,
   `/proc/<pid>/cmdline`), and anywhere journald echoed the unit.
2. **The device could not own its settings.** Each deploy regenerated the unit
   from the developer's shell environment, so there was no way to change a
   setting on the Pi and have it survive the next deploy.

This learning records the convention adopted to fix both: the application reads
its own config file, the secret lives only inside that `0600` file, and deploy
seeds the file once.

## Guidance

**1. The application owns a single config file; flags override it.**

A `internal/config` package defines a `Config` struct, `Default()`, and
`Load(path)` (TOML via `github.com/pelletier/go-toml/v2`). `main` resolves
settings with the precedence **explicit flag > config-file value > built-in
default**:

```go
def := config.Default()
configPath := flag.String("config", "/etc/ubctrl/config.toml", "...")
// ...define flags with def.* as their defaults...
flag.Parse()

cfg := loadConfig(*configPath, isFlagSet("config")) // defaults <- file
applyFlagOverrides(&cfg, ...)                        // <- explicit flags only
```

"Explicit flag" is detected with `flag.Visit` (it visits only flags that were
actually set), **not** by comparing against the default value — so a flag set to
the same value as its default still deterministically wins over the file.

**2. Missing-file semantics depend on whether `-config` was explicit.**

- Default path absent → run on defaults + flags (keeps `go run ./cmd/ubctrl
  -port ...` and the mock device working locally).
- `-config` set explicitly to a missing/unreadable path → fatal.
- Present-but-malformed file → fatal with the parse error.

`Load` preserves `fs.ErrNotExist` so the caller can tell "no file" from "bad
file" with `errors.Is(err, fs.ErrNotExist)`.

**3. The secret never appears on the command line.**

Because the app reads the password from the file, `deploy.sh` stops emitting any
`-mqtt-*` flags. `ExecStart` becomes just:

```ini
ExecStart=/opt/ubctrl/ubctrl -config /etc/ubctrl/config.toml
```

The config file (which contains the password in plaintext) is installed `0600`,
owned by the service user. `ConfigurationDirectory=ubctrl` lets systemd manage
`/etc/ubctrl`.

**4. Deploy seeds the config once, then never overwrites it.**

The Pi becomes the source of truth. `deploy.sh` generates a config from its env
vars, transfers it to a `0600` temp path, and installs it **only if no config
exists yet**; an existing file is left untouched and the transferred copy
removed:

```bash
if [ -e "$CONFIG_FILE" ]; then
  echo "config exists -- leaving it untouched (seed-once)."
else
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$SEED" "$CONFIG_FILE"
fi
```

**5. Generate the seed safely.** Create local/remote temp copies with
`umask 077` / `install -m 600` so the secret is never world-readable in `/tmp`,
remove them on exit, and escape values written into TOML basic strings
(backslash and double-quote) so a password with special characters round-trips.

## Why This Matters

- **Secret hygiene:** secrets in `ExecStart`/`ps` are visible to any local user
  and to log scrapers. Moving the secret into a `0600` file owned by the service
  user removes it from every command-line and unit-file surface.
- **Operability:** seed-once means an operator can edit `/etc/ubctrl/config.toml`
  on the device and redeploy the binary freely without losing settings — the
  device owns its configuration.
- **Local-dev ergonomics preserved:** flag-over-file precedence plus
  tolerant default-path handling keeps the mock/`go run` workflow intact while
  production runs entirely from the file.

This is the project's standard for any future setting or secret: add it to the
`Config` struct and the seed generator, never to `ExecStart`.

## When to Apply

- Adding a new runtime setting → extend `internal/config.Config`, `Default()`,
  and the `deploy.sh` seed generator; optionally add a matching flag for local
  overrides.
- Introducing a new secret → it goes in the `0600` config file, never as a flag
  or unit-file value.
- Changing a setting on a deployed device → edit the file on the Pi (seed-once
  will not overwrite it), or delete it and redeploy to re-seed.

## Examples

Before — secret on the command line (visible in `systemctl cat` and `ps`):

```ini
ExecStart=/opt/ubctrl/ubctrl -http 0.0.0.0:8080 -mqtt-broker tcp://h:1883 -mqtt-password s3cr3t
```

After — secret confined to a `0600` file the app reads:

```ini
ExecStart=/opt/ubctrl/ubctrl -config /etc/ubctrl/config.toml
ConfigurationDirectory=ubctrl
```

```toml
# /etc/ubctrl/config.toml  (0600, owned by the service user)
http_addr = "0.0.0.0:8080"
baud      = 19200

[mqtt]
broker   = "tcp://h:1883"
password  = "s3cr3t"   # never reaches ExecStart or ps
```

## Related

- Plan: `docs/plans/2026-06-30-001-feat-persistent-config-secrets-plan.md`
- Trade-off accepted: the password is plaintext at `0600`. Encrypted-at-rest
  secrets (`systemd-creds` / `LoadCredential`, TPM-bound) are the stronger
  option if this threat model tightens, at the cost of systemd ≥ 250 and more
  moving parts.
- Side fix during this work: a duplicate `package mqtt` line in
  `internal/mqtt/client_test.go` was breaking `go vet`/`go test ./...`; removing
  the duplicate restored the suite.
