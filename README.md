# MDTrace

Records the completion count ("agents completed") and rating of Ingress Mission Day missions from
Ingress Intel every 5 minutes. It posts live updates with charts to Telegram, and when the event
ends it sends a bilingual (English / 中文) final summary with a Mission Day replay. Each Mission Day
is an **event** with its own settings, mission list and database, so the same install can follow
one MD after another.

- **Data and dashboard**: [szres/MDtrace-Data](https://github.com/szres/MDtrace-Data) holds the raw
  data of every published event and a static dashboard at <https://szres.github.io/MDtrace-Data/>.
  The first event is MD 2026 Zhuhai (6–8 Nov 2026).

> [!WARNING]
> MDTrace reads Ingress Intel's **unofficial, undocumented** web API with your own logged-in
> session. Automated access to Intel is against the
> [Niantic Terms of Service](https://nianticlabs.com/terms); using this tool can get your
> account restricted. It is not affiliated with or endorsed by Niantic. Use it at your own risk,
> keep the request rate low (the default is one request every 2.5 s, one run every 5 min), and
> never share your cookie.

## Layout

| Path | Purpose |
| --- | --- |
| `events/<slug>/` | One directory per Mission Day: `event.json`, `missions.json`, optional `geo/` |
| `cmd/mdtrace` | CLI: `collect`, `check`, `export`, `telegram-test`, `weather-test`, `final` |
| `internal/event` | Loads and validates `event.json` |
| `internal/intel` | Intel API client and `getMissionDetails` parser |
| `internal/store` | SQLite storage (`runs`, `snapshots`, `failures`, `weather_current`, `weather_hours`) |
| `internal/telegram` | Bot API client (rich messages, slideshows, plain-text fallback) |
| `internal/weather` | Apple WeatherKit REST client (ES256 developer token) |
| `analysis/charts.R` | ggplot2 charts, heat maps and animations used in updates and the final summary |
| `deploy/`, `scripts/` | launchd agent templates; install, scheduled stop, charts, dataset export |

The runtime lives in `~/.config/mdtrace`, outside `~/Documents`, where macOS privacy rules block
launchd jobs:

```text
~/.config/mdtrace/
  bin/mdtrace  charts.R  stop.sh            installed by the scripts
  intel.cookie  telegram.env  weatherkit.env  weatherkit.p8   shared by all events (mode 600)
  events/<slug>/
    event.json  missions.json  geo/         copied from the repository
    mdtrace.db  collect.log  final/         written while the event runs
```

Every `mdtrace` command takes `-event NAME` (an event under `~/.config/mdtrace/events`) or
`-event PATH`. `-dir` changes the shared settings directory.

## Set up a Mission Day

1. **Create the event directory** `events/<slug>/` with an `event.json`:

   ```json
   {
     "slug": "md2026-zhuhai",
     "name": { "en": "MD 2026 Zhuhai", "zh": "MD 2026 珠海" },
     "timezone": "Asia/Shanghai",
     "weather": {
       "lat": 22.2738, "lng": 113.579,
       "area": { "en": "the 情侣路 cluster", "zh": "情侣路一带" }
     },
     "mapFocus": {
       "label": { "en": "情侣路 cluster", "zh": "情侣路片区" },
       "orders": [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11]
     }
   }
   ```

   | Field | Meaning |
   | --- | --- |
   | `slug` | Lower-case letters, digits and hyphens. Names the runtime directory and the dataset folder |
   | `name` | Title of updates, charts and the dashboard, in English and Chinese |
   | `timezone` | IANA zone of the event. All message times, chart axes and exports use it |
   | `weather` | Optional. The point WeatherKit is asked about, and how messages name the area. Without it, weather is off |
   | `mapFocus` | Optional. A dense group of missions (by order) that the map charts and dashboard show in a zoomed panel |

   Unknown fields are rejected, so a typo cannot silently drop a setting.

2. **Add `missions.json`**: the missions to track, in the `mission-links` format.
   `missionGuid` is the Intel GUID; `order`, `displayName` and `title` label the charts.

   ```json
   { "kind": "mission-links", "missions": [
     { "order": 1, "displayName": "海滨泳场", "title": "MD 2026: Zhuhai, 海滨泳场",
       "missionGuid": "dbd8b6bf31fe47bcb2e90c76c89463ad.1c",
       "missionUrl": "https://link.ingress.com/mission/dbd8b6bf31fe47bcb2e90c76c89463ad.1c",
       "sourceRouteId": "16cdbb21-…" } ] }
   ```

3. **Optionally add the map geometry** (`geo/routes.json`, `geo/coastline.json`). Without it the
   charts leave out the maps. The script needs network access for the coastline:

   ```sh
   python3 -I scripts/import-geo.py events/<slug> path/to/site/public/missions.json
   ```

   The site file lists each route's portals and is joined on `sourceRouteId`. The coastline is
   OpenStreetMap data, © OpenStreetMap contributors, ODbL 1.0.

4. **Check the event**: `go run ./cmd/mdtrace check -event ./events/<slug>` validates everything and
   prints a summary (missions, weather point, routes, zoom panel).

5. **Install and start collecting** (needs Go 1.26+, and R for charts):

   ```sh
   ./scripts/install.sh events/<slug>                     # test, build, copy the event, load the agent
   ./scripts/schedule-stop.sh <slug> "2027-05-02 18:00"   # end time, in the event's time zone
   ```

One event is collected at a time. Installing another event moves the agent to it; the earlier
event's database and charts stay in its own directory. `./scripts/uninstall.sh` stops the agent
and keeps all data.

### Intel session cookie

Log in to <https://intel.ingress.com> in a browser, open the developer tools, select any request
to `intel.ingress.com` in the Network tab, and copy the value of its `Cookie` request header.
It must contain a session cookie (`SACSID` or `sessionid`) and `csrftoken`.

```sh
pbpaste > ~/.config/mdtrace/intel.cookie && chmod 600 ~/.config/mdtrace/intel.cookie
```

The file is read on every run, so replacing it renews the session; `-cookies FILE` points to
another file. When the session expires, the run is recorded as failed, a macOS notification is
shown, and (with Telegram) an alert asks for a fresh cookie.

Intel sometimes serves all-zero statistics for a mission for about an hour (seen for #03 on
2026-11-07). Unique completions never return to zero, so a 0 reading for a mission that already
has completions is skipped: it is logged as `skip`, recorded in `failures`, and the mission keeps
its last count in updates. It does not count as a failed run.

### Scheduled stop and final summary

`./scripts/schedule-stop.sh <slug> "YYYY-MM-DD HH:MM"` installs a one-off agent
`io.github.szres.mdtrace.stop`, which runs `stop.sh` at that time (in the event's time zone).
`stop.sh` does the following:

1. Unloads the collector.
2. Re-enables a channel line paused as `# Paused <date>: TELEGRAM_CHANNEL_ID=…`, so the channel gets
   the final summary but no more regular updates.
3. Sends the final summary and keeps all data.
4. Removes itself.

`./scripts/install.sh events/<slug>` starts collection again.

The final summary (`mdtrace final -event <slug> -charts charts.R`) goes to the private chat, and
to the channel if one is configured. It is two messages, English then Chinese. Each has its own
chart set, with all chart text in that language (`charts.R --lang zh`), and contains:

- totals and the event span
- highlights: most/fewest completions, highest/lowest rating, quickest/longest runs, busiest hour,
  halfway point, weather
- every mission's completions, rating, and rating change since its first rating

Its slideshow starts with `event_anim.mp4`, a replay of the whole Mission Day: one frame per
10 min, with the heat trail of the past hour, completions and rating per mission, an hourly
timeline, and temperature and rain. Whole-event charts follow.

Copies are saved in `events/<slug>/final/`, with the Chinese set in `final/zh/`. `-preview` sends
the summary to the private chat only, marked as a preview.

## Telegram

If `~/.config/mdtrace/telegram.env` exists (mode 600) with `TELEGRAM_BOT_TOKEN` and
`TELEGRAM_CHAT_ID`, the collector sends the following. An optional `TELEGRAM_CHANNEL_ID` (Bot API
form, e.g. `-1001234567890`) also receives updates; comment that line out to pause channel posts.
Alerts go only to the private chat. The bot must be a channel admin with permission to post.

```sh
# ~/.config/mdtrace/telegram.env (mode 600)
TELEGRAM_BOT_TOKEN=123456789:AA...
TELEGRAM_CHAT_ID=123456789
TELEGRAM_CHANNEL_ID=-1001234567890
```

- **Updates**: at most one every 30 minutes, and only when a completion count or rating changed
  in the last 30 minutes.
  - Each update is a Telegram rich message (`sendRichMessage`, Bot API 10.1+). It contains a
    heading with the event name, the total, a table of every mission (completions, change against
    the snapshot taken 30 minutes earlier, rating), and a slideshow of the charts.
  - The charts are five images plus the animated map, rendered by `analysis/charts.R`.
  - If the charts fail, the update is sent without them. If Telegram rejects the rich message,
    that destination gets plain text.
- **Failure alerts**: on a failed run (expired session, Intel errors), repeated at most every
  2 hours while it stays broken, then a "recovered" message.
- **Weather alerts**: when WeatherKit fails (repeated at most every 12 hours), then a "working
  again" message. Updates still go out, without weather.

`mdtrace telegram-test -event <slug> -charts analysis/charts.R` sends a preview of the current
update (rich message with charts) to the private chat only. Telegram failures are logged and never
stop collection.

## Weather (Apple WeatherKit)

If `~/.config/mdtrace/weatherkit.env` exists and the event has a `weather` point, each run also
fetches the weather there from the WeatherKit REST API. It gets current conditions plus the
hourly series for the past 24 hours and the next 7 hours. Stored hours are upserted, so the
history follows WeatherKit's revisions.

Updates then show the current conditions and the next 3 hours, but only if they were observed
within the last 90 minutes. `total.png` also gains a temperature and rain panel.

```sh
# ~/.config/mdtrace/weatherkit.env (mode 600); the key file is weatherkit.p8 next to it (mode 600)
WEATHERKIT_TEAM_ID=ABCDE12345                  # Apple Developer team
WEATHERKIT_KEY_ID=FGHIJ67890                   # key with WeatherKit enabled (AuthKey_<id>.p8)
WEATHERKIT_SERVICE_ID=com.example.mdtrace.weather   # registered Services ID
```

The location belongs to the event, so `WEATHERKIT_LAT` and `WEATHERKIT_LNG` are rejected here.
`mdtrace weather-test -event <slug>` fetches once and prints the result. A failed fetch is logged
and never stops collection. Weather data: Apple Weather
(<https://weatherkit.apple.com/legal-attribution.html>).

## Use the data

```sh
~/.config/mdtrace/bin/mdtrace export -event <slug> -o md.csv -weather weather.csv
launchctl kickstart -k gui/$(id -u)/io.github.szres.mdtrace   # collect now
tail -f ~/.config/mdtrace/events/<slug>/collect.log
```

Exported times are in the event's time zone. CSV columns: `completed_count` (cumulative unique
completions), `completed_delta` (change since that mission's previous snapshot), `rating_percent`,
`average_duration_ms`, author/faction.

The Mac must be awake for the schedule to run. launchd runs a missed slot once on wake.

### Publish to the dataset

```sh
./scripts/export-dataset.sh <slug> ../MDtrace-Data
```

This writes `events/<slug>/` in the dataset repository: `event.json`, `data/` (the CSV tables, a
self-contained SQLite copy and `missions.json`) and `geo/` when the event has map geometry. It
also adds the event to `events.json`, newest first, which is the dashboard's event list (the
entry records whether the event has a map). Run `npm test` in the dataset repository before you
commit: it checks that `events.json` and the event folders agree.

The export leaves out the bot's internal tables and replaces run errors that mention local paths.
It fails if any output still contains your home directory. Re-running it replaces that event only.

## Test

```sh
go test ./...
```

## Charts (ggplot2)

```sh
./scripts/charts.sh <slug>     # export + render into ./charts/<slug> (needs R with ggplot2, dplyr, scales, ragg, patchwork, jsonlite, gifski, av)
Rscript analysis/charts.R export.csv out/ --event events/<slug> [--weather weather.csv] [--final] [--lang en|zh]
```

`charts.R` reads the event's name, time zone, weather area and map focus from `event.json`, and
its map geometry from `geo/` when present.

`mdtrace` runs R with `LANG`/`LC_ALL=en_US.UTF-8`, because launchd jobs have no locale and without
one R cannot read Chinese mission titles. Charts use the ChillDINGothic font (Medium and Bold)
when it is installed, and PingFang SC otherwise.

| File | Shows |
| --- | --- |
| `total.png` | Total completions, completions added per hour, and (with weather) temperature and rain |
| `completions.png` | Cumulative completions per mission (small multiples, shared scale) |
| `pace.png` | Completions per hour, mission × time heatmap; shows the crowd moving along the route |
| `ratings.png` | Rating per mission from its first rating |
| `map.png` | Heat map of the last hour's completions, spread along each route (Gaussian kernel). Shows a city overview, plus a zoom panel on the event's `mapFocus` if it has one |
| `map_anim.gif` | The last 2 hours, one frame per 5-min read. Each read's new completions flash as heat and fade out over 30 min (one colour scale for all frames), with a per-read timeline. 2400×1600. Telegram gets the same animation as `map_anim.mp4`, because it re-encodes uploaded GIFs |
| `event_anim.mp4` | With `--final`: the whole event replayed, one frame per 10 min |

Without a `mapFocus`, every label goes on the overview, and labels of missions close together
overlap. Give a dense group of missions a `mapFocus` so they get their own zoomed panel.

Observed on 2026-11-07: Intel refreshes each mission's counters about **once an hour**, and each
mission refreshes at its own minute (e.g. #07–#09 at :40, #01 at :05). The total therefore moves
almost every 5 minutes, while a single mission moves hourly. Charts bin per hour for this reason.

## Measure Intel's update cadence

Intel's responses carry no caching headers, so the server-side refresh interval has to be
inferred from the data. With real-time counters, an actively played mission changes in almost
every run. With batched counters, changes cluster in some runs (often all missions at once)
and the other runs show no change.

```sh
sqlite3 -header -column ~/.config/mdtrace/events/<slug>/mdtrace.db "
WITH d AS (
  SELECT run_id, completed_count - LAG(completed_count) OVER (
           PARTITION BY mission_guid ORDER BY run_id) AS delta
  FROM snapshots)
SELECT r.id AS run, datetime(r.started_at, 'localtime') AS started,
       SUM(d.delta > 0) AS missions_changed, SUM(d.delta) AS completions_added
FROM runs r JOIN d ON d.run_id = r.id
GROUP BY r.id ORDER BY r.id;"
```

## License

Code: [MIT](LICENSE). Each event's `geo/coastline.json` is derived from OpenStreetMap data,
© OpenStreetMap contributors, available under the
[Open Database License](https://opendatacommons.org/licenses/odbl/1-0/).
