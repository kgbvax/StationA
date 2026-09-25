#!/usr/bin/env python3
"""Build assets/geo/places.json — the place-name layer of the Mercator map.

Source: Natural Earth 10m populated places ("simple"), public domain:
  https://raw.githubusercontent.com/nvkelso/natural-earth-vector/master/geojson/ne_10m_populated_places_simple.geojson

Keeps Europe (the VHF map's working area) and only what the painter needs:
[name, lat, lng, scalerank] rows, scalerank 0 = most important. Cities only —
Natural Earth has no small towns, so a village is not on the map.

Usage: tool/make_places.py <ne_10m_populated_places_simple.geojson>
"""
import json
import sys

LAT = (34.0, 72.0)
LNG = (-25.0, 45.0)


def main(src: str) -> None:
    with open(src, encoding="utf-8") as f:
        features = json.load(f)["features"]
    rows = []
    for feat in features:
        p = feat["properties"]
        lat, lng = p["latitude"], p["longitude"]
        if not (LAT[0] <= lat <= LAT[1] and LNG[0] <= lng <= LNG[1]):
            continue
        rows.append([p["name"], round(lat, 4), round(lng, 4), int(p["scalerank"])])
    # Most important first: the painter draws in order and skips labels that
    # would overlap an earlier one, so capitals win over suburbs.
    rows.sort(key=lambda r: (r[3], r[0]))
    with open("assets/geo/places.json", "w", encoding="utf-8") as f:
        json.dump(rows, f, ensure_ascii=False, separators=(",", ":"))
    print(f"{len(rows)} places written to assets/geo/places.json")


if __name__ == "__main__":
    main(sys.argv[1])
