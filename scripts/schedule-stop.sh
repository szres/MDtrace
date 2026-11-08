#!/bin/bash
# Schedule the end of collection, in the event's own time zone.
#   ./scripts/schedule-stop.sh md2026-zhuhai "2026-11-08 13:02"
# At that time the collector agent is unloaded (data kept), the final summary is sent, and the
# one-off stop agent removes itself.
# Cancel with: launchctl bootout gui/$(id -u)/io.github.szres.mdtrace.stop; rm ~/Library/LaunchAgents/io.github.szres.mdtrace.stop.plist
set -euo pipefail

if [ $# -ne 2 ]; then
  sed -n '2,6p' "$0" >&2
  exit 2
fi
repo="$(cd "$(dirname "$0")/.." && pwd)"
source "$repo/scripts/lib.sh"
event="$(resolve_event "$1")"
when="$2"
zone="$(event_field "$event" .timezone)"
label="io.github.szres.mdtrace.stop"
plist="$HOME/Library/LaunchAgents/$label.plist"

epoch="$(TZ="$zone" date -j -f "%Y-%m-%d %H:%M:%S" "$when:00" +%s)" || { echo "invalid time: $when" >&2; exit 2; }
if [ "$epoch" -le "$(date +%s)" ]; then
  echo "$when $zone is in the past" >&2
  exit 2
fi
# launchd calendar intervals use the Mac's local time zone.
read -r month day hour minute <<<"$(date -r "$epoch" '+%-m %-d %-H %-M')"

install -m 755 "$repo/scripts/stop.sh" "$config_dir/stop.sh"
mkdir -p "$(dirname "$plist")"
sed -e "s|@DIR@|$config_dir|g" -e "s|@EVENT@|$event|g" -e "s|@MONTH@|$month|" -e "s|@DAY@|$day|" \
  -e "s|@HOUR@|$hour|" -e "s|@MINUTE@|$minute|" "$repo/deploy/$label.plist.in" > "$plist"
plutil -lint "$plist" >/dev/null
launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$plist"
echo "collection of $(basename "$event") ends $(TZ="$zone" date -r "$epoch" '+%a %F %H:%M %Z') = $(date -r "$epoch" '+%a %F %H:%M %Z') local"
