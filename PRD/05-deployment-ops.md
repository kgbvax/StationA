# 05 — Deployment & Operations Specification

This document specifies how the Mühle station-automation system is deployed, configured, and operated, written for an engineering team that will **re-construct the whole system from scratch on a different technology stack**. It covers: the host and infrastructure inventory (§1), the conventions every deployed service must follow (§2), the per-component deployment deviations (§3), the operator/bench tooling that supports day-to-day operations (§4), the monitoring and failure-runbook expectations (§5), and the open decisions the sources leave unresolved (§6). Behavior contracts here are **stack-agnostic and normative**; concrete technology names (Go, systemd, ssh, adb, ESPHome, Flutter, Mosquitto) appear only in clearly-marked *Reference-implementation notes* as non-normative background describing the current deployment.

Background for the non-radio reader: **amateur radio (ham radio)** is the licensed hobby of two-way radio communication; the "Mühle" station (site prefix `muehle`) is one such installation with an HF (high-frequency, 1.8–54 MHz) and a UHF (144–440 MHz) station. A **bridge** is a small service that fronts one physical device (a transceiver, a power amplifier, a relay board) and mirrors it onto a shared **MQTT** bus — a lightweight publish/subscribe protocol where clients exchange messages on hierarchical text topics via a central **broker**. A **slot** is one component's topic namespace, e.g. `muehle/hf/radio`. A **retained** message is stored by the broker and re-delivered to every future subscriber until overwritten or cleared. **LWT (Last Will and Testament)** is a broker feature that publishes a pre-registered message on a client's behalf when that client disconnects uncleanly — the station uses it for liveness (`<slot>/status` = `online`/`offline`). The full bus contract lives in `02-interface-spec.md`; this document covers only deployment and operations.

---

## 1. Hosts & infrastructure inventory

### 1.1 shari — the service host

**shari** is the station's single Linux compute host: a Raspberry Pi (64-bit ARM, "arm64" architecture) at `192.168.1.139`, login user `io` (admin operations via that user with privilege escalation). All non-interactive services run on shari. It is a **known single point of failure** (see `docs` research, defect list): shari's loss takes every shari-hosted bridge's MQTT presence offline simultaneously — which is *correct* behavior (each bridge's LWT fires) — and the station degrades to manual operation, which is acceptable only because safety interlocks are hardware-based (see `06-safety.md`).

Services deployed on shari (each an independent, separately-restartable unit):

| Service | Slot(s) it fronts | Notes |
|---|---|---|
| flexbridge | `muehle/hf/radio` | FLEX-8400 transceiver, over the network |
| ultrabridge | `muehle/hf/ant-ctrl` | Ultrabeam RCU-06 antenna controller, USB-serial |
| acom1200s-pa-bridge | `muehle/hf/pa` | ACOM 1200S power amplifier, serial (telemetry only) |
| wrc-rotator-bridge | `muehle/hf/rotator` | Yaesu G-450DC rotator via an AF6SA "WRC" controller, over a websocket |
| atr1k-tuner-bridge | `muehle/hf/tuner` | ATR-1000 antenna tuner, wifi binary WebSocket |
| shelly-power-bridge | `muehle/power/master`, `muehle/power/psu-13v8` | one process, two slots (smart mains plugs) |
| antennaselect | `muehle/hf/antenna-select` | logic slot, no device |
| powerseq | `muehle/hf/power-seq` | logic slot, no device (startup/shutdown sequencer) |
| hadiscovery | `muehle/hf/discovery` | logic slot, no device (Home Assistant discovery renderer) |
| hf-mqtt-capture | — (no slot) | passive bus recorder (§4.2) |
| testui | — (no slot) | web bus monitor/stimulator (§4.3) |
| hf-console-web | — (no slot) | serves the console's web build on port 8091 (§3) |

Requirements:

- **SHALL**: every service listed above shall run on the single service host, each under its own supervisor-managed unit with a dedicated unprivileged system user, independently startable and restartable.
- **SHALL**: the host name `shari` and its address `192.168.1.139` shall be deployment configuration; nothing in any component may hard-code them (only the *role vocabulary* is code).
- **SHALL**: host liveness shall be treated as load-bearing for operations: the bus model reserves `muehle/host/shari` (fields `online`, `temp_c`, `load`) and `muehle/host/shack-pc` (field `online`) as host-liveness nodes, but **no component in the current repo publishes them** — they are model-only. A reconstruction that wants host monitoring must decide to implement them (see §6).

### 1.2 shack-pc — the interactive host

**shack-pc** is a Windows PC in the radio shack at `192.168.1.197` (remote-copy user `iotte`). It hosts exactly one component: the **UHF rotator console** (`muehle/uhf/rotator`), which is an *interactive terminal application started by hand by a human operator* — deliberately not a background service, because its safety model requires a human at the keyboard to arm remote motion (see `03-components/pelcobridge2.md` and §3 of this document). Install target: `C:/Users/iotte/pelcobridge2/`.

### 1.3 The operator tablet

The station's primary operator console is a **fixed-mount Android tablet**, connected to the deploy workstation over **USB**. It is not a service host: the console application is installed as a self-sideloaded app (no app store), and it is a pure MQTT client plus one HTTP event feed — it has **no MQTT presence of its own** (no slot, no `/meta`, no `/state`, no LWT, no heartbeat; it publishes only to `muehle/<slot>/cmd` topics). Reinstallation with `adb install -r` (install-over, keep data) is the supported update path *precisely because it preserves the broker credentials stored on the device*. See `04-console.md` for the console's full specification; deployment details in §3.

### 1.4 Embedded devices

These are devices whose firmware speaks the canonical MQTT schema directly over wifi — device, adapter, and host collapse into one node:

- **1:6 antenna switch** (`muehle/hf/ant-switch`) — relay board, wifi, firmware-configured (ESPHome YAML in the reference implementation).
- **M5 Stamp PLC #1** (`muehle/hf/switch` + `muehle/hf/pa-arm`) — two slots from one firmware: relays 3/4 drive the PA/transceiver remote-on lines, relay 1 is the fail-safe PA arm relay.
- **M5 Stamp PLC #2** (`muehle/uhf/pol-ctrl`, X-Quad antenna polarization relays) — **documented gap: no PLC #2 firmware exists in the repo**; treat the slot as unimplemented, not as a component to deploy.
- **Shelly smart plugs** (`muehle/power/master`, `muehle/power/psu-13v8`) — commercial smart plugs fronted by the shari-hosted shelly-power-bridge; the plugs themselves are stock firmware.

Firmware for the two custom embedded families is flashed over **USB** from a workstation (the antenna-switch firmware family additionally supports over-the-network firmware updates once it is on wifi — its standard workflow). Firmware deployment is otherwise out of scope of the service conventions in §2.

### 1.5 The MQTT broker — current and planned topology

- **Current production broker**: a Mosquitto MQTT broker at `tcp://192.168.1.50:1883`, MQTT username `hf`, **persistent store enabled** (retained `meta`/`state` messages survive a broker restart). All deployed components point here.
- **Planned migration (open decision, §6)**: work exists on an unmerged, **undeployed** feature branch (commit `cd58466`, dated 2026-08-26) to run a second, shack-local Mosquitto broker on shari at `192.168.1.139:1883`, authoritative for `muehle/#` and bridged to the untouched broker at `192.168.1.50:1883` — so the station keeps its bus when the shack-to-house network link drops. That branch adds a `mqtt-broker/` component (broker configuration, access-control list, seed-once deploy) and repoints the console's default broker host and the web proxy's default broker flag to `.139`.
- A PRD based on this text **SHALL treat `192.168.1.50:1883` as the current production broker** and record the shack-local broker as a decision point, not as deployed behavior. Note one live documentation conflict: the console project's local instructions already *document* the shack broker at `192.168.1.139:1883` while the deployed code still defaults to `192.168.1.50:1883` — code wins for current behavior.
- **SHALL (both variants)**: broker credentials shall never appear on any command line, in any process list, or in any unit-file `ExecStart` (§2.2).
- **SHALL**: the broker shall retain `meta`/`state`/`status` messages across broker restarts (persistent store); a reconstruction on a different broker product must preserve retention and last-will semantics.

### 1.6 Workstations (build machines)

Developer workstations cross-compile every artifact and push it to the target host. No component is built on its target host. The bench tooling (§4.4) also runs on a workstation, attached to hardware over USB-serial adapters.

---

## 2. Service conventions (normative)

These apply to every non-interactive service (all services in §1.1). Deviations are listed in §3.

### 2.1 Configuration file and precedence

Every service takes its configuration from **one file**, default path `/etc/<SERVICE>/config.toml`, owned by the service's dedicated user, file mode **0600** (it may hold the MQTT password in plaintext — an accepted trade-off; the stronger option of a supervisor-provided credential store is recorded as a future hardening in §6). The file format is TOML in the reference implementation; **the contract is the path, ownership, permissions, and the precedence rules below**, not the syntax.

Precedence, highest first — this exact ladder is a **behavior contract**:

1. **Explicit command-line flag** — "explicit" is detected by *whether the flag was actually passed*, not by comparing against the default value: a flag explicitly set to its default value still wins over the file.
2. **Config-file value.**
3. **Built-in default.**

Missing-file semantics (**contract**):

- Default config path absent → the service runs on built-in defaults + flags (this keeps local mock/bench workflows working).
- Config path **explicitly named** but missing or unreadable → **fatal at startup**. A mistyped path must never quietly run on defaults.
- File present but malformed → **fatal**, with the parse error printed.

Environment-variable override: each service also accepts configuration via environment variables prefixed with its own name (uppercased, hyphens→underscores), e.g. `FLEXBRIDGE_MQTT_PASSWORD`, `POWERSEQ_MQTT_PASSWORD`, `TESTUI_MQTT_PASSWORD`, `ATR1K_TUNER_BRIDGE_MQTT_PASSWORD`, `PELCOBRIDGE2_MQTT_PASSWORD`. This is the sanctioned secrets channel (§2.2).

Common configuration keys every service shares (`[mqtt]` section): `broker`, `client_id` (default derived from the slot address as `<site>-<station>-<slot>` with `/`→`-`, e.g. `muehle-hf-radio`), `site`, `station`, `slot`, `user`, `password`. Identity keys `location` (building label) and `host` (compute-node name) are published in `/meta` and **come from configuration, never code** (see `02-interface-spec.md` §2).

### 2.2 Secrets handling

- **SHALL**: no secret shall ever appear on a command line, in a unit `ExecStart`, in a process listing, or in a repository. Two sanctioned patterns exist:
  1. **Password directly in the 0600 config file** (used by e.g. the antenna-controller bridge).
  2. **EnvironmentFile** (the "flexbridge pattern"): the supervisor loads a second 0600 file, e.g. `EnvironmentFile=/etc/flexbridge/flexbridge.env` containing `FLEXBRIDGE_MQTT_PASSWORD=<pw>`, and the application reads the environment variable, which overrides the config-file value. Used by flexbridge, powerseq, hadiscovery, atr1k-tuner-bridge, and the web test tool (on the target; its *checked-in* bench config is a defect — §4.3).
- **SHALL**: the deployed config files on the target host are the only authoritative copy of secrets for that host; deploy tooling shall not overwrite them (§2.3) and shall not require the secret to pass through the workstation for routine redeploys.
- **SHALL**: the passive recorder (§4.2), whose seeded config would otherwise need the password pre-filled on the workstation, shall obtain it **on the target device**: if the seeded password is empty, its deploy script reads the shared MQTT password on-device from the first readable `<SERVICE>_MQTT_PASSWORD=` line of the existing bridge environment files (search order: the PA bridge's, flexbridge's, hadiscovery's, the tuner bridge's `.env` files) and injects it into the recorder's config with an on-device script.

### 2.3 Seed-once deployment

The deploy flow for every service follows **seed-once** semantics (**contract**):

1. The deploy script builds the artifact on the workstation (§2.5), transfers it to the target, and installs it (reference layout: binary at `/opt/<SERVICE>/<SERVICE>`).
2. It generates a config file (from the developer's environment / an example file) with restrictive creation (`umask 077` / equivalent, target mode 0600) and installs it **only if no config file exists yet**. An existing file is left byte-for-byte untouched ("config exists — leaving it untouched (seed-once)"), and the staged copy is removed after the comparison.
3. Consequence (intended): **the device owns its settings.** An operator can edit the config on the host and redeploy binaries freely without losing changes. To re-seed: delete the file on the host, then redeploy.
4. The seed generator shall escape backslash and double-quote so passwords containing special characters round-trip through the generated file.

### 2.4 Supervisor hardening

Requirements for whatever service supervisor the new stack uses — each statement is testable by inspecting the unit definition:

- **SHALL**: each service runs under a **dedicated unprivileged system user/group** named after the service (no login shell, no home directory), never as root, never shared between services.
- **SHALL**: each unit shall set the equivalent of: `NoNewPrivileges=true` (no privilege escalation by the process or its children), `ProtectSystem` (the OS filesystem read-only to the service — `full` normally, `strict` where the service needs no writes at all), `ProtectHome=true`, `PrivateTmp=true` (isolated `/tmp`), and a managed configuration directory (systemd's `ConfigurationDirectory=<SERVICE>`, which makes `/etc/<SERVICE>` read-only-but-present to the service).
- **SHALL**: `ExecStart` shall contain only the binary and the `-config <path>` argument — **never any secret or other environment**.
- **SHALL**: the unit shall declare dependencies such that the service starts after network availability (`network-online.target` equivalent) and restarts on failure with a 5 s delay (`Restart=on-failure`, `RestartSec=5`).
- **SHALL** (serial services only): the unit shall grant exactly the device access needed — a supplementary group for the serial devices (reference: `dialout`) and device-allow rules for the serial device classes (`char-ttyUSB`, `char-ttyACM`, `char-tty`, read-write) — rather than blanket device access.
- **SHALL** (serial services only): the deploy script shall install a udev rule (or equivalent device-ownership mechanism) that assigns the USB-serial adapter (default USB vendor id `0403`, the FTDI family) to the serial group with mode 0660, because distribution defaults vary; reload and re-trigger the device rules after install.
- **SHALL** (services with a writable path, e.g. the log recorder): the unit shall enumerate the writable path explicitly (`ReadWritePaths=/var/log/hf-mqtt-capture`) and nothing else writable.
- **SHALL** (web-facing services, the test tool and the console web proxy): the unit shall use the strongest sandbox set — `ProtectSystem=strict`, `PrivateDevices`, kernel-tunables/modules/cgroup protections, `RestrictAddressFamilies=AF_INET AF_INET6`, namespace/realtimes/SUID restrictions, `RemoveIPC`, an **empty capability bounding set**, a managed state directory, and **resource ceilings** (the web test tool: memory 128 MiB, tasks 64; the console web proxy: memory 64 MiB, tasks 32 — the in-memory bus tree must not be able to OOM the shared host).

*Reference-implementation note (non-normative).* The current deployment uses systemd units generated by each project's `deploy.sh`; serial handling per the udev rule above; management over ssh (`ssh io@192.168.1.139`, `journalctl -u <svc> -f`, `sudo systemctl restart <svc>`, `sudo -e /etc/<svc>/config.toml`).

### 2.5 Build & artifact flow

- **SHALL**: every workstation-target service binary shall be built as a **static** executable for the target architecture (`linux/arm64`, no dynamic-library dependency on the target), with path and symbol trimming for small, reproducible artifacts. Any mechanism producing a static arm64 binary is acceptable.
- **SHALL**: the build shall happen on the workstation and the artifact shall be copied to the target; the target host compiles nothing.
- **SHALL**: one deploy script per component shall wrap the whole flow — build, transfer, install binary, seed-once config (§2.3), install/refresh supervisor unit, restart — so that a redeploy is a single command from the component directory.
- **SHALL**: each service module shall remain independently buildable against the shared runtime library via a direct local-module reference (not only via a whole-workspace build), so a single component can be built and redeployed without touching the others.

*Reference-implementation note.* Builds use `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`; the shared module is referenced by each component's `replace … => ../shared` directive.

### 2.6 Naming and derived names

For every deployed service, one name derives everything: the **component directory name** = binary name = supervisor unit name = service user = install directory (`/opt/<name>`) = environment-variable prefix. Device bridges are named `<devtag>-<function>-bridge` where `<devtag>` names the device family or control interface (e.g. `atr1k-tuner-bridge`, `shelly-power-bridge`); logic-only slots drop the `-bridge` suffix (`antennaselect`, `powerseq`, `hadiscovery`). The current deployment carries two **legacy names** (`flexbridge`, `ultrabridge`) that violate the convention; renaming them is a deferred decision that touches live units (§6). A reconstruction **SHALL** adopt consistent naming from the start; the *slot addresses* are the bus contract and never change.

### 2.7 Runtime robustness constraints (library-independent, normative)

Two constraints are recorded as requirements because each caused a live incident in the reference stack; they apply regardless of the MQTT library chosen:

1. **Connect must be cancellable.** During a broker outage, a pending connect attempt that ignores the shutdown signal leaves the service hanging until the supervisor force-kills it (hit live by the PA bridge: SIGTERM during broker downtime hung the process until the supervisor's kill timeout). **SHALL**: aborting the service during a connection attempt shall complete shutdown promptly; connection establishment shall be interruptible by the shutdown signal.
2. **Message handlers must never block.** Incoming-message callbacks run on the connection's dispatch thread in common MQTT libraries; a handler that synchronously publishes or blocks deadlocks the whole client (hit live by the discovery consumer). **SHALL**: inbound-message handling shall be isolated from the receive path — handlers enqueue work onto a bounded queue drained by a single worker that serializes state mutation and publishing; when the queue is full the job shall be **dropped** (preferred over blocking). The console-side equivalent (§4/`04-console.md`) drops slow-consumer events rather than ever stalling the bus side.

---

## 3. Per-component deployment matrix & deviations

Deviations from the §2 defaults, per component. Components not listed follow §2 exactly (shari, systemd-style supervisor, seed-once 0600 config).

| Component | Host | Mechanism | Deviations from §2 |
|---|---|---|---|
| flexbridge | shari | service | Secrets via EnvironmentFile (`FLEXBRIDGE_MQTT_PASSWORD`), not the TOML |
| ultrabridge | shari | service | Serial: device-allow rules + udev rule; password lives in the 0600 TOML; self-heals serial re-enumeration (§5.3) |
| acom1200s-pa-bridge | shari | service | Serial; deliberately model-specific name (deviation from the family-tag naming rule) |
| wrc-rotator-bridge | shari | service | Device reached over a websocket, not serial |
| atr1k-tuner-bridge | shari | service | Device over wifi binary WebSocket; env prefix `ATR1K_TUNER_BRIDGE_*` |
| shelly-power-bridge | shari | service | One process fronts **two slots** (`power/master`, `power/psu-13v8`) |
| antennaselect / powerseq / hadiscovery | shari | service | Logic slots — no device, no serial hardening |
| hf-mqtt-capture | shari | service | Writable path `/var/log/hf-mqtt-capture`; password seeded on-device (§2.2); captures only `muehle/hf/#` by default |
| testui | shari | service | Strongest sandbox set + resource ceilings (§2.4); HTTP on `0.0.0.0:8090`; **security caveat §4.3** |
| hf-console-web (webbridge) | shari | service | Serves the console web build + a byte-transparent WebSocket→TCP MQTT proxy on `0.0.0.0:8091`; supervisor unit flags: `-listen`, `-mqtt-broker`, `-web-root`; sandbox set as §2.4 web tier |
| **pelcobridge2** | **shack-pc (Windows)** | **interactive — NOT a service** | No supervisor unit, no auto-start. A human starts the binary by hand whenever the UHF rotator is used; **arming is a further manual keyboard act inside the program**. This deviation is inseparable from its safety model (remote motion must never be possible without a present human). Deploy: cross-compile `windows/amd64`, copy the binary as `pelcobridge2.exe` to `C:/Users/iotte/pelcobridge2/`, seed-once a 0600 `config.toml` from a local seed file (the operator must set `[serial] port` and re-run); host/user/destination overridable. The deploy script prints the manual start line and an arming reminder. A fatal startup error holds the console open with "press Enter to exit…" so a double-clicked exe's error is readable. MQTT password via environment variable preferred, config-file fallback tolerated for GUI-launched processes (never a CLI flag). |
| **hf-console (tablet app)** | Android tablet | sideloaded app | Built on the workstation behind a static-analysis + test gate, then `adb install -r` over USB (target device id `HA2CLVAY`, app id `codeberg.kgbvax.hf_console`). `-r` (reinstall, keep data) is **required**: it preserves the on-device broker credentials, so redeploys never re-enter setup. No app store. The app manifest must carry the network permission in the **main** manifest (not only debug/profile overlays — a real past failure where the release build had no socket authority) and must permit cleartext TCP (raw non-TLS MQTT socket). |
| **hf-console (web channel)** | shari (via hf-console-web) | service-deployed static build | The console's web build is produced by the same build gate and deployed *by the console's own deploy script*: build web bundle, cross-compile the proxy binary, install both under `/opt/hf-console-web/`, install the hardened unit, serve at `http://shari:8091/`. |
| ant-switch firmware | embedded | USB-flash (+ over-network updates) | Not a §2 service; firmware speaks the four-plane schema directly over wifi |
| m5stamp-hf-ctrl firmware | embedded | USB-flash | Two slots (`hf/switch`, `hf/pa-arm`) from one firmware. **PLC #2 firmware (`uhf/pol-ctrl`) does not exist — gap.** |
| pelcotest | workstation | none — bench tool | Never deployed anywhere; run by hand over a USB-serial adapter (§4.4) |

---

## 4. Operations

### 4.1 Inspecting the bus

Day-to-day bus inspection is a plain MQTT client subscribing to the station subtree, with credentials supplied by the operator's environment (never typed into a shell command that lands in history — station rule):

- Whole bus: subscribe `muehle/#` and print topic + payload.
- One slot: subscribe `muehle/hf/radio/#`.
- The broker is at `192.168.1.50:1883`, user `hf` (§1.5 for the planned second broker).

*Reference-implementation note.* The current tool is `mosquitto_sub -h 192.168.1.50 -u hf -P "$MQTT_PASSWORD" -t 'muehle/#' -v` with the password exported ahead of time.

**SHALL**: any tooling the reconstruction ships for bus inspection shall read credentials from the environment or a 0600 file, never from an interactive command line that stores history.

### 4.2 hf-mqtt-capture — passive bus recorder (deployed service)

Purpose: when a bridge misbehaves, answer "what actually appeared on the bus in the last hours?" It subscribes to the station subtree and writes **every message verbatim** to timestamped files. It has no slot, publishes **nothing ever** (not even a last-will — a diagnostic that emits traffic changes the thing it observes; publish-silence is contract), no command surface beyond start/stop, and nothing depends on it.

Normative requirements (a reconstruction of the recorder, or of any tool that replaces it, shall keep):

- Subscription `{site}/{station}/#`, default `muehle/hf/#`, QoS 1. **Note the default captures only the HF subtree — UHF bus traffic is not recorded at all today.** A reconstruction may widen the filter to the whole site (an improvement, provided `muehle/hf/#` stays covered).
- **Log line format is a contract for ops tooling** — one line per message:
  - `<RFC3339Nano UTC timestamp> <topic> <raw payload bytes verbatim>\n`
  - marker lines (connect/shutdown) share the format with an **empty topic field**: `<RFC3339Nano UTC> <marker text>\n` — a parser must tolerate the empty-topic form.
  - Payload bytes are written verbatim, never re-serialized.
- **File layout is a contract**: `<log_dir>/<YYYY-MM-DD>/<HH>.log`, both components in **UTC**, opened append-only, files 0644, date directories 0755. Rotation at the UTC hour boundary; flush after every message.
- On every connection: write the marker `[capture] connected broker=<broker> topic=<topic>`; on graceful shutdown: `[capture] shutting down`.
- Connection handling: clean session (missed messages while offline are simply absent, no queueing), auto-reconnect, re-subscribe on reconnect; subscribe failure logs an error and continues running.
- Retention: on rotation, delete whole date-directories older than `retention_hours` (default 72). **Documented consequence (actual behavior, not the idealized claim)**: cleanup granularity is whole days, so with the default the tool keeps between **72 and 96 hours** of logs. A reconstruction may fix the granularity but the PRD must not promise hour-precision deletion, and tooling must not assume it.
- Validation at startup: missing default config → run on defaults; malformed or explicitly-missing config → exit code 2; validation requires non-empty `broker`, `user`, `site`, `station`, `log_dir`, `retention_hours > 0`.
- Default config keys: `broker tcp://192.168.1.50:1883`, `user hf`, `password ""` (stored in the TOML for this service — no EnvironmentFile), `site muehle`, `station hf`, `log_dir /var/log/hf-mqtt-capture`, `retention_hours 72`.
- Known defect to not reproduce: the rotation path closes the old file **before** opening the new one; if the open fails (disk full, permission loss) the writer dereferences a null writer and crashes. A reconstruction shall open the new file before releasing the old one.

### 4.3 testui — web bus monitor & stimulator (deployed service, with a security mandate)

Purpose: a browser UI to watch and deliberately poke the bus — per-slot cards driven by each slot's `expose` descriptor, a message ticker, and command panels that can publish arbitrary commands, simulate retained state, and clear retained planes. The browser never talks MQTT; a relay process holds the broker credentials and the single subscription. It is a **dev/operator tool, explicitly not the operator console** (the console is the tablet app). Deployed on shari, HTTP on `0.0.0.0:8090`.

Behavior that **SHALL** survive into any replacement tool (these encode the bus contract):

1. The browser may publish **only under the configured site prefix** (`muehle/`); anything else → rejected ("topic outside configured site").
2. A publish topic's last path segment must be one of the four plane names `meta | state | status | cmd`; anything else → rejected ("unknown plane").
3. A **retained publish to `/cmd` is rejected** — matching the station-wide rule that commands are one-shot, never retained (see `02-interface-spec.md` §5).
4. Clearing a retained topic = zero-length retained publish at QoS 1.
5. Two-layer liveness rendering, three colors: red = slot's `/status` ≠ `online`; orange = bridge online but `/state.device_online` ≠ true (or a logic slot with no `device_online` field); green = `/state.device_online == true`. A replacement must not collapse the two layers.
6. Command-panel fallback order: expose-driven (writable fields / actions from `/meta.expose`) → role-registry fallback (the reconciler's operator-hold `request`; the antenna switch's `select`) → raw JSON editor. The command payload builder shall follow the station `/cmd` conventions exactly (documented deviations included).
7. After sending a command, watch the slot's `/state` for a change (fire-and-observe), not assume success.

**SECURITY — normative mandate.** The current tool has two defects that a re-implementation **MUST NOT** reproduce, and the PRD makes the fix mandatory rather than optional:

1. **A real-looking MQTT password is committed in the tool's bench config file in the repository** (with a comment saying it was fetched from the deployed host). This is a leaked credential: any reconstruction SHALL treat that value as compromised, SHALL NOT commit any real credential to any repository, and the operator SHALL rotate the `hf` MQTT password as part of the migration. The deployed path itself is correct (separate 0600 EnvironmentFile on the host, TOML carries no secret).
2. **The publish endpoint has no authentication and the HTTP listener binds the whole LAN** (`0.0.0.0:8090`): anyone on the LAN can publish arbitrary commands to the station bus through the tool. A re-implementation **SHALL mandate authentication on every publish-capable endpoint** (and/or bind only to an operations VLAN); unauthenticated LAN-wide publish surfaces are a blocking defect in any successor tool.

Other recorded behaviors (contract for a replacement): SSE snapshot-then-update event stream with the full slot map on connect and updates dropped for slow clients (a refresh re-syncs); MQTT client id deliberately distinct from any slot-derived bridge id; initial broker-connect failure is fatal to the process (the supervisor restarts it) — a replacement may instead serve the UI and show "reconnecting".

### 4.4 pelcotest — bench tool and its measured facts (not deployed)

`pelcotest` is a manual bench TUI used once to re-engineer the serial behavior of the UHF pan/tilt head. It is **never deployed and needs no reconstruction**; what must survive is the **knowledge** it produced, which is binding contract for any UHF-rotator bridge (full detail in `03-components/pelcobridge2.md`):

- The head **ignores absolute-position commands** as sent naively by the bench tool — position control needs a quiet-line protocol (and see §6 for the recorded bench-vs-bridge discrepancy on this point).
- **Tilt readback is unusable as an elevation value**: the manual's degrees×100 claim is false for the tilt reply word; no model of the word is verified. A bridge must not present it as elevation.
- **Readback while the tilt motor runs is checksum-valid garbage** — trustworthy only once the motor has halted; no checksum can filter it.
- The head is Pelco-D/Pelco-P **adaptive per received frame** (answers in the protocol the query arrived in).
- A protocol decoder needs the **gap-flush rule**: an incomplete frame is held only until a receive gap of ~1.5 frame times (minimum 20 ms) and then flushed as noise; without it, a truncated reply can merge with the next reply into a checksum-valid frame carrying a **fabricated position word** (observed on the bench).
- Bench parameters: RS-485 via USB-serial, 8N1, default baud 2400 (family documented 1200–9600 — ambiguous), Pelco address default 1; the Pelco-P address convention is an **unverified assumption**.
- Tool epistemics worth copying in any successor: raw hex first; vendor claims labeled as untrusted documentation; operator hypotheses labeled UNVERIFIED; destructive frames require explicit confirmation against the built frame bytes; the sweep recorder always transmits a stop frame before exit, including on interrupt.

---

## 5. Monitoring & failure runbook

### 5.1 Liveness model — how to read a component's health

Two distinct liveness layers, and **consumers AND operations must check both**:

1. **Bridge/process liveness** — `<slot>/status`, a plain retained string `online`/`offline` set by the MQTT last-will mechanism. Answers: *is the software component connected to the broker?*
2. **Device-link liveness** — `<slot>/state.device_online` (boolean inside the state snapshot, device slots only), conventionally paired with `<slot>/state.error` when false. Answers: *is the hardware behind the running bridge reachable?* (cable pulled, device powered off) while `/status` stays `online`.

Rules (normative; the failure that produced them was live):

- **SHALL**: any consumer or operator display acting on a slot's state shall require **both** layers before treating the slot as healthy. Keying on `/status` alone is a recorded live failure (the reconciler chattered when a device link died while its bridge stayed up); keying on `/state` alone trusts retained data that may be stale.
- **SHALL**: a wait-for-state consumer (e.g. the sequencer waiting for the PA to confirm power) shall also require the slot's `/status == online`, so a dead device cannot pass on a stale retained state.
- **Recorded actual behavior (not the idealized doc claim)**: on a **clean process shutdown the broker does not fire the last-will** — the bridge is supposed to publish `offline` itself, and if it does not (or is stopped between publishes), a **retained `online` can persist on `/status` for a stopped service**. Runbooks and dashboards must not trust `/status` alone; cross-check against the supervisor (`systemctl status` equivalent) or the heartbeat/state freshness. See §6 for the open decision this creates.
- **Heartbeat convention**: the PA-arm firmware republishes its (even identical) state snapshot every **10 s** as a slow heartbeat so its own absence is detectable. State freshness can substitute for liveness only when a component declares such a heartbeat; change-only publishers (see the radio, §5.3) provide no such signal.

### 5.2 Day-to-day operator access

- Service status, log tail, restart, and config edit on the service host, per service (reference: `systemctl status/restart <svc>`, `journalctl -u <svc> -f`, `sudo -e /etc/<svc>/config.toml` over `ssh io@192.168.1.139`).
- Log expectations: every bridge logs unknown/invalid command payloads and drops them (never crashes on bad intent); device-link failures are logged with a human-readable error that also lands in `/state.error`.
- The passive recorder (§4.2) is the black-box record for "what was on the bus"; the web test tool (§4.3) is the interactive check.

### 5.3 Known failure modes — symptoms, causes, remedies

Each row is observed live or on the bench in the reference deployment. A reconstruction's runbook SHALL cover the equivalent scenario for the equivalent component.

| # | Symptom | Cause | Remedy / required behavior |
|---|---|---|---|
| 1 | Antenna-controller bridge log: `write request: Input/output error`; bridge keeps running | USB FTDI serial adapter dropped and re-enumerated under a new device node; stale handle | **SHALL self-heal**: reopen the port by its stable by-id path on I/O error rather than dying (deployed behavior). Manual: replug + supervisor restart. |
| 2 | PA arm permit (`pa-arm.armed`) drops while the radio is idle-but-healthy | The arm relay de-energizes unless the radio's `/state` refreshes within **10 s**; the radio bridge publishes `/state` **only on change**, so an idle radio starves the heartbeat | Known live fragility. **Hard requirement for any reconstruction**: the radio bridge (or an equivalent mechanism) shall republish a heartbeat state at least every **≤5 s** so the 10 s window is never starved. Interim remedy: poke the radio or restart its bridge. |
| 3 | Reconciler repeatedly selects/deselects ("chatter") during a device outage | Consumer keyed on `/status` alone; device link died while bridge stayed up | Fixed by the two-layer rule (§5.1). Symptom class to regression-test. |
| 4 | A service hangs on shutdown during a broker outage; supervisor must force-kill | Connect attempt ignored the shutdown signal (§2.7 item 1) | **SHALL**: cancellable connect. |
| 5 | A bridge silently stops processing messages while connected | Message handler blocked on a synchronous publish; dispatch thread deadlocked (§2.7 item 2) | **SHALL**: handlers never block; queue + single worker. |
| 6 | `/status` still reads `online` for a service that is stopped | Clean shutdown publishes no last-will; retained `online` persists (§5.1) | Cross-check supervisor state; never gate safety on `/status`. |
| 7 | Sequencer reports `phase=idle`, `fault="<step>: <reason>"`, station partially powered | A wait step timed out (default deadline 120 s per step) | By design: **no rollback** — driven slots hold their last retained command; operator resolves the faulted slot and re-runs `start`/`stop`. |
| 8 | UHF rotator re-homes itself unprompted mid-session | The head's periodic self-check re-arms after the head power-cycles, and the current bridge sends the disable frame only **once per process start**, not once per reconnect | **SHALL (improvement over current)**: re-send the self-check-disable frame after every successful (re)open of the link. Manual remedy: restart the console with the head reachable. |
| 9 | UHF rotator link dead after adapter re-enumeration; no auto-recovery | Auto-heal retries once after a 2 s cooldown and gives up permanently on a failed reopen | Manual: the TUI's manual reopen key (always works). A reconstruction should retry indefinitely (throttled). |
| 10 | Console (tablet) shows offline forever after booting before the broker was up; taps do nothing | Initial-connect failure is swallowed with no retry loop; publishes while disconnected are silently dropped | Known console defect (`04-console.md`); remedy is restarting the app after the broker is reachable. A reconstruction shall add an app-level connect retry and surface dropped commands. |
| 11 | Bus recorder crashes on hour rollover under disk-full / permission loss | Rotation closes the old file before opening the new; null-writer panic (§4.2) | Fixed by open-before-close; check host disk space in monitoring. |
| 12 | Antenna switch to a port takes effect while RF is present → relay contact damage risk | Manual switching during transmit | Guarded in the console and reconciler (fail-closed RF guard); hardware cold-switch contract in `02-interface-spec.md` §6. Never switch under RF. |
| 13 | First key-up after an auto-ground goes "into the short" | Reconciler re-activation can race the switch settling after a ground event | Recorded live gap (project memory): after any grounded→selected transition, the operator shall confirm the switch reports `settled` before transmitting; reconstruction should close the race. |
| 14 | Tilt readback of the UHF head shows values like 224° or garbage while moving | Head firmware behavior (§4.4): tilt word is not elevation; readback mid-motion is checksum-valid garbage | Never trust the tilt word; only read back when the motor has halted. |

---

## 6. Open decisions & unresolved facts

These are unresolved by the sources; a reconstruction must decide each explicitly. Do **not** silently adopt either variant.

1. **Broker topology** (§1.5). Variant A: single broker at `192.168.1.50:1883` (all deployed code defaults). Variant B: shack-local broker on shari `192.168.1.139:1883`, authoritative for `muehle/#`, bridged to `.50` — implemented on an unmerged, undeployed feature branch (`feat/shack-local-mqtt-broker`, commit `cd58466`, planned resume ~2026-09-02), including a console-setup default repoint. Evidence both ways: every deployed default points at `.50`; the console project's local docs already name `.139`. The console itself is topology-agnostic (host is a config field); only defaults differ. **Decision needed before deploy tooling is finalized.**
2. **Ultrabeam switch port: 3 vs 4.** Repo-root documentation and the integration model say the Ultrabeam beam is on antenna-switch port 3 (fan-dipole port 6, dummy load port 1); the deployed reconciler seed config *and* the console's antenna map say port 4. The authoritative artifact — the live config on the Raspberry Pi — was not readable when this PRD was written. Port numbers are per-site configuration and must never be hard-coded; the physical truth requires on-device confirmation.
3. **`device_online` publication form.** The integration model says the field is "omitted when true"; the deployed bridges publish `device_online: true` explicitly. Consumers must treat both forms as equivalent (absence = true) — that consumer rule is normative — but a reconstruction must pick a producer convention (recommend: mandate explicit boolean for all device slots, and update the model).
4. **Clean-shutdown `/status`.** Actual behavior: retained `online` persists after a clean stop (no will on graceful disconnect), so `/status` can lie about a stopped service. A reconstruction must either (a) replicate actual behavior and rely on the §5.1 cross-checks, or (b) tighten the contract so a clean shutdown always publishes `offline` itself — and must not assume (b) holds of legacy peers during migration.
5. **Absolute-position commands on the UHF head.** The bench tool measured the head **ignoring** absolute-set opcodes; the production console's set ladder converges when sets are bracketed by quiet-line windows. The two research files disagree; likely reconciliation is that sets only land on a quiet line (the bench never provided one). Any re-implementation must bench-verify before relying on absolute sets, and must keep the jog/stop fallback.
6. **Committed testui credential.** A real-looking MQTT password sits in a repository config file (§4.3). It must be treated as leaked: remove from the repo, rotate the `hf` password on the broker. Decide whether the historical value needs revocation on the old broker before/after any migration.
7. **Recorder retention granularity** (§4.2): `retention_hours` is day-granular in effect (72–96 h actual retention at the default). Keep the day-directory scheme so ops tooling matches, or fix to true hourly — decide; do not promise hour-precision deletion either way. Also decide whether to widen the default capture filter to include the UHF subtree (currently unrecorded).
8. **Host-liveness nodes** (`muehle/host/shari`, `muehle/host/shack-pc`) are in the bus model but published by nothing. Implement a host-liveness publisher or remove the nodes from the model.
9. **PLC #2 firmware** (`muehle/uhf/pol-ctrl`) does not exist in the repo despite the slot table attributing it to the m5stamp project — implement or formally descope.
10. **Legacy service names** (`flexbridge`, `ultrabridge`) violate the naming convention; renaming touches live supervisor units and configs. A fresh reconstruction should simply use consistent names; only a migration of the existing deployment faces this decision.
11. **Secrets hardening ceiling.** Plaintext secrets in 0600 files is an accepted trade-off given the threat model (trusted LAN). If the threat model tightens (or the testui-style tooling stays deployed), move to supervisor-provided credentials (`systemd-creds`/`LoadCredential` equivalent).
12. **Pelco-P addressing on the UHF head** is an unverified assumption (the head may be zero-indexed for Pelco-P, in which case every P frame is addressed one unit off and silently ignored). Bench-verify before using Pelco-P at all; the default envelope is Pelco-D.