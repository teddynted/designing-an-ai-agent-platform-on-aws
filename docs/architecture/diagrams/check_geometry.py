#!/usr/bin/env python3
"""
Geometry lint for aws-architecture.svg.

The SVG is hand-authored with explicit coordinates, so the failure mode is not
invalid XML — it is a shape quietly sitting on top of another shape. Nothing
errors; the diagram just starts lying. These checks caught, in order:

  * flow arrows routed straight through component cards
  * ingress arrows striking the AZ-A subnet labels
  * an annotation struck through by the VPC container border

Run after any edit to the SVG:

    python3 docs/architecture/diagrams/check_geometry.py

Exits non-zero on any violation. This does not replace looking at the diagram
(`qlmanage -t -s 1440 -o /tmp <svg>`), it just catches what the eye skips.
"""

from __future__ import annotations

import pathlib
import re
import sys

SVG = pathlib.Path(__file__).with_name("aws-architecture.svg")

# Approximate advance width per character, as a fraction of font size. Rough,
# but the checks below only care whether a border falls *inside* a text run.
CHAR_W = 0.55
FONT_SIZE = {
    "t-title": 21, "t-sub": 12, "t-zone": 12, "t-name": 11,
    "t-desc": 9, "t-note": 9, "t-legend": 10,
}

Box = tuple[str, float, float, float, float]  # name, x1, y1, x2, y2


def containers(svg: str) -> list[Box]:
    out = []
    for m in re.finditer(
        r'<rect class="box ([\w-]+)" x="(\d+)" y="(\d+)" width="(\d+)" height="(\d+)"', svg
    ):
        name = m.group(1)
        x, y, w, h = (int(g) for g in m.groups()[1:])
        out.append((name, x, y, x + w, y + h))
    return out


def cards(svg: str) -> list[Box]:
    pat = r'<g transform="translate\((\d+),(\d+)\)">\s*<rect class="card" width="(\d+)" height="(\d+)"'
    return [
        ("card", int(x), int(y), int(x) + int(w), int(y) + int(h))
        for x, y, w, h in re.findall(pat, svg)
    ]


def texts(svg: str) -> list[tuple[str, float, float, float, float]]:
    """Top-level texts only (two-space indent); tile labels live inside <g> and move with it."""
    out = []
    for m in re.finditer(r'^  <text class="([\w-]+)"([^>]*)>([^<]+)</text>', svg, re.M):
        cls, attrs, body = m.groups()
        if cls not in FONT_SIZE:
            continue
        x = float(re.search(r'x="([\d.]+)"', attrs).group(1))
        y = float(re.search(r'y="([\d.]+)"', attrs).group(1))
        fs = FONT_SIZE[cls]
        w = CHAR_W * fs * len(body)
        if 'text-anchor="end"' in attrs:
            x1 = x - w
        elif 'text-anchor="middle"' in attrs:
            x1 = x - w / 2
        else:
            x1 = x
        out.append((body, x1, y - fs * 0.78, x1 + w, y + fs * 0.22))
    return out


def overlaps(a: Box, b: Box) -> bool:
    return max(a[1], b[1]) < min(a[3], b[3]) and max(a[2], b[2]) < min(a[4], b[4])


def contained(a: Box, b: Box) -> bool:
    return a[1] >= b[1] and a[3] <= b[3] and a[2] >= b[2] and a[4] <= b[4]


def main() -> int:
    svg = SVG.read_text()
    conts, crds, txts = containers(svg), cards(svg), texts(svg)
    fails: list[str] = []

    # A card must be wholly inside every container it touches.
    for c in crds:
        for k in conts:
            if overlaps(c, k) and not contained(c, k):
                fails.append(f"card at ({c[1]},{c[2]}) straddles the edge of {k[0]}")

    # Containers must nest, never partially overlap.
    for i, a in enumerate(conts):
        for b in conts[i + 1:]:
            if overlaps(a, b) and not (contained(a, b) or contained(b, a)):
                fails.append(f"{a[0]} partially overlaps {b[0]} (containers must nest)")

    # No container border may pass through a text run.
    for body, tx1, ty1, tx2, ty2 in txts:
        t = ("text", tx1, ty1, tx2, ty2)
        for k in conts:
            x_ov = max(tx1, k[1]) < min(tx2, k[3])
            y_ov = max(ty1, k[2]) < min(ty2, k[4])
            for by in (k[2], k[4]):
                if x_ov and ty1 < by < ty2:
                    fails.append(f"{k[0]} border (y={by}) strikes text {body[:36]!r}")
            for bx in (k[1], k[3]):
                if y_ov and tx1 < bx < tx2:
                    fails.append(f"{k[0]} border (x={bx}) strikes text {body[:36]!r}")

    for f in fails:
        print(f"  ✗ {f}")
    n = f"{len(crds)} cards, {len(conts)} containers, {len(txts)} labels"
    print(f"\n{'ALL CLEAR' if not fails else f'{len(fails)} VIOLATIONS'}  ({n})")
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
