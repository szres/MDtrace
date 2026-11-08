#!/bin/bash
# Export the collected data as a publishable dataset (CSV, SQLite, mission list and map geometry).
#   ./scripts/export-dataset.sh OUT_DIR [DB]
# OUT_DIR gets data/ and geo/. Run errors that mention local paths are replaced by a neutral
# description, and the bot's internal tables (alerts, last_update) are left out.
set -euo pipefail

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
  sed -n '2,4p' "$0" >&2
  exit 2
fi
out="$1"
db="${2:-$HOME/.config/mdtrace/mdtrace.db}"
repo="$(cd "$(dirname "$0")/.." && pwd)"
[ -f "$db" ] || { echo "no database at $db" >&2; exit 1; }

mkdir -p "$out/data" "$out/geo"
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

export_csv() {
  sqlite3 -csv -header "$work/mdtrace.sqlite" "$2" > "$out/data/$1.csv"
}
export_csv runs "SELECT id, started_at, finished_at, missions_total, missions_ok, error FROM runs ORDER BY id"
export_csv snapshots "SELECT run_id, mission_order, mission_guid, title, author, faction, completed_count,
  rating_e6, average_duration_ms, fetched_at FROM snapshots ORDER BY run_id, mission_order"
export_csv failures "SELECT run_id, mission_guid, error, failed_at FROM failures ORDER BY run_id, mission_guid"
export_csv weather_current "SELECT run_id, as_of, condition, temperature, apparent_temperature, humidity,
  precipitation_intensity, wind_speed FROM weather_current ORDER BY run_id"
export_csv weather_hours "SELECT hour_start, condition, temperature, precipitation_chance, precipitation_amount,
  fetched_at FROM weather_hours ORDER BY hour_start"

cp "$work/mdtrace.sqlite" "$out/data/mdtrace.sqlite"
cp "$repo/missions.json" "$out/data/missions.json"
cp "$repo/geo/routes.json" "$repo/geo/coastline.json" "$out/geo/"

if grep -rla -e "$HOME" -e '/Users/' "$out/data" "$out/geo"; then
  echo "export still contains local paths in the files above" >&2
  exit 1
fi
for table in runs snapshots failures weather_current weather_hours; do
  printf '%-16s %6d rows\n' "$table" "$(($(wc -l < "$out/data/$table.csv") - 1))"
done
