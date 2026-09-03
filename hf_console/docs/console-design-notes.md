# Salvage: 04-console.md
> Extracted from PRD/04-console.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [unique] Design lineage (§6.3) — the `sas/` mockups, forbidden looks, and the handoff rules

The visual/interaction exploration lives in the repo under `sas/` as static HTML mockups. It is **design input, not software**. Two rounds exist:

1. **Information-architecture directions** (from the design brief — a fixed-mount tablet where every action is one tap, readable in dim light. Explicitly forbidden: neon on black, glassmorphism, thin trendy fonts, "corporate dashboard" looks). The directions: **A "Shop Panel"** (chunky industrial grid), **B "Logbook Strips"** (full-width ruled strips), **C "Split Deck"** (persistent left world-pane + switching right command pane). The brief also fixed console requirements that are behavior contract regardless of visuals: every action one tap. A shared fault strip at the bottom. Offline controls grey out with a reason. Interlocks block dangerous actions with no confirmation dialogs (rejected actions surface as faults). Dark + light themes. (The later mockups relaxed the brief's original "no frequency readout" constraint. They show band/mode/frequency in the top bar — the later artifact wins.)
2. **Four high-fidelity tablet previews**:
   - **Airbus / process-control** — flight-deck aesthetic. Every state color gets a dim tinted "annunciator tile" box. So alarms read as lit instrument tiles.
   - **DCs / dark-dense** — near-black SCADA look (SCADA = industrial control-room software, here describing the visual style), single cyan accent, maximum information density (datablock grids, monospace everywhere).
   - **Exact design-system** — consumer-grade tokenized system (brand purple, 16 px card radii, pill controls). This direction risks drifting toward the forbidden corporate-dashboard look the most.
   - **Hybrid (DCs + at-a-glance grid)** — the synthesis: the DCs palette and typography combined with an at-a-glance grid layout. This is the most complete artifact and the approved high-fidelity reference (also named in `hf_console/CLAUDE.md`).

All four previews agree on a **shared content model**. This model is the requirement, because the direction can otherwise change. The content model: top-bar station identity + radio context + band strip. A banner row for beam-direction warnings. Compass with beam-lobe wedges + target line + presets. PA block with meters + OPERATE/STANDBY. Tuner block with TUNE MEM / TUNE FULL / inline state. Antenna port row + AUTO + direction segment + RETRACT. Station-power card with sequencer phase + relay toggles. A climate placeholder. A bottom RX/TX/INHIBITED-style status strip. Interaction rules from the handoff are behavior contract for the console: one-tap actions. Shared fault strip. Offline greying-with-reason. Interlocks-not-confirmation-dialogs. Dashed target line hidden within 5° of target. Sequencer pacing on real liveness confirmations.

Theme tokens (three schemes dc/paper/forest, exact hex values, invariant semantics: accent = active/informational, green = healthy/allowed, amber = degraded/attention, red = danger/offline/blocked/live-TX, orange = hot-end of the power meter; condensed display / humanist sans / bundled monospace fonts; flat 4 px buttons with solid-red "dangerActive" states; 44×40 min touch targets with two deliberate exceptions — rotator presets, compass zoom stack; purple-bordered station-power card; ×1.25/×1.1 type scale ≥1400/≥1200 px; decorative animations must respect platform reduced-motion) are a full spec in the appendix below (salvaged from the PRD) — worth re-reading when touching `hf_console/lib/ui/theme.dart`.

## [defect] Design-handoff rules consciously not implemented (§6.3)

Two further handoff rules are **conscious deviations** in the shipped console, not silent drops. First, the handoff disables manual relay toggles while a sequence is in progress. Second, it chains an open-loop supply cascade: mains off forces PSU, TRX, PA, and arm off. The shipped console implements neither rule. Its relay toggles gate on slot liveness only (§3.9). Sequencing and the cascade belong to the server side (see `03-components/powerseq.md` and `06-safety.md`). A reconstruction must record its own choice for both rules.

Verdict: still open in `hf_console/lib/ui/widgets/power_panel.dart` — the START gate keys only on `running`/`starting` and relay toggles gate on slot liveness; no sequence-in-progress lockout, no console-side cascade (cascade ownership is server-side; confirm powerseq implements it before treating this console deviation as safe).

## [requirement] Operator-safety UI rules (§4) — console-own, not mirrored elsewhere

These rules are the console's core reason for existing. They are testable requirements. Each traces to a real incident or a deliberate design decision.

- **R4.1 LINK DOWN banner.** While the broker connection is down, a full-width red strip renders at the very top of every page. The copy is exact: `LINK DOWN — DATA STALE · COMMANDS NOT DELIVERED`. It disappears the instant the link returns. The panels do NOT disable taps one by one. Commands simply do not arrive, and the banner promises exactly that. (Consequence: a user can tap with no per-press feedback. The banner is the only warning. A reconstruction can add per-press feedback. It must not remove the banner.)
- **R4.2 No command while offline.** The console disables every command button unless its owning slot passes two-layer liveness (bridge `/status` AND `/state.device_online`).
- **R4.3 Fail-closed cross-panel RF guard (cold-switch).** The console must refuse a direct-drive antenna-switch port change (manual / reconciler-offline path) unless the RF check confirms safe. RF reads as "on" if ANY of three independent paths says so: radio `tx == "tx"`, radio `tuning == true`, PA `keyed == "tx"`. RF counts as "safe" only if the radio link is up AND `tx == "rx"` AND `tuning != true` AND `keyed != "tx"`. **Unknown blocks** (fail closed). Confirmed RX allows the change. Rationale: transmit power on an antenna relay arcs its contacts. The reconciler path (auto mode, reconciler online) is exempt because the reconciler arbitrates inhibit ordering itself.
- **R4.4 Never invent OFF (or any definite state) from missing data.** A missing `hf/switch.pa` shows `RELAY ?`, never "PA RELAY OFF". A missing PSU `power` key never triggers the PSU root-cause line. A missing `ant-switch.selected` shows Unknown, never Grounded. An absent selector never shows "AUTO".
- **R4.5 Red means "shouting", not "error", in these states**: the GROUNDED port button renders solid red even while active (grounded = no antenna connected = operating impossible). The MANUAL override button renders solid red while engaged. ARMED renders red (the interlock is hot). RETRACT and danger actions use red-tinted styling.
- **R4.6 Stale data must be loud.** Offline rows carry a stamp of when each slot went dark, never render time. The console reports silent expected slots after the 3 s connect grace. In the PA tag a live TX outranks plumbing.
- **R4.7 Motion/settling discipline.** The console locks Ultrabeam direction commands while moving (RETRACT excepted). It locks tuner tune commands while settling. It disables START STATION while the sequencer reports running/starting.
- **R4.8 The DX overlay never interferes with control.** It shows read-only content. Tap = aim, right-gutter vertical drag = zoom. The gestures do not overlap.
- **R4.9 Band colors stay fixed across themes** — the horstreporter correspondence is a cross-component visual invariant (full palette lives in `docs/conventions/band-mode-reference.md`).
- **R4.10 Payload shapes are contracts.** A reconstruction must reproduce the value-key deviations exactly (they live in `hf_console/CLAUDE.md`). Deployed bridges parse them as-is. The console project does NOT re-implement them.

Related hard requirement (from a live incident, restated because the console is the surface where it bites): the PA arm relay de-energizes if its inputs do not refresh within 10 s. The enabling input is the radio's `/state`, and the radio bridge publishes `/state` only on change. When the radio is idle-but-healthy this starves the heartbeat. The interlock drops out. The operator sees the ARM panel flip to SAFE with no fault. Any reconstruction of the radio bridge must republish radio state at least every 5 s. Or it must give a liveness mechanism with the same effect (see `03-components/flexbridge.md`, `06-safety.md`). The console cannot fix this. It must merely survive it and render it truthfully.

## [unique] DX-spot feed (SSE overlay) — consolidated spec summary (§2.9)

The DX overlay is display-only. It runs **independently of the MQTT broker**. Loss of either feed must never disable the other. The overlay is active if and only if the configuration holds a station locator. Without it the compass shows beam heading only. The map panels render without spot data.

### Transport

- Endpoint: `GET {baseUrl}/api/stream?qth={qth}&minutes=30&surroundings=true`. Here `qth` = the station callsign if configured, else the Maidenhead locator (both percent-encoded). `baseUrl` defaults to `https://horstreporter.kgbvax.net`. `minutes=30` makes the server replay the last 30 minutes of spots on each (re)connect. *(Code/comment discrepancy in the reference: source comments say `minutes=15`. The URL parameter in code is authoritative: 30.)*
- Protocol: SSE. Each spot arrives as one default (unnamed) event with a single `data:` line. Parsing follows the HTML spec: a blank line dispatches; strip one leading space; join multi-line `data:` with `\n`; ignore `:` comments and `event:`/`id:`/`retry:` lines for data purposes. **But any received line resets the idle watchdog**.
- Request headers: `Accept: text/event-stream`, `Accept-Encoding: identity` (prevents gzip so the parser can split the stream by lines), `Cache-Control: no-cache`.
- **No keepalives**: the server sends `data:` only when a spot arrives. It sends no `: comment` heartbeat lines. The client therefore arms an **idle watchdog: 5 minutes without any received line forces the connection closed and a reconnect** (a half-open TCP connection otherwise never surfaces as an error). A **15 s connection timeout** bounds the connect phase. The client treats a non-200 status as a disconnect.
- **Reconnect/backoff**: the service layer owns this, not the transport. The first delay is **2 s**. Each failure multiplies it by 1.5, capped at **60 s**. Any good event resets it to 2 s. The browser build must suppress its EventSource built-in auto-reconnect. Losing SSE's `Last-Event-ID` resume is harmless. Every connection re-fetches the history window anyway.
- Consequences of the watchdog: on a genuinely quiet band the client reconnects every ≤ 5 minutes and re-ingests up to 30 minutes of spot history. Dedup (below) keeps this correct. Do not remove the watchdog without putting a replacement in its place. A half-open feed otherwise leaves stale dots forever (the 60 s prune timer is the backstop).

### Spot payload (`streamSpot` shape)

| Field | Type | Meaning |
|---|---|---|
| `lat`, `lng` | number | **the remote (DX) station's position in degrees**, already resolved server-side — NOT the reporter's position. The console must place dots from these values. The locator serves only labels and grid squares. |
| `snr` | number → int | signal-to-noise ratio, dB (FT8/FT4 spots). |
| `ageSeconds` | number → int | how old the report was when the server emitted it. |
| `locator` | string | the remote station's Maidenhead locator (can be empty). |
| `band` | string | band label, for example `"20m"`. |
| `sourceType` | string | `"mqtt"` (FT8/FT4 through MQTT), `"dxcluster"`, `"rbn"`, `"wspr"`. |
| `sender`, `receiver` | string, can be absent | callsigns, dxcluster only. |

### Ingest rules (exact)

- **R2.9.1** Only `sourceType == "mqtt"` spots (FT8/FT4) enter the store. The ingest drops dxcluster, rbn, and wspr spots. Rationale: the console's overlay intentionally mirrors horstreporter's azimuthal view band-for-band.
- **R2.9.2** The ingest drops spots missing `lat`/`lng`. It also drops spots missing all of locator+receiver+sender (nothing to place or label).
- **R2.9.3 Mode-aware SNR gate**, driven live by the radio mode (`muehle/hf/radio` `state.mode`): `usb`/`lsb`/`am`/`fm`/`data` → SSB family, threshold **0 dB**. `cw` → CW family, threshold **−15 dB**. Anything else (unknown/blank) → gating **off**. The ingest drops spots below threshold. Changing the filter does not clear current spots. A reconnect re-ingests history under the new threshold. The UI shows the active gate as a filter chip: `"SNR off"` / `"SSB ≥ 0dB"` / `"CW ≥ -15dB"`.
- **R2.9.4 Dedup key**: `"<locator>|<receiver??''>|<sender??''>|<band>|<sourceType>"`. On repeat the service keeps the freshest spot (lowest server `ageSeconds`). Higher SNR breaks ties. The service **always** refreshes the kept spot's local receive time. So an actively re-spotted station does not age out. Known limitation: the key ignores lat/lng. A station that reports a moved position under the same locator/sender/band keeps its first coordinates.
- **R2.9.5 Live age** of a spot = `ageSeconds` + seconds since local receipt. Max live age **600 s**. Enforcement happens at ingest and by a **60 s prune timer** that runs even when the feed is quiet.
- **R2.9.6 Cap 80 spots**, sorted by live age ascending, ties broken by SNR descending.
- **R2.9.7 UI notifications throttled to ≤ ~2 Hz (500 ms coalesce)** so a busy band cannot repaint-storm.
- **R2.9.8 Grid-square aggregation** (shared by both maps): the service groups spots by the first 4 characters of their uppercased locator. Per square the **dominant band** = the band with the most spots (first-max wins ties). The **score** = mean of the top quarter of SNR values (sort descending, take `ceil(n/4)` clamped to ≥1, average) — used for opacity. Squares with no band data do not contribute. Spots with <4-char locators do not contribute. Opacity ramp (exact): the ramp starts at 0.45 for a score of 0 dB. It falls to 0.15 at −10 dB. The slope is 0.03/dB below 0 dB and 0.015/dB above. Floor 0.10, cap 0.75.
- Source-type fallback colors for spots with no band: mqtt→green, dxcluster→accent, rbn→amber, wspr→orange, other→muted text color. (The fixed band palette itself lives in `docs/conventions/band-mode-reference.md` — not duplicated here.)

### Projections and geometry (both maps share one spot service)

**AEQD (compass disc), exact math** (a port of horstreporter's own implementation so the two frontends' maps coincide). Earth radius 6371 km. Disc radius = `min(w,h) × 0.47`. Scale = `radius × zoom / π`. The math flips the y-axis. The implementation clips the projection 0.02 rad shy of the antipode. Horizon at 20015 km. Maidenhead decode: fields 20°×10° anchored at (−180,−90), squares 2°×1°, subsquares 5′×2.5′ with center offsets. The decoder supports 2/4/6-char locators. The cell center is the projected point. The configured station locator sets the AEQD center.

**Web Mercator (map panel), exact math**: standard EPSG:3857, spherical Mercator, earth radius 6378137, 256 px tile. The implementation clamps latitude to ±85.05112878°. It wraps longitude relative to the map center so antimeridian crossings render as a short jump. It detects antimeridian cuts in coastline rings by a |Δlng| > 180° break in the subpath. Minimum zoom = log2(height/256). (Land-fill seam-cut invariants additionally live in the hf_console memory notes.)

**Landmasses**: a bundled static world outline asset (Natural Earth 50m country polygons, ~1600 rings / ~100k vertices, ~3 MB). The app loads this asset lazily once. It never fetches it at runtime. A malformed asset must produce an empty coastline list, never a crash (the compass renders without coastlines rather than failing).

**Performance contract**: interactive zoom on tablet hardware must stay visually smooth. This criterion is deliberately unquantified further. The acceptance procedure is a manual zoom-drag test on the reference tablet with typical world-layer complexity. The animation must not visibly stutter. The reference gets this with a rasterized world-layer cache keyed on center/zoom/size/colors — the caching is implementation detail, the smoothness is the contract.

## [decision] Unresolved open decisions and unresolved facts (§7)

Each item lists the evidence for the variants. None is silently resolved. Verdicts in *[square brackets]* were code-checked 2026-09-03 against `hf_console/`.

1. **Ultrabeam switch port: 3 or 4.** Repo-root documentation and the integration model say switch port 3. The console's port map (`hf_console/lib/store/wiring.dart`), the reconciler's example config, and its deploy seed say port 4. The deployed `/etc/antennaselect/config.toml` on shari is authoritative. The team MUST confirm it on-device. *Verdict: still open — `wiring.dart` still maps `port4 → Ultrabeam` while `docs/station-integration-model.md` (wiring-map example) says `port3: ultrabeam`. Note: the fan-dipole half of the old conflict is now resolved in the live docs (port 6 everywhere). Dummy load (port 1) is consistent everywhere.*
2. **Broker topology: one broker or two.** *Verdict: largely superseded — `hf_console/CLAUDE.md` now pins the shack-local broker at `192.168.1.139:1883` and the two-broker work is merged to main. Confirm the broker migration is actually deployed on shari before closing.*
3. **`device_online` form.** The integration model says writers can omit the key when true. Deployed bridges publish explicit `device_online: true`. Consumers treat absence-as-true once a state snapshot has arrived. Whether a reconstruction mandates explicit-true everywhere is open. See `02-interface-spec.md`. *Still open (listed as an M0 on-device decision in project memory).*
4. **Chosen design direction.** The hybrid preview is the approved high-fidelity reference (per `hf_console/CLAUDE.md`). The shipped app is the hybrid's split-deck skeleton rendered in the DCs dark visual language. But the shipped Flutter theme's concrete values diverge in detail from every mockup. A reconstruction must decide whether "hybrid preview" or "shipped app" is the visual target of record.
5. **`station_callsign` is a dead key.** The console reads it at startup. When present, the console prefers it as the SSE `qth=` parameter over the locator. But no UI writes it. Either wire an editor for it (for example in the gear sheet) or drop it. The spot feed's `qth` parameter then always carries the locator. *Verdict: still open — `lib/main.dart:83` and `lib/store/credential_store.dart:27` read it; no UI writer exists.*
6. **SSE watchdog churn.** The 5-minute idle watchdog guarantees disconnect/reconnect cycles on quiet bands. Each cycle re-ingests up to 30 minutes of replayed history. Dedup makes it correct, but the client repeats the work. The reference source comments even disagree with themselves about the replay window — 15 vs 30. The code URL parameter 30 is authoritative. Options: keep both watchdog and replay window (reference behavior), shrink the replay `minutes`, or negotiate a real keepalive with the spot service. Any change must keep "no stale dots on a half-open feed".
7. **First-connect failure behavior.** The reference swallows a failed first connect. It relies on library auto-reconnect that arms only after a first successful connection. A console booted before the broker is reachable can stay offline until an operator restarts it. A reconstruction must define an app-level retry (for example periodic reconnect attempts). Or it must accept and document the same limitation. *Verdict: still open — `lib/mqtt/mqtt_service.dart` sets only `autoReconnect`/`resubscribeOnAutoReconnect`; no app-level first-connect retry.*
8. **START enabled during phase `stopping`.** The shipped START STATION button gates on `running`/`starting` but not on `stopping`. So a tap mid-shutdown sends `{"action":"start"}` during the stop sequence. The sequencer tolerates it. Whether the console must also gate `stopping` is open. *Verdict: still open — `power_panel.dart:90` gates on `running`/`starting` only.*
9. **Web-channel credential storage.** The web build keeps the broker password in plain browser-local storage (no secure storage exists in browsers). The team must decide consciously: preserve as a trusted-LAN trade-off, or replace with per-session entry. Document the decision.
10. **`dvk_status` value set.** Only `idle` appears in the console's own fixtures. The radio bridge owns the full vocabulary (`playback`, `recording`, `preview`, `disabled`). *Informational — `flexbridge/docs/radio2mqtt-schema.md` owns this vocabulary.*
11. **Climate panel.** Hard-coded placeholder values (21.4 °C, 612 ppm) imply telemetry that does not exist anywhere in the station. Keep it as a labeled placeholder, or drop it.
12. **Faults bar render-time fallback.** The store contract says offline stamps must come from the store, never render time. The shipped UI keeps a render-time clock as a last-resort fallback when a stamp is absent. A reconstruction must define the fallback (or make it impossible). Do not inherit the contradiction silently. *Verdict: still open — `lib/ui/widgets/faults_bar.dart:12` uses `DateTime.now()` with a `_clockTime` fallback.*
13. **Mic-profile list transport.** The `mic_profiles` field arrives on the bus as a sorted JSON array of profile-name strings. (The caret-delimited form is the vendor protocol reply that the radio bridge parses. The console never sees that form.) SmartSDR reports no active-mic name. So `state.mic_profile` is the bridge's client-side "last loaded" tracking. Any reconstruction of the radio bridge owns this contract. The console only renders it. *Informational — contract owned by the radio bridge.*
14. **`antenna-select` mode value set.** The console fixtures exercise only `auto` and `manual`. Values beyond these two are unknown. The antenna panel header uppercases whatever the slot reports while online. So an unexpected value must degrade gracefully there. A reconstruction must define how unknown mode values render.

## [unique] Console personality note

A band-heckling-dragon mascot image sits beside the RETRACT button — a deliberate bit of station personality. Keep it or drop it. Operators recognize it.

---

## Appendix: theme spec (verbatim from PRD/04-console.md §6.1–6.2, salvaged 2026-09-03)

### 6.1 Color schemes

Three user-switchable schemes. The user can switch at the top bar at any time (buttons DC / PA / FO). Exact reference values (a reconstruction can refine the hex values, but dark-first, near-black cards, monospace readouts, and the semantic assignments are the visual contract):

- **dc** (default, dark): page `#0A0C10`, pane `#0F1218`, card `#151923`, land `#232C3E`, lines `#2A3142`/`#3D4558`, text `#DEE3EC` (normal) / `#8C97AB` (muted) / `#5B6579` (faint), accent cyan `#5FB2C9`, green `#5CCB8A`, amber `#D7B04A`, red `#D9685C`, orange `#D99A5F`.
- **paper** (light): warm-grey page `#E5E3DD`, card `#F9F8F5`, ink `#1A1A1A`, accent `#005F99`, green `#1E6B48`, amber `#A36A00`, red `#B52E1D`, orange `#C45F18`.
- **forest** (dark green): page `#151A17`, accent gold `#C8A45C`, green `#6DB88B`.

Semantics are invariant across schemes: accent = active/informational, green = healthy/allowed, amber = degraded/attention, red = danger/offline/blocked/live-TX, orange = hot-end of the power meter.

### 6.2 Typography and controls

- Fonts: a condensed display face for headings, a humanist sans for body, and a monospace face for **all data, readouts, labels and buttons**. The fonts ship as bundled assets. The app never fetches them at runtime (the console must work with no internet beyond the LAN).
- Buttons: flat, 4 px radius, 1 px border. Pane background when inactive. Accent fill with black/white foreground when active. Red-tinted for danger. **Solid red fill** for "dangerActive" states (grounded, manual override, armed).
- Minimum touch target 44×40 (48×48 for icon buttons), with two documented deliberate exceptions (rotator preset buttons, compass zoom stack). A reconstruction must consciously re-decide these.
- A purple-bordered card variant marks the station-power panel.
- Type scale for width: ≥1400 px → ×1.25, ≥1200 px → ×1.1.
- All decorative animations (the 700 ms pulsing dot, the 180 ms fades, the PA marker decay) must respect the platform reduced-motion setting when the platform supplies one. Under reduced motion the console must show a static presentation that stays legible.
