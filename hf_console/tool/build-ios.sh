#!/usr/bin/env bash
set -euo pipefail

# Build a self-signed sideload IPA for hf_console on iPhone.
#
# Prerequisites (one-time, interactive — not scriptable):
#   1. Open ios/Runner.xcworkspace in Xcode.
#   2. Runner target → Signing & Capabilities → "Automatically manage signing"
#      → pick your Apple ID as Team. (Free Apple ID = 7-day expiry; paid = 1 yr.)
#   Do NOT commit your account-specific DEVELOPMENT_TEAM to a shared repo file.
#
# Produces: build/ios/ipa/hf_console.ipa

cd "$(dirname "$0")/.."

echo "==> quality gate (analyze + test)"
./tool/prebuild.sh

echo "==> regenerate launcher icons (idempotent)"
dart run flutter_launcher_icons || true

echo "==> flutter build ipa --release (development export for sideloading)"
# --export-method=development: signs with the Xcode-managed development
# profile so the IPA installs on your own device via sideload. The default
# (app-store) needs an "iOS Distribution" cert a free Apple ID lacks.
flutter build ipa --release --export-method=development

IPA="build/ios/ipa/hf_console.ipa"
if [[ ! -f "$IPA" ]]; then
  echo "✕ IPA export failed — $IPA not produced." >&2
  echo "  Common cause: no valid development provisioning profile. Re-check" >&2
  echo "  Xcode → Runner → Signing & Capabilities (Team + 'Automatically manage signing')." >&2
  exit 1
fi

echo "==> IPA ready: $IPA ($(du -h "$IPA" | cut -f1))"
echo "    Install via Xcode → Devices and Simulators, or:"
echo "    ios-deploy --bundle $IPA"