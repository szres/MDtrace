#!/bin/bash
# Schedule the end of collection.
#   ./scripts/schedule-stop.sh "2026-11-08 13:02" [Asia/Shanghai]
# At that time the collector agent is unloaded (data kept) and the one-off stop agent removes itself.
# Cancel with: launchctl bootout gui/$(id -u)/io.github.szres.mdtrace.stop; rm ~/Library/LaunchAgents/io.github.szres.mdtrace.stop.plist
set -euo pipefail

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
  sed -n '2,5p' "$0" >&2
  exit 2
fi
when="$1"
zone="${2:-Asia/Shanghai}"
repo="$(cd "$(dirname "$0")/.." && pwd)"
dir="$HOME/.config/mdtrace"
label="io.github.szres.mdtrace.stop"
plist="$HOME/Library/LaunchAgents/$label.plist"

epoch="$(TZ="$zone" date -j -f "%Y-%m-%d %H:%M:%S" "$when:00" +%s)" || { echo "invalid time: $when" >&2; exit 2; }
if [ "$epoch" -le "$(date +%s)" ]; then
  echo "$when $zone is in the past" >&2
  exit 2
fi
# launchd calendar intervals use the Mac's local time zone.
read -r month day hour minute <<<"$(date -r "$epoch" '+%-m %-d %-H %-M')"

install -m 755 "$repo/scripts/stop.sh" "$dir/stop.sh"
mkdir -p "$(dirname "$plist")"
sed -e "s|@DIR@|$dir|g" -e "s|@MONTH@|$month|" -e "s|@DAY@|$day|" -e "s|@HOUR@|$hour|" -e "s|@MINUTE@|$minute|" \
  "$repo/deploy/$label.plist.in" > "$plist"
plutil -lint "$plist" >/dev/null
launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$plist"
echo "collection ends $(TZ="$zone" date -r "$epoch" '+%a %F %H:%M %Z') = $(date -r "$epoch" '+%a %F %H:%M %Z') local"
