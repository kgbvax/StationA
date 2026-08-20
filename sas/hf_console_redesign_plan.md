# hf_console UI redesign — plan

## What the console is for
A fixed-mount Android tablet on the operating desk that controls the HF station:
rotator direction, Ultrabeam pattern, antenna switch, PA operate/standby + meters,
ATR-1000 tuner, and station power/HVAC. Every action must be reachable in one tap.
The operator may be wearing headphones, in dim light, or in a hurry. Readability
and muscle-memory matter more than polish.

## Constraints from the brief
- Page selector: **Station**, **HF**, **UHF** (placeholder).
- HF page must contain, in one click each:
  - Rotator: current azimuth, 3 dB lobe direction/width, drag-to-set target,
    pronounced dotted target line while slewing, preset shortcuts (NA/SA/VK/…),
    Ultrabeam direction mode + band/retracted status.
  - Antenna select: Isolated / Dummy / Ultrabeam / Fan-Dipole / NN1 / NN2,
    Auto / Manual mode.
  - PA: FWD power bar (0–1200 W, 1000–1200 red), SWR bar (0–4, ≥1.5 orange,
    ≥3 red), current error message, Operate / Standby.
  - Tuner: inline/bypass + tune controls.
- No frequency/mode readout (already on the primary operating UI).
- Dark + light mode; responsive landscape tablet.
- No stereotypical "AI" or "corporate dashboard" look. Avoid neon-on-black,
  thin trendy fonts, glassmorphism, or excessive rounded corners.
- Text must be legible first.

## What I noticed / suggest adding
- **Fault strip**: a shared area at the bottom of every page shows the last
  three faults. Active faults are highlighted (e.g. red border, solid background);
  cleared/past faults are muted (e.g. greyed, struck-through, or dimmed). This
  absorbs the PA error message so the PA panel only shows meters and controls.
- **Offline state**: when MQTT drops or a slot goes down, the affected control
  should grey out and show why, not just stop updating.
- **Station page**: power master + PSU-13V8 toggles/summary + HVAC placeholder.
- **UHF page**: placeholder — I propose a simple "UHF controls not yet wired"
  card so the tab selector feels complete.
- **Dangerous operations are blocked by interlocks**: no extra taps, holds, or
  confirmation dialogs. If an action is rejected, the resulting fault appears in
  the shared fault strip.

## Three design directions

All three share the same data and controls. They differ in information
architecture, visual metaphor, and layout rhythm. Each ships with a dark and
a light palette.

---

### Direction A — "Shop Panel"
A utilitarian wall-mounted control panel. Big, flat modules, chunky tactile
buttons, thick separators. The aesthetic is deliberately plain-industrial:
everything looks like a label you could tape to a relay box. Readability is
maximized by generous padding and high-contrast fills.

**Layout**
- Top: wide page tabs as large, unambiguous segments: `[STATION] [HF] [UHF]`.
- HF page is a regular 2×2-ish grid of modules, all visible at once:
  ```
  +------------------+------------------+
  |  ROTATOR         |  PA + METERS     |
  |  (large compass) |                  |
  +------------------+------------------+
  |  ANTENNA SELECT  |  TUNER           |
  +------------------+------------------+
  |  ULTRABEAM       |
  +------------------+
  ```
- Each module is a bordered rectangle with a bold header. Buttons sit in a
  single row of equal-height cells at the bottom of their module.
- Rotator presets are wide shortcut buttons, not tiny pills.

**Color**
- Dark: near-charcoal page (`#15171A`), slightly lighter card (`#1E2126`),
  off-white text, muted blue for active state, amber for warn, red for danger.
- Light: very light warm grey page (`#F2F0EA`), white card, near-black text,
  same semantic colors but slightly darker for contrast.

**Typography**
- Data and labels: IBM Plex Mono / Saira — already in the project.
- Module titles: bold sans, medium size, all-caps, letter-spaced.

**Signature / risk**
- **Risk taken**: deliberately chunky, almost un-styled buttons with visible
  borders and 0–2 px radius. In an era of soft pill buttons, this makes the
  console feel like hardware — but it could look "unfinished" if not executed
  with consistent spacing. The discipline is in the grid, not the decoration.

**Pros/cons**
- + Fastest scanning; everything is visible.
- + Very easy to hit targets.
- − Can feel crowded on a 10" tablet if all modules stay expanded.

---

### Direction B — "Logbook Strips"
Inspired by amateur radio log sheets and air-traffic control strips: each
subsystem is one horizontal band that spans the full width. You read the page
like a list, top to bottom. Strong typographic hierarchy, ruled hairlines,
monospace readouts, and a "paper" sensibility even in dark mode.

**Layout**
- Top: slim page tabs as folder-style tabs along the top edge.
- HF page is a stack of full-width strips:
  ```
  [ HF · Rotator     ]  [azim] [compass mini] [presets NA SA VK ...] [mode FWD 180 BI]
  [ HF · Antenna     ]  [current] [ISOL] [DUMMY] [ULTRABEAM] [FAN-DIP] [NN1] [NN2] [AUTO/MAN]
  [ HF · PA          ]  [FWD bar 0...1200W] [SWR bar 0...4] [msg] [OPERATE] [STANDBY]
  [ HF · Tuner       ]  [IN LINE] [BYPASS] [TUNE MEM] [TUNE FULL]
  ```
- Each strip has a left label column, a center data column, and a right action
  column. Hairline rules separate strips.
- The rotator strip contains a compact compass/rose on the left and presets on
  the right.

**Color**
- Dark: deep ink (`#0D1117`) page, dark navy strip (`#151B23`), warm amber for
  active, cream/off-white for text, subtle grey rules.
- Light: warm white page (`#FDFCF8`), pale grey strip (`#F3F1EA`), dark grey
  ink text, same amber/blue/red semantic accents but on paper.

**Typography**
- Section labels: a sturdy serif or slab for the "paper logbook" voice.
- Readouts: monospace, medium weight.
- Action buttons: sans, compact, with ruled borders.

**Signature / risk**
- **Risk taken**: a serif/slab display face in an industrial control context.
  It could feel too literary, but paired with monospace data it reads as
  "precision paperwork" rather than decoration. The single memorable element
  is the ruled strip layout itself.

**Pros/cons**
- + Scales naturally to different tablet widths; each strip just gets wider.
- + Very scannable: each subsystem is one reading line.
- − Rotator compass is smaller than in Direction A; drag-to-set target needs a
  wider strip or an expanded state. We can make the rotator strip taller.

---

### Direction C — "Split Deck"
A two-zone layout: the **world pane** on the left always shows the antenna
compass + selected antenna; the **command pane** on the right switches with the
page tabs. This keeps spatial orientation visible while changing contexts.

**Layout**
- Top: three tabs switch only the right pane. A small status bar sits above.
- Left pane (≈55 % width, fixed):
  - Large circular compass with drag-to-set, target dotted line, lobe wedges.
  - Below it: current antenna and a one-line mode summary.
- Right pane (≈45 % width, page-specific):
  - **HF page**: vertical stack of control cards — Rotator presets & mode,
    Antenna select, PA meters, Tuner.
  - **Station page**: Power cards + HVAC placeholder.
  - **UHF page**: placeholder card.

**Color**
- Dark: dark graphite page, slightly lighter graphite panes, subtle inner
  shadow to separate left/right. Semantic colors are subdued except for the
  target dotted line and TX warn.
- Light: cool grey page, white panes, soft shadow between panes.

**Typography**
- Mixed: sans for UI chrome, monospace for data, slightly condensed display
  face for the big azimuth readout.

**Signature / risk**
- **Risk taken**: the tabs do not switch the whole screen — they switch only
  one pane. This breaks the conventional mobile-page model but matches how
  physical consoles keep a context display always in view. The risk is that
  users may expect tabs to change everything; the tab labels must clearly
  describe the right-pane content.

**Pros/cons**
- + Best for keeping rotator context while operating PA/tuner.
- + Distinctive; hard to mistake for a generic app.
- − Less horizontal room for PA meters and antenna buttons on small tablets.
- − More complex responsive logic: left pane may collapse on very small screens.

---

## What I recommend
I will build interactive HTML mockups for all three directions so you can see
the same content in each IA and pick one. Once you choose a direction, I will
refine the palette and implement it in `hf_console/lib/ui/...` as the new
Flutter UI.

## Open questions
1. Are you OK with Direction A's chunky hardware-like buttons, or do you want
   slightly softer but still flat treatment?
2. For Direction B, is the ruled-strip / paper-logbook metaphor acceptable,
   or does it feel too informal for a station console?
3. For Direction C, do you like the idea of a persistent left pane, or do you
   prefer tabs that change the whole screen?
