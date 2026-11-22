#!/usr/bin/env bash
# build.sh — renders the hf_console quick reference.
#
#   docs/quickref/build.sh
#
# 0. ffmpeg grabs one current frame from the antenna cam's live HLS preview
#    ($VHFCAM_BASE_URL/hls/live.m3u8, default http://scmino:8083, LAN,
#    no auth) for the CAM sheet. If the grab fails, the sheet shows the
#    CAMERA STREAM STOPPED state and the build goes on.
# 1. QUICKREF_RENDER=1 flutter test test/tool/quickref_render_test.dart
#    → docs/quickref/hf-console-quickref.html (self-contained, images inlined)
#      plus PNGs + call-out JSON in $QUICKREF_OUT (default /tmp/quickref).
# 2. Headless Chrome prints it → docs/quickref/hf-console-quickref.pdf
#    (A4 landscape, one sheet per page). Set CHROME to override the binary.
# 3. Copies both into the station portal (stationportal/internal/web/assets/
#    quickref.{html,pdf}, served at http://scmino/quickref and /quickref.pdf);
#    redeploy the portal (cd stationportal && ./deploy.sh) to publish them.
#
# Call-outs live in test/tool/quickref_sheets.dart, layout in template.html.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
app="$(cd "$here/../.." && pwd)"
cd "$app"

out="${QUICKREF_OUT:-/tmp/quickref}"
mkdir -p "$out"
cam="${VHFCAM_BASE_URL:-http://scmino:8083}"
frame="$out/cam-frame.jpg"
rm -f "$frame"
if command -v ffmpeg >/dev/null 2>&1 &&
   ffmpeg -nostdin -loglevel error -y -rw_timeout 10000000 -i "$cam/hls/live.m3u8" \
     -frames:v 1 -vf scale=1280:-2 -q:v 3 "$frame" && [[ -s "$frame" ]]; then
  echo "build.sh: CAM sheet uses a frame from $cam"
  export QUICKREF_CAM_FRAME="$frame"
else
  echo "build.sh: no cam frame from $cam (ffmpeg missing or stream down); CAM sheet shows the stopped state" >&2
fi

QUICKREF_RENDER=1 flutter test test/tool/quickref_render_test.dart
cp "$here/hf-console-quickref.html" "$app/../stationportal/internal/web/assets/quickref.html"

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
cp "$here/hf-console-quickref.pdf" "$app/../stationportal/internal/web/assets/quickref.pdf"
echo "wrote $here/hf-console-quickref.html and hf-console-quickref.pdf"
