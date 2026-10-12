"""Stitch the per-scenario PPMs into out/sheet.png at 3x, with the button columns marked."""
import sys
from pathlib import Path
from PIL import Image, ImageDraw

OUT = Path(sys.argv[1])
SCALE = 3
CENTRES = (40, 120, 200)  # keep in step with ui::BTN_CX
ORDER = ["cold", "powered", "wait", "held", "fault", "armed", "nolink", "wifionly"]

shots = [(n, Image.open(OUT / f"{n}.ppm")) for n in ORDER if (OUT / f"{n}.ppm").exists()]
w, h = shots[0][1].size
cols = 2
rows = (len(shots) + cols - 1) // cols
pad, label = 16, 18
sheet = Image.new("RGB", (cols * (w * SCALE + pad) + pad, rows * (h * SCALE + label + pad) + pad), (38, 38, 42))
d = ImageDraw.Draw(sheet)
for i, (name, im) in enumerate(shots):
    x = pad + (i % cols) * (w * SCALE + pad)
    y = pad + (i // cols) * (h * SCALE + label + pad)
    d.text((x, y), name, fill=(200, 200, 205))
    big = im.resize((w * SCALE, h * SCALE), Image.NEAREST)
    sheet.paste(big, (x, y + label))
    for letter, cx in zip("ABC", CENTRES):
        px = x + cx * SCALE
        d.polygon([(px - 6, y + label + h * SCALE + 2), (px + 6, y + label + h * SCALE + 2), (px, y + label + h * SCALE + 10)], fill=(200, 200, 205))
sheet.save(OUT / "sheet.png")
