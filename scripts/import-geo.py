#!/usr/bin/env python3
"""Build an event's geographic inputs for the map charts.

    python3 -I scripts/import-geo.py events/md2026-zhuhai /path/to/site/public/missions.json

The second file is the mission site's list with each route's portals; it is joined to the
event's missions.json on sourceRouteId. Writes, inside the event directory:
  geo/routes.json     route polylines (WGS-84) keyed by mission GUID
  geo/coastline.json  OpenStreetMap natural=coastline ways around the routes (via Overpass)

Coastline data (c) OpenStreetMap contributors, ODbL 1.0.
"""

import json
import math
import sys
import urllib.parse
import urllib.request
from pathlib import Path

OVERPASS_URL = "https://overpass-api.de/api/interpreter"
PAD_DEGREES = 0.04
# Simplification tolerance (~25 m) keeps the coastline small while staying accurate at map scale.
SIMPLIFY_DEGREES = 0.00025


def load_routes(event_dir: Path, site_missions_path: Path) -> list[dict]:
    site = json.loads(site_missions_path.read_text(encoding="utf-8"))
    tracked = json.loads((event_dir / "missions.json").read_text(encoding="utf-8"))
    guid_by_route = {m["sourceRouteId"]: m for m in tracked["missions"]}

    routes = []
    for mission in site["missions"]:
        route_id = mission["sourceRouteId"]
        match = guid_by_route.get(route_id)
        if match is None:
            raise SystemExit(f"route {route_id} ({mission['displayName']}) is not in missions.json")
        points = [[p["lat"], p["lng"]] for p in mission["portals"]]
        if len(points) < 2:
            raise SystemExit(f"route {route_id} has fewer than 2 portals")
        routes.append({
            "order": match["order"],
            "missionGuid": match["missionGuid"],
            "displayName": match["displayName"],
            "points": points,
        })
    if len(routes) != len(guid_by_route):
        raise SystemExit(f"site has {len(routes)} routes, missions.json has {len(guid_by_route)}")
    return sorted(routes, key=lambda r: r["order"])


def bounds(routes: list[dict]) -> tuple[float, float, float, float]:
    lats = [p[0] for r in routes for p in r["points"]]
    lngs = [p[1] for r in routes for p in r["points"]]
    return (min(lats) - PAD_DEGREES, min(lngs) - PAD_DEGREES, max(lats) + PAD_DEGREES, max(lngs) + PAD_DEGREES)


def fetch_coastline(south: float, west: float, north: float, east: float) -> list[list[list[float]]]:
    query = f'[out:json][timeout:90];way["natural"="coastline"]({south},{west},{north},{east});out geom;'
    request = urllib.request.Request(
        OVERPASS_URL,
        data=urllib.parse.urlencode({"data": query}).encode(),
        headers={"User-Agent": "mdtrace-map-import/1.0"},
    )
    with urllib.request.urlopen(request, timeout=120) as response:
        payload = json.load(response)
    ways = []
    for element in payload.get("elements", []):
        geometry = element.get("geometry") or []
        line = [[round(g["lat"], 5), round(g["lon"], 5)] for g in geometry]
        line = simplify(line, SIMPLIFY_DEGREES)
        if len(line) >= 2:
            ways.append(line)
    if not ways:
        raise SystemExit("Overpass returned no coastline in the route area")
    return ways


def simplify(points: list[list[float]], tolerance: float) -> list[list[float]]:
    """Douglas-Peucker line simplification (iterative)."""
    if len(points) < 3:
        return points
    keep = [False] * len(points)
    keep[0] = keep[-1] = True
    stack = [(0, len(points) - 1)]
    while stack:
        start, end = stack.pop()
        (y1, x1), (y2, x2) = points[start], points[end]
        length = math.hypot(x2 - x1, y2 - y1)
        best, best_index = 0.0, -1
        for i in range(start + 1, end):
            y, x = points[i]
            if length == 0:
                distance = math.hypot(x - x1, y - y1)
            else:
                distance = abs((x2 - x1) * (y1 - y) - (x1 - x) * (y2 - y1)) / length
            if distance > best:
                best, best_index = distance, i
        if best > tolerance:
            keep[best_index] = True
            stack.extend([(start, best_index), (best_index, end)])
    return [p for p, k in zip(points, keep) if k]


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit(__doc__)
    event_dir = Path(sys.argv[1])
    if not (event_dir / "event.json").is_file():
        raise SystemExit(f"{event_dir} is not an event directory (no event.json)")
    routes = load_routes(event_dir, Path(sys.argv[2]))
    out = event_dir / "geo"
    out.mkdir(exist_ok=True)
    (out / "routes.json").write_text(json.dumps({"routes": routes}, ensure_ascii=False, indent=1), encoding="utf-8")

    coastline = fetch_coastline(*bounds(routes))
    (out / "coastline.json").write_text(
        json.dumps({"attribution": "© OpenStreetMap contributors (ODbL 1.0)", "ways": coastline}),
        encoding="utf-8",
    )
    points = sum(len(w) for w in coastline)
    print(f"wrote {len(routes)} routes and {len(coastline)} coastline ways ({points} points) to {out}")


if __name__ == "__main__":
    main()
