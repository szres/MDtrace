#!/bin/bash
# Export the collected snapshots (and weather, if any) and render the ggplot charts.
#   ./scripts/charts.sh [output-dir]   (default: ./charts)
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
out="${1:-$repo/charts}"
work="$(mktemp -d -t mdtrace-charts)"
trap '/bin/rm -rf "$work"' EXIT

"$HOME/.config/mdtrace/bin/mdtrace" export -tz Asia/Shanghai -o "$work/export.csv" -weather "$work/weather.csv"
options=(--geo "$repo/geo")
# The header line alone means no weather has been collected yet.
if [ "$(wc -l < "$work/weather.csv")" -gt 1 ]; then
  options+=(--weather "$work/weather.csv")
fi
Rscript "$repo/analysis/charts.R" "$work/export.csv" "$out" "${options[@]}"
