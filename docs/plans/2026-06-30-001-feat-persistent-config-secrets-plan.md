---
title: "feat: Persistent on-target configuration with locked-down secret file"
date: 2026-06-30
type: feat
status: planned
plan_depth: standard
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
product_contract_source: ce-plan-bootstrap
---

# feat: Persistent on-target configuration with locked-down secret file

## Problem & Goal

`ubctrl` currently has **no persistent configuration on the target machine**. All
runtime settings (HTTP address, serial port, baud, MQTT broker/credentials) are
passed as **CLI flags**. `deploy.sh` reads environment variables on the
developer's workstation and bakes them directly into the systemd unit's
`ExecStart=` line (`deploy.sh:59-65`, `deploy.sh:85`).

Two concrete problems follow:

1. **The MQTT password is exposed.** It is written into
   `/etc/systemd/system/ubctrl.service` (readable via `systemctl cat ubctrl`),
   appears in the process command line (`ps aux`, `/proc/<pid>/cmdline`), and is
   logged wherever the unit's `ExecStart` is echoed.
2. **The Pi cannot own its own settings.** Every deploy regenerates the unit from
   the developer's shell env. There is no way to change a setting on the device
   itself and have it survive the next deploy.

**Goal:** Give the target machine a persistent configuration file that the
application reads directly, store the MQTT password inside that file with
restrictive (`0600`) permissions so it never appears in the unit file,
`ExecStart`, or process listing, and make `deploy.sh` seed that file once without
overwriting subsequent on-device edits.

## Decisions (confirmed with user)

| Fork | Decision | Rationale |
|------|----------|-----------|
| Config delivery | **Application reads its own config file** (not systemd `EnvironmentFile`) | Structured, typed config the app owns; no reliance on shell-env semantics |
| Secret storage | **Single locked-down file** holds non-secret config *and* the MQTT password (`0600`) | Fewer moving parts; one permission boundary to reason about |
| Deploy behavior | **Seed once, never overwrite** — Pi becomes source of truth after first deploy | Operators can edit settings on the device; binary redeploys don't clobber config |

## Scope

**In scope**
- A config-loading package in the Go app that reads a single on-disk config file.
- Precedence: explicit CLI flag > config-file value > built-in default (preserves
  the current flag-driven dev/mock workflow).
- A `-config` flag pointing at the file (default `/etc/ubctrl/config.toml`).
- `deploy.sh` changes: stop baking settings/secrets into `ExecStart`; seed the
  config file once with `0600` perms owned by the service user; leave an existing
  file untouched.
- systemd unit changes: `ExecStart` reduced to `<binary> -config <path>`; add
  `ConfigurationDirectory` so `/etc/ubctrl` is created with correct ownership.
- Doc updates (`README_API.md` / `README.md`) describing the config file.

**Out of scope**
- What the application does (control logic, web UI, MQTT entity model).
- Encrypted-at-rest secrets (`systemd-creds` / `LoadCredential`) — explicitly not
  chosen; the `0600` plaintext file is the agreed boundary.
- Config hot-reload / live re-read while running (settings apply at process
  start; a restart picks up changes).
- Multi-environment config layering beyond the single file + flags.

## Key Technical Decisions

### KTD-1: Config file format — TOML (recommended)
The file is hand-edited on the device and contains a secret, so it benefits from
comments and clear structure. Recommendation: **TOML** via
`github.com/pelletier/go-toml/v2` (well-maintained, single dependency, supports
comments). 

- **Alternative — YAML** (`gopkg.in/yaml.v3`): equally readable; heavier and
  whitespace-sensitive.
- **Alternative — JSON** (stdlib, zero deps): no comments, poor for a
  hand-edited secret-bearing file. Rejected for ergonomics.

If avoiding any new dependency is preferred over comment support, fall back to a
stdlib `KEY=value` parser — but that re-creates env-file semantics the user
explicitly steered away from. Proceed with TOML unless the reviewer objects.

### KTD-2: Precedence and "explicitly set" detection
Load order: start from defaults → overlay file values → overlay only the flags
the user explicitly passed. Detect explicit flags with `flag.Visit` (which visits
only set flags) rather than comparing against defaults, so a flag set to the same
value as a default still wins deterministically.

### KTD-3: Missing-file semantics
- If `-config` holds the **default** path and the file is **absent** → not an
  error; run on defaults + flags (preserves local `go run ... -port ...` and mock
  workflows).
- If `-config` is **explicitly** set to a path that is **absent or unreadable** →
  fatal error (operator asked for a file that isn't there).
- A present-but-malformed file → fatal error with the parse message.

### KTD-4: Secret never transits as a flag
`deploy.sh` stops emitting `-mqtt-password` (and the other settings) on
`ExecStart`. The password only ever lives inside the `0600` config file. During
seeding, the generated file is transferred and installed with restrictive perms
(see IU-3) so it does not sit world-readable in `/tmp`.

## Implementation Units

### IU-1: Config package (`internal/config`)
**Files**
- `internal/config/config.go` (new)
- `internal/config/config_test.go` (new)

**Work**
- Define a `Config` struct mirroring current flags:
  - `HTTPAddr string` (default `127.0.0.1:8080`)
  - `SerialPort string` (default empty → mock)
  - `Baud int` (default `19200`)
  - `MQTT` nested struct: `Broker`, `ClientID` (default `ubctrl`),
    `Prefix` (default `ubctrl`), `User`, `Password string`
- `func Default() Config` returning the defaults above.
- `func Load(path string) (Config, error)` — read + unmarshal (TOML) over
  `Default()`. Returns the parse error on malformed input. Caller decides whether
  a missing file is fatal (per KTD-3), so `Load` surfaces `os.IsNotExist`
  distinctly (e.g. return the wrapped error; caller checks `errors.Is(err, fs.ErrNotExist)`).
- Add `github.com/pelletier/go-toml/v2` to `go.mod`.

**Test scenarios** (`config_test.go`)
- `Default()` returns documented defaults.
- `Load` over a complete file populates every field including `MQTT.Password`.
- `Load` over a partial file keeps defaults for omitted keys.
- `Load` on a malformed file returns a non-nil parse error.
- `Load` on a missing path returns an error satisfying `errors.Is(err, fs.ErrNotExist)`.

### IU-2: Wire config into `main` (`cmd/ubctrl/main.go`)
**Files**
- `cmd/ubctrl/main.go` (edit)

**Work**
- Add `-config` flag, default `/etc/ubctrl/config.toml`.
- Keep all existing flags (they remain the override layer).
- After `flag.Parse()`:
  1. `cfg := config.Default()`
  2. Load the file per KTD-3 (default-path-missing tolerated; explicit-missing
     fatal). Track whether `-config` was explicitly set via `flag.Visit`.
  3. Overlay file values into `cfg` (already done inside `Load`).
  4. Use `flag.Visit` to overlay only explicitly-set flags onto `cfg`.
- Replace the direct `*flag` dereferences feeding the controller / serial /
  MQTT client with the resolved `cfg` fields (`main.go:19-26`, `34-48`).

**Test scenarios**
- Manual/behavioral (no unit harness around `main` today): document in PR
  description — (a) no file + flags → flags win; (b) file present, no flags →
  file values used; (c) file + conflicting flag → flag wins. Consider extracting
  the resolve step into a small testable helper in `internal/config` (e.g.
  `Resolve(base Config, set func(*Config))`) if a unit test is wanted without a
  `main` harness.

### IU-3: `deploy.sh` — seed-once config, no secrets in unit
**Files**
- `deploy.sh` (edit)

**Work**
- Remove flag assembly of runtime settings into `ARGS`/`ExecStart`
  (`deploy.sh:58-65`, `:85`). `ExecStart` becomes:
  `ExecStart=${INSTALL_DIR}/${BINARY} -config ${CONFIG_FILE}` where
  `CONFIG_FILE=/etc/ubctrl/config.toml`.
- Generate a `config.toml` locally from the existing env vars
  (`HTTP_ADDR`, `SERIAL_PORT`, `BAUD`, `MQTT_*`). Write it with `umask 077` /
  `install -m 600` so the local temp copy is not world-readable.
- `scp` the generated file to a `0600` temp path on the target
  (`/tmp/config.toml.new`, created restrictively).
- In the remote install block (`deploy.sh:114-133`): **seed once** — if
  `${CONFIG_FILE}` already exists, delete the transferred temp copy and leave the
  device file untouched; if absent, `install -o "$SERVICE_USER" -g "$SERVICE_USER"
  -m 600 /tmp/config.toml.new "${CONFIG_FILE}"`. Always remove the temp copy at
  the end.
- Ensure `/etc/ubctrl` exists with correct ownership (created by systemd via
  `ConfigurationDirectory`, see IU-4; `deploy.sh` can `mkdir -p` defensively
  before first start).

**Optional refinement**
- Before generating/transferring the secret, `ssh` a `test -f "${CONFIG_FILE}"`
  check; if present, skip generating and sending the secret entirely. Avoids
  putting the password on the wire on every redeploy. Low effort; include if
  cheap.

### IU-4: systemd unit hardening (within `deploy.sh` heredoc)
**Files**
- `deploy.sh` (edit, unit heredoc at `:77-105`)

**Work**
- Add `ConfigurationDirectory=ubctrl` so systemd creates `/etc/ubctrl` owned by
  the service user with mode `0755` (the file inside stays `0600`).
- Confirm `ProtectSystem=full` still permits reading `/etc/ubctrl` (it does;
  `full` makes `/usr`, `/boot`, `/etc` read-only but readable — the service only
  needs to *read* config). The seed/write happens out-of-band via `deploy.sh`
  with sudo, not by the service.
- Remove the now-unused settings from the generated `ExecStart`.

### IU-5: Documentation
**Files**
- `README_API.md` and/or `README.md` (edit)

**Work**
- Document the config file location, format (TOML example **without** a real
  password), the flag-over-file precedence, and the seed-once deploy behavior.
- Note explicitly that the file holds the MQTT password and must remain `0600`.

## Sequencing & Dependencies

1. **IU-1** (config package) — foundation, independently testable.
2. **IU-2** (wire into main) — depends on IU-1.
3. **IU-3 + IU-4** (`deploy.sh` + unit) — depend on the `-config` flag from IU-2
   existing; can be developed in parallel with IU-2 but must land together.
4. **IU-5** (docs) — last, reflects final shape.

## Risks & Mitigations

| Risk | Mitigation |
|------|------------|
| Secret briefly world-readable in `/tmp` during transfer | Create temp copies with `0600` from the start; `install -m 600`; remove on exit (IU-3) |
| Existing deployments break when `ExecStart` flags disappear | First deploy after this change seeds a config file from current env vars, preserving behavior; verify service status in the deploy output (already printed at `deploy.sh:132`) |
| `flag` precedence bug silently ignores file or flags | KTD-2 uses `flag.Visit` (explicit-set detection) with direct unit coverage in IU-1 for the file layer; document the manual matrix for IU-2 |
| New TOML dependency unwanted | KTD-1 records JSON-stdlib and `KEY=value` fallbacks; swap is localized to `internal/config` |
| Service user cannot read `/etc/ubctrl/config.toml` | File installed `-o $SERVICE_USER`; `ConfigurationDirectory` aligns ownership; deploy output surfaces a startup failure immediately |
| `ProtectSystem`/sandbox blocks reading config | `/etc/` stays readable under `ProtectSystem=full`; no `ReadWritePaths` needed since the service only reads |

## Verification

- `go test ./internal/config/...` passes all IU-1 scenarios.
- `go build ./...` and the cross-compile in `deploy.sh:71` succeed.
- Fresh deploy to a Pi with no prior config: file seeded `0600`, service starts,
  MQTT connects, `systemctl cat ubctrl` shows **no password** and `ps aux` shows
  only `ubctrl -config /etc/ubctrl/config.toml`.
- Edit a value on the Pi, redeploy the binary: device config **unchanged**
  (seed-once), new binary picks it up after restart.
- Run locally with `-port`/`-mqtt-broker` flags and no config file: still works on
  defaults + flags (mock path intact).

## Product Contract preservation
N/A — solo plan (`product_contract_source: ce-plan-bootstrap`); no upstream
brainstorm to preserve.
