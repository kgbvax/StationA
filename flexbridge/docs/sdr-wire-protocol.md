# Salvage: flexbridge.md
> Extracted from PRD/03-components/flexbridge.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [protocol] SmartSDR UDP discovery (radio, not mandatory)

If the config does not set a radio host, the bridge discovers the radio:

- Send the literal ASCII payload `discovery` as a UDP datagram to `255.255.255.255:4992` from an ephemeral port.
- The radio replies with a whitespace-separated `key=value` text payload, e.g.
  `version=3.4.1 serial=1234-5678-8400.12345 nickname=Flex6400 model=FLEX-8400 ip=192.168.1.50 port=4992 status=Available`.
- Parse the keys (case-insensitive) `serial`, `model`, `nickname`, `ip`, `port`, `version`, `status`; assume reply port 4992 when the reply has no port key.
- Read deadline: the caller's context deadline, else 3 s (host-configured path) or 5 s (autodiscovery path).
- If a wanted serial is configured, skip replies from other radios (case-insensitive serial compare) until the wanted one arrives or the timeout expires.
- Without a configured serial: keep the first reply provisionally; return immediately on a reply with `status=Available`; on timeout with only a non-Available reply seen, return the first reply.
- Failure modes: bind/broadcast failure, or read timeout with no reply at all (both are errors for the connect cycle).

## [protocol] SmartSDR TCP framing (port 4992)

The protocol is **newline-delimited ASCII**. Three line kinds, identified by the first character:

- `C<handle>|<command> <args...>` — client → radio. The bridge always uses handle **1**, so its commands look like `C1|version`.
- `R<handle>|<seq>|<body>` — reply to a command. The body is typically `0|OK` or `0|<error>`.
- `S<handle>|<topic> <topic-args...> <key=value> <key=value> ...` — asynchronous status frames, usually handle `0`.

Frame-parsing rules (normative): trim `\r\n` first. The first byte must be `R`, `S` or `C` (anything else → line skipped). Everything up to the first `|` is the handle, the rest is the body. For `S` frames: the first whitespace-delimited word is the **topic**; the remaining raw text splits into topic-args (leading words that are not `key=value` tokens) and a key→value field map. Tokenization must preserve double-quoted substrings (quotes stay in place, not stripped) so values with spaces round-trip. Every command send carries a write deadline of 5 s (or shorter under a caller context deadline); handshake reply reads carry a 5 s deadline likewise.

## [protocol] Handshake — exactly once per connection

After TCP connect, send each command and read lines until a matching `R1|` reply arrives; interleaved `S|` status frames go to the status handler (none lost, none buffered indefinitely):

1. `version` — connection probe. Failure stops the connect cycle.
2. `sub slice all`
3. `sub radio all`
4. `sub interlock all`
5. `sub atu all`
6. `sub pan all` — panadapter status (pan handles + band/center); needed to address band changes. Failure of any of 2–6 stops the connect cycle.
7. `info` — awaited but **non-fatal** if it fails. Reply body is comma-separated `key="value"` pairs; extract `model` (e.g. `FLEX-8400`), `chassis_serial` (e.g. `1126-1213-8400-3564`), `firmware_version` (e.g. `3.8.19`); strip a leading `<seq>|` prefix.
8. `sub dvk all` — **fire-and-forget** (no await). SmartSDR v4+/licensed only; a v3 or unlicensed radio rejects it and that must not break the handshake. The read loop consumes the reply, if any, and drops it. Sent *after* the awaited commands so no reply can look like another reply's.
9. `profile mic info` — **fire-and-forget**. Undocumented one-shot (used by FlexLib); the radio replies asynchronously with a `profile mic list=...` status frame. Best-effort; on older radios the mic-profile list stays empty.

The handshake must not send `client udpport` or `meter list` — no meter telemetry is part of the contract.

The reference matcher ignores the reply sequence number (any `R1|...` line ends the await). Safe only because the handshake serializes one command at a time; see the [defect] item on sequence numbers.

## [protocol] Radio-side command strings (complete list)

All runtime commands are fire-and-forget on the wire; confirmation comes through the status stream ("fire-and-observe"):

| Purpose | Wire command (appended after `C1|`) |
|---|---|
| DVK play memory N (also keys TX) | `dvk playback_start id=<N>` (N in 1–12) |
| DVK stop memory N (unkeys TX) | `dvk playback_stop id=<N>` |
| Band change through native band-stacking | `display pan s <panHandle> band=<bandNumber>` — bandNumber is the wavelength in meters (`20m`→`20`, `160m`→`160`, `6m`→`6`); panHandle is the verbatim hex handle string |
| Mic-profile load | `profile mic load "<name>"` (name double-quoted, can include spaces) |

There is deliberately **no** set_freq, set_mode, or set_power command — the bridge never tunes the radio except by band (hard safety boundary).

Native **band-stacking** is the radio's per-band memory of the last-used frequency and mode. A `display pan s ... band=` change recalls that memory, so the radio, not the bridge, picks the new frequency. This memory recall is why the band-transition hold (below) must suppress the band-change transient.

## [protocol] Status frames consumed (frame parsing rules)

The bridge routes on `frame.Topic`:

**`slice` — per-slice receiver state.** Topic args: first = slice index (integer), second = receiver index (can be absent). Slice frames are **incremental**: only changed fields appear; each frame must merge onto the previously stored state for that slice index. Fields parsed: `RF_frequency=<MHz float>` (primary; conversion must use rounding, not truncation — `round(mhz × 1e6)`; truncation is 1-Hz-low for ≈1.2 % of 10-Hz-step frequencies — a correctness bug, not a nicety), legacy fallback `freq=<dotted Hz>` (e.g. `14.100.000`, dots stripped), `mode` (raw firmware string, normalized before publication), `active=1|0`, `tx=1|0`, `agc_mode=` (older firmware: `agc=`), `filter_lo`/`filter_hi`, `pan=<handle>`. A malformed frequency value is non-fatal: keep the previous frequency and still apply the frame's remaining fields (warn log). **Slice removal** must delete the tracked slice entry; detect through any of a trailing topic-arg `removed`, `in_use=0`, or `removed=1` — missing this leaves a phantom slice that flips the published state (live-observed). Per-slice band is tracked with hysteresis per slice index (below).

**`display` — panadapter status; two-word-topic quirk.** The SmartSDR panadapter status topic is the **two-word "display pan"**: the frame arrives with `Topic == "display"` and the literal word `pan` as the first topic arg. The bridge must gate on `Topic == "display"` **and then** on topic-args starting with `pan`, and ignore frames with other leading args (`panafall`, `panf`). Routing on the single word `pan` alone (or treating the topic as one word) is the classic re-implementation error. Pan handle = the topic arg after the literal `pan` (kept as the raw hex string). Fields: `band=<wavelength>` (often absent — pan status carries `center`, not band), `center=<MHz float>` → Hz (rounded). Removal detection is the same as slice removal. A tracked pan reporting `band == <target band number>` confirms a commanded band change and releases the band-transition hold early.

**`interlock` — transmit state.** `state=<STATE>`, uppercase: `RECEIVING`, `TRANSMITTING`, `READY`, `PTT_REQUESTED`, `ERROR`, else `UNKNOWN`. Only `state=TRANSMITTING` sets the published `tx="tx"`. The `cause=` field is captured but not used downstream.

**`atu` — antenna-tuner status.** `status=<value>` (case-insensitive, e.g. `tuned`, `bypass`, `tuning`); the published `tuning` flag is true iff `status == "tuning"`. The parser also reads `active=1`.

**`radio` — radio-wide status.** `drive=<0-100>` → published `drive`. `tuning=1|0` drives the same published `tuning` flag as the ATU state (see the [defect] item on last-writer-wins semantics). Parsers for `tx_power`/`tune_power` exist but neither is part of the contract.

**`dvk` — Digital Voice Keyer status (SmartSDR v4+; flows only after `sub dvk all`).** Frame shape: `S<h>|dvk status=<idle|recording|preview|playback|disabled> [id=<N>] [enabled=1|0]`. `added`/`deleted` memory-library frames carry no `status=` key and must be ignored (no state change). `idle` or `disabled` must force the active memory id to 0 (cleared) even if the frame carries an id.

**`profile` — mic-profile list; caret-list quirk.** Mic profiles are **not broadcast** (`sub radio all` does not include them). The list arrives only as the asynchronous reply to the one-shot `profile mic info` sent during the handshake: `S<h>|profile mic list=Default^Default FHM-1^…^RTTYDefault^`. Normative parsing rules: names are **caret-delimited** (`^`); **names include spaces**; a **trailing caret** is present and must not yield an empty entry; the value cannot be split at spaces — the parser must read the raw body after the word `mic` by hand and take everything up to end-of-line after `list=`. The list is an **authoritative full snapshot**: rebuild (replace) the known set from it, sort lexicographically, publish as `/state.mic_profiles`; if the client-tracked active name is no longer in the list, clear it. Track only profile type `mic`; ignore `global`/`transmit` profile frames and `importing=`/`exporting=` flags. **The radio never reports the active mic profile** (mic profiles are load-only presets with no "current" pointer, unlike global profiles, which emit `current=<name>`); honor a `current=` frame defensively if firmware ever emits one — as of firmware v4.2.20 (observed) it never arrives, so the active name is tracked client-side.

## [protocol] Band derivation, edge hysteresis, active-slice selection

The radio reports no band; the bridge derives it from `freq_hz` (edges inclusive, IARU Region 1 / Germany allocations — identical to `docs/conventions/band-mode-reference.md`): 160m 1,800,000–2,000,000; 80m 3,500,000–4,000,000; 60m 5,351,500–5,366,500; 40m 7,000,000–7,300,000; 30m 10,100,000–10,150,000; 20m 14,000,000–14,350,000; 17m 18,068,000–18,168,000; 15m 21,000,000–21,450,000; 12m 24,890,000–24,990,000; 10m 28,000,000–29,700,000; 6m 50,000,000–54,000,000; 2m 144,000,000–146,000,000; 70cm 430,000,000–440,000,000; 23cm 1,240,000,000–1,300,000,000. Frequency inside an allocation → that label; HF general coverage 1.8–30 MHz outside all allocations → `gen`; anything else (VHF/UHF gaps, ≤0) → `unknown`.

**Band-edge hysteresis (2000 Hz, a default):** each slice tracks its own previous band. If the new frequency's candidate band is `gen`, check the border: a frequency within 2 kHz just past the previous band's upper edge or just below its lower edge stays in the previous band. Transitions into another ham band have no delay — hysteresis guards only exits into the general-coverage gap. **Per-slice (not global) previous-band state is load-bearing**: with a single global previous, switching the active slice between two slices clobbered an edge-dwelling slice's held band. A nonzero frequency that derives to `unknown`/`gen` after hysteresis triggers a warning log.

**Active-slice selection — deterministic, must be preserved:** among slices with `tx=1`, the **lowest index** wins; if none, among slices with `active=1`, the **lowest index** wins; if none, no state update. Lowest-index tie-breaking is not incidental: with unordered iteration, "first match found" makes freq/band/mode a coin flip per frame when two slices match — that was the exact live defect that produced flip-flopping published state.

**Invariant:** `/state.band` must always derive from `/state.freq_hz` — even after `set_band` (which uses native band-stacking; the radio picks the frequency). The bridge never accepts band from the radio or stores it as an independent setpoint.

## [protocol] Band-transition hold (suppresses the set_band transient)

After a successful `set_band`, arm a transition `{target band, deadline = now + 750 ms}` (750 ms is a default). Rationale: SmartSDR switches the panadapter's band immediately but retunes the slice asynchronously, emitting intermediate slice frames that still carry the old frequency (outside the target band). Without suppression the bridge can publish a transient wrong band and the antenna-selection consumer can chatter to the fallback antenna.

- While armed, unconfirmed, and before the deadline: suppress slice-derived band changes whose derived band ≠ target (keep the previously published band). Frequency and mode updates that land inside the target band still apply.
- Release the hold early when any tracked panadapter reports `band == <target's number>`.
- At deadline expiry the hold is gone; the bridge accepts the next derived band as-is.
- A second `set_band` during a hold replaces the transition (new target/deadline). Disconnect abandons any pending transition.

## [protocol] Mode normalization (raw firmware → canonical)

`USB→usb, LSB→lsb, CW/CW-U/CW-L→cw, AM/SAM→am, FM/NFM/DFM→fm, DIGU/DIGL/DATA-U/DATA-L/FDV/FDVU/FDVL/RTTY-U/RTTY-L/PKTUSB/PKTLSB/DSTR→data`. Anything else → empty → the bridge omits the `/state.mode` field entirely. The bridge must never publish raw firmware mode strings.

## [protocol] Disconnect reset behavior

On disconnect (not shutdown), `Reset` must clear the slice map, the per-slice band map, the pan map, and the mic-profile set; also the interlock state, the ATU status, the whole published state, the command sender, any pending band transition, and the HA-discovery-per-cycle flag. Then it must **publish** a `/state` snapshot with `device_online=false` and all values zeroed/omitted. This forced publish is mandatory: `/state` is otherwise change-only and can stay frozen on the last live values, hiding the disconnect from consumers that watch only `/state`.

If the `info` reply carried no serial, the bridge skips identity publication entirely (publishes nothing) — `/meta` is written only once per successful radio connect, only after the handshake, only with the radio's real identity from the `info` reply.

## [defect] Clean shutdown leaves retained `online` on `/status`

A graceful MQTT disconnect fires no will, so the retained `online` stays on the broker while the service is down; the schema documentation's claim that the bridge publishes `offline` on clean shutdown was never put in place. Only the `device_online` layer reduces the risk. It is a trap for new consumers. Verdict: **still open** — `cmd/flexbridge/main.go` sets only the Will (`offline`) and publishes `online` on connect; nothing publishes `offline` before a clean disconnect.

## [defect] No radio-side keepalive

A silently black-holed TCP path (no traffic, no read error) stays unnoticed — no ping, no read-timeout watchdog in the read loop; reconnect waits for the OS to notice. A re-implementation must add a watchdog; if it does, it must give the timing (see the [decision] item). Verdict: **still open** — `internal/flexradio/client.go` sets a read deadline only inside `sendAwaitReply` (handshake) and clears it afterwards; the steady-state read loop has no deadline or watchdog.

## [defect] `/cmd` queue saturation drops commands silently

Bounded queue (capacity 32 default), no drop logging. One burst of >32 commands before the worker drains loses intents without a trace. Do not reproduce: log drops. Verdict: **still open** — `shared/mqtt.Enqueue` drops on a full channel by design and its test asserts the silent drop.

## [defect] `dvk_play` is not gated on voice mode

Only an advisory debug log; the radio refuses in CW/data. A consumer can trigger a TX-keying action expecting an effect in a mode where it cannot work. Verdict: **still open** — `internal/bridge/bridge.go` `isVoiceMode` check emits `Debugf` only; command is not blocked.

## [defect] `tuning` is a last-writer-wins OR of two independent sources

ATU `status=tuning` and radio `tuning=1` each write the same published bool. A radio `tuning=0` frame immediately after an ATU `tuning` frame can clear the flag while the ATU still carries `status=tuning` (narrow race, never observed live). A re-implementation must OR the two live source states instead. Verdict: **still open** — `handleATU` and `handleRadio` in `internal/bridge/bridge.go` each assign `state.tuning` directly.

## [defect] Reply sequence numbers are ignored

The matcher takes the first `R1|` reply (sequence 0 is always used). Safe only because the handshake serializes one command at a time; do not reproduce under concurrency — track real sequence numbers. Verdict: **still open** — `internal/flexradio/client.go` `sendAwaitReply` comments explicitly: "We always use sequence 0 … so we match the first R1| reply we see."

## [defect] `set_mic_profile` active-name tracking is best-effort and unconfirmed

The bridge optimistically sets `/state.mic_profile` right after sending the load command, assuming success. A profile switched directly in the radio's GUI never shows up (the radio reports no active mic profile). A failed load leaves a wrong name published until the next list snapshot removes it. Verdict: **still open** by design (`micProfile` is client-side "last loaded"); see the matching [decision] item.

## [defect] Slice `pan=<handle>` field is best-effort

Confirmed live for this radio. If a firmware variant omits it, `set_band` silently falls back to the lowest tracked pan handle — that can target the wrong panadapter on multi-pan setups. Verdict: **still open** — `resolvePanHandle` in `internal/bridge/bridge.go` falls back to active-slice pan → single tracked pan → lowest handle.

## [defect] Client-id derivation is dead in practice

The code derives `<site>-<station>-<slot>` only when `mqtt.client_id` is empty, but the built-in default config pre-fills `"flexbridge"`, so the deployed client id is `flexbridge` while deploy-script comments claim the derivation is the default. Duplicate-connection diagnosis sees `flexbridge`, not `muehle-hf-radio`. A re-implementation must pick one scheme deliberately (see the [decision] item). Verdict: **still open** — derivation exists in `cmd/flexbridge/main.go`, but `internal/config/config.go` still defaults `client_id: "flexbridge"`.

## [defect] Band change needs a panadapter open in the radio's GUI

With none tracked, the command is a warn-log no-op; the bus does not show the manual precondition. Design note: any UI that commands `set_band` must surface "no panadapter tracked" as an error the operator can act on. Verdict: **still open** (no feedback channel exists; see the [decision] item on dvk gating for the same no-ack constraint).

## [defect] `profile mic save` is obsolete on SmartSDR v4+

The radio returns a malformed reply; profile creation uses a file-transfer mechanism out of scope. Re-implementations must not add a save action against the v4 wire protocol. Verdict: **still open as a constraint** (documented in `flexbridge/CLAUDE.md`; no save action exists in the code — keep it that way).

## [defect] Legacy embedded HA discovery vs external hadiscovery produce different HA node ids

`flexradio-<serial>` vs `muehle-hf-radio`. Switching paths leaves orphaned HA entities unless the deployment clears old discovery topics. Verdict: **still present** — the embedded path (gated off by default) still exists in `internal/bridge/bridge.go`.

## [defect] Dead meter/VITA-49 machinery and stale README

The reference README documents a long-gone per-field topic scheme (`flexbridge/<serial>/state/...`), meter topics, `[rates]` and `radio_udp_port` config, per-slice entities, and "frequency in MHz" — stale. The code keeps and tests UDP/VITA-49 meter machinery (`meters.go`, `vita49.go`, meter-list parser) that is **dead code**: no UDP listener is wired, and the handshake sends no `client udpport`/`meter list`. Where the PRD and the README disagree, the code-derived behavior wins. Verdict: **still open** — `meters.go`/`vita49.go` still present; handshake sends no meter commands (verified in `internal/flexradio/client.go`).

## [defect] `flexbridge/docs/radio2mqtt-schema.md` band table has drifted from the code

*(Found during this salvage's code check — not in the PRD.)* The live schema doc's band table carries wrong edges for 60m (5,060,000–5,450,000), 30m (10,000,000–10,200,000), 20m (14,000,000–14,400,000), 15m (21,000,000–21,450,000 ✓/10m 30,000,000 vs 29,700,000), plus extra rows (4m, 1.25m, 33cm) not in the code; the code table in `internal/flexradio/band.go` matches the authoritative `docs/conventions/band-mode-reference.md` (IARU Region 1/DL edges). The schema doc also says "Unknown firmware modes are lower-cased and passed through", while `NormalizeMode` returns `""` and the field is omitted. The PRD band table (above) is the correct one. Verdict: **still open** — fix `flexbridge/docs/radio2mqtt-schema.md` "Band values"/"Mode values" to match `internal/flexradio/band.go` and `status.go`.

## [decision] Clean-shutdown `/status`

Keep the reference behavior (clean disconnect leaves retained `online`; consumers rely on `device_online`), or make the contract mandate an explicit `offline` publish before graceful disconnect — the latter is cleaner for consumers and was never put in place. Station-wide consistency matters (other bridges share the behavior).

## [decision] MQTT client id scheme

Derived slot address (`muehle-hf-radio`) vs the pre-filled `flexbridge` (deployed). Derivation is more diagnosable. Pick one and make the deploy seed match.

## [decision] Radio-side keepalive

The reference has none. Does the bridge need a read-timeout watchdog / periodic `info` ping? No timing evidence exists for how quickly the system detects a black-holed path today. Any added watchdog timing is a new default that needs a decision.

## [decision] `dvk_play` voice-mode gating

Keep advisory-only (reference) or reject non-voice modes with an error? The radio already refuses. Rejecting in the bridge can make the failure visible on the bus, but no feedback channel exists today (no-ack contract).

## [decision] Meter telemetry scope

The README describes UDP VITA-49 meter streaming (SWR and power). SWR is the standing-wave ratio, a measure of how well the antenna matches the radio. That streaming is dead code. No station consumer needs radio meters today (the PA bridge measures power at the amplifier). Decide whether meters are scope; if yes, treat the README's meter table as the aspirational spec, not current behavior.

## [decision] Mic-profile "active" tracking

The radio reports no active mic profile (firmware v4.2.20 observed), so `/state.mic_profile` is a client-side guess. Options: keep the optimistic guess, or omit the field and let consumers track loads themselves. The bridge must keep the defensive `current=` frame handling either way (costless if firmware never emits it).

## [decision] Firmware-version assumptions

Live observation confirmed the SmartSDR quirks documented here — two-word pan topic, caret mic list, no active-mic pointer, `profile mic save` malformed reply — on firmware 3.8.19/v4.2.20-class radios. A re-implementation targeting substantially different firmware must re-check each quirk against the actual radio. No other document gives those behaviors.

## [unique] Timing constants (defaults) of the reference behavior contract

Radio reconnect backoff: first 2 s, ×1.5 per failed cycle, capped at 60 s (sleeps interruptible by shutdown). MQTT reconnect retry: 5 s. `/state` heartbeat: ≥ every 5 s while `device_online=true` (implemented: `cmd/flexbridge/main.go` `StateHeartbeat`). Handshake/read/write deadlines: 5 s. Radio discovery: 3 s (host-configured) / 5 s (autodiscovery). Band-transition hold: 750 ms. Band-edge hysteresis: 2000 Hz. MQTT clean-disconnect quiesce: 500 ms. `/cmd` queue capacity: 32. MQTT disconnect clean stop waits 500 ms so the library can flush in-flight publishes.

## [unique] `/cmd` action-validation rules (mic-profile name guard)

`set_mic_profile`: trim the value; validate non-empty, ≤64 chars, no `"`, no `\`, no control chars (the name is embedded in a double-quoted wire string — also an injection guard); else drop with warn. If the tracked profile list is non-empty and the name is not in it, drop as a likely typo (an empty list does NOT block — it only means `profile mic info` has not answered yet). `set_band`: label→number map `160m→160, 80m→80, 60m→60, 40m→40, 30m→30, 20m→20, 17m→17, 15m→15, 12m→12, 10m→10, 6m→6`; any other label (`2m`, `70cm`, `23cm`, `gen`, `unknown`, garbage) gets a warn log and no radio write; target pan handle resolution: active slice's `pan` handle if tracked, else the lexicographically lowest tracked handle, else no-op with warn. `dvk_play`: id validated 1–12, else dropped; non-voice mode logs a warning (not blocked). `dvk_stop`: with value stop memory N (validated 1–12); without value stop the now-active memory from the live `/state` id; if none active, warn and drop. Validation drops are silent on the bus (no error topic, no acknowledgment).

---

## Verified fixed / dropped (code checks 2026-09-03)

- **Change-only `/state` starving the PA-arm 10 s heartbeat (PRD §3.4, HARD requirement)** — FIXED: `cmd/flexbridge/main.go` runs `go b.StateHeartbeat(ctx, bridge.DefaultStateHeartbeat)`; comment cites the 10 s PA-arm heartbeat explicitly.
- **`device_online` wire form (PRD open decision 2)** — settled in `docs/station-integration-model.md` (explicit bool; memory + consumers rely on the explicit form).
- **Broker topology (PRD open decision 1)** — covered by `docs/conventions/mqtt-topology.md`; two-broker work merged to main.
- Topic tables, `/meta`/`/state` payload shapes, HA-discovery payload details, `/cmd` action list, config keys, env overrides, deployment steps, systemd unit — covered by `flexbridge/docs/radio2mqtt-schema.md`, `flexbridge/CLAUDE.md`, `docs/conventions/*`; not salvaged.