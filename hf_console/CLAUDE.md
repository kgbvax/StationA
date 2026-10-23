# hf_console — Mühle HF station tablet/phone console

Flutter console for operating the HF portion of the Mühle station from an Android tablet or an iPhone. Connects directly to the MQTT broker — raw TCP on Android/iOS, WebSocket via the Go bridge on web. The setup screen and the web bridge default to the HA broker `192.168.1.50:1883`, the live station broker for most clients (root `CLAUDE.md` live-state note). The station broker on scmino (`192.168.1.178:1883`, see `../mqtt-broker/README.md`) mirrors `muehle/#` both ways; a console pointed at it keeps working when the shack↔house link is down. shari runs no broker.

## Design

See `../sas/tablet_console_hybrid_preview.html` for the approved high-fidelity reference. Direction: DCs color scheme + controls (near-black cards, cyan/green/amber/red semantic palette, monospace readouts) combined with the at-a-glance console-grid layout from the design handoff.

## Architecture

- `lib/main.dart` — app entry, orientation (responsive: phones may rotate, tablets stay landscape), theme, root provider
- `lib/mqtt/mqtt_service.dart` — `mqtt_client` wrapper: connect, subscribe `muehle/#`, publish, retained semantics, reconnect
- `lib/mqtt/client_factory*.dart` — platform-appropriate MQTT client (`MqttServerClient` on Android, `MqttBrowserClient` over WebSocket on web)
- `webbridge/` — Go HTTP server + WebSocket-to-MQTT bridge for the web deployment
- `lib/store/bus_store.dart` — `ChangeNotifier` holding all slot state; computed offline list
- `lib/store/wiring.dart` — `ANTENNA_MAP`, `CMD_RETAIN`, cmd payload builders, value-key deviations
- `lib/ui/theme.dart` — color/type tokens, `AppTheme.bandColor(...)` for the
  compass (see below)
- `lib/ui/screens/console_screen.dart` — single-screen layout (Station/HF/UHF/CAM pseudo-tabs)
- `lib/ui/widgets/*.dart` — compass, PA meter, tuner, antenna, power, tx indicator, confirm dialog
- `lib/ui/widgets/station_schematic.dart` + `lib/store/station_schematic.dart` — the Station page's left pane (tablet
  only): a read-only process-control schematic of the whole station (see "Station schematic" below)
- `lib/store/ant_mode.dart` — the antenna-mode snapshot (`muehle/hf/ant-mode`) parsed per the slot's reading rules;
  `BusStore.antMode` holds the last valid one (see "Antenna mode" below)
- `lib/ui/widgets/ant_mode_panel.dart` — the antenna-mode strip under the antenna switch (HF page)
- `lib/store/station_hosts.dart` — slot → component/host map for the schematic's hosts row; mirrors the stationportal
  inventory `[[software]]` table (`test/store/station_hosts_test.dart` cross-checks it)
- `lib/vhfcam/*.dart` — antenna-cam feed client: HLS player state + poller for
  vhfcam-restream's preview server (`:8083` on scmino). Bus-independent HTTP;
  `muehle/hf/vhfcam` is deliberately NOT in `expectedSlots` — the cam is an
  accessory, not a station slot, its silence is not a station fault.
- `lib/dxspot/world_geometry.dart` — singleton loader for the bundled
  Natural Earth 50m coastline outlines (`assets/geo/world.geojson`,
  ~3 MB raw / ~1 MB gzipped). Lazy-loads once at startup; the compass
  painter AEQD-projects the rings against the same `Aeqd` instance that
  positions the DX spots, so the continent outlines line up with the spot
  dots.

## MQTT

Uses `mqtt_client` v10.x, MQTT 3.1.1, direct TCP to the broker entered on first launch (setup screen default `192.168.1.50:1883`; see the intro). Credentials stored in `flutter_secure_storage`; entered on first launch (use the dedicated `console` account below).

Create a dedicated broker user `console` with narrow ACL (as in `../mqtt-broker/acl.conf.example`; not seeded on the scmino broker yet, whose only local account is `hf`):
- subscribe: `muehle/+/+/state`, `muehle/+/+/meta`, `muehle/+/+/status`
- publish: `muehle/+/+/cmd`

Do NOT reuse the broad `hf` user; do not embed station-wide credentials in the APK.

## Map modules

Two map modules share `DxMapContainer`:

- **HF** (`DxMapContainer(rotator: hfRotator)`, HF tab): azimuthal
  compass with a Mercator toggle; beam, tap-to-aim and STOP live on the compass.
  The Station tab has no map: its left pane is the station schematic (the
  two-column split stays so the top-bar toggles don't move).
- **VHF/UHF** (`DxMapContainer.vhf()`, UHF + CAM tabs): Mercator only, opens at
  zoom 7 on the station (up to 12), draws the UHF az rotator's great-circle
  beam (±20°) and target line, aims by tap (great-circle bearing from the
  station, `goto` to `uhf/az-rotator`), and carries PARK (one bridge-side
  `park` intent — both sat axes to the bridge's configured park, az 200° /
  el 0°, shown on the sat panel's key from /meta; it also retunes the IC-9700 when its bridge advertises a park target, and selects vertical polarization — retained `set_pol v` to `uhf/pol-ctrl` when online) and a STOP that stops both
  sat axes. It also draws the satellite OscarWatch is tracking
  (`muehle/uhf/sat-track`, `lib/store/sat_track.dart`): a green diamond on
  the sub-satellite point with `name  EL n°  range km`, the footprint circle
  (horizon distance for `alt_km`) and the great circle from the station —
  dimmed below the horizon, hidden unless tracking + device_online + bridge
  online + link up. While a satellite is tracked the view auto-frames the
  station and the sub-satellite point (`lib/dxspot/map_fit.dart`, zoom capped
  at the opening zoom 7) and the rotator beam/target line draws amber instead
  of cyan; the first pan/pinch/zoom hands the view to the operator until the
  next pass or the crosshair (RESET). One finger pans, two fingers pinch-zoom
  about their centre. Bearings are only
  as good as the station locator — use 6 characters (the settings dialog
  warns on 4).

Beam, footprint and station line are great circles drawn by
`lib/dxspot/overlay_geometry.dart`: sampled by arc length (≤ 2°), projected
continuously across the antimeridian seam (repeated per world copy) and clipped
to the world rectangle, with latitudes up to ±89.5° projected for real so a ray
over a pole leaves the map at the edge and re-enters on the far meridian. The
beam reaches the farthest viewport point, capped at 4000 km (`_kBeamMaxKm`). `test/tool/overlay_render_test.dart` renders a zoom × pole grid to PNGs
for eyeballing (`OVERLAY_RENDER=1 flutter test test/tool/overlay_render_test.dart`,
output in `/tmp/overlay`).

The Mercator painter also draws `assets/geo/places.json` — cities from Natural
Earth 10m populated places (public domain, Europe), generated by
`tool/make_places.py`, thinned by zoom and label overlap. Cities only: small
towns are not in the dataset.

## Station schematic (Station tab)

On tablets the Station page's left pane is `StationSchematic` instead of a map (phones keep the power card + faults
column). One fixed 760×790 sheet, scaled to the pane: hosts on the LAN, the power path (house mains → the three Shelly
plugs → the 13.8 V TRX PSU bus feeding FLEX, ATR-1000, antenna switch, PLC #1 and IC-9700; the antenna plug feeding the
G-450, RCU-06 and the UHF rotators), the HF RF chain into the antenna switch (wiper on the selected port) and the UHF
chain. Read-only: a tap opens a details popover (bridge, host, liveness, every /state field); commands stay in the
panels. Drawn in the console's own idiom — powered lines green, selected RF path cyan, carrier red.

Liveness comes from `StationSchematicModel` (`lib/store/station_schematic.dart`) and follows the faults bar: the same
two-layer rule, the same standby / antenna-plug suppression (`antPoweredSlots`, shared with `faults_bar.dart`), the
sleeping shack PC and the idle IC-9700 session are not faults. It goes one step further than the fault list: a feed
confirmed off explains its devices — the 13.8 V PSU (`psuPoweredSlots`; PLC #1 and the ESPHome switch drop their LWT
with it, so their bridge-down reads unpowered too), and the PA remote-on relay for the ACOM. Plug power is three-state:
a dead plug bridge or unreachable Shelly draws its bus unknown (grey), never on. SWR thresholds are the tuner panel's
(2.0 warn, 3.0 alarm); the PA's only count while keyed. Plugs show `/state.apower` in W once shelly-power-bridge
publishes it (the metering gap in `../shelly-power-bridge/docs/known-issues.md`), the relay word until then.

Eyeball real renders (bundled fonts, every scheme × scenario) with
`SCHEMATIC_RENDER=1 flutter test test/tool/station_schematic_render_test.dart` → `/tmp/schematic/*.png`
(`SCHEMATIC_TAP=muehle/hf/pa` also opens that block's popover in the running scenario). The whole tablet Station page
(top bar, schematic, power card, faults) renders with `PAGE_RENDER=1 flutter test test/tool/station_page_render_test.dart`
→ `/tmp/station-page/*.png` (plus the other pages, and the HF page under a storm lock as `page-hf-storm.png`). The HTML idiom study that led
to this design is `../sas/station_schematic_variants.html`.

## Antenna mode (HF tab)

The station antenna mode — `normal` | `storm` | `maintenance` — lives on the logic slot `muehle/hf/ant-mode`
(component `antmode`, contract `../antmode/docs/ant-mode-mqtt-api.md`). In storm and maintenance the switch is
grounded, the Ultrabeam elements are retracted and the HF rotator is parked; the **bridges** refuse motion and execute
the park themselves. The console only mirrors the lock so a refused tap is never silent:

- `BusStore.antMode` reads `/state` per the slot's rules (same as `shared/antmode.Parse`): no retained state →
  normal; a locked snapshot holds while antmode's `/status` is offline (a dead owner never unlocks); a malformed one
  keeps the last known mode (counted in `malformedPayloads`); `ts` is not checked.
- `AntModePanel` — the strip under the antenna switch (phones: after it in the scroll column): NORMAL / STORM /
  MAINT. keys, each change behind a confirmation dialog (`set_ant_mode`); REPARK (confirmed) while locked; phase,
  park/drift, source, auto exit, wind, presence and the active DWD warnings as mono lines; the storm suggestion
  (`storm_suggested`) as a banner with ENTER STORM (same confirmation) / DISMISS (`dismiss_suggestion`). With antmode
  down the keys are off and a line names the mode that is still enforced.
- Locked controls (greyed, with the reason in the card's pill band): Ultrabeam FORWARD / 180° / BI-DIR / AUTO and
  the 6 m forward correction (RETRACT stays live); the antenna-switch port keys (AUTO/MANUAL stay live — they act
  once the lock is gone); compass tap-to-aim, Mercator tap-to-aim and the selected-station chip in storm **and**
  maintenance (the chip reads `STORM LOCK · parked n°`, or `MAINTENANCE · NUDGE ±5° ONLY`). The HF map rail has
  STOP, which always stays live, plus `+5°` / `−5°` nudge keys in maintenance with `lock.nudge`. The reason
  states only what is confirmed: `parked n°` needs antmode phase `parked` (else `parking → n°` / `parking`, or
  `PARK FAILED`); the switch card's `grounded`
  needs ant-switch `selected: off` (else `grounding…`); the Ultrabeam card's `elements held in` needs ultrabridge
  `retracted` and not `moving` (else `elements not confirmed in`).
- Every `/cmd` to `hf/ant-mode` carries `ts` (UTC, whole seconds): antmode drops a request older than 30 s or more
  than 5 s ahead, so a request retained while antmode was dead never applies hours later. The panel re-checks
  antmode's liveness when the confirmation dialog closes.
- Aiming (`lib/store/aim.dart`, `aimRoute`): under any lock an HF aim (disc, map or chip tap) sends nothing — a tap
  can name any bearing, and one stray tap would slew the brakeless G-450 a full sweep with people at the mast. In
  maintenance with `lock.nudge` the rail's `+5°` / `−5°` keys (`publishNudge`, `maintenanceNudgeStepDeg`) send
  `set_az` = readback ± 5° (step bounded to 5°, target clamped to 0..450, never wrapped mod 360) with
  `"override":"maintenance"` straight to `hf/rotator`, never through beamsteer (even with AUTO on); they are off
  without a readback, the rotator bridge or the link. The rotator bridge does not bound the override itself. The sat
  rotators are not locked by the antenna mode.
- The faults bar carries an amber lock banner or the storm suggestion above the list (station state, not faults); a
  failed park still lands in the list via the slot's `/state.fault`. The station schematic draws an `Ant mode` pill
  beside Beam steer (warn while locked or suggesting, alarm on a fault).

## Antenna cam (CAM tab)

The CAM tab plays vhfcam-restream's live HLS preview (`/hls/live.m3u8` on the
cam server, default `http://192.168.1.178:8083`) and mirrors its radio-audio
controls (`POST /api/cmd/{audio_on,audio_off,power_on}` — audio_on takes the
IC-9700 CI-V session, exactly like the :8083 reference page). Its RECORDING
card starts/stops a server-side recording of the preview (`POST
/api/rec/start|stop`; progress = the `rec` block of `/api/radio-status`, a
REC badge also sits on the feed header). The server enforces the limits and
holds the radio audio while recording; downloads happen on the :8083 page.
Two deliberate platform notes:

- The base URL is a user setting, key `vhfcam_base_url` (CredentialStore,
  editable in the gear sheet, default `http://192.168.1.178:8083`). A saved
  value still pointing at shari (`.139`/`.140`/`shari`, pre-2026-09-30 host)
  is treated as unset and resolves to the scmino default
  (`resolveVhfcamBaseUrl`).
- `macos/Runner/Info.plist` carries `NSAppTransportSecurity →
  NSAllowsLocalNetworking` — AVPlayer refuses the cleartext LAN URL without
  it. This is the LAN-scoped exception and deliberately NOT set on iOS
  (see the iPhone section); the web build gets a placeholder instead of the
  feed (no HLS/TS decoder in the browser player).

## Band colors (horstreporter convention)

`AppTheme.bandColor(String band)` in `lib/ui/theme.dart` is the canonical
band-color entry point for this app. The palette is hard-coded to match
horstreporter's `static/utils.js:577-592` `bandColors` table — NOT to the
DC/Paper/Forest theme palettes. Reason: the visual correspondence
with the horstreporter web frontend is deliberate; a spot a user noticed on
the web stays recognizable when it appears on the tablet, regardless of
which theme is active. Grey fallback (`#555555`) matches horstreporter's
`bandColors.all` and is used for unknown / unbanded labels. The full table
and rationale live in `docs/conventions/band-mode-reference.md` — that file
is the source of truth; this app's palette must be kept in sync with it.

## Retained /cmd policy

Retained cmd slots (self-healing steady state): `power/psu-13v8`, `power/antennas` (rotators + Ultrabeam controller plug, ANT PWR toggle), `power/shack-pc-mains` (bwpc mains plug, PC PWR toggle — off asks for confirmation), `hf/switch`, `hf/pa-arm`, `hf/ant-switch`, `hf/antenna-select`, `hf/beam-steer` (`enable`/`disable` — the smart-rotation toggle survives a beamsteer restart), `uhf/pol-ctrl` (`set_pol` — desired phase re-applies after a controller reboot or broker reconnect, KTD13's deliberate contrast with the one-shot sat rotators).

Retained but one-shot (consumer clears the topic after every execution — a command does NOT re-apply if the bridge restarts): `hf/ant-ctrl`, `hf/ant-mode` (`set_ant_mode` / `repark` / `dismiss_suggestion`; antmode applies, then clears — a request sent during a short antmode restart is applied once it is back, within the 30 s window of the `ts` the console stamps on it).

Non-retained (one-shot): `power/shack-pc` (`sleep` — retained, it would re-sleep the PC on every wake), `hf/pa`, `hf/rotator`, `hf/tuner`, `hf/power-seq`, `hf/radio` (DVK `play`/`stop`), `uhf/az-rotator`, `uhf/el-rotator` (sat `goto`/`stop` — a stale retained or queued motion must never replay against real antennas), `uhf/radio` (icom9700-radio-bridge, receive-only since 2026-09: the action set is exactly `audio_on`/`audio_off`/`power_on`/`monitor_on`/`monitor_off`/`park_radio` — `park_radio` retunes via the bridge's rigctld on scmino; there is no PTT/arm/tuning path anymore).

The machine-readable source of truth is `cmdRetain` in `lib/store/wiring.dart`; keep this list in sync with it.

## Value-key deviations

- `ant-switch` / `antenna-select`: value-key-only, no `action`
- `uhf/radio` actions carry no `value` — the action name is the whole intent (receive-only posture, 2026-09; the per-VFO string-value deviation is gone with the removed tuning actions)
- `pa-arm.set_enabled`: value is a **string** `"true"` / `"false"`
- `tuner.set_inline`: value is a real JSON **bool**
- `tuner.tune`: value is a **string** `"mem"` / `"full"`

## Build

### Android tablet (APK)

Run the prebuild gate before every release APK:

```bash
cd hf_console
tool/prebuild.sh      # flutter analyze + flutter test
flutter build apk --release
```

Sideload `build/app/outputs/flutter-apk/app-release.apk` onto the tablet.

### Web channel on scmino

There is also a web deployment on scmino (`192.168.1.178`; on shari until
2026-10-02) for browser access from the LAN:

```bash
cd hf_console
./deploy.sh           # flutter prebuild + flutter build web + Go bridge + deploy
```

This builds the Flutter web app and a small Go HTTP/WebSocket bridge, then
installs them on scmino as the `hf-console-web` systemd service on port `8091`:

```
http://scmino:8091/
```

The browser cannot open raw TCP sockets, so the web build uses WebSocket. The
Go bridge (`webbridge/`) serves the static Flutter build at `/` and forwards
the `/mqtt` WebSocket stream byte-for-byte to the station broker
(`-mqtt-broker`, default `192.168.1.50:1883` — the live HA broker). The page
derives its WebSocket endpoint from its own origin (`Uri.base`), so the build
works on whichever host serves it. The Android APK continues to connect
directly over TCP.

### iPhone (IPA, self-sideloaded)

The same Flutter app targets iOS. On iPhone the console reflows into a single
vertical scroll (DX map on top, then every control panel) via the
`shortestSide < 600` branch in `_HfPage`; tablets keep the side-by-side grid.
Raw-TCP MQTT bypasses iOS App Transport Security (ATS governs only
NSURLSession/HTTP/WS), so **no `NSAllowsArbitraryLoads` is set** — adding it
would be an App-Review red flag and is unnecessary. The DX-spot HTTPS feed is
ATS-safe. Credentials go to the iOS Keychain (`flutter_secure_storage`); no
Keychain Sharing entitlement is needed for a single-app install.

One-time signing setup (interactive, in Xcode — do NOT commit your Apple team
ID to the repo): open `ios/Runner.xcworkspace` → Runner target → Signing &
Capabilities → "Automatically manage signing" → pick your Apple ID. A free
Apple ID yields a 7-day-expiring profile (and 3-apps/week limit); a paid
account gives 1 year.

```bash
cd hf_console
tool/build-ios.sh       # prebuild gate + regen launcher icons + flutter build ipa --release
```

Sideload `build/ios/ipa/hf_console.ipa` via Xcode → Devices and Simulators, or
`ios-deploy --bundle build/ios/ipa/hf_console.ipa`.

A no-signing smoke build (compile + plugin-link check, no Apple ID needed):

```bash
flutter build ios --release --no-codesign
```

## Verification

- `tool/prebuild.sh` passes (`flutter analyze` + `flutter test`)
- `flutter build apk --release` succeeds
- `flutter build ios --release --no-codesign` succeeds (iOS compile + plugin link, no signing needed)
- On the LAN tablet: enter `console` broker creds, confirm subscription traffic via `mosquitto_sub -t 'muehle/#' -v`
- Offline fonts render correctly (no `google_fonts` runtime fetch)
- Reduced motion disables animated transitions
- All touch targets ≥48dp
