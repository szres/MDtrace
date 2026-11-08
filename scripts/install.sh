#!/bin/bash
# Build mdtrace, install an event and (re)load the launchd agent that collects it.
#   ./scripts/install.sh events/md2026-zhuhai
# Shared settings (telegram.env, weatherkit.env, intel.cookie) stay in ~/.config/mdtrace; the
# event's files, database, log and final charts go in ~/.config/mdtrace/events/<slug>. One event
# is collected at a time: installing another event moves the agent to it and keeps the old data.
# The runtime lives outside ~/Documents because macOS privacy rules block launchd jobs from it.
set -euo pipefail

if [ $# -ne 1 ]; then
  sed -n '2,7p' "$0" >&2
  exit 2
fi
repo="$(cd "$(dirname "$0")/.." && pwd)"
source "$repo/scripts/lib.sh"
src="$(resolve_event "$1")"
slug="$(event_field "$src" .slug)"
event="$config_dir/events/$slug"
label="io.github.szres.mdtrace"
plist="$HOME/Library/LaunchAgents/$label.plist"

cd "$repo"
go test ./...
mkdir -p "$config_dir/bin"
go build -o "$config_dir/bin/mdtrace" ./cmd/mdtrace
# Validate before anything is copied, so a broken event never replaces a working one.
"$config_dir/bin/mdtrace" check -dir "$config_dir" -event "$src"

if [ "$src" != "$event" ]; then
  mkdir -p "$event"
  cp "$src/event.json" "$src/missions.json" "$event/"
  /bin/rm -rf "$event/geo"
  if [ -d "$src/geo" ]; then
    mkdir -p "$event/geo"
    cp "$src/geo/routes.json" "$src/geo/coastline.json" "$event/geo/"
  fi
fi
cp analysis/charts.R "$config_dir/charts.R"

previous="$(plutil -extract ProgramArguments.5 raw "$plist" 2>/dev/null || true)"
if [ -n "$previous" ] && [ "$previous" != "$event" ]; then
  echo "switching collection from $previous to $event (its data is kept)"
fi
sed -e "s|@DIR@|$config_dir|g" -e "s|@EVENT@|$event|g" deploy/$label.plist.in > "$plist"
plutil -lint "$plist" >/dev/null

launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$plist"

echo "installed: $event  (agent $label runs every 5 min)"
echo "log:       $event/collect.log"
if [[ ! -s "$config_dir/intel.cookie" ]]; then
  echo "next:      put the Cookie header of a logged-in intel.ingress.com tab in $config_dir/intel.cookie (chmod 600)" >&2
fi
