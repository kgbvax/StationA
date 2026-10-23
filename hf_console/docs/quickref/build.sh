#!/usr/bin/env bash
# build.sh — renders the hf_console quick reference.
#
#   docs/quickref/build.sh
#
# 1. QUICKREF_RENDER=1 flutter test test/tool/quickref_render_test.dart
#    → docs/quickref/hf-console-quickref.html (self-contained, images inlined)
#      plus PNGs + call-out JSON in $QUICKREF_OUT (default /tmp/quickref).
# 2. Headless Chrome prints it → docs/quickref/hf-console-quickref.pdf
#    (A4 landscape, one sheet per page). Set CHROME to override the binary.
#
# Call-outs live in test/tool/quickref_sheets.dart, layout in template.html.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
app="$(cd "$here/../.." && pwd)"
cd "$app"

QUICKREF_RENDER=1 flutter test test/tool/quickref_render_test.dart

chrome="${CHROME:-}"
if [[ -z "$chrome" ]]; then
  for c in "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
           "/Applications/Chromium.app/Contents/MacOS/Chromium" \
           "$(command -v google-chrome || true)" "$(command -v chromium || true)"; do
    if [[ -n "$c" && -x "$c" ]]; then chrome="$c"; break; fi
  done
fi
if [[ -z "$chrome" ]]; then
  echo "build.sh: no Chrome/Chromium found (set CHROME); HTML written, PDF skipped" >&2
  exit 0
fi

"$chrome" --headless=new --disable-gpu --no-pdf-header-footer \
  --print-to-pdf="$here/hf-console-quickref.pdf" \
  "file://$here/hf-console-quickref.html" 2>/dev/null
echo "wrote $here/hf-console-quickref.html and hf-console-quickref.pdf"
