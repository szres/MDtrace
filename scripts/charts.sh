#!/bin/bash
# Export an event's snapshots (and weather, if any) and render the ggplot charts.
#   ./scripts/charts.sh md2026-zhuhai [output-dir]   (default: ./charts/<slug>)
set -euo pipefail

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
  sed -n '2,3p' "$0" >&2
  exit 2
fi
repo="$(cd "$(dirname "$0")/.." && pwd)"
source "$repo/scripts/lib.sh"
event="$(resolve_event "$1")"
out="${2:-$repo/charts/$(basename "$event")}"
work="$(mktemp -d -t mdtrace-charts)"
trap '/bin/rm -rf "$work"' EXIT

"$config_dir/bin/mdtrace" export -dir "$config_dir" -event "$event" -o "$work/export.csv" -weather "$work/weather.csv"
options=(--event "$event")
# The header line alone means no weather has been collected yet.
if [ "$(wc -l < "$work/weather.csv")" -gt 1 ]; then
  options+=(--weather "$work/weather.csv")
fi
Rscript "$repo/analysis/charts.R" "$work/export.csv" "$out" "${options[@]}"
