#!/bin/bash
# Render src/ui.h to tools/lcd-preview/out/sheet.png (every scenario in preview.cpp,
# 3x, with the three button columns marked). Needs SDL2 (brew install sdl2), python3
# with Pillow, and the M5GFX library that `pio run` fetches:
#   M5GFX_SRC=<…/libdeps/m5stamp-plc1/M5GFX/src> ./tools/lcd-preview/render.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
G=${M5GFX_SRC:-$here/../../.pio/libdeps/m5stamp-plc1/M5GFX/src}
[ -d "$G" ] || { echo "M5GFX not found at $G — run 'pio run' or set M5GFX_SRC" >&2; exit 1; }
build=${TMPDIR:-/tmp}/lcd-preview-build
out=$here/out
mkdir -p "$build" "$out"
INC=(-DLGFX_SDL -I"$G" -I/opt/homebrew/include/SDL2 -I/opt/homebrew/include)
cd "$build"
# The library is portable C/C++; compile it once and keep the objects.
for f in "$G"/lgfx/utility/*.c $(find "$G"/lgfx/Fonts -name '*.c'); do
  o=$(basename "$f").o; [ -f "$o" ] || clang -O1 -w -c "${INC[@]}" "$f" -o "$o"
done
for f in $(find "$G"/lgfx/Fonts -name '*.cpp'); do
  o=$(basename "$f").o; [ -f "$o" ] || clang++ -std=gnu++17 -O1 -w -c "${INC[@]}" "$f" -o "$o"
done
clang++ -std=gnu++17 -O1 -w "${INC[@]}" "$here/preview.cpp" \
  "$G"/lgfx/v1/{LGFXBase,lgfx_fonts,lgfx_v1,lgfx_v1_panel,lgfx_v1_platforms,lgfx_v1_touch}.cpp \
  ./*.o -L/opt/homebrew/lib -lSDL2 -o preview
./preview "$out"
python3 -I "$here/sheet.py" "$out"
echo "wrote $out/sheet.png"
