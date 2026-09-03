# hf_console — Mühle HF station tablet/phone console

Flutter console for operating the HF portion of the Mühle station from an Android tablet or an iPhone. Connects directly to the **shack MQTT broker on shari** (`192.168.1.139:1883`; see `../mqtt-broker/README.md`) — raw TCP on Android/iOS, WebSocket via the Go bridge on web. The shack broker is bridged to the Home Assistant broker, so the console keeps working when the shack↔house link is down.

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
- `lib/ui/screens/console_screen.dart` — single-screen layout
- `lib/ui/widgets/*.dart` — compass, PA meter, tuner, antenna, power, climate, tx indicator, confirm dialog
- `lib/dxspot/world_geometry.dart` — singleton loader for the bundled
  Natural Earth 50m coastline outlines (`assets/geo/world.geojson`,
  ~3 MB raw / ~1 MB gzipped). Lazy-loads once at startup; the compass
  painter AEQD-projects the rings against the same `Aeqd` instance that
  positions the DX spots, so the continent outlines line up with the spot
  dots.

## MQTT

Uses `mqtt_client` v10.x, MQTT 3.1.1, direct TCP to the shack broker on shari (`192.168.1.139:1883`). Credentials stored in `flutter_secure_storage`; entered on first launch (use the dedicated `console` account below).

Create a dedicated broker user `console` with narrow ACL:
- subscribe: `muehle/#`
- publish: `muehle/+/cmd`

Do NOT reuse the broad `hf` user; do not embed station-wide credentials in the APK.

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

Retained cmd slots (self-healing steady state): `power/master`, `power/psu-13v8`, `hf/switch`, `hf/pa-arm`, `hf/ant-switch`, `hf/antenna-select`.

Retained but one-shot (consumer clears the topic after every execution — a command does NOT re-apply if the bridge restarts): `hf/ant-ctrl`.

Non-retained (one-shot): `hf/pa`, `hf/rotator`, `hf/tuner`, `hf/power-seq`.

## Value-key deviations

- `ant-switch` / `antenna-select`: value-key-only, no `action`
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

### Web channel on shari

There is also a web deployment on shari for browser access from the LAN:

```bash
cd hf_console
./deploy.sh           # flutter prebuild + flutter build web + Go bridge + deploy
```

This builds the Flutter web app and a small Go HTTP/WebSocket bridge, then
installs them on shari as the `hf-console-web` systemd service on port `8091`:

```
http://shari:8091/
```

The browser cannot open raw TCP sockets, so the web build uses WebSocket. The
Go bridge (`webbridge/`) serves the static Flutter build at `/` and forwards
the `/mqtt` WebSocket stream byte-for-byte to the shack broker on shari
(`192.168.1.139:1883`). The Android APK continues to connect directly over TCP.

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
