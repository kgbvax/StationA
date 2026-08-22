#!/usr/bin/env bash
set -euo pipefail

# Pre-sideload quality gate for hf_console.
# Run this before building a release APK.

cd "$(dirname "$0")/.."

echo "==> flutter analyze"
flutter analyze

echo "==> flutter test"
flutter test

echo "==> all checks passed"
