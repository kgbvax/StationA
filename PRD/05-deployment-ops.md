# 05 — Deployment and Operations Specification

This document specifies how we deploy, configure, and operate the Mühle station-automation system. It addresses an engineering team that will **re-construct the whole system from scratch on a different technology stack**. It covers the host and infrastructure inventory (§1). It covers the conventions that every deployed service must follow (§2). It covers the per-component deployment deviations (§3). It covers the operator/bench tooling that supports day-to-day operations (§4). It covers the checks and failure-runbook expectations (§5). It covers the open decisions that the sources leave unresolved (§6).

Behavior contracts here are **stack-agnostic and normative**. Concrete technology names (Go, systemd, ssh, adb, ESPHome, Flutter, Mosquitto) appear only in clearly-marked *Reference-implementation notes*. These notes are non-normative background that describes the current deployment. One exception exists: `adb` names the Android platform's own install mechanism, so it appears bare where the tablet channel requires it.

Background for the non-radio reader: **amateur radio (ham radio)** is the licensed hobby of two-way radio communication. The "Mühle" station (site prefix `muehle`) is one such installation. It has an HF (high-frequency, 1.8–54 MHz) and a UHF (144–440 MHz) station. A **bridge** is a small service that fronts one physical device (a transceiver, a power amplifier, a relay board) and mirrors it onto a shared **MQTT** bus. MQTT is a lightweight publish/subscribe protocol. Clients exchange messages on hierarchical text topics through a central **broker**. A **slot** is one component's topic namespace, for example `muehle/hf/radio`. A **retained** message is one that the broker stores and re-delivers to every future subscriber until someone overwrites or clears it. **LWT (Last Will and Testament)** is a broker feature: the broker publishes a pre-registered message on a client's behalf when that client disconnects uncleanly. The station uses it for liveness (`<slot>/status` = `online`/`offline`). The full bus contract lives in `02-interface-spec.md`. This document covers only deployment and operations.

---

## 1. Hosts and infrastructure inventory

### 1.1 shari — the service host

**shari** is the station's single Linux compute host: a Raspberry Pi (64-bit ARM, "arm64" architecture) at `192.168.1.139`, login user `io` (admin operations through that user with privilege escalation). All non-interactive services run on shari. It is a **known single point of failure** (see `docs` research, defect list). If shari disappears, every shari-hosted bridge's MQTT presence goes offline at the same time. That is *correct* behavior (each bridge's LWT fires). The station then degrades to manual operation. This is acceptable only because safety interlocks are hardware-based (see `06-safety.md`).

Services deployed on shari (each an independent, separately-restartable unit):

| Service | Slots it fronts | Notes |
|---|---|---|
| flexbridge | `muehle/hf/radio` | FLEX-8400 transceiver, over the network. |
| ultrabridge | `muehle/hf/ant-ctrl` | Ultrabeam RCU-06 antenna controller, USB-serial. |
| acom1200s-pa-bridge | `muehle/hf/pa` | ACOM 1200S power amplifier, serial (telemetry only). |
| wrc-rotator-bridge | `muehle/hf/rotator` | Yaesu G-450DC rotator through an AF6SA "WRC" controller, over a websocket. |
| atr1k-tuner-bridge | `muehle/hf/tuner` | ATR-1000 antenna tuner, wifi binary WebSocket. |
| shelly-power-bridge | `muehle/power/master`, `muehle/power/psu-13v8` | one process, two slots (smart mains plugs). |
| antennaselect | `muehle/hf/antenna-select` | logic slot, no device. |
| powerseq | `muehle/hf/power-seq` | logic slot, no device (startup/shutdown sequencer). |
| hadiscovery | `muehle/hf/discovery` | logic slot, no device (Home Assistant discovery renderer. Home Assistant is an open-source home-automation platform. The station runs it as a bus consumer). |
| hf-mqtt-capture | — (no slot) | passive bus recorder (§4.2). |
| testui | — (no slot) | web bus display/stimulator (§4.3). |
| hf-console-web | — (no slot) | serves the console's web build on port 8091 (§3). |

Requirements:

- **MUST**: every service in the table above must run on the single service host. Each service must run under its own supervisor-managed unit with a dedicated unprivileged system user. Each service must start and restart independently.
- **MUST**: the host name `shari` and its address `192.168.1.139` must be deployment configuration, not code. Only the *role vocabulary* is code.
- **MUST**: operations must treat host liveness as load-bearing. The bus model reserves `muehle/host/shari` (fields `online`, `temp_c`, `load`) and `muehle/host/shack-pc` (field `online`) as host-liveness nodes. **No component in the current repo publishes them** — they are model-only. A reconstruction that wants host checks must decide to add them (see §6).

### 1.2 shack-pc — the interactive host

**shack-pc** is a Windows PC in the radio shack at `192.168.1.197` (remote-copy user `iotte`). It hosts exactly one component: the **UHF rotator console** (`muehle/uhf/rotator`). That console is an *interactive terminal application that a human operator starts by hand* — deliberately not a background service. Its safety model needs a human at the keyboard to arm remote motion (see `03-components/pelcobridge2.md` and §3 of this document). Install target: `C:/Users/iotte/pelcobridge2/`.

### 1.3 The operator tablet

The station's primary operator console is a **fixed-mount Android tablet**. The deploy workstation connects to it over **USB**. It is not a service host. Sideloading installs the console application as a self-sideloaded app (no app store). It is a pure MQTT client plus one HTTP event feed. It has **no MQTT presence of its own** (no slot, no `/meta`, no `/state`, no LWT, no heartbeat). It publishes only to `muehle/<slot>/cmd` topics. Reinstallation with `adb install -r` (install-over, keep data) is the supported update path *precisely because it preserves the broker credentials stored on the device*. See `04-console.md` for the console's full specification. Deployment details are in §3.

### 1.4 Embedded devices

These are devices whose firmware speaks the canonical MQTT schema directly over wifi — device, adapter, and host collapse into one node:

- **1:6 antenna switch** (`muehle/hf/ant-switch`) — relay board, wifi, firmware-configured (ESPHome YAML in the reference implementation).
- **M5 Stamp PLC #1** (`muehle/hf/switch` + `muehle/hf/pa-arm`) — two slots from one firmware. A **PLC** is a programmable logic controller: here a relay/microcontroller board. Relays 3/4 drive the PA/transceiver remote-on lines. Relay 1 is the fail-safe PA arm relay.
- **M5 Stamp PLC #2** (`muehle/uhf/pol-ctrl`, X-Quad antenna polarization relays) — **documented gap: no PLC #2 firmware exists in the repo**. Treat the slot as unimplemented, not as a component to deploy.
- **Shelly smart plugs** (`muehle/power/master`, `muehle/power/psu-13v8`) — commercial smart plugs that the shari-hosted shelly-power-bridge fronts. The plugs themselves are stock firmware.

A workstation flashes the firmware for the two custom embedded families over **USB** (the antenna-switch firmware family additionally supports over-the-network firmware updates once it is on wifi — its standard workflow). Firmware deployment is otherwise out of scope of the service conventions in §2.

### 1.5 The MQTT broker — current and planned topology

- **Current production broker**: an MQTT broker at `tcp://192.168.1.50:1883` (reference product: Mosquitto), MQTT username `hf`, **persistent store enabled** (retained `meta`/`state` messages survive a broker restart). All deployed components point here.
- **Planned migration (open decision, §6)**: an unmerged, **undeployed** feature branch (commit `cd58466`, dated 2026-08-26) contains the work. It runs a second, shack-local broker on shari at `192.168.1.139:1883`. That broker is authoritative for `muehle/#` and bridges to the untouched broker at `192.168.1.50:1883`. The station then keeps its bus when the shack-to-house network link drops. That branch adds a `mqtt-broker/` component (broker configuration, access-control list, seed-once deploy). It also repoints the console's default broker host and the web proxy's default broker flag to `.139`.
- A PRD based on this text **MUST treat `192.168.1.50:1883` as the current production broker**. It must record the shack-local broker as a decision point, not as deployed behavior. One live documentation conflict exists: the console project's local instructions already *document* the shack broker at `192.168.1.139:1883`, but the deployed code still defaults to `192.168.1.50:1883`. Code wins for current behavior.
- **Broker accounts (contract for any reconstruction)**: the bus uses role-scoped MQTT accounts. The `hf` account serves the deployed services. It has full bus access. The console uses a dedicated `console` account (the console's default user name). Its intended access-control list permits subscribe `muehle/#` and publish only `muehle/+/cmd` — least privilege for a read-mostly operator surface. The planned `mqtt-broker/` component ships this access-control list as seed-once configuration. **MUST**: a reconstruction's broker must enforce per-role access control at least this narrow. The console must never hold the service account.
- **MUST (both variants)**: broker credentials must never appear on any command line, in any process list, or in any unit-file `ExecStart` (§2.2).
- **MUST**: the broker must retain `meta`/`state`/`status` messages across broker restarts (persistent store). A reconstruction on a different broker product must preserve retention and last-will semantics.

### 1.6 Workstations (build machines)

Developer workstations cross-compile every artifact and push it to the target host. Nobody builds a component on its target host. The bench tooling (§4.4) also runs on a workstation, attached to hardware over USB-serial adapters.

---

## 2. Service conventions (normative)

These apply to every non-interactive service (all services in §1.1). §3 lists the deviations.

### 2.1 Configuration file and precedence

Every service takes its configuration from **one file**, default path `/etc/<SERVICE>/config.toml`. The service's dedicated user owns the file, and the file mode is **0600**. It can hold the MQTT password in plaintext — an accepted trade-off. §6 records the stronger option of a supervisor-provided credential store as future hardening. The file format is TOML in the reference implementation. **The contract is the path, ownership, permissions, and the precedence rules below**, not the syntax.

Precedence, highest first — this exact ladder is a **behavior contract**:

1. **Explicit command-line flag** — *whether the operator actually passed the flag* decides "explicit", not a comparison against the default value. A flag explicitly set to its default value still wins over the file.
2. **Config-file value.**
3. **Built-in default.**

Missing-file semantics (**contract**):

- Default config path absent → the service runs on built-in defaults + flags (this keeps local mock/bench workflows working).
- Config path **explicitly named** but missing or unreadable → **fatal at startup**. A mistyped path must never quietly run on defaults.
- File present but malformed → **fatal**, with the parse error printed.

Environment-variable override: each service also accepts configuration through environment variables prefixed with its own name (uppercased, hyphens→underscores), for example `FLEXBRIDGE_MQTT_PASSWORD`, `POWERSEQ_MQTT_PASSWORD`, `TESTUI_MQTT_PASSWORD`, `ATR1K_TUNER_BRIDGE_MQTT_PASSWORD`, `PELCOBRIDGE2_MQTT_PASSWORD`. This is the sanctioned secrets channel (§2.2).

Common configuration keys every service shares (`[mqtt]` section): `broker`, `client_id` (default derived from the slot address as `<site>-<station>-<slot>` with `/`→`-`, for example `muehle-hf-radio`), `site`, `station`, `slot`, `user`, `password`. The service publishes the identity keys `location` (building label) and `host` (compute-node name) in `/meta`. They **come from configuration, never code** (see `02-interface-spec.md` §2).

Two bridges (the radio bridge and the antenna-controller bridge) also carry a **legacy embedded Home Assistant discovery** path. Two keys gate it: `discovery_prefix` (default `homeassistant`) and `publish_ha_discovery` (default **false**). The gate is off everywhere. The team will delete the legacy embedded discovery once the separate discovery consumer (`hadiscovery`, §1.1) has proven itself. A reconstruction must not re-implement it.

### 2.2 Secrets handling

- **MUST**: a secret must never appear on a command line, in a unit `ExecStart`, or in a process listing. It must never appear in a repository. Two sanctioned patterns exist:
  1. **Password directly in the 0600 config file** (the antenna-controller bridge uses this pattern, for example).
  2. **EnvironmentFile** (the "flexbridge pattern"): the supervisor loads a second 0600 file, for example `EnvironmentFile=/etc/flexbridge/flexbridge.env` containing `FLEXBRIDGE_MQTT_PASSWORD=<pw>`, and the application reads the environment variable, which overrides the config-file value. flexbridge, powerseq, hadiscovery, atr1k-tuner-bridge, and the web test tool use this pattern on the target (its *checked-in* bench config is a defect — §4.3).
- **MUST**: the deployed config files on the target host are the only authoritative copy of secrets for that host. Deploy tooling must not overwrite them (§2.3) and must not need the secret to pass through the workstation for routine redeploys.
- **MUST**: the passive recorder (§4.2) must get its password **on the target device**. Without this rule, its seeded config needs the password pre-filled on the workstation. If the seeded password is empty, its deploy script reads the shared MQTT password on-device. It reads it from the first readable `<SERVICE>_MQTT_PASSWORD=` line of the existing bridge environment files (search order: the PA bridge's, flexbridge's, hadiscovery's, the tuner bridge's `.env` files). It then injects that password into the recorder's config with an on-device script.

### 2.3 Seed-once deployment

The deploy flow for every service follows **seed-once** semantics (**contract**):

1. The deploy script builds the artifact on the workstation (§2.5). It transfers it to the target and installs it (reference layout: binary at `/opt/<SERVICE>/<SERVICE>`).
2. It generates a config file (from the developer's environment / an example file) with restrictive creation (`umask 077` / matching mechanism, target mode 0600). It installs this file **only if no config file exists yet**. It leaves an existing file byte-for-byte untouched ("config exists — leaving it untouched (seed-once)"). It removes the staged copy after the comparison.
3. Consequence (intended): **the device owns its settings.** An operator can edit the config on the host and redeploy binaries freely without losing changes. To re-seed: delete the file on the host, then redeploy.
4. The seed generator must escape backslash and double-quote. Passwords with special characters then round-trip through the generated file.

### 2.4 Supervisor hardening

Requirements for whatever service supervisor the new stack uses — each statement is testable by inspecting the unit definition:

- **MUST**: each service must run under a **dedicated unprivileged system user/group** named after the service. The setup needs no login shell and no home directory. Never run a service as root. Never share the account between services.
- **MUST**: each unit must set the same of `NoNewPrivileges=true` (the process and its children get no privilege escalation). It must set `ProtectSystem` — the OS filesystem becomes read-only to the service (`full` normally, `strict` where the service needs no writes at all). It must set `ProtectHome=true` and `PrivateTmp=true` (isolated `/tmp`). It must set a managed configuration directory (systemd's `ConfigurationDirectory=<SERVICE>`, which makes `/etc/<SERVICE>` read-only-but-present to the service).
- **MUST**: `ExecStart` must hold only the binary and the `-config <path>` argument — **never any secret or other environment**.
- **MUST**: the unit must declare dependencies such that the service starts after network availability (the `network-online.target` matching mechanism). It must also restart on failure with a 5 s delay (`Restart=on-failure`, `RestartSec=5`).
- **MUST** (serial services only): the unit must grant exactly the device access needed — a supplementary group for the serial devices (reference: `dialout`). It must add device-allow rules for the serial device classes (`char-ttyUSB`, `char-ttyACM`, `char-tty`, read-write) rather than blanket device access.
- **MUST** (serial services only): the deploy script must install a udev rule (or a matching device-ownership mechanism). The rule must assign the USB-serial adapter (default USB vendor id `0403`, the FTDI family) to the serial group with mode 0660, because distribution defaults vary. Reload and re-trigger the device rules after install.
- **MUST** (services with a writable path, for example the log recorder): the unit must list the writable path explicitly (`ReadWritePaths=/var/log/hf-mqtt-capture`) and nothing else writable.
- **MUST** (web-facing services — the test tool and the console web proxy): the unit must use the strongest sandbox set. This set has `ProtectSystem=strict`, `PrivateDevices`, kernel-tunables/modules/cgroup protections, `RestrictAddressFamilies=AF_INET AF_INET6`, namespace/realtimes/SUID restrictions, `RemoveIPC`, an **empty capability bounding set**, and **resource ceilings**. Ceilings: the web test tool gets memory 128 MiB and tasks 64. The console web proxy gets memory 64 MiB and tasks 32 — the in-memory bus tree must not be able to OOM the shared host. Only the web test tool needs a managed state directory (reference: `StateDirectory=testui` with `ReadWritePaths=/var/lib/testui`). The console web proxy needs no state directory and no writable path at all.

*Reference-implementation note (non-normative).* The current deployment uses systemd units that each project's `deploy.sh` generates. Serial handling follows the udev rule above. Management goes over ssh (`ssh io@192.168.1.139`, `journalctl -u <svc> -f`, `sudo systemctl restart <svc>`, `sudo -e /etc/<svc>/config.toml`).

### 2.5 Build and artifact flow

- **MUST**: the workstation must build every workstation-target service binary as a **static** executable for the target architecture (`linux/arm64`, no dynamic-library dependency on the target). The build must trim paths and symbols for small, reproducible artifacts. Any mechanism that produces a static arm64 binary is acceptable.
- **MUST**: the build must happen on the workstation. The artifact then goes to the target as a copy. The target host compiles nothing.
- **MUST**: one deploy script per component must wrap the whole flow. The flow is build, transfer, binary install, seed-once config (§2.3), supervisor unit install/refresh, and restart. A redeploy is then a single command from the component directory.
- **MUST**: each service module must stay independently buildable against the shared runtime library. Use a direct local-module reference, not only a whole-workspace build. The team can then build and redeploy one component without touching the others.

*Reference-implementation note.* Builds use `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`. Each component references the shared module through its `replace … => ../shared` directive.

### 2.6 Naming and derived names

For every deployed service, one name derives everything: the **component directory name** = binary name = supervisor unit name = service user = install directory (`/opt/<name>`) = environment-variable prefix. We name device bridges `<devtag>-<function>-bridge`, where `<devtag>` names the device family or control interface (for example `atr1k-tuner-bridge`, `shelly-power-bridge`). In that pattern, `<function>` is the canonical role with internal hyphens collapsed to one token (`ant-switch`→`antswitch`, `ant-ctrl`→`antctrl`). The `_` in `waveshare_relay-antswitch-bridge` is a recorded deliberate deviation. Logic-only slots drop the `-bridge` suffix (`antennaselect`, `powerseq`, `hadiscovery`). The current deployment carries two **legacy names** (`flexbridge`, `ultrabridge`) that violate the convention. The deferred rename targets are `flex-radio-bridge` and `ultrabeam-ant-ctrl-bridge`. Renaming them touches live units (§6), so the team deferred that decision. A reconstruction **MUST** adopt consistent naming from the start. The *slot addresses* are the bus contract and never change.

### 2.7 Runtime robustness constraints (library-independent, normative)

This section records two constraints as requirements, because each caused a live incident in the reference stack. They apply regardless of the MQTT library chosen:

1. **A connect must be cancellable**. During a broker outage, a pending connect that ignores the shutdown signal leaves the service hanging until the supervisor force-kills it. The PA bridge met this live: SIGTERM during broker downtime hung the process until the supervisor's kill timeout. **MUST**: aborting the service while it connects must complete shutdown before the supervisor's stop timeout expires (reference default: 90 s). Shutdown must never need the force-kill path. Connection establishment must be interruptible by the shutdown signal.
2. **Message handlers must never block.** Incoming-message callbacks run on the connection's dispatch thread in common MQTT libraries. A handler that synchronously publishes or blocks deadlocks the whole client (the discovery consumer met this live). **MUST**: inbound-message handling must stay isolated from the receive path. Handlers enqueue work onto a bounded queue. A single worker drains that queue and serializes state mutation and publishing. When the queue is full, the job is **dropped** (this is better than blocking). The console-side matching rule (§4/`04-console.md`) drops slow-consumer events rather than ever stalling the bus side.

---

## 3. Per-component deployment matrix and deviations

Deviations from the §2 defaults, per component. Components not in the table follow §2 exactly (shari, systemd-style supervisor, seed-once 0600 config).

| Component | Host | Mechanism | Deviations from §2 |
|---|---|---|---|
| flexbridge | shari | service | Secrets through EnvironmentFile (`FLEXBRIDGE_MQTT_PASSWORD`), not the config file. |
| ultrabridge | shari | service | Serial: device-allow rules + udev rule. The password lives in the 0600 config file. The bridge self-heals serial re-enumeration (§5.3). |
| acom1200s-pa-bridge | shari | service | Serial. The name is deliberately model-specific (deviation from the family-tag naming rule). |
| wrc-rotator-bridge | shari | service | The bridge reaches the device over a websocket, not through serial. |
| atr1k-tuner-bridge | shari | service | Device over wifi binary WebSocket. Env prefix `ATR1K_TUNER_BRIDGE_*`. |
| shelly-power-bridge | shari | service | One process fronts **two slots** (`power/master`, `power/psu-13v8`). |
| antennaselect / powerseq / hadiscovery | shari | service | Logic slots — no device, no serial hardening. |
| hf-mqtt-capture | shari | service | Writable path `/var/log/hf-mqtt-capture`. The deploy seeds the password on-device (§2.2). It captures only `muehle/hf/#` by default. |
| testui | shari | service | Strongest sandbox set + resource ceilings (§2.4). HTTP on `0.0.0.0:8090`. Note the security caveat in §4.3. |
| hf-console-web (webbridge) | shari | service | Serves the console web build and a byte-transparent WebSocket→TCP MQTT proxy on `0.0.0.0:8091`. Supervisor unit flags: `-listen`, `-mqtt-broker`, `-web-root`. Sandbox set as §2.4 web tier. |
| **pelcobridge2** | **shack-pc (Windows)** | **interactive — NOT a service** | No supervisor unit, no auto-start. A human starts the binary by hand whenever the UHF rotator is in use. **Arming is a further manual keyboard act inside the program.** This deviation is inseparable from its safety model (remote motion must never be possible without a present human). Deploy: cross-compile `windows/amd64`. Copy the binary as `pelcobridge2.exe` to `C:/Users/iotte/pelcobridge2/`. Seed-once a 0600 `config.toml` from a local seed file. The operator must set `[serial] port` and re-run. Host/user/destination are overridable. The deploy script prints the manual start line and an arming reminder. A fatal startup error holds the console open with "press Enter to exit…" so a double-clicked exe's error is readable. Prefer the MQTT password in an environment variable. The tool tolerates a config-file fallback for GUI-launched processes — never a CLI flag. |
| **hf-console (tablet app)** | Android tablet | sideloaded app | The workstation builds the app behind a static-analysis + test gate. Then `adb install -r` over USB (target device id `HA2CLVAY`, app id `codeberg.kgbvax.hf_console`). `-r` (reinstall, keep data) is **necessary**: it preserves the on-device broker credentials, so redeploys never re-enter setup. No app store. The app manifest must carry the network permission in the **main** manifest (not only debug/profile overlays — a real past failure: the release build had no socket authority). It must also allow cleartext TCP (raw non-TLS MQTT socket). |
| **hf-console (web channel)** | shari (through hf-console-web) | service-deployed static build | The same build gate produces the console's web build. The console's own deploy script deploys it. The script builds the web bundle, cross-compiles the proxy binary, installs both under `/opt/hf-console-web/`, installs the hardened unit, and serves at `http://shari:8091/`. Security trade-offs and deviations: §3.1. |
| **hf-console (iOS channel)** | iOS device | self-sideloaded app | The same application, self-sideloaded as an IPA (no app store). Credentials go to the iOS Keychain. The app speaks raw-TCP MQTT. Raw TCP bypasses Apple's App Transport Security, so the app needs no ATS exception. No server-side deploy. See `04-console.md`. |
| ant-switch firmware | embedded | USB-flash (+ over-network updates) | Not a §2 service. The firmware speaks the four-plane schema directly over wifi. |
| m5stamp-hf-ctrl firmware | embedded | USB-flash | Two slots (`hf/switch`, `hf/pa-arm`) from one firmware. **PLC #2 firmware (`uhf/pol-ctrl`) does not exist — gap.** |
| pelcotest | workstation | none — bench tool | Never deployed anywhere. An operator runs it by hand over a USB-serial adapter (§4.4). |

A fourth auxiliary project, `sas/`, holds static design mockups and screenshots for the console user interface. It is never deployed and contains no runtime code. See `04-console.md` for the visual specification.

### 3.1 hf-console web channel — security trade-offs and recorded deviations

§4.3 makes unauthenticated, LAN-exposed command surfaces a blocking defect for the web test tool. The console's web channel carries the same class of issues. The sources record them as accepted LAN-trust trade-offs. A reconstruction must not reproduce them silently:

- **Plaintext browser credential storage.** Browsers offer no secure storage. The web build stores the broker password in plaintext browser-local storage. This is a recorded accepted trade-off. A reconstruction must either accept and document it, or solve it.
- **Open WebSocket tunnel.** The proxy binds `0.0.0.0:8091`. Its WebSocket upgrade accepts any Origin. Any LAN-hosted web page can therefore open a broker tunnel through it. A reconstruction must apply the §4.3 mandate to this endpoint: authenticate it, or restrict its origin and bind address.
- **Hard-coded browser endpoint (recorded deviation).** The browser client ignores the configured broker host. It connects to a hard-coded `ws://192.168.1.139:8091/mqtt`. This deviates from §1.1 (no hard-coded host names or addresses in code). A reconstruction must make the endpoint configurable, or must record the deviation in the same explicit way.

---

## 4. Operations

### 4.1 Inspecting the bus

For day-to-day bus inspection, use a plain MQTT client that subscribes to the station subtree. The operator's environment supplies the credentials — never type them into a shell command that lands in history (station rule):

- Whole bus: subscribe `muehle/#` and print topic + payload.
- One slot: subscribe `muehle/hf/radio/#`.
- The broker is at `192.168.1.50:1883`, user `hf` (§1.5 for the planned second broker).

*Reference-implementation note.* The current tool is `mosquitto_sub -h 192.168.1.50 -u hf -P "$MQTT_PASSWORD" -t 'muehle/#' -v` with the password exported ahead of time.

**MUST**: Any tooling that the reconstruction ships for bus inspection must read credentials from the environment or a 0600 file. It must never read them from an interactive command line that stores history.

### 4.2 hf-mqtt-capture — passive bus recorder (deployed service)

Purpose: when a bridge misbehaves, answer "what actually appeared on the bus in the last hours?" The recorder subscribes to the station subtree. It writes **every message verbatim** to timestamped files. It has no slot. It publishes **nothing ever** (not even a last-will). Reason: a diagnostic that emits traffic changes the thing it observes. Publish-silence is contract. It has no command surface beyond start/stop, and nothing depends on it.

Normative requirements (a reconstruction of the recorder, or of any tool that replaces it, must keep):

- Subscription `{site}/{station}/#`, default `muehle/hf/#`, QoS 1 (the MQTT delivery level that guarantees at-least-once delivery). **Note**: the default captures only the HF subtree. The tool records no UHF bus traffic today. A reconstruction can widen the filter to the whole site (an improvement, provided `muehle/hf/#` stays covered).
- **The log line format is a contract for ops tooling** — one line per message.
  - `<RFC3339Nano UTC timestamp> <topic> <raw payload bytes verbatim>\n`
  - Marker lines (connect/shutdown) share the format with an **empty topic field**: `<RFC3339Nano UTC> <marker text>\n` — a parser must tolerate the empty-topic form.
  - The tool writes payload bytes verbatim, never re-serialized.
- **The file layout is a contract**: `<log_dir>/<YYYY-MM-DD>/<HH>.log`, both components in **UTC**. The tool opens files append-only. File mode 0644. Date directory mode 0755. Rotation at the UTC hour boundary. Flush after every message.
- On every connection: write the marker `[capture] connected broker=<broker> topic=<topic>`. On graceful shutdown: write `[capture] shutting down`, disconnect with a **250 ms grace**, then flush and close the file.
- Client id: `{site}-{station}-hf-mqtt-capture` (default `muehle-hf-hf-mqtt-capture`). It is deliberately **not** slot-derived — the recorder has no slot.
- Connection handling: clean session (missed messages while offline are simply absent, no queueing), auto-reconnect, re-subscribe on reconnect. A subscribe failure only logs an error, and the tool continues running.
- Retention: on rotation, delete whole date-directories older than `retention_hours` (default 72). **Documented consequence (actual behavior, not the idealized claim)**: cleanup granularity is whole days. With the default, the tool keeps between **72 and 96 hours** of logs. A reconstruction can fix the granularity, but the PRD must not promise hour-precision deletion, and tooling must not assume it.
- Validation at startup: missing default config → run on defaults. Malformed or explicitly-missing config → exit code 2. Validation needs non-empty `broker`, `user`, `site`, `station`, `log_dir`, `retention_hours > 0`. A failure of the **first broker connect** is fatal with **exit code 1** — a distinct code from the config-exit code 2.
- Default config keys: `broker tcp://192.168.1.50:1883`, `user hf`, `password ""` (stored in the config file for this service — no EnvironmentFile), `site muehle`, `station hf`, `log_dir /var/log/hf-mqtt-capture`, `retention_hours 72`.
- Known defect to not reproduce: the rotation path closes the old file **before** it opens the new one. If the open fails (disk full, permission loss), the writer dereferences a null writer and crashes. A reconstruction must open the new file before it releases the old one.

### 4.3 testui — web bus display and stimulator (deployed service, with a security mandate)

Purpose: a browser UI to watch and deliberately poke the bus — per-slot cards, each driven by the slot's `expose` descriptor. It has a message ticker. Its command panels can publish arbitrary commands, simulate retained state, and clear retained planes. The browser never talks MQTT. A relay process holds the broker credentials and the single subscription. It is a **dev/operator tool, explicitly not the operator console** (the console is the tablet app). shari hosts it, HTTP on `0.0.0.0:8090`.

Behavior that **MUST** survive into any replacement tool (these encode the bus contract):

1. The browser can publish **only under the configured site prefix** (`muehle/`). Anything else → rejected ("topic outside configured site").
2. A publish topic's last path segment must be one of the four plane names `meta | state | status | cmd`. Anything else → rejected ("unknown plane").
3. The tool rejects **a retained publish to `/cmd`** — this matches the station-wide rule that commands are one-shot, never retained (see `02-interface-spec.md` §1.5).
4. Clearing a retained topic = a zero-length retained publish at QoS 1.
5. Two-layer liveness rendering, three colors. Red: the slot's `/status` ≠ `online`. Orange: bridge online but `/state.device_online` ≠ true (or a logic slot with no `device_online` field). Green: `/state.device_online == true`. A replacement must not collapse the two layers.
6. Command-panel fallback order: expose-driven (writable fields / actions from `/meta.expose`) → role-registry fallback (the reconciler's operator-hold `request`, the antenna switch's `select`) → raw JSON editor. The command payload builder must follow the station `/cmd` conventions exactly (documented deviations included).
7. After sending a command, watch the slot's `/state` for a change (fire-and-observe). Do not assume success.
8. The relay's browser-protocol JSON shapes are part of the contract (field names exact). A snapshot event is `{order, slots[]}`: `order` lists slot addresses in first-seen order. Each slot is `{address, meta, state, status, cmd}`, each plane a message object or null. Each plane message is `{topic, payload, retained, ts, object}` — `ts` is the relay receive time (RFC3339Nano UTC), `object` repeats the payload for JSON payloads. An update event adds `address` and `plane`, and sets `cleared: true` for a retained-clear. The relay ignores non-plane topics under the site prefix entirely.

**SECURITY — normative mandate.** The current tool has two defects that a re-implementation **MUST NOT** reproduce. The PRD makes the fix mandatory rather than a choice:

1. **The tool's bench config file in the repository holds a real-looking MQTT password** (with a comment saying it came from the deployed host). This is a leaked credential. Any reconstruction MUST treat that value as compromised. It must not commit any real credential to any repository. The operator must rotate the `hf` MQTT password as part of the migration. The deployed path itself is correct (separate 0600 EnvironmentFile on the host, TOML carries no secret).
2. **The publish endpoint has no authentication and the HTTP listener binds the whole LAN** (`0.0.0.0:8090`). Anyone on the LAN can publish arbitrary commands to the station bus through the tool. A re-implementation **MUST** mandate authentication on every publish-capable endpoint (and/or bind only to an operations VLAN). Unauthenticated LAN-wide publish surfaces are a blocking defect in any successor tool.

Other recorded behaviors (contract for a replacement): SSE (server-sent events: a long-lived HTTP stream of `data:` events) snapshot-then-update event stream with the full slot map on connect. The stream drops updates for slow clients (a refresh re-syncs). The MQTT client id is deliberately different from any slot-derived bridge id. A broker-connect failure at process start is fatal to the process (the supervisor restarts it) — a replacement can instead serve the UI and show "reconnecting".

### 4.4 pelcotest — bench tool and its measured facts (not deployed)

`pelcotest` is a manual bench TUI (terminal user interface: the whole user interface is text in a terminal). An operator used it once to re-engineer the serial behavior of the UHF pan/tilt head. It is **never deployed and needs no reconstruction**. What must survive is the **knowledge** it produced. That knowledge binds any UHF-rotator bridge as contract (full detail in `03-components/pelcobridge2.md`):

- The head **ignores absolute-position commands** as sent naively by the bench tool. Position control needs a quiet-line protocol (and see §6 for the recorded bench-vs-bridge discrepancy on this point).
- **Tilt readback is unusable as an elevation value**. The manual's degrees×100 claim is false for the tilt reply word. Nobody has checked any model of the word. A bridge must not present it as elevation.
- **Readback while the tilt motor runs is checksum-valid garbage** — it is trustworthy only once the motor has halted. No checksum can filter it.
- The head is Pelco-D/Pelco-P **adaptive per received frame** (it answers in the protocol the query arrived in). Pelco-D and Pelco-P are two byte-level serial protocols for CCTV pan/tilt hardware.
- A protocol decoder needs the **gap-flush rule**. It holds an incomplete frame only until a receive gap of ~1.5 frame times (minimum 20 ms). It then flushes the frame as noise. Without the rule, a truncated reply can merge with the next reply into a checksum-valid frame that carries a **false position word** (seen on the bench).
- Bench parameters: RS-485 (a multi-drop serial electrical bus) through USB-serial, 8N1 (8 data bits, no parity bit, 1 stop bit), default baud 2400 (family documented 1200–9600 — ambiguous), Pelco address default 1. The Pelco-P address convention is an **unverified assumption**.
- Tool epistemics worth copying in any successor: raw hex first. Label vendor claims as untrusted documentation. Label operator hypotheses UNVERIFIED. Destructive frames need explicit confirmation against the built frame bytes. The sweep recorder always transmits a stop frame before exit, including on interrupt.

---

## 5. Checks and failure runbook

### 5.1 Liveness model — how to read a component's health

Two distinct liveness layers exist, and **consumers AND operations must check both**:

1. **Bridge/process liveness** — `<slot>/status`, a plain retained string `online`/`offline` set by the MQTT last-will mechanism. Answers: *does the software component have a connection to the broker?*
2. **Device-link liveness** — `<slot>/state.device_online` (boolean inside the state snapshot, device slots only), conventionally paired with `<slot>/state.error` when false. Answers: *can the running bridge reach the hardware behind it?* (cable pulled, device powered off) while `/status` stays `online`.

Rules (normative — the failure that produced them was live):

- **MUST**: A consumer or operator display that acts on a slot's state must **check both** layers. Only then can it treat the slot as healthy. Keying on `/status` alone is a recorded live failure (the reconciler chattered when a device link died while its bridge stayed up). Keying on `/state` alone trusts retained data that can be stale.
- **MUST**: A wait-for-state consumer (for example the sequencer waiting for the PA to confirm power) must also see the slot's `/status == online`. A dead device then cannot pass on a stale retained state.
- **Recorded actual behavior (not the idealized doc claim)**: On a **clean process shutdown the broker does not fire the last-will**. The bridge must publish `offline` itself. If it does not (or if it stops between publishes), a **retained `online` can persist on `/status` for a stopped service**. Runbooks and dashboards must not trust `/status` alone. They must cross-check against the supervisor (`systemctl status` or the matching call) or the heartbeat/state freshness. See §6 for the open decision this creates.
- **Heartbeat convention**: The PA-arm firmware republishes its state snapshot every **10 s** as a slow heartbeat (even with no content change). Its own absence is then detectable. State freshness can substitute for liveness only when a component declares such a heartbeat. Change-only publishers (see the radio, §5.3) provide no such signal.

### 5.2 Day-to-day operator access

- Service status, log tail, restart, and config edit on the service host, per service (reference: `systemctl status/restart <svc>`, `journalctl -u <svc> -f`, `sudo -e /etc/<svc>/config.toml` over `ssh io@192.168.1.139`).
- Log expectations: every bridge logs unknown/invalid command payloads and drops them (never crashes on bad intent). Device-link failures get a log with a human-readable error that also lands in `/state.error`.
- The passive recorder (§4.2) is the black-box record for "what was on the bus". The web test tool (§4.3) is the interactive check.

### 5.3 Known failure modes — symptoms, causes, remedies

We observed each row live or on the bench in the reference deployment. A reconstruction's runbook MUST cover the matching scenario for the matching component.

| # | Symptom | Cause | Remedy / required behavior |
|---|---|---|---|
| 1 | Antenna-controller bridge log: `write request: Input/output error`. The bridge keeps running. | USB FTDI serial adapter dropped and re-enumerated under a new device node. The handle is stale. | **MUST self-heal**: on an I/O error, reopen the port by its stable by-id path rather than die (deployed behavior). Manual: replug + supervisor restart. |
| 2 | PA arm enable (`pa-arm.armed`) drops while the radio is idle-but-healthy. | The arm relay de-energizes unless the radio's `/state` refreshes within **10 s**. The radio bridge publishes `/state` **only on change**. An idle radio therefore starves the heartbeat. | Known live fragility. **Hard requirement for any reconstruction**: the radio bridge (or a matching mechanism) must republish a heartbeat state at least every **≤5 s**. The 10 s window then never starves. Interim remedy: poke the radio or restart its bridge. |
| 3 | Reconciler repeatedly selects/deselects ("chatter") during a device outage. | The consumer keyed on `/status` alone. The device link died while the bridge stayed up. | The two-layer rule (§5.1) fixed this. Regression-test this symptom class. |
| 4 | A service hangs on shutdown during a broker outage. The supervisor must force-kill. | The connect call ignored the shutdown signal (§2.7 item 1). | **MUST**: cancellable connect. |
| 5 | A bridge silently stops processing messages while connected. | A message handler blocked on a synchronous publish. The dispatch thread deadlocked (§2.7 item 2). | **MUST**: handlers never block. Use a queue plus a single worker. |
| 6 | `/status` still reads `online` for a stopped service. | A clean shutdown publishes no last-will. A retained `online` persists (§5.1). | Cross-check supervisor state. Never gate safety on `/status`. |
| 7 | Sequencer reports `phase=idle`, `fault="<step>: <reason>"`, station partially powered. | A wait step timed out (default deadline 120 s per step). | By design: **no rollback** — driven slots hold their last retained command. The operator resolves the faulted slot and re-runs `start`/`stop`. |
| 8 | UHF rotator re-homes itself unprompted mid-session. | The head's periodic self-check re-arms after the head power-cycles. The current bridge sends the disable frame only **once per process start**, not once per reconnect. | **MUST (improvement over current)**: re-send the self-check-disable frame after every successful (re)open of the link. Manual remedy: restart the console with the head reachable. |
| 9 | UHF rotator link dead after adapter re-enumeration. No auto-recovery. | Auto-heal retries once after a 2 s cooldown, then gives up permanently on a failed reopen. | Manual: the TUI's manual reopen key (always works). A reconstruction must retry indefinitely at a 2 s reopen cooldown (the current auto-heal interval). |
| 10 | Console (tablet) shows offline forever after booting before the broker was up. Taps do nothing. | The code swallows the initial-connect failure with no retry loop. It silently drops publishes made while disconnected. | Known console defect (`04-console.md`). Remedy: restart the app after the broker is reachable. A reconstruction must add an app-level connect retry and surface dropped commands. |
| 11 | Bus recorder crashes on hour rollover under disk-full / permission loss. | Rotation closes the old file before opening the new. A null-writer panic results (§4.2). | Open-before-close fixes the defect. Check host disk space in the checks. |
| 12 | An antenna switch to a port takes effect while RF is present → relay contact damage risk. (RF = radio-frequency energy in the feedline.) | Manual switching during transmit. | The console and reconciler guard this (fail-closed RF guard). The hardware cold-switch contract is in `02-interface-spec.md` §6. Never switch under RF. |
| 13 | First key-up (key-up = starting a transmission) after an auto-ground goes "into the short". | Reconciler re-activation can race the switch settling after a ground event. | Recorded live gap (project memory). After any grounded→selected transition, the operator must confirm the switch reports `settled` before transmitting. A reconstruction must close the race. |
| 14 | Tilt readback of the UHF head shows values like 224° or garbage while moving. | Head firmware behavior (§4.4): the tilt word is not elevation. Readback mid-motion is checksum-valid garbage. | Never trust the tilt word. Read back only when the motor has halted. |
| 15 | All soft bindings stop together: band-follow, tuner-follow, PA-follow, antenna selection. Retained state goes stale. | The reconciler process (antennaselect) died. It is a coordination single point of failure. | Mitigation: the supervisor restarts it automatically (§2.4). Until it returns, the station degrades to manual operation. An explicit "reconciler offline" operator indication is an open wish (§6). |

---

## 6. Open decisions and unresolved facts

The sources leave these unresolved. A reconstruction must decide each explicitly. Do **not** silently adopt either variant.

1. **Broker topology** (§1.5). Variant A: a single broker at `192.168.1.50:1883` (all deployed code defaults). Variant B: a shack-local broker on shari `192.168.1.139:1883`, authoritative for `muehle/#`, bridged to `.50`. An unmerged, undeployed feature branch (`feat/shack-local-mqtt-broker`, commit `cd58466`, planned resume ~2026-09-02) contains its implementation, including a console-setup default repoint. Evidence both ways: every deployed default points at `.50`. The console project's local docs already name `.139`. The console itself is topology-agnostic (the host is a config field). Only defaults differ. **Decision needed**: the team must decide this before deploy tooling is final.
2. **Ultrabeam switch port: 3 or 4.** Repo-root documentation and the integration model say the Ultrabeam beam is on antenna-switch port 3 (fan dipole port 6, dummy load port 1). The deployed reconciler seed config *and* the console's antenna map say port 4. A third conflicting claim exists inside the integration model itself: its passive-resource list says the fan dipole is on port 2. The authoritative artifact — the live config on the Raspberry Pi — stayed unreadable while the authors wrote this PRD. Port numbers are per-site configuration. Never hard-code them. The deployed on-device config plus physical inspection is the only resolution path.
3. **`device_online` publication form.** The integration model says the field is "omitted when true". The deployed bridges publish `device_online: true` explicitly. Consumers must treat both forms as the same (absence = true) — that consumer rule is normative. A reconstruction must pick a producer convention (recommendation: mandate the explicit boolean for all device slots, and update the model).
4. **Clean-shutdown `/status`.** Actual behavior: a retained `online` persists after a clean stop (no will on graceful disconnect). `/status` can therefore lie about a stopped service. A reconstruction must either (a) replicate actual behavior and rely on the §5.1 cross-checks, or (b) tighten the contract so a clean shutdown always publishes `offline` itself. It must not assume (b) holds true of legacy peers during migration.
5. **Absolute-position commands on the UHF head.** The bench tool measured the head **ignoring** absolute-set opcodes. The production console's set ladder converges when quiet-line windows bracket the sets. The two research files disagree. A likely reconciliation: sets only land on a quiet line (the bench never provided one). Any re-implementation must check this on the bench before relying on absolute sets. It must keep the jog/stop fallback.
6. **Committed testui credential.** A real-looking MQTT password sits in a repository config file (§4.3). Treat it as leaked: remove it from the repo. Rotate the `hf` password on the broker. Decide whether the historical value needs revocation on the old broker before/after any migration.
7. **Recorder retention granularity** (§4.2): `retention_hours` is day-granular in effect (72–96 h actual retention at the default). Keep the day-directory scheme so ops tooling matches, or fix it to true hourly — decide this. Do not promise hour-precision deletion either way. Also decide whether to widen the default capture filter to include the UHF subtree (the tool records none of it now).
8. **Host-liveness nodes** (`muehle/host/shari`, `muehle/host/shack-pc`) are in the bus model, but nothing publishes them. Add a host-liveness publisher or remove the nodes from the model.
9. **PLC #2 firmware** (`muehle/uhf/pol-ctrl`) does not exist in the repo, although the slot table attributes it to the m5stamp project. Add it or formally descope it.
10. **Legacy service names** (`flexbridge`, `ultrabridge`) violate the naming convention. Renaming touches live supervisor units and configs. A fresh reconstruction must simply use consistent names. Only a migration of the existing deployment faces this decision.
11. **Secrets hardening ceiling.** Plaintext secrets in 0600 files is an accepted trade-off given the threat model (trusted LAN). If the threat model tightens (or the testui-style tooling stays deployed), move to supervisor-provided credentials (`systemd-creds`/`LoadCredential` or the matching mechanism).
12. **Pelco-P addressing on the UHF head** is an unverified assumption (the head can be zero-indexed for Pelco-P. Then every P frame has an address one unit off, and the head silently ignores it). The rotator console removed Pelco-P framing on 2026-08-29 and speaks only Pelco-D. Check the addressing on the bench before any re-introduction of Pelco-P framing.
13. **Logging integration model (proposed, unimplemented).** The integration-model drafts define future roles `qso-log` and `bandmap` and topics `<station>/log/meta`, `<station>/log/state`, `<station>/log/status`, `<station>/log/event`, and `<station>/spots/event`. No component in the repo implements any of them. A reconstruction must decide explicitly: build the logging model or descope it. Until decided, treat those topics as not existing.
14. **Reconciler-offline indication.** antennaselect is a coordination single point of failure (§5.3 row 15). The supervisor restarts it automatically, but no component shows "reconciler offline" to the operator meanwhile. Add an explicit operator indication, or accept the stale-retained-state window.