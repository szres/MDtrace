#!/usr/bin/env Rscript
# Render MD mission tracking charts from `mdtrace export -tz Asia/Shanghai` CSV output.
#
#   Rscript analysis/charts.R <export.csv> <output-dir> [--geo DIR] [--weather weather.csv] [--final] [--lang en|zh]
#
# --geo     directory with routes.json and coastline.json from scripts/import-geo.py (enables map.png)
# --weather hourly weather from `mdtrace export -weather` (adds a temperature/rain panel to total.png)
# --final   end-of-event set: map.png covers the whole event and event_anim.mp4 replays it,
#           instead of the last-hour map and the last-2-hours animation
# --lang    language of all chart text: en (default) or zh
#
# Outputs (PNG):
#   total.png        cumulative completions across all missions, completions added per hour and,
#                    with --weather, temperature and rain
#   completions.png  cumulative completions per mission (small multiples)
#   pace.png         completions per hour, mission x time heatmap (shows the crowd moving)
#   ratings.png      rating per mission over time (small multiples)
#   map.png          heat map of the last hour's completions spread along the routes (only with --geo)
#   map_anim.gif     the last 2 hours, one frame per read: new completions flash and fade over 30 min (only with --geo)
#   map_anim.mp4     the same animation as H.264 video, sent to Telegram (plays as a looping GIF there)
#   event_anim.mp4   with --final: the whole event replayed, one frame per 10 min: heat trail, completions
#                    and rating per mission, hourly timeline

suppressPackageStartupMessages({
  library(ggplot2)
  library(dplyr)
  library(scales)
})

usage <- "usage: Rscript charts.R <export.csv> <output-dir> [--geo DIR] [--weather weather.csv] [--final] [--lang en|zh]"
args <- commandArgs(trailingOnly = TRUE)
positional <- character()
options <- list()
i <- 1
while (i <= length(args)) {
  if (args[[i]] %in% c("--geo", "--weather", "--lang")) {
    if (i == length(args)) stop(usage, call. = FALSE)
    options[[sub("^--", "", args[[i]])]] <- args[[i + 1]]
    i <- i + 2
  } else if (args[[i]] == "--final") {
    options$final <- TRUE
    i <- i + 1
  } else if (startsWith(args[[i]], "--")) {
    stop("unknown option ", args[[i]], "\n", usage, call. = FALSE)
  } else {
    positional <- c(positional, args[[i]])
    i <- i + 1
  }
}
if (length(positional) != 2) {
  stop(usage, call. = FALSE)
}
csv_path <- positional[[1]]
out_dir <- positional[[2]]
geo_dir <- if (is.null(options$geo)) NA_character_ else options$geo
weather_path <- if (is.null(options$weather)) NA_character_ else options$weather
final_mode <- isTRUE(options$final)
lang <- if (is.null(options$lang)) "en" else options$lang
if (!lang %in% c("en", "zh")) {
  stop("--lang must be en or zh\n", usage, call. = FALSE)
}
dir.create(out_dir, showWarnings = FALSE, recursive = TRUE)

tz <- "Asia/Shanghai"
# Intel refreshes each mission's counters about once an hour (each mission at its own minute),
# so finer bins alternate between a refresh and nothing.
bin_minutes <- 60

# Reference palette (dataviz skill, light mode): one series hue, text inks, recessive grid.
surface <- "#fcfcfb"
ink_primary <- "#0b0b0b"
ink_secondary <- "#52514e"
grid_color <- "#e6e5e0"
series_blue <- "#2a78d6"
sequential_blues <- c("#cde2fb", "#9ec5f4", "#6da7ec", "#3987e5", "#256abf", "#184f95", "#0d366b")
# Published charts use ChillDINGothic (covers Latin and CJK): Medium for text, Bold for titles; Regular
# is too thin to stay crisp at chart sizes. The static faces are registered by file because a name
# lookup can resolve to the variable font's condensed instances. PingFang SC keeps the charts readable
# on a machine without the font.
chart_faces <- local({
  installed <- systemfonts::system_fonts()$path
  sapply(c(plain = "ChillDINGothic_Medium.otf", bold = "ChillDINGothic_Bold.otf"),
         function(file) installed[basename(installed) == file][1])
})
font_family <- if (!anyNA(chart_faces)) {
  systemfonts::register_font("ChillDINGothic Chart", plain = chart_faces[["plain"]], bold = chart_faces[["bold"]])
  "ChillDINGothic Chart"
} else {
  message("ChillDINGothic Medium/Bold are not installed; using PingFang SC")
  "PingFang SC"
}
emoji_family <- "Apple Color Emoji"

# Weather icons for WeatherKit condition codes; clear skies get a moon between 18:00 and 06:00,
# and an hour with measurable rain shows rain even when the condition code says cloudy.
condition_icons <- c(
  Clear = "☀️", MostlyClear = "☀️", Hot = "☀️", PartlyCloudy = "⛅", MostlyCloudy = "☁️", Cloudy = "☁️",
  Foggy = "🌫️", Haze = "🌫️", Smoky = "🌫️", Drizzle = "🌦️", SunShowers = "🌦️", Rain = "🌧️", HeavyRain = "🌧️",
  FreezingDrizzle = "🌧️", FreezingRain = "🌧️", Thunderstorms = "⛈️", IsolatedThunderstorms = "⛈️",
  ScatteredThunderstorms = "⛈️", StrongStorms = "⛈️", Breezy = "💨", Windy = "💨", TropicalStorm = "🌀",
  Hurricane = "🌀")
weather_icon <- function(code, time, rain_mm = 0) {
  hour <- as.integer(format(time, "%H", tz = "Asia/Shanghai"))
  night <- hour < 6 | hour >= 18
  icon <- unname(condition_icons[code])
  icon[is.na(icon)] <- ""
  icon[night & code %in% c("Clear", "MostlyClear")] <- "🌙"
  icon[night & code == "PartlyCloudy"] <- "☁️"
  icon[rain_mm >= 0.3 & !icon %in% c("🌧️", "⛈️", "🌦️")] <- "🌦️"
  icon
}
# Every third hour, so icons along a day-long strip do not crowd.
icon_hours <- function(hours) {
  hours |>
    filter(as.integer(format(time, "%H", tz = "Asia/Shanghai")) %% 3 == 0) |>
    mutate(icon = weather_icon(condition, time, precipitation_amount)) |>
    filter(icon != "")
}

# Chart text in the chosen language.
tr <- function(en, zh) if (lang == "zh") zh else en
zh_weekdays <- c("周日", "周一", "周二", "周三", "周四", "周五", "周六")
weekday <- function(time) {
  if (lang == "zh") zh_weekdays[as.POSIXlt(time, tz = tz)$wday + 1] else format(time, "%a", tz = tz)
}
# Axis labels such as "Sat 12:00" / "周六 12:00"; scales may pass NA for breaks outside the range.
day_time_labels <- function(time) {
  ifelse(is.na(time), NA_character_, paste(weekday(time), format(time, "%H:%M", tz = tz)))
}
replay_stamp <- function(time) {
  if (lang == "zh") {
    lt <- as.POSIXlt(time, tz = tz)
    sprintf("%d月%d日 %s %s", lt$mon + 1, lt$mday, weekday(time), format(time, "%H:%M", tz = tz))
  } else {
    format(time, "%a %d %b %H:%M", tz = tz)
  }
}
condition_names_zh <- c(
  Clear = "晴", MostlyClear = "大致晴朗", Hot = "炎热", PartlyCloudy = "局部多云", MostlyCloudy = "多云",
  Cloudy = "阴", Foggy = "雾", Haze = "霾", Smoky = "烟雾", Drizzle = "毛毛雨", SunShowers = "太阳雨",
  Rain = "雨", HeavyRain = "大雨", FreezingDrizzle = "冻毛毛雨", FreezingRain = "冻雨", Thunderstorms = "雷暴",
  IsolatedThunderstorms = "局部雷暴", ScatteredThunderstorms = "零星雷暴", StrongStorms = "强雷暴",
  Breezy = "微风", Windy = "大风", TropicalStorm = "热带风暴", Hurricane = "台风")
# WeatherKit condition code to words, e.g. "MostlyCloudy" -> "mostly cloudy" / "多云".
describe_condition <- function(code) {
  english <- tolower(gsub("([a-z])([A-Z])", "\\1 \\2", code))
  if (lang != "zh") return(english)
  ifelse(is.na(condition_names_zh[code]), english, unname(condition_names_zh[code]))
}

raw <- read.csv(csv_path, fileEncoding = "UTF-8-BOM", stringsAsFactors = FALSE)
required <- c("run_id", "run_started_at", "mission_order", "mission_guid", "title", "completed_count", "completed_delta", "rating_percent")
missing <- setdiff(required, names(raw))
if (length(missing) > 0) {
  stop("export is missing columns: ", paste(missing, collapse = ", "), call. = FALSE)
}
if (nrow(raw) == 0) {
  stop("export contains no snapshots", call. = FALSE)
}

# Export timestamps carry the +08:00 offset; the first 19 characters are local Zhuhai time.
# Every mission in a run is plotted at the run's start so a run never straddles a bin edge.
snapshots <- raw |>
  mutate(
    time = as.POSIXct(substr(run_started_at, 1, 19), format = "%Y-%m-%dT%H:%M:%S", tz = tz),
    name = sub("^MD \\d{4}: *[^,，]+[,，] *", "", title),
    label = sprintf("#%02d %s", mission_order, name),
    completed_delta = coalesce(completed_delta, 0L)
  )
if (anyNA(snapshots$time)) {
  stop("could not parse run_started_at timestamps", call. = FALSE)
}
mission_levels <- snapshots |> distinct(mission_order, label) |> arrange(mission_order) |> pull(label)
snapshots <- snapshots |> mutate(label = factor(label, levels = mission_levels))

# Skip the long flat stretch before anyone played; keep one bin of lead-in for context.
first_activity <- snapshots |> filter(completed_count > 0) |> summarise(t = min(time)) |> pull(t)
if (is.finite(first_activity)) {
  snapshots <- snapshots |> filter(time >= first_activity - bin_minutes * 60)
}
if (nrow(snapshots) == 0) {
  stop("no snapshots left to plot", call. = FALSE)
}
span_hours <- as.numeric(difftime(max(snapshots$time), min(snapshots$time), units = "hours"))
time_breaks <- if (span_hours > 12) "3 hours" else if (span_hours > 4) "1 hour" else "30 min"
time_scale <- scale_x_datetime(date_breaks = time_breaks, date_labels = "%H:%M", timezone = tz,
                               expand = expansion(mult = 0.01))
updated <- format(max(snapshots$time), "%m-%d %H:%M", tz = tz)

# The 18-panel charts have about 2 inches per panel: about 3 labels on whole local hours, with the
# weekday once a step spans part of a day.
facet_step_hours <- c(1, 2, 3, 6, 12, 24)
facet_step <- facet_step_hours[match(TRUE, span_hours / facet_step_hours <= 3, nomatch = length(facet_step_hours))]
facet_breaks <- function(limits) {
  first <- as.POSIXct(format(limits[1], "%Y-%m-%d %H:00:00", tz = tz), tz = tz)
  candidates <- seq(first, limits[2], by = 3600)
  candidates[candidates >= limits[1] & as.integer(format(candidates, "%H", tz = tz)) %% facet_step == 0]
}
facet_time_scale <- scale_x_datetime(breaks = facet_breaks, timezone = tz,
                                     labels = if (facet_step >= 6) day_time_labels else scales::label_time("%H:%M", tz = tz),
                                     expand = expansion(mult = 0.01))

theme_md <- theme_minimal(base_family = font_family, base_size = 11) +
  theme(
    plot.background = element_rect(fill = surface, colour = NA),
    panel.background = element_rect(fill = surface, colour = NA),
    panel.grid.major = element_line(colour = grid_color, linewidth = 0.3),
    panel.grid.minor = element_blank(),
    text = element_text(colour = ink_primary),
    axis.text = element_text(colour = ink_secondary, size = 8),
    axis.title = element_text(colour = ink_secondary, size = 9),
    plot.title = element_text(face = "bold", size = 14),
    plot.subtitle = element_text(colour = ink_secondary, size = 9, margin = margin(b = 10)),
    plot.caption = element_text(colour = ink_secondary, size = 8),
    strip.text = element_text(colour = ink_primary, size = 9, hjust = 0),
    legend.title = element_text(colour = ink_secondary, size = 9),
    legend.text = element_text(colour = ink_secondary, size = 8),
    plot.margin = margin(14, 16, 10, 12)
  )

save_png <- function(plot, file, width, height) {
  ggsave(file.path(out_dir, file), plot, width = width, height = height, dpi = 240,
         device = ragg::agg_png, bg = surface)
}

caption <- sprintf(tr("Source: Ingress Intel, sampled every 5 min · updated %s (UTC+8)",
                      "数据：Ingress Intel，每 5 分钟采样 · 更新于 %s（UTC+8）"), updated)

# Start of the bin_minutes-wide bin each time falls in.
bin_start <- function(time) {
  as.POSIXct(floor(as.numeric(time) / (bin_minutes * 60)) * bin_minutes * 60, origin = "1970-01-01", tz = tz)
}

# 1. Total completions across all missions, with the new completions per hour underneath
#    (two measures, two panels on a shared time axis).
total <- snapshots |>
  group_by(run_id) |>
  summarise(time = min(time), completed = sum(completed_count), missions = n(), .groups = "drop") |>
  filter(missions == max(missions))
# The newest bin is still filling up, so it is drawn lighter.
hourly <- snapshots |>
  mutate(bin = bin_start(time)) |>
  group_by(bin) |>
  summarise(added = sum(completed_delta), .groups = "drop") |>
  mutate(partial = bin == max(bin))
# Both panels span whole bins so their time axes line up.
total_xlim <- coord_cartesian(xlim = c(min(hourly$bin), max(hourly$bin) + bin_minutes * 60))
peak_hour <- hourly |> filter(!partial, added > 0) |> slice_max(added, n = 1, with_ties = FALSE)
latest_total <- total |> slice_max(time, n = 1, with_ties = FALSE)
p_total <- ggplot(total, aes(time, completed)) +
  geom_line(colour = series_blue, linewidth = 0.9) +
  geom_point(data = latest_total, colour = series_blue, size = 2.4) +
  geom_text(data = latest_total, aes(label = comma(completed)), family = font_family,
            colour = ink_primary, hjust = 1.15, vjust = -0.8, size = 3.6) +
  time_scale +
  scale_y_continuous(labels = comma, expand = expansion(mult = c(0, 0.12)), limits = c(0, NA)) +
  total_xlim +
  labs(x = NULL, y = tr("Total completions", "累计完成数")) +
  theme_md +
  theme(axis.text.x = element_blank())

p_hourly <- ggplot(hourly, aes(bin + bin_minutes * 30, added, fill = partial)) +
  geom_col(width = bin_minutes * 60 * 0.85) +
  geom_text(data = peak_hour, aes(label = comma(added)), family = font_family, colour = ink_primary,
            vjust = -0.5, size = 3.2) +
  scale_fill_manual(values = c(`FALSE` = series_blue, `TRUE` = sequential_blues[2]), guide = "none") +
  time_scale +
  scale_y_continuous(labels = comma, expand = expansion(mult = c(0, 0.18)), limits = c(0, NA)) +
  total_xlim +
  labs(x = NULL, y = tr("New per hour", "每小时新增")) +
  theme_md

# Weather for the same hours: temperature line, rain hours shaded (darker = more rain).
weather_hours <- NULL
if (!is.na(weather_path)) {
  weather_hours <- read.csv(weather_path, stringsAsFactors = FALSE)
  if (!all(c("hour_start", "temperature", "precipitation_amount") %in% names(weather_hours))) {
    stop("weather csv is missing columns", call. = FALSE)
  }
  weather_hours <- weather_hours |>
    mutate(time = as.POSIXct(substr(hour_start, 1, 19), format = "%Y-%m-%dT%H:%M:%S", tz = tz)) |>
    filter(!is.na(time), time >= min(hourly$bin), time <= max(hourly$bin))
  if (nrow(weather_hours) == 0) weather_hours <- NULL
}

panels <- list(p_total, p_hourly)
heights <- c(3, 2)
hourly_position <- if (is.null(weather_hours)) tr("bottom", "下") else tr("middle", "中")
subtitle <- sprintf(tr("Top: sum of unique completions over all %d missions · %s: completions added per hour (light bar = hour in progress)",
                       "上：全部 %d 个任务的独立完成数总和 · %s：每小时新增完成数（浅色柱 = 进行中的小时）"),
                    length(mission_levels), hourly_position)
if (!is.null(weather_hours)) {
  rain <- weather_hours |> filter(precipitation_amount > 0)
  latest_weather <- weather_hours |> slice_max(time, n = 1, with_ties = FALSE)
  icon_y <- max(weather_hours$temperature) + 0.45 * max(1, diff(range(weather_hours$temperature)))
  p_hourly <- p_hourly + theme(axis.text.x = element_blank())
  p_weather <- ggplot(weather_hours, aes(time + bin_minutes * 30, temperature)) +
    geom_rect(data = rain, aes(xmin = time, xmax = time + bin_minutes * 60, ymin = -Inf, ymax = Inf,
                               alpha = precipitation_amount),
              inherit.aes = FALSE, fill = sequential_blues[3]) +
    geom_line(colour = ink_primary, linewidth = 0.7) +
    geom_point(data = latest_weather, colour = ink_primary, size = 1.8) +
    geom_text(data = latest_weather, aes(label = sprintf("%.0f°C", temperature)), family = font_family,
              colour = ink_primary, hjust = 1.2, vjust = -0.6, size = 3.2) +
    geom_text(data = icon_hours(weather_hours), aes(y = icon_y, label = icon), family = emoji_family, size = 4.2) +
    scale_alpha_continuous(range = c(0.35, 0.9), guide = "none") +
    time_scale +
    scale_y_continuous(labels = function(x) paste0(x, "°"), expand = expansion(mult = c(0.25, 0.2))) +
    total_xlim +
    labs(x = NULL, y = tr("Temperature", "气温")) +
    theme_md
  panels <- list(p_total, p_hourly, p_weather)
  heights <- c(3, 2, 1.7)
  subtitle <- paste0(subtitle, tr("\nBottom: temperature and conditions at the 情侣路 cluster; blue bands = rain (darker = heavier). Weather: Apple Weather",
                                  "\n下：情侣路一带的气温与天气；蓝色区块 = 降雨（越深雨越大）。天气：Apple Weather"))
}
p_total_combo <- patchwork::wrap_plots(panels, ncol = 1, heights = heights) +
  patchwork::plot_annotation(title = tr("MD 2026 Zhuhai · total mission completions", "MD 2026 珠海 · 任务总完成数"),
                             subtitle = subtitle,
                             caption = caption, theme = theme_md)
save_png(p_total_combo, "total.png", 10, if (is.null(weather_hours)) 6.5 else 7.8)

# 2. Cumulative completions per mission, shared y so missions compare honestly.
p_completions <- ggplot(snapshots, aes(time, completed_count)) +
  geom_line(colour = series_blue, linewidth = 0.7) +
  facet_wrap(~label, ncol = 6) +
  facet_time_scale +
  scale_y_continuous(labels = comma, limits = c(0, NA), expand = expansion(mult = c(0, 0.08))) +
  labs(title = tr("Completions per mission", "各任务累计完成数"),
       subtitle = tr("Cumulative unique completions; all panels share the same scale",
                     "累计独立完成数；所有小图使用同一刻度"),
       x = NULL, y = tr("Completions", "完成数"), caption = caption) +
  theme_md +
  theme(panel.spacing = unit(10, "pt"), axis.text.x = element_text(angle = 0))
save_png(p_completions, "completions.png", 14, 7.5)

# 3. Pace: completions per hour, mission x time. The diagonal shows the route flow.
pace <- snapshots |>
  mutate(bin = bin_start(time)) |>
  group_by(label, bin) |>
  summarise(completions = sum(completed_delta), .groups = "drop")
p_pace <- ggplot(pace, aes(bin + bin_minutes * 30, label, fill = completions)) +
  geom_tile(colour = surface, linewidth = 0.6, width = bin_minutes * 60) +
  scale_fill_gradientn(colours = sequential_blues, limits = c(0, NA), name = tr("Completions\nper hour", "每小时\n完成数"),
                       guide = guide_colourbar(barheight = unit(90, "pt"), barwidth = unit(8, "pt"))) +
  time_scale +
  scale_y_discrete(limits = rev(mission_levels)) +
  labs(title = tr("Pace: completions per hour", "节奏：每小时完成数"),
       subtitle = tr("Darker = more agents finished that mission in that hour (Intel refreshes each mission hourly)",
                     "颜色越深 = 该小时完成该任务的特工越多（Intel 约每小时刷新一次各任务）"),
       x = NULL, y = NULL, caption = caption) +
  theme_md +
  theme(panel.grid.major = element_blank(), axis.text.y = element_text(colour = ink_primary, size = 9))
save_png(p_pace, "pace.png", 12, 6.5)

# 4. Ratings. Intel reports 0% until a mission's first rating arrives, so each mission's leading
# zeros mean "not rated yet" and are dropped; later zeros are real and kept.
ratings <- snapshots |>
  group_by(label) |>
  arrange(time, .by_group = TRUE) |>
  filter(cumany(rating_percent > 0)) |>
  ungroup()
if (nrow(ratings) > 0) {
  p_ratings <- ggplot(ratings, aes(time, rating_percent)) +
    geom_line(colour = series_blue, linewidth = 0.7) +
    facet_wrap(~label, ncol = 6, drop = FALSE) +
    facet_time_scale +
    scale_y_continuous(limits = c(0, 100), breaks = c(0, 50, 100), labels = function(x) paste0(x, "%")) +
    labs(title = tr("Rating per mission", "各任务评分"),
         subtitle = tr("Share of positive ratings on Intel, from each mission's first rating",
                       "Intel 上的好评比例，从各任务的首个评分开始"),
         x = NULL, y = tr("Rating", "评分"), caption = caption) +
    theme_md +
    theme(panel.spacing = unit(10, "pt"))
  save_png(p_ratings, "ratings.png", 14, 7.5)
}

# 5. Map: where completions happen, as a heat surface. Each mission's completions are spread
# evenly along its route and smoothed with a Gaussian kernel, so the heat follows the walked
# streets. The overview uses a wide kernel to show the city-scale spread; the second panel
# zooms into the dense 情侣路 cluster with a narrow kernel.
if (!is.na(geo_dir)) {
  routes_json <- jsonlite::fromJSON(file.path(geo_dir, "routes.json"), simplifyVector = FALSE)$routes
  coast_json <- jsonlite::fromJSON(file.path(geo_dir, "coastline.json"), simplifyVector = FALSE)
  to_frame <- function(points, ...) {
    data.frame(lat = vapply(points, function(p) p[[1]], numeric(1)),
               lng = vapply(points, function(p) p[[2]], numeric(1)), ...)
  }
  route_paths <- bind_rows(lapply(routes_json, function(r) {
    to_frame(r$points, mission_guid = r$missionGuid, order = r$order, name = r$displayName, seq = seq_along(r$points))
  }))
  coastline <- bind_rows(lapply(seq_along(coast_json$ways), function(i) to_frame(coast_json$ways[[i]], way = i)))

  # Each mission's newest snapshot: the newest run may still be in progress or have failed for some missions.
  latest <- snapshots |>
    group_by(mission_guid) |>
    slice_max(run_id, n = 1, with_ties = FALSE) |>
    ungroup() |>
    select(mission_guid, completed_count)
  route_paths <- route_paths |> inner_join(latest, by = "mission_guid")
  if (n_distinct(route_paths$mission_guid) != length(routes_json)) {
    stop("geo routes do not match the exported missions", call. = FALSE)
  }

  # Local equirectangular scale: degrees to km at Zhuhai's latitude.
  km_per_lat <- 110.57
  km_per_lng <- 111.32 * cos(mean(route_paths$lat) * pi / 180)

  # Resample a route every step_km so long and short legs carry heat in proportion to length.
  densify <- function(lat, lng, step_km = 0.015) {
    if (length(lat) < 2) return(data.frame(lat = lat, lng = lng))
    pieces <- lapply(seq_len(length(lat) - 1), function(i) {
      len <- sqrt(((lat[i + 1] - lat[i]) * km_per_lat)^2 + ((lng[i + 1] - lng[i]) * km_per_lng)^2)
      t <- seq(0, 1, length.out = max(1, ceiling(len / step_km)) + 1)[-1]
      data.frame(lat = lat[i] + t * (lat[i + 1] - lat[i]), lng = lng[i] + t * (lng[i + 1] - lng[i]))
    })
    bind_rows(data.frame(lat = lat[1], lng = lng[1]), bind_rows(pieces))
  }
  # Each resampled point carries an equal share of its mission's count.
  heat_samples <- route_paths |>
    arrange(order, seq) |>
    group_by(mission_guid) |>
    reframe(densify(lat, lng)) |>
    group_by(mission_guid) |>
    mutate(share = 1 / n()) |>
    ungroup()

  # Gaussian KDE on a regular grid. Heat is linear in the per-mission counts, so each panel keeps
  # one unit surface per mission (one column of basis) and any map is basis %*% counts. The kernel
  # is separable, so each unit surface is two matrix products.
  heat_kernel <- function(xlim, ylim, bandwidth_km, cells) {
    width_km <- diff(xlim) * km_per_lng
    height_km <- diff(ylim) * km_per_lat
    nx <- if (width_km >= height_km) cells else max(2, round(cells * width_km / height_km))
    ny <- if (height_km > width_km) cells else max(2, round(cells * height_km / width_km))
    gx <- seq(xlim[1], xlim[2], length.out = nx)
    gy <- seq(ylim[1], ylim[2], length.out = ny)
    kx <- exp(-(outer(gx, heat_samples$lng, "-") * km_per_lng)^2 / (2 * bandwidth_km^2))
    kyt <- t(exp(-(outer(gy, heat_samples$lat, "-") * km_per_lat)^2 / (2 * bandwidth_km^2)))
    missions <- unique(heat_samples$mission_guid)
    basis <- vapply(missions, function(guid) {
      idx <- which(heat_samples$mission_guid == guid)
      as.vector(kx[, idx, drop = FALSE] %*% (heat_samples$share[idx] * kyt[idx, , drop = FALSE]))
    }, numeric(nx * ny))
    list(grid = expand.grid(lng = gx, lat = gy), basis = basis, missions = missions)
  }
  # counts: completions per mission, named by mission GUID; missing missions count as zero.
  heat_values <- function(kernel, counts) {
    weights <- unname(counts[kernel$missions])
    weights[is.na(weights)] <- 0
    as.vector(kernel$basis %*% weights)
  }
  # Scale to peak (capped at 1). Faint heat fades into transparency instead of stopping at a hard
  # threshold, which would show the grid cells as stair-steps along the edge.
  heat_frame <- function(kernel, values, peak) {
    grid <- kernel$grid
    grid$heat <- if (peak > 0) pmin(values / peak, 1) else 0
    grid$opacity <- ifelse(grid$heat < 0.003, 0, pmin(1, grid$heat / 0.04))
    grid
  }

  centres <- route_paths |>
    group_by(mission_guid, order, name) |>
    # summarise() evaluates in order, so the east point must be taken before lat/lng become means.
    summarise(east_lat = lat[which.max(lng)], east_lng = max(lng), lat = mean(lat), lng = mean(lng),
              .groups = "drop")

  cluster_orders <- 1:11
  cluster <- route_paths |> filter(order %in% cluster_orders)
  zoom <- list(lng = range(cluster$lng) + c(-0.004, 0.012), lat = range(cluster$lat) + c(-0.003, 0.003))
  overview <- list(lng = range(route_paths$lng) + c(-0.03, 0.03), lat = range(route_paths$lat) + c(-0.02, 0.02))
  # Overview labels go right of the hot spot, except near the right edge where they would be clipped
  # or when another mission sits just to the right at the same height.
  right_edge <- overview$lng[1] + 0.7 * diff(overview$lng)
  outer_missions <- centres |> filter(!order %in% cluster_orders)
  crowded_right <- vapply(seq_len(nrow(outer_missions)), function(i) {
    o <- outer_missions
    any(o$lng > o$lng[i] & o$lng < o$lng[i] + 0.04 & abs(o$lat - o$lat[i]) < 0.008)
  }, logical(1))
  outer_missions <- outer_missions |>
    mutate(left = lng > right_edge | crowded_right, label_lng = ifelse(left, lng - 0.008, lng + 0.008),
           hjust = ifelse(left, 1, 0))
  cluster_missions <- centres |> filter(order %in% cluster_orders)

  overview_kernel <- heat_kernel(overview$lng, overview$lat, 0.35, 420)
  zoom_kernel <- heat_kernel(zoom$lng, zoom$lat, 0.07, 420)

  heat_scale <- scale_fill_gradientn(colours = sequential_blues, limits = c(0, 1), na.value = NA,
                                     breaks = c(0.1, 1), labels = c(tr("low", "低"), tr("peak", "峰值")),
                                     name = tr("Heat", "热度"))
  # The panels do not clip (labels may run past the edge), so the coastline is clipped to each
  # panel's box here: every segment is cut at the box edges (Liang-Barsky).
  coast_segments <- coastline |>
    group_by(way) |>
    mutate(xend = lead(lng), yend = lead(lat)) |>
    ungroup() |>
    filter(!is.na(xend)) |>
    transmute(x = lng, y = lat, xend, yend)
  clip_segments <- function(seg, xlim, ylim) {
    dx <- seg$xend - seg$x
    dy <- seg$yend - seg$y
    t0 <- rep(0, nrow(seg))
    t1 <- rep(1, nrow(seg))
    keep <- rep(TRUE, nrow(seg))
    edges <- list(list(p = -dx, q = seg$x - xlim[1]), list(p = dx, q = xlim[2] - seg$x),
                  list(p = -dy, q = seg$y - ylim[1]), list(p = dy, q = ylim[2] - seg$y))
    for (e in edges) {
      parallel <- e$p == 0
      keep <- keep & !(parallel & e$q < 0)
      ratio <- e$q / e$p
      entering <- !parallel & e$p < 0
      leaving <- !parallel & e$p > 0
      t0[entering] <- pmax(t0[entering], ratio[entering])
      t1[leaving] <- pmin(t1[leaving], ratio[leaving])
    }
    keep <- keep & t0 <= t1
    data.frame(x = seg$x + t0 * dx, y = seg$y + t0 * dy, xend = seg$x + t1 * dx, yend = seg$y + t1 * dy)[keep, ]
  }
  coast_layer <- function(xlim, ylim) {
    geom_segment(data = clip_segments(coast_segments, xlim, ylim), aes(x, y, xend = xend, yend = yend),
                 colour = "#8f8d84", linewidth = 0.35, lineend = "round")
  }
  overview_coast <- coast_layer(overview$lng, overview$lat)
  zoom_coast <- coast_layer(zoom$lng, zoom$lat)
  map_theme <- theme_md +
    theme(panel.grid.major = element_blank(), axis.text = element_blank(), axis.title = element_blank(),
          panel.border = element_rect(colour = grid_color, fill = NA, linewidth = 0.6))

  # Rain tints the panel background light blue, deeper for heavier rain. It sits under the heat so
  # it never competes with the heat, routes or labels.
  rain_wash <- function(xlim, ylim, rain_mm) {
    if (rain_mm < 0.05) return(NULL)
    annotate("rect", xmin = xlim[1], xmax = xlim[2], ymin = ylim[1], ymax = ylim[2],
             fill = sequential_blues[1], alpha = min(0.6, 0.25 + 0.15 * rain_mm))
  }

  # The overview and zoom panels for one heat surface; label_for(mission_guid) gives each label,
  # NA hides it.
  map_panels <- function(overview_heat, zoom_heat, label_for, text_size = 3, rain_mm = 0) {
    outer_labels <- outer_missions |> mutate(label = label_for(mission_guid)) |> filter(!is.na(label))
    zoom_labels <- cluster_missions |> mutate(label = label_for(mission_guid)) |> filter(!is.na(label))
    p_overview <- ggplot() +
      rain_wash(overview$lng, overview$lat, rain_mm) +
      geom_raster(data = overview_heat, aes(lng, lat, fill = heat, alpha = opacity), interpolate = TRUE) +
      overview_coast +
      annotate("rect", xmin = zoom$lng[1], xmax = zoom$lng[2], ymin = zoom$lat[1], ymax = zoom$lat[2],
               fill = NA, colour = ink_secondary, linewidth = 0.4, linetype = "dashed") +
      annotate("text", x = zoom$lng[2] + 0.004, y = zoom$lat[2], label = "情侣路 #01–#11 →",
               hjust = 0, vjust = 1, family = font_family, size = text_size, colour = ink_secondary) +
      geom_text(data = outer_labels, aes(label_lng, lat, label = label, hjust = hjust),
                family = font_family, size = text_size, colour = ink_primary) +
      heat_scale + scale_alpha_identity() +
      coord_quickmap(xlim = overview$lng, ylim = overview$lat, expand = FALSE, clip = "off") +
      labs(subtitle = sprintf(tr("All %d missions", "全部 %d 个任务"), length(mission_levels))) +
      map_theme
    p_zoom <- ggplot() +
      rain_wash(zoom$lng, zoom$lat, rain_mm) +
      geom_raster(data = zoom_heat, aes(lng, lat, fill = heat, alpha = opacity), interpolate = TRUE) +
      zoom_coast +
      geom_path(data = cluster, aes(lng, lat, group = mission_guid), colour = ink_secondary,
                linewidth = 0.25, alpha = 0.6, lineend = "round", linejoin = "round") +
      geom_text(data = zoom_labels, aes(east_lng, east_lat, label = label),
                hjust = 0, nudge_x = 0.0012, family = font_family, size = text_size, colour = ink_primary) +
      heat_scale + scale_alpha_identity() +
      coord_quickmap(xlim = zoom$lng, ylim = zoom$lat, expand = FALSE, clip = "off") +
      labs(subtitle = tr("情侣路 cluster · thin lines = routes", "情侣路片区 · 细线 = 路线")) +
      map_theme
    patchwork::wrap_plots(p_overview, p_zoom, widths = c(1, 1), guides = "collect")
  }
  mission_label <- function(guid, value) {
    m <- centres[match(guid, centres$mission_guid), ]
    sprintf("%02d %s · %s", m$order, m$name, value)
  }
  map_caption <- paste(caption, tr("· Coastline © OpenStreetMap contributors", "· 海岸线 © OpenStreetMap 贡献者"))

  if (final_mode) {
    # 5a (final). Static map of the whole event, each panel scaled to its own hottest spot.
    totals <- setNames(latest$completed_count, latest$mission_guid)
    overview_values <- heat_values(overview_kernel, totals)
    zoom_values <- heat_values(zoom_kernel, totals)
    p_map <- map_panels(heat_frame(overview_kernel, overview_values, max(overview_values)),
                        heat_frame(zoom_kernel, zoom_values, max(zoom_values)),
                        function(guid) mission_label(guid, comma(unname(totals[guid])))) +
      patchwork::plot_annotation(
        title = tr("Where agents played · the whole Mission Day", "特工们在哪里玩 · 整个任务日"),
        subtitle = sprintf(tr("%s unique completions · heat = completions spread along each route (each panel scaled to its hottest spot) · label: number, name, completions",
                              "共 %s 次独立完成 · 热度 = 沿各任务路线分布的完成数（每个面板按其最热点缩放）· 标签：编号、名称、完成数"),
                           comma(sum(totals))),
        caption = map_caption, theme = theme_md)
  } else {
    # 5a. Static map: completions in the last hour, each panel scaled to its own hottest spot.
    # Intel refreshes each mission about hourly, so the hour holds one refresh of every mission.
    map_end <- max(snapshots$time)
    map_start <- map_end - 60 * 60
    last_hour <- snapshots |>
      filter(time > map_start, time <= map_end) |>
      group_by(mission_guid) |>
      summarise(added = sum(completed_delta), .groups = "drop")
    last_hour_added <- setNames(last_hour$added, last_hour$mission_guid)
    hour_total <- sum(last_hour$added)
    overview_values <- heat_values(overview_kernel, last_hour_added)
    zoom_values <- heat_values(zoom_kernel, last_hour_added)
    p_map <- map_panels(heat_frame(overview_kernel, overview_values, max(overview_values)),
                        heat_frame(zoom_kernel, zoom_values, max(zoom_values)),
                        function(guid) {
                          added <- unname(last_hour_added[guid])
                          mission_label(guid, sprintf("+%d", ifelse(is.na(added), 0L, added)))
                        }) +
      patchwork::plot_annotation(
        title = sprintf(tr("Where agents are playing · last hour (%s–%s)", "特工们在哪里玩 · 最近一小时（%s–%s）"),
                        format(map_start, "%H:%M", tz = tz), format(map_end, "%H:%M", tz = tz)),
        subtitle = sprintf(tr("%s · heat = completions in the hour, spread along each route (each panel scaled to its hottest spot) · label: number, name, +completions",
                              "%s · 热度 = 该小时内沿各路线分布的完成数（每个面板按其最热点缩放）· 标签：编号、名称、+完成数"),
                           if (hour_total > 0) sprintf(tr("%s completions in the last hour", "最近一小时 %s 次完成"), comma(hour_total))
                           else tr("No completions in the last hour", "最近一小时没有完成")),
        caption = map_caption, theme = theme_md)
  }
  save_png(p_map, "map.png", 14, 8)

  # 5b. Animated map: the last two hours, one frame per 5-minute read. Each read's new completions
  # flash as heat and fade out linearly over 30 minutes; one colour scale for the whole animation.
  anim_span <- 2 * 60 * 60
  fade <- 30 * 60
  anim_width <- 2400
  anim_height <- 1600
  anim_dpi <- 200
  anim_fps <- 2
  run_times <- sort(unique(as.numeric(snapshots$time)))
  frame_times <- run_times[run_times > max(run_times) - anim_span]
  if (!final_mode && length(frame_times) >= 2) {
    reads <- snapshots |>
      mutate(t = as.numeric(time)) |>
      filter(t > min(frame_times) - fade, completed_delta > 0) |>
      select(t, mission_guid, completed_delta)
    frame_reads <- lapply(frame_times, function(now) {
      reads |>
        filter(t <= now, now - t < fade) |>
        mutate(weight = completed_delta * (1 - (now - t) / fade))
    })
    faded_counts <- lapply(frame_reads, function(r) {
      by_mission <- r |> group_by(mission_guid) |> summarise(w = sum(weight), .groups = "drop")
      setNames(by_mission$w, by_mission$mission_guid)
    })
    overview_frames <- lapply(faded_counts, function(counts) heat_values(overview_kernel, counts))
    zoom_frames <- lapply(faded_counts, function(counts) heat_values(zoom_kernel, counts))
    overview_peak <- max(unlist(overview_frames))
    zoom_peak <- max(unlist(zoom_frames))

    per_read <- snapshots |>
      mutate(t = as.numeric(time)) |>
      filter(t %in% frame_times) |>
      group_by(t) |>
      summarise(added = sum(completed_delta), .groups = "drop") |>
      mutate(time = as.POSIXct(t, origin = "1970-01-01", tz = tz))
    read_step <- 5 * 60  # the collector's schedule; manual runs can fall closer together

    frame_dir <- tempfile("map-frames-")
    dir.create(frame_dir)
    frame_files <- character(length(frame_times))
    for (i in seq_along(frame_times)) {
      now <- frame_times[i]
      recent <- frame_reads[[i]] |> group_by(mission_guid) |> summarise(added = sum(completed_delta), .groups = "drop")
      recent_added <- setNames(recent$added, recent$mission_guid)
      label_for <- function(guid) {
        added <- unname(recent_added[guid])
        ifelse(is.na(added), NA_character_, mission_label(guid, sprintf("+%d", added)))
      }
      # Timeline: the current read dark, reads still fading in proportion, the rest light.
      timeline <- per_read |>
        mutate(age = now - t, glow = ifelse(age >= 0 & age < fade, 1 - age / fade, 0))
      p_timeline <- ggplot(timeline, aes(time, added)) +
        geom_col(fill = sequential_blues[1], width = read_step * 0.8) +
        geom_col(aes(alpha = glow), fill = series_blue, width = read_step * 0.8) +
        geom_vline(xintercept = as.POSIXct(now, origin = "1970-01-01", tz = tz), colour = ink_secondary,
                   linewidth = 0.4) +
        scale_alpha_identity() +
        scale_x_datetime(date_breaks = "30 min", date_labels = "%H:%M", timezone = tz) +
        scale_y_continuous(labels = comma, breaks = scales::breaks_pretty(3)) +
        labs(x = NULL, y = tr("Per read", "每次读取")) +
        theme_md
      frame <- patchwork::wrap_plots(
        map_panels(heat_frame(overview_kernel, overview_frames[[i]], overview_peak),
                   heat_frame(zoom_kernel, zoom_frames[[i]], zoom_peak), label_for, text_size = 2.8),
        p_timeline, ncol = 1, heights = c(5, 1)) +
        patchwork::plot_annotation(
          title = sprintf(tr("Where agents are playing · %s", "特工们在哪里玩 · %s"),
                          format(as.POSIXct(now, origin = "1970-01-01", tz = tz), "%H:%M", tz = tz)),
          subtitle = sprintf(tr("Last 2 hours, one frame per 5-min read · new completions flash and fade over 30 min · %s in the past 30 min · label: +completions",
                                "最近 2 小时，每 5 分钟读取一帧 · 新完成闪现并在 30 分钟内淡出 · 过去 30 分钟 %s 次 · 标签：+完成数"),
                             comma(sum(frame_reads[[i]]$completed_delta))),
          caption = map_caption, theme = theme_md)
      frame_files[i] <- file.path(frame_dir, sprintf("frame-%03d.png", i))
      ggsave(frame_files[i], frame, width = anim_width / anim_dpi, height = anim_height / anim_dpi, dpi = anim_dpi,
             device = ragg::agg_png, bg = surface)
    }
    # Hold the latest frame before the loop restarts. The GIF is for local viewing; Telegram gets the
    # MP4, because it re-encodes uploaded GIFs and their 256-colour frames lose detail twice.
    sequence <- c(frame_files, rep(tail(frame_files, 1), 4))
    invisible(gifski::gifski(sequence, file.path(out_dir, "map_anim.gif"),
                             width = anim_width, height = anim_height, delay = 1 / anim_fps, progress = FALSE))
    av::av_encode_video(sequence, file.path(out_dir, "map_anim.mp4"), framerate = anim_fps,
                        vfilter = "format=yuv420p", codec = "libx264", verbose = FALSE)
    unlink(frame_dir, recursive = TRUE)
  }

  # 5c. Event replay (final only): the whole Mission Day, one frame per 10 minutes. The map shows a
  # heat trail of the last hour (each read fades out linearly) on one colour scale throughout,
  # beside every mission's completions and rating at that moment and an hourly timeline.
  if (final_mode) {
    replay_step <- 10 * 60
    replay_trail <- 60 * 60
    replay_width <- 2560
    replay_height <- 1440
    replay_dpi <- 2560 / 12
    replay_fps <- 8
    observed <- snapshots |> mutate(t = as.numeric(time))
    replay_times <- seq(ceiling(min(observed$t) / replay_step) * replay_step, max(observed$t), by = replay_step)
    if (length(replay_times) == 0 || tail(replay_times, 1) < max(observed$t)) {
      replay_times <- c(replay_times, max(observed$t))
    }
    reads <- observed |> filter(completed_delta > 0) |> select(t, mission_guid, completed_delta)
    trail_counts <- lapply(replay_times, function(now) {
      trail <- reads |>
        filter(t <= now, now - t < replay_trail) |>
        group_by(mission_guid) |>
        summarise(w = sum(completed_delta * (1 - (now - t) / replay_trail)), .groups = "drop")
      setNames(trail$w, trail$mission_guid)
    })
    overview_frames <- lapply(trail_counts, function(counts) heat_values(overview_kernel, counts))
    zoom_frames <- lapply(trail_counts, function(counts) heat_values(zoom_kernel, counts))
    overview_peak <- max(unlist(overview_frames))
    zoom_peak <- max(unlist(zoom_frames))
    bar_limit <- max(observed$completed_count) * 1.34
    # Weather is optional: a temperature strip with rain shading under the timeline, and the
    # conditions of the current hour in the subtitle.
    replay_weather <- if (is.null(weather_hours)) NULL else weather_hours |> mutate(t = as.numeric(time))
    if (!is.null(replay_weather)) {
      replay_icons <- icon_hours(replay_weather)
      replay_icon_y <- max(replay_weather$temperature) + 0.5 * max(1, diff(range(replay_weather$temperature)))
    }

    replay_dir <- tempfile("replay-frames-")
    dir.create(replay_dir)
    replay_files <- character(length(replay_times))
    for (i in seq_along(replay_times)) {
      now <- replay_times[i]
      now_time <- as.POSIXct(now, origin = "1970-01-01", tz = tz)
      state <- observed |>
        filter(t <= now) |>
        group_by(mission_guid) |>
        slice_max(t, n = 1, with_ties = FALSE) |>
        ungroup() |>
        mutate(rating = ifelse(rating_percent > 0, sprintf("%.0f%%", rating_percent), "–"))
      counts_now <- setNames(state$completed_count, state$mission_guid)

      p_bars <- ggplot(state, aes(completed_count, label)) +
        geom_col(fill = series_blue, width = 0.7) +
        geom_text(aes(label = comma(completed_count)), hjust = -0.2, family = font_family, size = 2.5,
                  colour = ink_primary) +
        geom_text(aes(x = bar_limit, label = rating), hjust = 1, family = font_family, size = 2.5,
                  colour = ink_secondary) +
        scale_y_discrete(limits = rev(mission_levels)) +
        scale_x_continuous(limits = c(0, bar_limit), expand = expansion(mult = 0)) +
        labs(x = NULL, y = NULL, subtitle = tr("Completions · rating", "完成数 · 评分")) +
        theme_md +
        theme(panel.grid.major = element_blank(), axis.text.x = element_blank(),
              axis.text.y = element_text(colour = ink_primary, size = 7))
      p_timeline <- ggplot(hourly, aes(bin + bin_minutes * 30, added, fill = as.numeric(bin) <= now)) +
        geom_col(width = bin_minutes * 60 * 0.85) +
        geom_vline(xintercept = now_time, colour = ink_secondary, linewidth = 0.4) +
        scale_fill_manual(values = c(`FALSE` = sequential_blues[1], `TRUE` = series_blue), guide = "none") +
        scale_x_datetime(date_breaks = "6 hours", labels = day_time_labels, timezone = tz) +
        scale_y_continuous(labels = comma, breaks = scales::breaks_pretty(3)) +
        coord_cartesian(xlim = range(hourly$bin) + c(0, bin_minutes * 60)) +
        labs(x = NULL, y = tr("Per hour", "每小时")) +
        theme_md
      hour_now <- if (is.null(replay_weather)) NULL else replay_weather |> filter(t <= now, now - t < 3600)
      if (!is.null(hour_now) && nrow(hour_now) == 0) hour_now <- NULL
      rain_now <- if (is.null(hour_now)) 0 else hour_now$precipitation_amount[1]
      right_column <- p_bars
      if (!is.null(hour_now)) {
        p_badge <- ggplot() +
          annotate("text", x = 0.02, y = 0.5, label = weather_icon(hour_now$condition[1], hour_now$time[1], rain_now),
                   family = emoji_family, size = 11, hjust = 0) +
          annotate("text", x = 0.24, y = 0.5, label = sprintf("%.0f°C", hour_now$temperature[1]),
                   family = font_family, fontface = "bold", size = 7, hjust = 0, colour = ink_primary) +
          annotate("text", x = 0.5, y = 0.5, family = font_family, size = 3, hjust = 0, colour = ink_secondary,
                   label = sprintf("%s\n%s", describe_condition(hour_now$condition[1]),
                                   if (rain_now > 0) sprintf(tr("rain %.1f mm/h", "降雨 %.1f 毫米/时"), rain_now)
                                   else tr("dry", "无雨"))) +
          scale_x_continuous(limits = c(0, 1), expand = expansion(0)) +
          scale_y_continuous(limits = c(0, 1), expand = expansion(0)) +
          theme_void()
        right_column <- patchwork::wrap_plots(p_badge, p_bars, ncol = 1, heights = c(0.6, 5))
      }
      panels <- list(
        patchwork::wrap_plots(
          map_panels(heat_frame(overview_kernel, overview_frames[[i]], overview_peak),
                     heat_frame(zoom_kernel, zoom_frames[[i]], zoom_peak),
                     function(guid) mission_label(guid, comma(unname(counts_now[guid]))), text_size = 2.3,
                     rain_mm = rain_now),
          right_column, widths = c(2.3, 1)),
        p_timeline + theme(axis.text.x = if (is.null(replay_weather)) element_text(colour = ink_secondary, size = 8)
                                         else element_blank()))
      heights <- c(5, 1)
      if (!is.null(replay_weather)) {
        p_replay_weather <- ggplot(replay_weather, aes(time + bin_minutes * 30, temperature)) +
          geom_rect(data = filter(replay_weather, precipitation_amount > 0),
                    aes(xmin = time, xmax = time + bin_minutes * 60, ymin = -Inf, ymax = Inf,
                        alpha = precipitation_amount),
                    inherit.aes = FALSE, fill = sequential_blues[3]) +
          geom_line(colour = ink_primary, linewidth = 0.5) +
          geom_text(data = replay_icons, aes(y = replay_icon_y, label = icon), family = emoji_family, size = 3.8) +
          geom_vline(xintercept = now_time, colour = ink_secondary, linewidth = 0.4) +
          scale_alpha_continuous(range = c(0.35, 0.9), guide = "none") +
          scale_x_datetime(date_breaks = "6 hours", labels = day_time_labels, timezone = tz) +
          scale_y_continuous(labels = function(x) paste0(x, "°"), breaks = scales::breaks_pretty(3),
                             expand = expansion(mult = c(0.1, 0.15))) +
          coord_cartesian(xlim = range(hourly$bin) + c(0, bin_minutes * 60)) +
          labs(x = NULL, y = tr("Temp · rain", "气温 · 降雨")) +
          theme_md
        panels <- c(panels, list(p_replay_weather))
        heights <- c(5, 1, 1)
      }
      frame <- patchwork::wrap_plots(panels, ncol = 1, heights = heights) +
        patchwork::plot_annotation(
          title = sprintf(tr("MD 2026 Zhuhai · %s", "MD 2026 珠海 · %s"), replay_stamp(now_time)),
          subtitle = sprintf(tr("%s unique completions so far · heat = completions in the past hour, fading · bars: completions and rating per mission%s",
                                "目前共 %s 次独立完成 · 热度 = 过去一小时的完成数（渐隐）· 柱状：各任务完成数与评分%s"),
                             comma(sum(state$completed_count)),
                             if (is.null(replay_weather)) "" else tr(" · light blue map and blue bands = rain", " · 浅蓝地图与蓝色区块 = 降雨")),
          caption = paste(map_caption, if (is.null(replay_weather)) "" else tr("· Weather: Apple Weather", "· 天气：Apple Weather")),
          theme = theme_md)
      replay_files[i] <- file.path(replay_dir, sprintf("frame-%04d.png", i))
      ggsave(replay_files[i], frame, width = replay_width / replay_dpi, height = replay_height / replay_dpi,
             dpi = replay_dpi, device = ragg::agg_png, bg = surface)
    }
    # Hold the final frame for three seconds.
    av::av_encode_video(c(replay_files, rep(tail(replay_files, 1), replay_fps * 3)),
                        file.path(out_dir, "event_anim.mp4"), framerate = replay_fps,
                        vfilter = "format=yuv420p", codec = "libx264", verbose = FALSE)
    unlink(replay_dir, recursive = TRUE)
  }
}

cat("charts written to", normalizePath(out_dir), "\n")
