# MDTrace

Records the completion count ("agents completed") and rating of the MD 2026 Zhuhai Ingress
missions from Ingress Intel every 5 minutes, posts live updates with charts to Telegram, and
sends a bilingual (English / 中文) final summary with a Mission Day replay when the event ends.

- **Data and dashboard**: [szres/MDtrace-Data](https://github.com/szres/MDtrace-Data) holds the
  raw data collected for MD 2026 Zhuhai (6–8 Nov 2026) and a static dashboard at
  <https://szres.github.io/MDtrace-Data/>.

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
| `missions.json` | The 18 missions to track (Intel GUID, order, display name) |
| `cmd/mdtrace` | CLI: `collect`, `export`, `telegram-test`, `weather-test`, `final` |
| `internal/intel` | Intel API client and `getMissionDetails` parser |
| `internal/store` | SQLite storage (`runs`, `snapshots`, `failures`, `weather_current`, `weather_hours`) |
| `internal/telegram` | Bot API client (rich messages, slideshows, plain-text fallback) |
| `internal/weather` | Apple WeatherKit REST client (ES256 developer token) |
| `analysis/charts.R` | ggplot2 charts, heat maps and animations used in updates and the final summary |
| `geo/` | Mission route polylines and OpenStreetMap coastline for the map charts |
| `deploy/`, `scripts/` | launchd agent templates, install / uninstall / scheduled stop |

Runtime files live in `~/.config/mdtrace` (`bin/mdtrace`, `missions.json`, `mdtrace.db`,
`collect.log`, and the config files below). They are kept outside `~/Documents`, where macOS
privacy rules block launchd jobs.

## Install (macOS)

Needs Go 1.26+, and R for charts (see [Charts](#charts-ggplot2)).

```sh
./scripts/install.sh     # test, build, install, load agent io.github.szres.mdtrace (runs every 5 min)
./scripts/uninstall.sh   # stop agent; data is kept
```

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

### Scheduled stop

```sh
./scripts/schedule-stop.sh "2026-11-08 13:02" Asia/Shanghai
```

A one-off agent `io.github.szres.mdtrace.stop` runs `stop.sh` at that time: it unloads the
collector, re-enables a channel line paused as `# Paused <date>: TELEGRAM_CHANNEL_ID=…` (so the
channel gets the final summary but no more regular updates), sends the final summary, keeps all
data, and removes itself. `./scripts/install.sh` starts collection again.

The final summary (`mdtrace final -tz Asia/Shanghai -charts charts.R`) goes to the private chat
(and the channel, if configured) as two messages, English then Chinese, each with its own chart
set whose text is in that language (`charts.R --lang zh`): totals, event span, highlights
(most/fewest completions, highest/lowest rating, quickest/longest runs, busiest hour, halfway
point, weather), and every mission's completions, rating and rating change since its first
rating. Its slideshow starts with `event_anim.mp4`, a replay of the whole Mission Day (one frame
per 10 min: heat trail of the past hour, completions and rating per mission, hourly timeline,
temperature and rain), followed by whole-event charts. Copies are saved in
`~/.config/mdtrace/final/` (Chinese set in `final/zh/`). `-preview` sends it to the private chat
only, marked as a preview.

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

- **Updates**: at most one every 30 minutes, when any completion count or rating changed over
  the last 30 minutes. Each update is a Telegram rich message (`sendRichMessage`, Bot API 10.1+)
  with a heading, the total, a table of every mission (completions, change against the snapshot
  taken 30 minutes earlier, rating), and a slideshow of the charts (five images plus the
  animated map) rendered by `analysis/charts.R` (installed to `~/.config/mdtrace/charts.R`, with
  `geo/` for the map). If charts fail, the update is sent without them. If Telegram rejects the
  rich message, that destination gets plain text.
- **Failure alerts**: on a failed run (expired session, Intel errors), repeated at most every
  2 hours while it stays broken, then a "recovered" message.
- **Weather alerts**: when WeatherKit fails (repeated at most every 12 hours), then a "working
  again" message. Updates still go out, without weather.

`mdtrace telegram-test -tz Asia/Shanghai -charts analysis/charts.R` sends a preview of the
current update (rich message with charts) to the private chat only. Telegram failures are logged
and never stop collection.

## Weather (Apple WeatherKit)

If `~/.config/mdtrace/weatherkit.env` exists, each run also fetches the weather at the 情侣路
cluster from the WeatherKit REST API: current conditions plus the hourly series for the past
24 hours and next 7 hours. Stored hours are upserted, so history follows WeatherKit's revisions.
Updates then show the current conditions and the next 3 hours (only if observed within the last
90 minutes), and `total.png` gains a temperature/rain panel.

```sh
# ~/.config/mdtrace/weatherkit.env (mode 600); the key file is weatherkit.p8 next to it (mode 600)
WEATHERKIT_TEAM_ID=ABCDE12345                  # Apple Developer team
WEATHERKIT_KEY_ID=FGHIJ67890                   # key with WeatherKit enabled (AuthKey_<id>.p8)
WEATHERKIT_SERVICE_ID=com.example.mdtrace.weather   # registered Services ID
WEATHERKIT_LAT=22.2738
WEATHERKIT_LNG=113.5790
```

`mdtrace weather-test -tz Asia/Shanghai` fetches once and prints the result. A failed fetch is
logged and never stops collection. Weather data: Apple Weather
(<https://weatherkit.apple.com/legal-attribution.html>).

## Use the data

```sh
~/.config/mdtrace/bin/mdtrace export -tz Asia/Shanghai -o md2026.csv -weather weather.csv
launchctl kickstart -k gui/$(id -u)/io.github.szres.mdtrace   # collect now
tail -f ~/.config/mdtrace/collect.log
```

CSV columns: `completed_count` (cumulative unique completions), `completed_delta` (change since
that mission's previous snapshot), `rating_percent`, `average_duration_ms`, author/faction.

The Mac must be awake for the schedule to run. launchd runs a missed slot once on wake.

### Publish the dataset

```sh
./scripts/export-dataset.sh ../MDtrace-Data   # writes data/ and geo/ (default DB: ~/.config/mdtrace/mdtrace.db)
```

The export writes the CSV tables, a self-contained SQLite copy, `missions.json` and the map geometry.
It leaves out the bot's internal tables and replaces run errors that mention local paths. It fails if
any output still contains your home directory.

## Test

```sh
go test ./...
```

## Charts (ggplot2)

```sh
./scripts/charts.sh            # export + render PNGs into ./charts (needs R with ggplot2, dplyr, scales, ragg, patchwork, jsonlite, gifski, av)
Rscript analysis/charts.R export.csv out/ [--geo geo] [--weather weather.csv] [--final] [--lang en|zh]
```

`mdtrace` runs R with `LANG`/`LC_ALL=en_US.UTF-8`: launchd jobs have no locale, and without one R
cannot read the Chinese mission titles. Charts use the ChillDINGothic font (Medium and Bold) when
it is installed, and PingFang SC otherwise.

| File | Shows |
| --- | --- |
| `total.png` | Total completions, completions added per hour, and (with weather) temperature and rain |
| `completions.png` | Cumulative completions per mission (small multiples, shared scale) |
| `pace.png` | Completions per hour, mission × time heatmap; shows the crowd moving along the route |
| `ratings.png` | Rating per mission from its first rating |
| `map.png` | Heat map of the last hour's completions spread along each route (Gaussian kernel): city overview plus a zoom on the 情侣路 cluster (#01–#11) |
| `map_anim.gif` | The last 2 hours, one frame per 5-min read: each read's new completions flash as heat and fade out over 30 min (one colour scale for all frames), with a per-read timeline. 2400×1600; Telegram gets the same animation as `map_anim.mp4`, because it re-encodes uploaded GIFs |
| `event_anim.mp4` | With `--final`: the whole event replayed, one frame per 10 min |

The map uses `geo/routes.json` (route polylines from the Zhuhai MD site, matched by
`sourceRouteId`) and `geo/coastline.json` (OpenStreetMap coastline, © OpenStreetMap contributors,
ODbL 1.0). Regenerate both when routes change, which needs network for the coastline:

```sh
python3 -I scripts/import-geo.py path/to/zhuhai-md/public/missions.json
```

Observed on 2026-11-07: Intel refreshes each mission's counters about **once an hour**, and each
mission refreshes at its own minute (e.g. #07–#09 at :40, #01 at :05). The total therefore moves
almost every 5 minutes, while a single mission moves hourly. Charts bin per hour for this reason.

## Measure Intel's update cadence

Intel's responses carry no caching headers, so the server-side refresh interval has to be
inferred from the data. With real-time counters, an actively played mission changes in almost
every run. With batched counters, changes cluster in some runs (often all missions at once)
and the other runs show no change.

```sh
sqlite3 -header -column ~/.config/mdtrace/mdtrace.db "
WITH d AS (
  SELECT run_id, completed_count - LAG(completed_count) OVER (
           PARTITION BY mission_guid ORDER BY run_id) AS delta
  FROM snapshots)
SELECT r.id AS run, datetime(r.started_at, '+8 hours') AS started_cst,
       SUM(d.delta > 0) AS missions_changed, SUM(d.delta) AS completions_added
FROM runs r JOIN d ON d.run_id = r.id
GROUP BY r.id ORDER BY r.id;"
```

## License

Code: [MIT](LICENSE). `geo/coastline.json` is derived from OpenStreetMap data,
© OpenStreetMap contributors, available under the
[Open Database License](https://opendatacommons.org/licenses/odbl/1-0/).
