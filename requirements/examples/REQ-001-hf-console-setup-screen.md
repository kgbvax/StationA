---
id: REQ-001
project: hf_console
title: Make the setup screen reachable after credentials are stored
created: 2026-09-18
deploy: manual-device
---

## Requirement

The setup screen (`lib/ui/screens/setup_screen.dart`) is shown only while no
broker credentials are stored (`_showConsole` gate in `lib/main.dart:172-202`).
Once credentials are saved there is no way back on the device: a wrong host, a
rotated `console` broker password, or a moved broker cannot be corrected — the
install must be wiped to reach setup again.

Add a path from the console back into the setup screen, prefilled with the
stored values, that saves and reconnects on save.

## Acceptance criteria

- [ ] Console screen offers a persistent affordance (top-bar gear) that opens
      the setup screen
- [ ] Setup screen opens prefilled with stored `mqtt_host`, `mqtt_port`,
      `mqtt_user`, `station_locator`, `horstreporter_base_url`; the password
      field starts empty and leaving it blank keeps the stored password
- [ ] Save reuses the existing `onSave` flow (`lib/main.dart:179-198`): write
      storage, reconnect MQTT, reconfigure + restart the DX spot service,
      return to the console
- [ ] First-launch behavior unchanged: no stored creds → splash → setup
- [ ] A save with a wrong password that fails to connect still lands on the
      console with its offline indicator — no crash, no setup-screen loop
      (the splash-expiry invariant at `lib/main.dart:207-219` must not regress)
- [ ] `tool/prebuild.sh` passes (flutter analyze + flutter test) and
      `flutter build apk --release` succeeds

## Constraints

- Theme tokens from `lib/ui/theme.dart`; touch targets ≥ 48dp
  (`hf_console/CLAUDE.md`)
- Never log or render the stored password
- Changes stay inside `hf_console/` — no `shared/` or other-component edits

## Notes

- Tablet deploy is manual: `adb install -r build/app/outputs/flutter-apk/app-release.apk`
  (`-r` keeps stored credentials — good for testing this very requirement)
- The iOS/web builds are not in scope; do not break them
  (`flutter build ios --release --no-codesign` smoke if cheap)

## Outcome

(not processed yet — this file lives in examples/ and is never picked up;
copy it into pending/ to run it)
