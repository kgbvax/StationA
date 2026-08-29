# Research spec — auxiliary projects: `hf-mqtt-capture/`, `testui/`, `pelcotest/`, `sas/`

Scope: the four auxiliary (non-slot) directories of the stationa monorepo at
`/Users/ingomar.otter/dev/stationa`. This document is written so a team using a
different technology stack can decide, for each tool, whether it must be
reconstructed, at what priority, and exactly what behavior a reconstruction must
reproduce. It is based on the actual code, not the READMEs; README/code
disagreements are called out.

Background terms (the reader is assumed to know nothing about amateur radio):
- **MQTT** — a lightweight publish/subscribe message bus. Clients publish JSON
  messages to hierarchical topic strings; the central **broker** (here at
  `192.168.1.50:1883`) fans messages out to subscribers. A **retained** message
  is kept by the broker and re-delivered to every new subscriber until
  overwritten or cleared with a zero-length publish.
- **The station bus** — all station components ("slots", e.g. `muehle/hf/radio`,
  `muehle/hf/pa`) publish four planes per slot: `/meta` (static identity +
  capability description), `/state` (retained JSON snapshot), `/status`
  (bare string `online`/`offline` via MQTT Last-Will), `/cmd` (commands, never
  retained). See `docs/station-integration-model.md`.
- **Ham/amateur radio station "Mühle"** — the physical radio site this whole
  ecosystem controls. **shari** is the Raspberry Pi (192.168.1.139) that runs
  all services.
- **Pelco-D / Pelco-P** — two related serial protocols for pan/tilt/zoom heads,
  originally for CCTV cameras; here used to steer a UHF antenna rotator
  (303Z/3050DZ pan/tilt head over RS-485).

---

## 0. Executive summary — maturity, deployment, re-construction priority

| Project | What it is | Maturity | Deployed? | Needed in a re-construction? | Priority |
|---|---|---|---|---|---|
| `hf-mqtt-capture/` | Passive MQTT bus recorder (logs every message to hourly files) | Small, complete, hardened deploy script | Yes — systemd service `hf-mqtt-capture` on shari | Optional diagnostic. Not part of station behavior, but extremely cheap and the only black-box record of bus traffic | **P3** (nice-to-have; build early if anything, it aids debugging the rest) |
| `testui/` | Web-based schema-aware MQTT monitor **and stimulator** (browser UI over an HTTP+SSE relay; can publish/clear arbitrary bus topics) | Working, feature-rich, **no tests** | Yes — systemd service `testui` on shari, LAN-served on `0.0.0.0:8090` | Dev/operator tool. Not part of station runtime, but its **publish-safety rules** (reject retained `/cmd`, site-prefix scoping) encode bus contract and any re-construction benefits from an equivalent bus-inspection tool | **P3** (as a tool); its safety rules are **contract** and must be preserved in whatever tool replaces it |
| `pelcotest/` (`ptest`) | Manual bench TUI + sweep recorder for re-engineering the 303Z/3050DZ rotor's serial behavior | Mature for its purpose (heavy test coverage: `protocol_test.go`, `sweep_test.go`, `ui_test.go`) | **No** — bench tool run on a workstation over a USB-serial adapter; never a service | Not needed as software. The **knowledge it produced** (below) is contract for any UHF-rotator bridge: the head ignores absolute-position commands, tilt readback is unusable, RX is D/P-adaptive, checksum-valid garbage while moving | **P4** (tool itself); the measured facts are **P1 contract** for the UHF rotator bridge |
| `sas/` | Design assets for the hf_console Flutter UI redesign: HTML mockups, design plan, PNG screenshots, design handoff + copied MQTT schemas | Static HTML/Markdown only; no runtime code | No — authoring material | No software. It is the **visual specification** for the operator console; a re-construction needs the screenshots/HTML as design input | **P3** (design input for the console UI, not for bus behavior) |

None of the four is a bus slot; none publishes `/meta`, `/state`, `/status` or
`/cmd` of its own. They are observers, test drivers, and design material.

---

## 1. `hf-mqtt-capture/` — passive MQTT traffic recorder

### 1.1 Purpose & role

A pure diagnostic: it subscribes to the station MQTT subtree and writes every
message, verbatim, to timestamped log files on shari. It never publishes, has no
command surface, and no other component depends on it. It exists because when a
bridge misbehaves live, the question "what actually appeared on the bus in the
last hour?" otherwise has no answer. The README states this exactly and the code
agrees.

Single Go binary, module `hf-mqtt-capture`, in the workspace `go.work`; imports
`codeberg.org/kgbvax/stationa/shared/mqtt` (via `replace … => ../shared`) only
for the context-aware `Connect` helper.

### 1.2 Upstream interface

- **Transport**: MQTT over TCP to the broker, default `tcp://192.168.1.50:1883`,
  user `hf`, password from the config file.
- **Subscription**: one subscription, topic `{site}/{station}/#` (default
  `muehle/hf/#`), **QoS 1**. Note: the default config captures only the HF
  subtree, not `muehle/uhf/...` — UHF traffic is not recorded by default.
- Client options (exact): `ClientID = "{site}-{station}-hf-mqtt-capture"`
  (default `muehle-hf-hf-mqtt-capture`), `CleanSession = true`,
  `AutoReconnect = true`. No LWT (it publishes nothing, so no one needs to
  monitor its liveness), no keepalive override.
- Subscribe happens in the **OnConnect** handler, so a reconnect re-subscribes.
  On subscribe failure it logs an error and continues running (does not exit).
- On every connection it writes a marker line into the current log file:
  `[capture] connected broker=<broker> topic=<topic>` (empty topic field).
- Connection loss is handled by paho auto-reconnect plus a `ConnectionLost`
  log line; messages received while offline are simply absent from the log
  (CleanSession true means no queueing).

### 1.3 MQTT presence

- Subscribes: `muehle/hf/#` (QoS 1). Publishes: **nothing**, ever. There is no
  LWT and no heartbeat. BEHAVIOR CONTRACT: a reconstruction must remain
  publish-silent — a diagnostic that emits traffic changes the thing it
  observes.

### 1.4 Command surface

None. The only flags are `-config <path>` (default
`/etc/hf-mqtt-capture/config.toml`). No HTTP, no signals beyond
SIGINT/SIGTERM (graceful stop: writes `[capture] shutting down` marker,
disconnects with 250 ms grace, flushes and closes the file).

### 1.5 Behavior & state machine

1. Load config (missing file ⇒ use built-in defaults; malformed file ⇒ exit
   code 2; validation failure ⇒ exit 2). Validation requires non-empty
   `broker`, `user`, `site`, `station`, `log_dir` and `retention_hours > 0`.
2. Create the log directory tree (`MkdirAll`, mode 0755) and open the current
   hour's file.
3. Connect to the broker (context-aware; SIGINT/SIGTERM abort the connect
   attempt). Connect failure ⇒ exit 1.
4. On connect: write the `[capture] connected …` marker, subscribe QoS 1.
5. Per message: rotate if the UTC hour changed, then write one line and flush
   immediately (unbuffered in effect — one `bufio.Flush` per message).
6. On SIGINT/SIGTERM: write `[capture] shutting down`, disconnect, close.

**Log line format** (BEHAVIOR CONTRACT — a replay/analysis tool must be able to
parse it):

```
<RFC3339Nano UTC timestamp> <topic> <raw payload bytes verbatim>\n
<RFC3339Nano UTC timestamp> <marker text>\n          # when topic is empty
```

Example from README:
`2026-08-19T10:23:45.123Z muehle/hf/radio/state {"freq_hz":14175000,"band":"20m","tx":"rx"}`

**File layout** (BEHAVIOR CONTRACT): `<log_dir>/<YYYY-MM-DD>/<HH>.log` where
both components are the **UTC** hour at write time; files are opened
`O_CREATE|O_WRONLY|O_APPEND` mode 0644, date directories 0755. Rotation
boundary: `time.Now().UTC().Truncate(time.Hour)`.

**Retention**: on every rotation, date directories whose name parses as
`2006-01-02` and whose date is before `now - retention_hours` (truncated to
hour, then truncated to 24 h for the comparison) are removed whole with
`RemoveAll`. IMPLEMENTATION DETAIL with a visible consequence: despite the
`retention_hours` name, cleanup is **whole-day granularity** — all hours of the
oldest retained day survive together. With the default 72 h this means between
72 and 96 hours of logs are actually kept. A reconstruction may keep the exact
day-directory scheme (recommended, so ops tooling matches) or fix the
granularity, but the PRD should not promise hour-precision deletion.

### 1.6 Configuration

`/etc/hf-mqtt-capture/config.toml` (TOML, 0600, owned by the service user).
All keys, with defaults from `internal/config/config.go`:

| Key | Default | Meaning |
|---|---|---|
| `broker` | `tcp://192.168.1.50:1883` | MQTT broker URL |
| `user` | `hf` | MQTT username |
| `password` | `""` | MQTT password — **stored directly in the TOML** (unlike testui, which uses an EnvironmentFile) |
| `site` | `muehle` | first topic segment |
| `station` | `hf` | second topic segment; captures `{site}/{station}/#` |
| `log_dir` | `/var/log/hf-mqtt-capture` | root of the hourly log tree |
| `retention_hours` | `72` | log retention (see granularity caveat above) |

### 1.7 Deployment

`deploy.sh` cross-compiles `GOOS=linux GOARCH=arm64 CGO_ENABLED=0` (trimmed,
stripped) to `dist/hf-mqtt-capture-linux-arm64`, scps to shari
(`io@192.168.1.139` default), installs to `/opt/hf-mqtt-capture`, and installs
a systemd unit with: `After/Wants=network-online.target`, `Type=simple`,
`Restart=on-failure`, `RestartSec=5`, dedicated system user/group
`hf-mqtt-capture` (nologin, no home), `ConfigurationDirectory=hf-mqtt-capture`,
`NoNewPrivileges=true`, `ProtectSystem=full`, `ProtectHome=true`,
`PrivateTmp=true`, `ReadWritePaths=/var/log/hf-mqtt-capture`. Config is seeded
once (0600); later deploys never touch it. If the seeded password is empty,
the script pulls the shared `hf` MQTT password **on-device** from the first
readable `*MQTT_PASSWORD=` line of, in order:
`/etc/acom1200s-pa-bridge/acom1200s-pa-bridge.env`,
`/etc/flexbridge/flexbridge.env`,
`/etc/hadiscovery/hadiscovery.env`,
`/etc/atr1k-tuner-bridge/atr1k-tuner-bridge.env` — and injects it into the
TOML via an on-device python3 one-liner. No secret ever rides the command line.

### 1.8 Invariants & safety rules

- Never publishes to the broker (contract).
- Never modifies the messages it logs — payload bytes are written verbatim, not
  re-serialized.
- Rotation failure must not drop the message (intent; see fragility below).

### 1.9 Known defects & fragilities

- **Nil-writer panic path**: `rotateIfNeeded` closes the old file and sets
  `w.file = nil; w.bw = nil` *before* opening the new one. If the new open or
  the `MkdirAll` then fails, `writeLog` proceeds to `fmt.Fprintf(w.bw, …)` on a
  nil `*bufio.Writer` — a panic. The "keep going, do not drop the message"
  comment cannot be honored in that path. Rare (disk-full / permission loss
  only) but real.
- Retention granularity is days despite the `retention_hours` name (above).
- The `[capture] connected` / `[capture] shutting down` markers share the
  message line format with an empty topic field; a naive parser that splits on
  two spaces and expects a topic would choke. Documented here as contract.
- README example (`docs` claim "retains the last 72 hours") slightly overstates
  what day-granularity cleanup actually guarantees.

### 1.10 Re-implementation notes

**Preserve verbatim**: publish-silence; subscription topic pattern
`{site}/{station}/#` QoS 1; the log line format
(`<RFC3339Nano-UTC> <topic> <payload>`); the `<log_dir>/YYYY-MM-DD/HH.log`
UTC layout with 0644/0755 modes; the connected/shutdown marker lines; flush
per message; seed-once 0600 config with on-device password injection; the
systemd hardening set.
**Free to change**: language; the day-granularity retention (may fix to true
hourly); buffering strategy; adding a `uhf` capture instance (today UHF is not
recorded at all — a reconstruction with a single site-wide capture would be an
improvement, not a regression, as long as `muehle/hf/#` remains covered).

---

## 2. `testui/` — schema-aware MQTT monitor + stimulator (web)

### 2.1 Purpose & role

A web UI for humans to watch and (deliberately) poke the station bus: live
per-slot cards driven by `/meta.expose`, a message ticker, a station tree, and
command panels that can publish arbitrary `/cmd`, simulate retained `/state`,
and clear retained planes. The browser never talks MQTT; a Go relay process
holds the broker credentials and the single subscription. It is a dev/operator
tool — a "bus oscilloscope with a send button" — and is explicitly **not** an
operator console (that role belongs to the Flutter `hf_console` tablet app).

Not a Go-workspace member: module `testui`, standalone `go.mod` (paho +
go-toml only, no `shared/` import), built with `GOWORK=off`. `Makefile`:
`run` (build to `./testui`, run with `config.toml`), `build-linux`
(arm64 to `dist/testui-linux-arm64`).

### 2.2 Upstream interface

MQTT broker via paho, plus an inbound HTTP listener for browsers.

**MQTT client options (exact)**: broker from config; `ClientID` from config
(default `testui`, deliberately distinct from any slot-derived bridge client
ID); `CleanSession = false` (persistent session — a reconnect resumes missed
QoS-1 deliveries); `AutoReconnect = true`; `MaxReconnectInterval = 30 s`;
`OrderMatters(false)`; initial connect with `WaitTimeout(10 s)` and
`log.Fatalf` on failure (the process exits and lets systemd restart it).
Subscribes `{site}/#` (`muehle/#`, QoS 1) in the OnConnect handler — this
covers **both HF and UHF** stations, unlike hf-mqtt-capture's default.

### 2.3 MQTT presence

- Subscribes: `muehle/#` (QoS 1). Publishes: only what a human explicitly
  triggers via the HTTP API (see command surface). It has **no `/meta`,
  `/state`, `/status`, `/cmd` of its own** — it is not a slot.
- BEHAVIOR CONTRACT (safety rules enforced by the relay, must survive into any
  replacement tool):
  1. The browser may only publish under the configured site prefix
     (`muehle/`); anything else → HTTP 400 "topic outside configured site".
  2. A publish topic's last path segment must be one of
     `meta | state | status | cmd`; anything else → HTTP 400 "unknown plane".
  3. **A retained publish to `/cmd` is rejected** (HTTP 400, "retained publish
     to /cmd is rejected (integration model §8)") — matches the station-wide
     rule that commands are one-shot, never retained.
  4. Clearing a retained topic is done with a zero-length **retained** publish
     at QoS 1 (the standard MQTT retained-clear).

### 2.4 Command surface (HTTP API, all exact)

| Endpoint | Method | Body | Behavior |
|---|---|---|---|
| `/api/stream` | GET (SSE) | — | Opens a Server-Sent-Events stream. First event: `event: snapshot` with the full bus model JSON (below), then one `event: update` per bus message. Headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Accel-Buffering: no`. Client channel buffer 16; **a slow client's updates are silently dropped** (never blocks the bus) — a refresh re-syncs via the snapshot. |
| `/api/publish` | POST | `{"topic": string, "payload": string-or-JSON-value, "retain": bool, "qos": byte}` | Validates rules 1–3 above. If `payload` is a JSON string it is published as raw bytes; otherwise it is `json.Marshal`ed first (so the browser sends objects and the bus sees JSON). Publishes at the requested QoS; broker error → HTTP 502. Success: `{"ok":true,"retained":<bool>,"topic":"..."}`. |
| `/api/clear` | POST | `{"topic": string}` | Publishes zero-length retained payload at QoS 1 to the given topic (site-prefix checked). Success: `{"ok":true,"cleared":true,"topic":"..."}`. |
| `/healthz` | GET | — | Returns `ok`. |
| `/` | GET | — | Static files: from a `static/` dir on disk next to the binary (preferred, so edits don't need a rebuild) or embedded `go:embed` assets otherwise. |

**Bus model JSON** (both snapshot and update; field names exact):

```jsonc
// snapshot event
{ "order": ["muehle/hf/radio", …],           // slot addresses in first-seen order
  "slots": [ { "address": "muehle/hf/radio",
               "meta":    PlaneMsg|null, "state":  PlaneMsg|null,
               "status":   PlaneMsg|null, "cmd":    PlaneMsg|null }, … ] }

// PlaneMsg
{ "topic": "muehle/hf/radio/state",
  "payload": <decoded JSON value, or raw string for non-JSON payloads>,
  "retained": false,
  "ts": "2026-08-29T10:23:45.123456789Z",     // relay receive time, RFC3339Nano UTC
  "object": <same as payload, omitted for non-JSON> }

// update event
{ "address": "…", "plane": "state", "cleared": true?, …PlaneMsg }
```

Slot address = topic minus its last (plane) segment. An **empty payload**
(= retained-clear) sets that plane to null and emits an update with
`"cleared": true` and `"payload": null`. Non-plane topics under the site
prefix are ignored entirely.

### 2.5 Behavior & state machine

1. Load TOML config (path from `-config` flag, default `config.toml`);
   `TESTUI_MQTT_PASSWORD` env var overrides the file's `[mqtt] password` (this
   is how the systemd EnvironmentFile injects the secret).
2. Defaults if empty in config: `http_addr = "127.0.0.1:8090"`,
   `site = "muehle"`.
3. Connect MQTT (fatal after 10 s), subscribe in OnConnect.
4. Serve HTTP forever. Incoming MQTT messages flow through a 256-slot channel
   into a single `Bus` goroutine (serialized; no locks on the hot path beyond
   the model mutex), updating the in-memory slot map and broadcasting to SSE
   clients.

### 2.6 Configuration

`/etc/testui/config.toml` on shari (0600, seed-once). Keys:

| Key | Default | Meaning |
|---|---|---|
| `http_addr` | `127.0.0.1:8090` (code) / **`0.0.0.0:8090` seeded by deploy** | HTTP listen address. Deploy binds the LAN so a workstation browser reaches `http://shari:8090`. |
| `site` | `muehle` | subscribe `{site}/#`; browser may only publish under `{site}/` |
| `[mqtt] broker` | — (deploy seeds `tcp://192.168.1.50:1883`) | broker URL |
| `[mqtt] client_id` | `testui` | must stay distinct from any slot-derived bridge client ID |
| `[mqtt] user` | `hf` | MQTT username |
| `[mqtt] password` | `""` | overridden by `TESTUI_MQTT_PASSWORD` from `/etc/testui/testui.env` (0600 EnvironmentFile) — the TOML itself carries no secret on the target |

### 2.7 Deployment

`deploy.sh` mirrors hf-mqtt-capture's pattern (arm64 cross-build, scp, seed-once
config + seed-once env file, dedicated `testui` user) but with a
**substantially harder systemd unit**: `ProtectSystem=strict`,
`PrivateDevices`, `ProtectKernelTunables/Modules/ControlGroups`,
`RestrictAddressFamilies=AF_INET AF_INET6`, `RestrictNamespaces`,
`LockPersonality`, `RestrictRealtime`, `RestrictSUIDSGID`, `RemoveIPC`,
empty `CapabilityBoundingSet`/`AmbientCapabilities`,
`StateDirectory=testui`, `ReadWritePaths=/var/lib/testui`, plus resource
ceilings `MemoryMax=128M` and `TasksMax=64` (the bus tree is in-memory; the
cap exists so a runaway cannot OOM the shared Pi). `Restart=on-failure`,
`RestartSec=5`.

### 2.8 Frontend behavior (BEHAVIOR CONTRACT for any replacement tool)

The single-page app (`static/app.js`, ~1000 lines, no framework) encodes real
station semantics that must survive:

- **Liveness rail, three colors**: red = slot's `/status` ≠ `online` (bridge
  down); orange = bridge online but `/state.device_online` ≠ true (or a logic
  slot with no `device_online` field — antenna-select, discovery, power-seq);
  green = `/state.device_online === true`. This mirrors the station's
  two-layer liveness rule (bridge LWT vs device link) — a replacement must not
  collapse the two.
- **Band table** (Hz, from `docs/conventions/band-mode-reference.md`; used to
  derive a band pill next to `freq_hz` and to warn when the bus's `band`
  disagrees with the derived one): 160m 1.8–1.999999 MHz; 80m 3.5–3.999999;
  60m 5.3515–5.3665; 40m 7.0–7.299999; 30m 10.1–10.149999; 20m 14.0–14.349999;
  17m 18.068–18.167999; 15m 21.0–21.449999; 12m 24.89–24.989999; 10m
  28.0–29.699999; 6m 50–53.999999; 2m 144–146; 70cm 430–440. Fallback label
  outside these: `band-<rounded MHz>`. Canonical modes: `cw, usb, lsb, am, fm,
  data` (unknown mode renders as a warn pill).
- **Command panel, three fallback tiers**: (1) expose-driven — each
  `/meta.expose.fields[]` with `writable`+`command` renders a setpoint row,
  each `expose.actions[]` an action button; (2) role-registry fallback for
  `role == "reconciler"` (antennaselect operator-hold: `request` ∈
  `auto|off|port1..port6`, value-key-only payload `{"request": …}`, never
  retained) and slot-name-registry fallback for `ant-switch` (no `/meta` at
  all; button grid sending value-key-only `{"select":"portN"}` with options
  `off,port1..port6`); (3) raw JSON editor for anything unknown. Role
  `discovery` (hadiscovery) has no `/cmd` handler — its command panel is
  hidden entirely (`NO_CMD_ROLES = ['discovery']`).
- **`/cmd` payload builder (contract — mirrors the station-wide convention)**:
  descriptor with both `action` and `value_key` ⇒ `{"action":<a>, <value_key>:<v>}`;
  value-key only ⇒ `{<value_key>:<v>}`; action only ⇒ `{"action":<a>}`. Typed
  coercion per `value_type`: `int`/`float`/`bool`/enum. Actions with an empty
  `value_key` are pure buttons and must send **no** `value` key (sending
  `null` historically arrived as `""` and the bridge dropped it — the live
  "ATR not following" bug).
- **Fire-and-observe acknowledgment**: after sending, the button watches the
  slot's `/state` receive-timestamp for change, polling every 150 ms for up to
  3 s; change ⇒ `ack` state (flashes 1.5 s), timeout ⇒ `timeout` state.
  Commands that legitimately don't change `/state` (e.g. tuner retract) still
  show timeout — known and accepted.
- **Tools panel** per slot: edit-and-publish retained `/state` (simulates a
  bridge, for testing consumers), clear-retained buttons for `state`, `meta`,
  `cmd` (each behind a `confirm()`), and a raw four-plane view.
- SSE reconnect: on error, close and retry after 2 s. Ticker keeps the last 40
  messages. Cards are updated incrementally so an open dropdown survives
  ~10 Hz `/state` bursts from the PA (the "chooser-snap" bug, fixed).
- State values are rendered via DOM textContent, never `innerHTML`, because
  payload data is untrusted bridge output (XSS hardening worth preserving).

### 2.9 Known defects & fragilities

- **A real-looking MQTT password is committed in `testui/config.toml`** in the
  repo (with a comment saying it was fetched from shari). This is a leaked
  credential and a hygiene defect; the deploy path is correct (separate
  EnvironmentFile on the device), the checked-in bench config is not.
- The config file's header comment references `config.example.toml` and
  `make run`; the example file does not exist (README/code drift).
- Slow SSE clients silently miss updates until they reconnect (by design, but
  a replacement could offer a seq/catch-up).
- Initial MQTT connect failure is fatal (process exits; systemd restarts).
  During broker outage the UI's HTTP side also dies with it — acceptable for a
  dev tool, but a replacement could serve the UI statically and show
  "reconnecting" instead.
- No tests at all (single `main.go`, logic embedded in HTTP handlers).
- Deploy binds `0.0.0.0:8090` with **no authentication** on `/api/publish` —
  anyone on the LAN can publish arbitrary commands to the station bus through
  it. This is the single most important safety caveat of the tool; a
  reconstruction should at minimum document it, ideally add auth or bind to
  the ops VLAN only.

### 2.10 Re-implementation notes

**Preserve verbatim**: the three publish-safety rules (§2.3); the bus model
JSON shapes (§2.4); the two-layer liveness coloring; the `/cmd` payload
builder rules; the expose-driven → registry → raw fallback ordering; the
`{"action","value"}`-convention warnings; retained `/state` simulation and
retained-clear tooling (this is the tool's whole value as a bus test bench).
**Free to change**: language, framework, SSE vs WebSocket, the fatal-on-connect
behavior, card layout, and (recommended) adding authentication to the publish
endpoint. **Not needed at all** if the new stack ships an equivalent
bus-inspection/stimulation tool; but some such tool is strongly recommended
for bring-up and debugging of the re-construction itself.

---

## 3. `pelcotest/` (`ptest`) — manual bench TUI for the 303Z/3050DZ rotor

### 3.1 Purpose & role

A deliberately-minimal, deliberately-manual bench tool for re-engineering and
verifying the serial behavior of the **303Z/3050DZ PTZ pan/tilt head** — the
physical UHF rotator that `pelcobridge2` drives in production. It shares no code
with the bridge. Its product is not control but **knowledge**: it was used to
establish what the head actually does on RS-485 (versus what the vendor manual
claims), and its sweep recorder produces CSVs pairing known mechanical moves
with raw reply bytes.

Go module `ptest`, in `go.work`. TUI stack: charmbracelet bubbletea/bubbles/
lipgloss; serial via `go.bug.st/serial`. Heavy test coverage
(`protocol_test.go`, `sweep_test.go`, `ui_test.go`) — the most
test-covered component in this survey. Companion `cmd/ptest-mock` is a canned
rotor for loopback testing without hardware (pan modeled as real hundredths of
a degree; tilt reply emits a raw configurable word, default 0x8E90, because no
tilt model exists to model).

### 3.2 Upstream interface (the device, exact)

- **Transport**: serial, 8 data bits, no parity, 1 stop bit (8N1), over a
  USB-serial adapter to the head's RS-485 line. `-baud` default **2400**; the
  family is documented 1200–9600 and third-party controllers default to 9600 —
  if RX shows only unframed bytes, try other rates. `-addr` default **1**
  (Pelco-D address; manual's DIP range 0–64, flag accepts 0–255).
- **Pelco-D frame (7 bytes)**: `FF addr cmd1 cmd2 data1 data2 checksum`;
  checksum = `(addr+cmd1+cmd2+data1+data2) & 0xFF`. `cmd1` is `0x00` for
  every command this head documents.
- **Pelco-P frame (8 bytes)**: `A0 addr cmd1 cmd2 data1 data2 AF checksum`;
  checksum = XOR of bytes 0–6.
- The head is "Pelco-D/Pelco-P adaptive": it detects the protocol **per
  received frame** (start byte `0xFF` vs `0xA0`) and answers in the protocol
  the query arrived in. ptest's RX side therefore always decodes both; TX
  framing is a mode (`-p` flag, `p` key at runtime).
- ⚠ The Pelco-P **address convention is an unverified assumption**: strict
  Pelco-P gear is zero-indexed; ptest assumes this head is not. If it is
  zero-indexed, every P frame is addressed to unit *n+1* and silently ignored
  — which would masquerade as "does not answer Pelco-P".

**Command set (exact, from `protocol.go`, straight from the vendor doc
sheet)**:

| Menu entry | cmd2 | d1/d2 | Notes |
|---|---|---|---|
| tilt query ("41") | `0x53` | — | reply cmd2 `0x5B` |
| pan query ("43") | `0x51` | — | reply cmd2 `0x59` |
| tilt set ("42") | `0x4D` | deg×100 big-endian in d1/d2 | **IGNORED by the head** (confirmed live) |
| pan set ("44") | `0x4B` | deg×100 | **IGNORED by the head** (confirmed live) |
| defaults+self-test ("40") | `0x07` | d2 `0x7D` | destructive, y/n confirm; self-test re-homes the head |
| stop | `0x00` | — | stop all movement |
| up / down | `0x08` / `0x10` | tilt speed byte in d2 | speed `00`–`3F`, default `0x20` |
| left / right | `0x04` / `0x02` | pan speed byte in d1 | only one axis per frame, other byte `0x00` |
| preset set N | `0x03` | N in d2 | N 0–255 |
| preset call N | `0x07` | N in d2 | N 0–255; **d2 `0x7D` = defaults+self-test, d2 `0x78` = clear all presets — both y/n-confirmed by a check on the built frame**, not the menu label |
| 105 set/call | `0x03`/`0x07` | d2 `0x69` | disable / enable PTZ self-check |
| 17/18 set, 19 call | `0x03`/`0x07` | d2 `0x11`/`0x12`/`0x13` | limit-scan left/right start points, start limit scan |
| 22 set | `0x03` | d2 `0x16` | set line-scan speed |
| 70/71/72 call | `0x07` | d2 `0x46`/`0x47`/`0x48` | cruise stop time 5/10/15 s |
| 83/84 call | `0x07` | d2 `0x53`/`0x54` | cruise line 1 (presets 1–16) / line 2 (33–48) |
| 89 set/call | `0x03`/`0x07` | d2 `0x59` | max-angle line set / scan |
| AUX1 on / AUX2 off | `0x09` / `0x0B` | aux number 0 | |
| 110 set/call | `0x03`/`0x07` | d2 `0x6E` | guard position on/off |
| 111 call | `0x07` | d2 `0x6F` | guard return time — doc sheet's "29<N<251 s" is NOT an in-frame parameter (an earlier ptest bug wrote the seconds over the selector byte) |
| 120 call | `0x07` | d2 `0x78` | clear all presets, y/n confirm |
| raw frame | — | — | 7 (D) or 8 (P) hex bytes sent exactly as typed, never re-framed |

**Position readback semantics (contract-grade facts, established with this
tool)**:

- **Pan reply (cmd2 `0x59`)**: big-endian 16-bit word = degrees × 100
  (Pelco-standard). Treated as valid.
- **Tilt reply (cmd2 `0x5B`)**: meaning **UNKNOWN**. The manual's
  degrees×100 claim is false (readings render far outside the 0–90° travel,
  e.g. word 0x57B8 → "224.56°"). A linear raw-encoder-count model
  (`raw = raw_at_0 + 355.878·elevation`) was fitted, then **contradicted on
  the bench 2026-08-27**: elevation does not appear in the word at all.
  Additionally, while the tilt motor is running the readback is a stream of
  **checksum-valid garbage** — trustworthy only once the motor has halted; no
  checksum test can filter it. **Any UHF-rotator bridge re-construction must
  not use the tilt word as an elevation readout**; `ptest` renders it as raw
  hex + the manual's claim, explicitly flagged, plus a pure-observation
  `Δ prev tilt` count line.
- The head **ignores absolute set-position opcodes `0x4B`/`0x4D`** — so
  position control must be open-loop jog/stop (which is what pelcobridge2's
  interactive TUI does), and set-then-read-back cannot be used to probe the
  encoding.

### 3.3 Command surface (CLI flags, exact defaults)

`-port` (required unless `-list`), `-list`, `-addr 1`, `-baud 2400`, `-p`
(Pelco-P TX), `-tilt-cal "raw_at_0,raw_at_90"` (optional hypothesis, always
logged as `hyp:` + `UNVERIFIED`; off by default).

Sweep mode flags: `-sweep up|down`, `-sweep-out tilt-sweep.csv`,
`-sweep-move 1s`, `-sweep-settle 200ms`, `-sweep-post-tx 50ms`,
`-sweep-reply-wait 2s`, `-sweep-stable 3`, `-sweep-max-steps 200`,
`-sweep-speed 0x20` (must be `0x00`–`0x3F`; constraint: `sweep-move` ≥
`sweep-post-tx`).

TUI keys: ↑/↓ or j/k select; Enter send; Tab cycle panes; Esc cancel pending
parameter; `p` toggle TX D/P; g/G/home/end; pgup/pgdn scroll log; Ctrl+R
reopen serial port after read error (USB re-enumeration; header shows
`RX DEAD`); Ctrl+L clear log; Ctrl+C/Ctrl+Q quit.

### 3.4 Behavior & state machine

**TUI** (strictly manual — nothing is sent on a timer, nothing polls; every TX
is a keypress):

- RX assembler: syncs on start byte (`0xFF`→7-byte D, `0xA0`→8-byte P);
  validates the matching checksum; on bad checksum drops the leading byte and
  rescans. Rejected bytes are reported as `?? unframed` **in wire order**
  (never withheld — they used to buffer invisibly and look like "no answer"),
  chunked at 256 bytes per Feed. An incomplete frame is held only until the
  next receive gap = **~1.5 frame times at the configured baud** (min 20 ms:
  `frameLen*10 bits / baud × 1.5`), then flushed as `?? partial frame (gap)`.
  Without that bound a truncated reply merged with the next reply and — when
  the lost byte happened to be `0xFF` — produced a **checksum-valid frame
  with a fabricated position word**. This gap-flush behavior is contract for
  any protocol decoder for this head.
- Input hardening (each fixes a real past bug, keep the behavior): hex fields
  must be exactly two digits ("5" is rejected, not zero-extended); degree
  input rejects trailing junk (decimal comma "45,5" used to send 45.00°) and
  NaN; preset input rejects "0x7D"-style junk; every decode line is bounded
  to 50 columns so narrow terminals can't silently truncate the checksum
  verdict; short serial writes are errors; destructive frames are confirmed
  against the **built frame bytes**, not the menu label.

**Sweep recorder** (`-sweep`, the one automated mode, outside the TUI by
design). Per step: TX jog(dir, speed) → wait postTX (counted inside motor-on
time so total on-time == `-sweep-move`) → TX stop → wait postTX → wait settle
(200 ms default; readback trustworthy only halted) → TX tilt query (`0x53`)
→ wait postTX → read reply (bounded by `-sweep-reply-wait`). Loop ends when
the tilt word is identical for `-sweep-stable` consecutive readings (a
**missing reply breaks the run** rather than counting as stable) or at
`-sweep-max-steps`. CSV columns (exact header):
`step,iso_time,elapsed_ms,dir,motor_on_ms,reply_ms,tx_hex,rx_hex,chk_ok,word_dec,word_hex,d1_dec,d2_dec,delta_counts,note`.
Missing-reply steps leave value columns **empty** with the reason in `note`
(never a misleading 0); noise, partials and other-opcode frames are noted, so
nothing seen on the wire is dropped. File flushed after every step.
**Safety invariant**: a stop frame is always transmitted before the process
exits, including on SIGINT (which also flushes/closes the CSV, exit code
130) — the rotator is never left jogging. A stop-on-exit is a one-way latch so
it is sent exactly once.

### 3.5 Configuration / deployment

No config file, no systemd, no deploy script — flags only, run from a
workstation (`ptest`, `ptest -port /dev/tty.usbserial-… -addr 1`; Windows
cross-compile supported). Docs: vendor manual and (Chinese-language) command
sheet live in `pelcotest/docs/`, plus a survey of open-source 3050 PTZ
implementations. Bench artifacts `tilt-up.csv` / `tilt-sweep.csv` are checked
in.

### 3.6 Invariants & safety rules

- Never transmit on a timer in the TUI (contract of the tool's trust model).
- Always stop the motor on exit (sweep mode; contract).
- Never present a decode as measured fact — raw hex first, vendor claims
  labeled `doc:` (untrusted), operator hypotheses labeled `hyp:` + `UNVERIFIED`
  (contract of the trust model; also the right epistemics for any successor).
- Destructive frames (defaults+self-test, clear-presets, including via the
  plain preset-call-by-number path) require explicit y/n.

### 3.7 Known defects & fragilities

- All the "known defects" here are in the **device**, and the tool exists to
  document them: unusable tilt readback; ignored absolute-position opcodes;
  garbage-while-moving; unverified Pelco-P addressing; ambiguous baud.
- Tool-side fragilities: `-tilt-cal` hypothesis output can still be misread by
  a human despite the UNVERIFIED labeling; RX read errors leave the port dead
  until a human presses Ctrl+R (deliberate: nothing automatic).

### 3.8 Re-implementation notes

**The tool itself does not need re-construction** (P4) — it is a one-time
re-engineering instrument whose findings are captured in §3.2/§3.4 above and
in pelcobridge2's research. What **must** be preserved verbatim in any UHF
rotator bridge: the D/P frame formats and checksums; the command opcode table;
adaptive-per-frame RX; the gap-flush assembler behavior (~1.5 frame times) so
truncated replies can't merge into checksum-valid fabrications; never trust
the tilt word; never rely on `0x4B`/`0x4D` (jog/stop only); stop-before-exit;
destructive-preset confirmation; and the unverified-status of Pelco-P
addressing.

---

## 4. `sas/` — design assets for the hf_console redesign

### 4.1 Purpose & role

`sas/` (the name is opaque; treat it as "stationa design studio") contains the
design exploration for the **hf_console** Flutter app — the fixed-mount Android
tablet on the operating desk that is the station's primary operator console.
There is no runtime code: everything is static HTML mockups (each a
self-contained 1280×800 tablet frame with inline CSS and a small script for
the compass drawing), PNG renders, one Markdown plan, and a
`design_handoff_mobile_console/` bundle (a 1512-line "Design Component"
prototype `Station Dashboard.dc.html`, a thorough README-style handoff, and
authoritative copies of the per-bridge MQTT API schemas under
`mqtt-schemas/`). For a console PRD, `sas/` is the **visual and interaction
specification**; the behavior it depicts is defined by the MQTT contracts (the
`mqtt-schemas/` copies and `docs/station-integration-model.md`).

Two generations exist:

1. **First round — the redesign plan** (`hf_console_redesign_plan.md` plus
   `hf_console_design_a_shop_panel.html`, `…_b_logbook_strips.html`,
   `…_c_split_deck.html`, and `hf_console_color_variations.html`): three
   information-architecture directions, all sharing the same data/controls:
   - **A "Shop Panel"** — utilitarian wall-panel metaphor: big flat bordered
     modules in a 2×2-ish grid (rotator/PA/antenna/tuner + ultrabeam band),
     chunky tactile buttons (0–2 px radius), all-caps letter-spaced headers,
     dark palette `#15171A`/`#1E2126` or light `#F2F0EA`/white. Fastest
     scanning, everything visible; risk: crowded on 10″, can read unfinished.
   - **B "Logbook Strips"** — ham-logsheet/ATC-strip metaphor: each subsystem
     is one full-width horizontal band (label column / data column / action
     column) with ruled hairlines; serif/slab labels against monospace
     readouts; dark `#0D1117`/`#151B23`, light `#FDFCF8`/`#F3F1EA`. Scales
     naturally across widths; rotator compass gets cramped.
   - **C "Split Deck"** — two-zone: a fixed left "world pane" (≈55 %, always
     showing the compass + selected antenna) and a right command pane that
     alone switches with the Station/HF/UHF tabs. Keeps spatial orientation
     while operating PA/tuner; risk: breaks the tabs-change-everything
     expectation and squeezes meters on small tablets.
   The plan also fixes console requirements that are PRD-relevant regardless
   of visual direction: every action one tap; a shared **fault strip** at the
   bottom (last three faults, active highlighted, cleared muted); offline
   controls grey out with a reason; interlocks block dangerous actions with no
   confirmation dialogs (rejected actions surface as faults); **no
   frequency/mode readout** (the primary operating UI already shows it); dark
   + light themes; landscape tablet; explicitly avoid neon-on-black,
   glassmorphism, thin trendy fonts, "AI/corporate dashboard" looks.
   `hf_console_color_variations.html` is a fourth artifact from this round: a
   switcher comparing four palettes (`DC`, `PAPER`, `FOREST`, `AMBER`) applied
   to a console card.

2. **Second round — four tablet_console previews** (each 1280×800, with PNG
   renders; one per design direction, summarized for the console PRD):

   **`tablet_console_airbus_preview.html` — "Airbus / process-control".**
   A flight-deck / industrial-HMI aesthetic. Deep navy palette
   (`--bg:#0B1120`, cards `#0E1625`, hairlines `#1A2740`), IBM Plex Sans for
   chrome and IBM Plex Mono for every label and readout (uppercase, ~.10–.16
   em letter-spacing, 2 px radii). The signature move is the **boxed
   annunciator tile**: every state color gets a dim tinted background box
   (cyan `#60D6FF` on `#0D3B55`, green on `#0D3D2E`, amber on `#4A3412`, red
   on `#4A1A13`) so alarms read as lit instrument tiles, not as text color.
   Layout: a 40 px top bar (brand, all-online pill, right-aligned radio
   context band/mode/freq/drive), a full-width amber banner pill ("⚠ REVERSE
   — BEAM FIRES ASTERN"), a three-column main row (360 px beam-compass card
   with SVG compass + azimuth readout + slew line + preset buttons
   45/90/180/270/STOP; PA card with 2×3 datablock grid FWD/REFL/SWR/TEMP/BAND/
   MODE plus FWD and RFL bars and OPERATE/STANDBY; tuner card with its own
   datablock + FWD bar + TUNE MEM/TUNE FULL/IN LINE), then full-width rows:
   ANTENNA (port buttons OFF…FAN-DIPOLE + AUTO, direction segment
   FWD/180°/BIDIR + RETRACT + "⚠ PENDING UNTIL RX"), POWER (STOP STATION +
   sequencer phase/step + four relay toggles MAINS/PSU 13.8V/TRX/PA), CLIMATE
   (heating/cooling toggles, 21.4 °C, 612 ppm CO₂, "wiring later"), and a
   bottom TX strip. The look is deliberately austere, boxed, and
   label-driven — the closest of the four to real avionics/process HMIs.

   **`tablet_console_dcs_preview.html` — "DCs / dark-dense".**
   Same layout skeleton as the Airbus preview (top bar, banner, three-column
   main, antenna/power/climate rows, TX strip) but styled as a dark
   "distributed control system"/SCADA screen: near-black palette
   (`--bg:#101216`, cards `#13161B`), a single cyan accent `#27D7D8` (dim
   variant `#1E7A7A`) with green/amber/red reserved for states, and maximum
   information density — the PA and tuner cards use a **datablock grid**
   (1 px-gapped key/value tiles: `FWD W 820`, `REFL W 18`, `SWR 1.9`,
   `TEMP °C 54`…), thin 3 px drive bars, 3–4 px radii, and monospace
   everywhere. It trades the Airbus tinted boxes for raw density: more
   numbers visible per card, less color; the feel is a night-shift control
   room rather than a cockpit.

   **`tablet_console_exact_preview.html` — "Exact design-system tablet
   preview" (the "exact replica" direction).**
   A faithful application of the **Exact Design System** — the consumer-grade
   system also used by the mobile handoff below: brand purple `#402FD8`
   (`--purple-2:#7C5CFF`, lavender `#C6B8FF`), Inter for UI text with IBM
   Plex Mono for data, page `#050506`, translucent cards
   `rgba(255,255,255,.05)`, **16 px card radii and fully pill-shaped controls
   (999 px)**, pill status chips (`.pill.on/.warn/.red/.blue/.purple`), a
   purple-tinted power card (`rgba(124,92,255,.10)` + border
   `rgba(124,92,255,.35)`), semantic tokens success `#36B37E`, warning
   `#FF9F0A`, error `#FF4D4D`, process `#5AC8FA`, open `#FF375F`. Content is
   the same station model (compass, PA, tuner, antenna row, power row, climate
   row) but rendered with soft modern shapes instead of boxes — the direction
   that looks most like a contemporary commercial app. Risk flagged by the
   brief itself: this is the look most likely to drift toward the forbidden
   "corporate dashboard" feel; its strength is that it is the only direction
   backed by a complete, tokenized design system (see the handoff §4.2).

   **`tablet_console_hybrid_preview.html` — "hybrid (DCs + at-a-glance
   grid)".**
   The synthesis direction, and structurally the most evolved: DCS's dark
   dense palette and typography (`--page:#050506`, cards `#13161B`, cyan
   `#27D7D8`, IBM Plex Sans/Mono) combined with the Exact/handoff content
   model in a **4-column at-a-glance CSS grid**: a band-switch strip in the
   top bar (buttons 160/80/40/20/17/15/12/10/6, active = 20 m — i.e. radio
   context becomes an operator control surface, unlike the other three where
   it is read-only), a large compass card spanning 2 columns, PA and tuner one
   column each, an Ultrabeam full-width card, antenna select, a power card
   spanning 3 columns (purple-tinted, `rgba(124,92,255,.08)`, with sequencer
   phase/step and four relay chips) and a 1-column climate card. The PA meter
   gains tick marks at 500 W and 1000 W (41.66 % / 83.33 % of a 1200 W scale)
   and a green→amber gradient fill; the radio readout is enlarged (26 px
   band); the bottom strip models all three states RX/TX/**INHIBITED**. This
   preview is the closest artifact to what a real console page would ship as,
   and the PNG `tablet_console_hybrid_preview_tall.png` is the best single
   image reference in the repo.

   For the console PRD: pick one direction as the visual target (the hybrid
   is the most complete), but treat the **shared content model** — top-bar
   station identity + radio context + band strip; banner for beam-direction
   warnings; compass with lobe wedges + dashed target line + presets;
   PA datablock with OPERATE/STANDBY; tuner datablock with TUNE MEM/TUNE
   FULL/IN LINE; antenna port row + AUTO + direction segment + RETRACT +
   pending-until-RX note; station power card with sequencer phase/step and
   relay toggles; climate placeholder; bottom RX/TX/INHIBITED strip — as the
   requirement, since all four previews agree on it.

### 4.2 `design_handoff_mobile_console/` — the mobile handoff (design "3a")

A complete, high-fidelity handoff for a **phone-sized** single-screen operator
console ("the mobile restack of the desktop console grid"). The README defines
a vertical card stack (Header → Radio → PA → Tuner → Ultrabeam → Antenna
select → Rotator → **Station power** as the headline feature: one-button
`Start station`/`Stop station` sequencer plus per-relay toggles for Mains
(`power/master`), PSU 13.8 V (`power/psu-13v8`), Transceiver and PA
(`hf/switch` relays)). Behavior spec worth carrying into any console PRD:

- **Startup sequence**: mains on → PSU on → TRX on → PA on → arm PA, ending
  phase `running`; **shutdown** the reverse (disarm → PA off → TRX off → PSU
  off → mains off). The prototype paces steps ~850 ms; production must pace on
  **real liveness confirmations** (the powerseq `/state` model).
- **Safety cascade** (open-loop in the prototype, enforced by bridges in
  production): Mains off forces PSU+TRX+PA+arm off; PSU off forces TRX+PA+arm
  off; downstream toggles disabled while their upstream supply is off; radio
  cannot transmit unless mains+PSU+TRX are on. Manual relay toggles are
  disabled while a sequence is in progress.
- **Rotator interaction**: 168 px circular dial, drag-to-steer
  (`touch-action:none`), needle = current azimuth, **dashed target line**
  hidden once within 5° of target, coverage cones from Ultrabeam direction
  (forward = one 60° cone at heading; reverse = one 60° orange cone
  opposite; bidirectional = two 90° cones), no gradients; "Slewing → {n}°"
  while moving.
- Design tokens are fully specified (page `#050506`, cards 16 px radius,
  pill buttons `sm` 32 / `md` 40 / `lg` 48 px, amber "attention" treatment
  with text `#1A1B2B` for Standby/reverse/TRX-PA-On states, bar/needle
  transitions ~0.3–0.4 s, toggle thumb ~140 ms).
- The bundle's `mqtt-schemas/` are copies of the authoritative per-bridge
  MQTT API docs (flexbridge, acom1200s-pa, atr1k-tuner, ultrabridge,
  wrc-rotator, antennaselect, shelly-power-bridge, m5stamp-hf-ctrl,
  powerseq, plus the station integration model) — use the originals in
  `docs/` and the per-component research files as the source of truth; the
  copies exist so the handoff is self-contained.

### 4.3 Known defects & fragilities

- No defects (no runtime code), but note: the mockups embed
  Google-Fonts `<link>`s (IBM Plex / Inter) — fine for mockups, not for a
  production offline tablet UI; the Flutter app already ships its own fonts.
- The mockups' values (820 W, SWR 1.9, 21.4 °C, 612 ppm CO₂, azimuth 247°,
  "REV") are hard-coded demo data, not live semantics; do not read them as
  defaults.
- The first-round plan's "no frequency/mode readout" constraint is relaxed in
  the second-round previews (top bar shows band/mode/freq/drive) — the later
  artifact wins if the two disagree.
- `hf_console_color_variations.html` and the A/B/C designs are superseded by
  the tablet previews; keep them only as history.

### 4.4 Re-implementation notes

Nothing here is software to re-implement. For the PRD: the chosen visual
direction should be attached as screenshots (the four `tablet_console_*_preview.png`
files plus `tablet_console_hybrid_preview_tall.png` are the canonical
references); the interaction rules from §4.1 (one-tap actions, shared fault
strip, offline greying-with-reason, interlocks-not-confirmations, dashed
target line hidden within 5°, sequencer pacing on liveness, safety cascade)
are **behavior contract for the console**; the concrete pixel tokens in
§4.1/§4.2 are **implementation detail** — free to change, but legibility-first
and the "no neon/glassmorphism/thin-fonts" constraints are part of the brief
and should be preserved.