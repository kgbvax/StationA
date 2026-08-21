# hf_console — Mühle HF station tablet console

Native(-ish) Android tablet console for operating the HF portion of the Mühle station. Connects directly to the MQTT broker at `192.168.1.50:1883`.

## Design

See `../sas/tablet_console_hybrid_preview.html` for the approved high-fidelity reference. Direction: DCs color scheme + controls (near-black cards, cyan/green/amber/red semantic palette, monospace readouts) combined with the at-a-glance console-grid layout from the design handoff.

## Architecture

- `lib/main.dart` — app entry, landscape lock, theme, root provider
- `lib/mqtt/mqtt_service.dart` — `mqtt_client` wrapper: connect, subscribe `muehle/#`, publish, retained semantics, reconnect
- `lib/store/bus_store.dart` — `ChangeNotifier` holding all slot state; computed offline list
- `lib/store/wiring.dart` — `ANTENNA_MAP`, `CMD_RETAIN`, cmd payload builders, value-key deviations
- `lib/ui/theme.dart` — color/type tokens
- `lib/ui/screens/console_screen.dart` — single-screen layout
- `lib/ui/widgets/*.dart` — compass, PA meter, tuner, antenna, power, climate, tx indicator, confirm dialog

## MQTT

Uses `mqtt_client` v10.x, MQTT 3.1.1, direct TCP to `192.168.1.50:1883`. Credentials stored in `flutter_secure_storage`; entered on first launch.

Create a dedicated broker user `console` with narrow ACL:
- subscribe: `muehle/#`
- publish: `muehle/+/cmd`

Do NOT reuse the broad `hf` user; do not embed station-wide credentials in the APK.

## Retained /cmd policy

Retained cmd slots (self-healing steady state): `power/master`, `power/psu-13v8`, `hf/switch`, `hf/pa-arm`, `hf/ant-ctrl`, `hf/ant-switch`, `hf/antenna-select`.

Non-retained (one-shot): `hf/pa`, `hf/rotator`, `hf/tuner`, `hf/power-seq`.

## Value-key deviations

- `ant-switch` / `antenna-select`: value-key-only, no `action`
- `pa-arm.set_enabled`: value is a **string** `"true"` / `"false"`
- `tuner.set_inline`: value is a real JSON **bool**
- `tuner.tune`: value is a **string** `"mem"` / `"full"`

## Build

Run the prebuild gate before every release APK:

```bash
cd hf_console
tool/prebuild.sh      # flutter analyze + flutter test
flutter build apk --release
```

Sideload the resulting APK onto the tablet. No deploy to shari — the app runs on the tablet and connects to the broker over the LAN.

## Verification

- `tool/prebuild.sh` passes (`flutter analyze` + `flutter test`)
- `flutter build apk --release` succeeds
- On the LAN tablet: enter `console` broker creds, confirm subscription traffic via `mosquitto_sub -t 'muehle/#' -v`
- Offline fonts render correctly (no `google_fonts` runtime fetch)
- Reduced motion disables animated transitions
- All touch targets ≥48dp
