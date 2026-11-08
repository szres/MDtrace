#!/bin/bash
# Build mdtrace, install it to ~/.config/mdtrace and (re)load the launchd agent.
# The runtime lives outside ~/Documents because macOS privacy rules block launchd jobs from it.
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
dir="$HOME/.config/mdtrace"
label="io.github.szres.mdtrace"
plist="$HOME/Library/LaunchAgents/$label.plist"

cd "$repo"
go test ./...
mkdir -p "$dir/bin"
go build -o "$dir/bin/mdtrace" ./cmd/mdtrace


cp missions.json "$dir/missions.json"
cp analysis/charts.R "$dir/charts.R"
mkdir -p "$dir/geo"
cp geo/routes.json geo/coastline.json "$dir/geo/"

sed "s|@DIR@|$dir|g" deploy/$label.plist.in > "$plist"
plutil -lint "$plist" >/dev/null

launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$plist"

echo "installed: $dir  (agent $label runs every 5 min)"
echo "log:       $dir/collect.log"
if [[ ! -s "$dir/intel.cookie" ]]; then
  echo "next:      put the Cookie header of a logged-in intel.ingress.com tab in $dir/intel.cookie (chmod 600)" >&2
fi
