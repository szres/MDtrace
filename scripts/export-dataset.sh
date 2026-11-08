#!/bin/bash
# Publish an event's data into the dataset repository (szres/MDtrace-Data).
#   ./scripts/export-dataset.sh md2026-zhuhai ../MDtrace-Data
# Writes DATA_REPO/events/<slug>/ (event.json, data/ with CSV, SQLite and the mission list, and
# geo/ when the event has map geometry) and adds or updates the event in DATA_REPO/events.json.
# Run errors that mention local paths are replaced by a neutral description, and the bot's
# internal tables (alerts, last_update) are left out.
set -euo pipefail

if [ $# -ne 2 ]; then
  sed -n '2,7p' "$0" >&2
  exit 2
fi
repo="$(cd "$(dirname "$0")/.." && pwd)"
source "$repo/scripts/lib.sh"
event="$(resolve_event "$1")"
dataset="$(cd "$2" && pwd)"
slug="$(event_field "$event" .slug)"
db="$event/mdtrace.db"
[ -f "$db" ] || { echo "no database at $db" >&2; exit 1; }
"$config_dir/bin/mdtrace" check -dir "$config_dir" -event "$event" > /dev/null

out="$dataset/events/$slug"
work="$(mktemp -d)"
trap '/bin/rm -rf "$work"' EXIT

# Work on a consistent copy so a running collector cannot change the data mid-export.
sqlite3 "$db" ".backup '$work/mdtrace.sqlite'"
sqlite3 "$work/mdtrace.sqlite" > /dev/null <<'SQL'
DROP TABLE IF EXISTS alerts;
DROP TABLE IF EXISTS last_update;
UPDATE runs SET error = 'intel session not available'
  WHERE error LIKE '%not authenticated%' OR error LIKE '%/Users/%' OR error LIKE '%/home/%';
UPDATE failures SET error = 'mission request failed'
  WHERE error LIKE '%/Users/%' OR error LIKE '%/home/%';
PRAGMA journal_mode = DELETE;
VACUUM;
SQL

# Build the event's folder in the work directory and swap it in at the end, so a failed export
# never leaves a half-written event behind.
stage="$work/event"
mkdir -p "$stage/data"
export_csv() {
  sqlite3 -csv -header "$work/mdtrace.sqlite" "$2" > "$stage/data/$1.csv"
}
export_csv runs "SELECT id, started_at, finished_at, missions_total, missions_ok, error FROM runs ORDER BY id"
export_csv snapshots "SELECT run_id, mission_order, mission_guid, title, author, faction, completed_count,
  rating_e6, average_duration_ms, fetched_at FROM snapshots ORDER BY run_id, mission_order"
export_csv failures "SELECT run_id, mission_guid, error, failed_at FROM failures ORDER BY run_id, mission_guid"
export_csv weather_current "SELECT run_id, as_of, condition, temperature, apparent_temperature, humidity,
  precipitation_intensity, wind_speed FROM weather_current ORDER BY run_id"
export_csv weather_hours "SELECT hour_start, condition, temperature, precipitation_chance, precipitation_amount,
  fetched_at FROM weather_hours ORDER BY hour_start"

cp "$work/mdtrace.sqlite" "$stage/data/mdtrace.sqlite"
cp "$event/missions.json" "$stage/data/missions.json"
cp "$event/event.json" "$stage/event.json"
if [ -f "$event/geo/routes.json" ]; then
  mkdir -p "$stage/geo"
  cp "$event/geo/routes.json" "$event/geo/coastline.json" "$stage/geo/"
fi

if grep -rla -e "$HOME" -e '/Users/' "$stage"; then
  echo "export still contains local paths in the files above" >&2
  exit 1
fi

# The index lists every published event, newest first; the dashboard opens the first one. `geo`
# tells it whether the event has a map.
read -r start end <<<"$(sqlite3 -separator ' ' "$work/mdtrace.sqlite" \
  "SELECT min(started_at), max(started_at) FROM runs WHERE id IN (SELECT run_id FROM snapshots)")"
[ -n "$end" ] || { echo "$db has no snapshots to publish" >&2; exit 1; }
index="$dataset/events.json"
[ -f "$index" ] || echo '{"events": []}' > "$index"
has_geo=false
[ -d "$stage/geo" ] && has_geo=true
jq --slurpfile ev "$event/event.json" --slurpfile missions "$event/missions.json" \
  --arg start "$start" --arg end "$end" --argjson geo "$has_geo" '
  (($ev[0] | {slug, name, timezone})
    + {start: $start, end: $end, missions: ($missions[0].missions | length), geo: $geo}) as $entry
  | .events = ([.events[] | select(.slug != $entry.slug)] + [$entry] | sort_by(.start) | reverse)
' "$index" > "$work/events.json"

mkdir -p "$dataset/events"
/bin/rm -rf "$out"
mv "$stage" "$out"
mv "$work/events.json" "$index"

echo "published $slug to $out"
for table in runs snapshots failures weather_current weather_hours; do
  printf '%-16s %6d rows\n' "$table" "$(($(wc -l < "$out/data/$table.csv") - 1))"
done
