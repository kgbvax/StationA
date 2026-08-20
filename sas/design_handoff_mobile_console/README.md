# Handoff: Mühle station — mobile console (design 3a)

## Overview
A single-screen mobile operator console for the **Mühle** amateur-radio station.
It shows every slot's live state at a glance and exposes the controls an operator
needs from a phone, with **station power** (one-button startup/shutdown + per-relay
control, including TRX and PA power) as the headline addition. It is the mobile
restack of the desktop "console grid" (design 2a).

## About the design files
The files in this bundle are **design references authored in HTML** (a Design
Component prototype). They demonstrate the intended look, layout, copy, and
interaction — they are **not** production code to ship as-is. The task is to
**recreate this screen in the target mobile app's environment** (React Native,
SwiftUI, Flutter, Jetpack Compose, or a responsive web app) using that codebase's
established components, navigation, and state patterns. If no app exists yet,
pick the framework that best fits the team and implement there.

The live data and control semantics are defined by the **MQTT contracts** copied
into `mqtt-schemas/` — those are the source of truth for behavior; the HTML only
shows presentation.

## Fidelity
**High-fidelity.** Final colors, typography, spacing, states, and interactions
are all specified below and present in the prototype. Recreate the UI faithfully
using the app's own design-system primitives (buttons, toggles, pills).

## Design system
Built on the **Exact Design System** (brand purple `#402FD8`, lavender surfaces,
pill-shaped controls, Inter type, dark theme). The prototype loads the Exact
bundle; the app should map every component below to the equivalent Exact
component in the target codebase rather than re-styling raw elements. Substituted
in the prototype and to be replaced in production: the concentric-arc **Exact
logo mark** and any **Lucide** stand-in icons → real Exact icon SVGs.

---

## Screen: Mobile console (`3a`)
Single scrolling column inside a phone viewport. Design width **394px** device
(≈366px content column); everything is a vertical stack of cards with 12px gaps.
Dark theme throughout: page `#050506`, cards `rgba(255,255,255,.05)` on `#0a0a0c`.

Card order, top → bottom:
1. **Header** (station identity + health)
2. **Radio**
3. **PA · ACOM 1200S** (HF only)
4. **Tuner · ATR-1000** (HF only)
5. **Ultrabeam** (HF only)
6. **Antenna select** (HF only)
7. **Rotator** (azimuth dial)
8. **Station power** (sequencer + relays) — the primary new feature

`HF` vs `UHF` is a top-level station switch; the four HF-only cards are hidden on
UHF, which instead surfaces its own radio/rotator/polarization fields.

### Card: Header
- Row 1: a status dot (color = overall health) + **"Mühle · {health}"** (16px/800,
  `#fff`), and a right-aligned host **StatusPill** (dot + host name, e.g. `shari`).
- Row 2: full-width **SegmentedControl** — `HF station` / `UHF station`.
- Row 3: wrapping list of per-slot health dots (8px dot + 10.5px label at
  `rgba(255,255,255,.6)`): host, Radio, PA, Antenna switch, Antenna controller,
  Rotator, Tuner, Antenna-select, PA-arm.
- Overall health: `Nominal` (green `--highlight-success`), `Active` (blue
  `--bug-process`), `Fault` (amber `--highlight-warning`), `Degraded` (red
  `--highlight-error`), computed from the worst slot status.

### Card: Radio
- Overline **"Radio"** + right pill: **● TX** (bg `--bug-open`, white) when
  transmitting, else **RX** (bg `rgba(255,255,255,.08)`).
- Big band (30px/800), mode (15px/700 `--brand-color-lavender-1`), frequency
  (12px monospace, `MHz`, 3 decimals).
- Drive row: label + `{n}%`, then an 8px pill progress bar; fill is
  `--brand-color-purple-3` (RX) or `--bug-open` (TX), width = drive %.

### Card: PA · ACOM 1200S (HF)
- Overline + right-aligned `SWR {n.nn}`.
- Two buttons (md): **Operate** (primary when active) and **Standby** — when
  Standby is the active mode it is highlighted amber (attention wrap, see Tokens).
- Forward-power row: label + `{n} W`, then a 9px bar (`--highlight-success` fill,
  full-scale 500 W).
- Error banner (amber) shown when `pa.error` is set.

### Card: Tuner · ATR-1000 (HF)
- Overline. SWR shown large (28px monospace) colored by value: `<2` white,
  `2–3` amber, `≥3` red (`--bug-open`). State pill: **In line** (green) or
  **Bypassed** (amber).
- Buttons (sm): **Tune mem**, **Tune full**, and **Bypass/In line** toggle.
- Fault banner (amber) when `tuner.fault` set.

### Card: Ultrabeam (HF)
- Overline + `{band} · {freq}`.
- Direction buttons (sm): **Forward**, **180°**, **Bidirectional** (the active
  one is primary; **180°/reverse** highlights amber when active). Plus a
  destructive-outlined **Retract**.

### Card: Antenna select (HF)
- Overline + source pill: **Manual override** (amber) or **Auto (reconciler)** (green).
- "Selected: **{port label}**".
- Port buttons (sm) by name: `Off, P1…P6` (active = primary) + an **Auto** button
  (returns arbitration to the reconciler; disabled unless currently manual).

### Card: Rotator
- 168px circular azimuth dial, **drag to steer** (pointer events; `touch-action:none`).
  - N/S/E/W ticks; center hub; needle = current azimuth (purple, glowing).
  - **Coverage cone(s)** drawn from Ultrabeam direction: Forward = one 60° purple
    cone at heading; 180°/reverse = one 60° **orange, glowing** cone opposite the
    boom; Bidirectional = two 90° purple cones front and back. **No gradients.**
  - **Dashed target line** at the commanded heading; hidden once within 5° of target.
  - Large azimuth readout `{deg}°` (22px monospace).
- "Slewing → {target}°" label (blue) while moving.
- Heading preset buttons (sm) below.

### Card: Station power (new — the headline)
Distinct card: `rgba(124,92,255,.10)` fill, `1px solid rgba(124,92,255,.35)` border.
- Header: overline **"Station power"** + phase pill:
  - `Idle` (grey), `Starting`/`Stopping` (blue `--bug-process`), `Running` (green).
- **Primary action button**, full width, 48px (`lg`, `block`):
  - phase `idle` → **"Start station"** (primary)
  - phase `running` → **"Stop station"** (destructive-outlined)
  - during a sequence → **"Starting…"/"Stopping…"**, disabled.
- While sequencing: a step line (blue dot + `{step label}…`).
- Divider, then four **relay rows** (each ≥44px tap target): label (14px/700) +
  slot address sub (monospace) + state pill (**On**/**Off**) + a **Toggle**:
  - **Mains** — `power/master`
  - **PSU 13.8 V** — `power/psu-13v8`
  - **Transceiver** — `hf/switch · TRX` (On pill highlighted **amber**)
  - **Amplifier** — `hf/switch · PA` (On pill highlighted **amber**)
  - TRX/PA rows only appear on HF.

---

## Interactions & behavior
- **Startup sequence** (`Start station`): drives, in order, mains on → PSU on →
  TRX on → PA on → arm PA, ending in phase `running`. Prototype paces each step
  ~850ms; production paces on real liveness confirmations (see powerseq schema).
- **Shutdown sequence** (`Stop station`): reverse — disarm → PA off → TRX off →
  PSU off → mains off, ending `idle`.
- **Manual relays** stay individually toggleable while the sequencer is **idle/running**;
  they are disabled while a sequence is in progress.
- **Safety cascade** (open-loop model in the prototype; enforced by the bridges in
  production): turning **Mains** off forces PSU + TRX + PA + arm off; turning
  **PSU** off forces TRX + PA + arm off; downstream toggles are disabled while
  their upstream supply is off; the radio cannot transmit unless mains+PSU+TRX are on.
- **Rotator**: drag the dial or tap a preset to command a heading; the boom slews
  toward the target; dashed target line hides within 5°.
- **Station switch** (HF/UHF) swaps the whole card set and the underlying slot model.
- Two simulation toggles exist in the desktop harness (host outage, ant-ctrl fault)
  and cascade to every card — replicate as the app's real offline/fault handling.

## State management
Live state mirrors the MQTT bus; each slot's `/state` is the read model and each
control publishes a `/cmd`. Minimum state for this screen:
- `power.master.power`, `power.psu.power` — `on|off` (+ `device_online`).
- `hf.sw.trx`, `hf.sw.pa` — `on|off` (the `hf/switch` remote-on relays).
- `hf.paarm.enabled` / derived `armed`.
- `seq.phase` (`idle|starting|running|stopping`) + `seq.step`.
- `radio` (freq/band/mode/tx/drive), `pa` (mode/keyed/fwd/refl/temp/error),
  `tuner` (inline/swr/fwd/settling/fault), `antctrl` (freq/direction/moving/error),
  `antswitch` (selected/settled) + `reconciler` (source/mode/target), `rotator`
  (az/target/moving; UHF adds el).

See `mqtt-schemas/` for the exact `/cmd` payloads (all use the `{"action","value"}`
convention) and `/state` fields. Notably: `/cmd` for power/switch is **retained**
(self-healing steady state); the sequencer `/cmd` is **not retained** (one-shot).

## Design tokens (as used)
- **Colors:** brand purple `#402FD8` / `--brand-color-purple-3`; page `#050506`;
  card `rgba(255,255,255,.05)`; power card `rgba(124,92,255,.10)` + border
  `rgba(124,92,255,.35)`. Status: success `--highlight-success`, warning/amber
  `--highlight-warning`, error `--highlight-error`, in-progress `--bug-process`,
  TX/open `--bug-open`. Text: `#fff` / `rgba(255,255,255,.6)` / `rgba(255,255,255,.4)`.
  Coverage cones: purple `rgba(124,92,255,.5)`, reverse orange `rgba(255,159,10,.55)`
  with `drop-shadow` glow.
- **Radius:** cards 16px; pills/toggles 999px; buttons pill.
- **Type:** Inter (UI); monospace for freq/SWR/azimuth. Overlines 11px/700 uppercase,
  letter-spacing .05em, `rgba(255,255,255,.5)`.
- **Spacing:** 12px between cards; 14–15px card padding; relay rows ≥44px tall.
- **Buttons:** `sm` 32px, `md` 40px, `lg` 48px; variants primary / secondary /
  destructive-outlined; `block` for full width.
- **Amber "attention" treatment** (Standby active, reverse direction active, TRX/PA
  On pills): background `--highlight-warning`, text `#1A1B2B`.
- **Motion:** bar/needle transitions ~0.3–0.4s ease; toggle thumb ~140ms.

## Assets
No bitmap assets. Exact logo mark + status icons are vector — use the real Exact
icon set in production (the prototype substitutes Lucide where icons appear).

## Files
- `Station Dashboard.dc.html` — the full prototype. Design 3a is the mobile
  section (`id="3a"`); the same view-models drive desktop designs 2a/2b for
  cross-reference. Logic (state model, sequencer, safety cascade, rotator math)
  lives in the `<script>` class at the bottom.
- `mqtt-schemas/` — the authoritative on-the-wire contracts:
  `powerseq` (sequencer), `shelly-power-bridge` (mains/PSU), `m5stamp-hf-ctrl`
  (TRX/PA relays + PA-arm), `acom1200s-pa`, `atr1k-tuner`, `ultrabridge`,
  `wrc-rotator`, `antennaselect`, `flexbridge` (radio), plus
  `station-integration-model.md` (overarching addressing/`/meta`/`/state`/`/cmd` model).
