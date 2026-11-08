package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mdtrace/internal/event"
	"mdtrace/internal/store"
	"mdtrace/internal/telegram"
	"mdtrace/internal/weather"
)

// finalChartTimeout covers the whole-event replay, which renders hundreds of frames.
const finalChartTimeout = 15 * time.Minute

// rainyHourMM is the hourly rain amount from which an hour counts as rainy.
const rainyHourMM = 0.1

type finalMission struct {
	order         int
	name          string
	completed     int64
	rating        float64 // percent; 0 until the first rating
	firstRating   float64 // percent at the mission's first rated snapshot
	rated         bool
	avgDurationMs int64
}

type finalWeather struct {
	minTemp, maxTemp float64
	hours            int
	rainyHours       int
	rainMM           float64
	hottest          weather.Hour
	wettest          weather.Hour // zero when no rain fell
}

// missionHour is one mission's completions within one clock hour.
type missionHour struct {
	mission finalMission
	start   time.Time
	count   int64
}

// finalReport summarises the whole event for the messages sent when collection ends.
type finalReport struct {
	name      event.Text // the event's name, for the title
	loc       *time.Location
	start     time.Time // first run in which any mission gained a completion
	end       time.Time // last run
	total     int64
	missions  []finalMission
	peakStart time.Time // start of the hour with the most new completions
	peakCount int64
	bestHour  missionHour // the most completions one mission got in one hour
	halfwayAt time.Time   // first run by which half of the final total was reached
	weather   *finalWeather
	preview   bool
}

func buildFinalReport(rows []store.Row, hours []weather.Hour, names map[string]string, loc *time.Location) (finalReport, error) {
	if len(rows) == 0 {
		return finalReport{}, errors.New("no snapshots collected")
	}
	r := finalReport{loc: loc}
	byGUID := map[string]*finalMission{}
	perHour := map[time.Time]int64{}
	perMissionHour := map[string]map[time.Time]int64{}
	// totals[i] is the sum of every mission's latest count after run runs[i].
	var runs []time.Time
	var totals []int64
	current := map[string]int64{}
	for _, row := range rows {
		m := byGUID[row.MissionGUID]
		if m == nil {
			m = &finalMission{order: row.MissionOrder, name: names[row.MissionGUID]}
			if m.name == "" {
				m.name = row.Title
			}
			byGUID[row.MissionGUID] = m
			perMissionHour[row.MissionGUID] = map[time.Time]int64{}
		}
		// Rows are ordered by run, so the last one seen is the mission's final state.
		m.completed = row.CompletedCount
		m.rating = float64(row.RatingE6) / 10000
		m.avgDurationMs = row.AverageDurationMs
		if !m.rated && row.RatingE6 > 0 {
			m.rated, m.firstRating = true, m.rating
		}
		if row.RunStartedAt.After(r.end) {
			r.end = row.RunStartedAt
		}
		if row.CompletedDelta.Valid && row.CompletedDelta.Int64 > 0 {
			if r.start.IsZero() || row.RunStartedAt.Before(r.start) {
				r.start = row.RunStartedAt
			}
			hour := clockHour(row.RunStartedAt, loc)
			perHour[hour] += row.CompletedDelta.Int64
			perMissionHour[row.MissionGUID][hour] += row.CompletedDelta.Int64
		}
		current[row.MissionGUID] = row.CompletedCount
		if n := len(runs); n == 0 || !runs[n-1].Equal(row.RunStartedAt) {
			runs = append(runs, row.RunStartedAt)
			totals = append(totals, 0)
		}
		var sum int64
		for _, c := range current {
			sum += c
		}
		totals[len(totals)-1] = sum
	}
	if r.start.IsZero() {
		r.start = rows[0].RunStartedAt
	}
	for hour, n := range perHour {
		if n > r.peakCount || (n == r.peakCount && hour.Before(r.peakStart)) {
			r.peakStart, r.peakCount = hour, n
		}
	}
	for _, m := range byGUID {
		r.total += m.completed
		r.missions = append(r.missions, *m)
	}
	sort.Slice(r.missions, func(i, j int) bool { return r.missions[i].order < r.missions[j].order })
	for guid, byHour := range perMissionHour {
		m := *byGUID[guid]
		for hour, n := range byHour {
			b := r.bestHour
			if n > b.count || (n == b.count && (m.order < b.mission.order || (m.order == b.mission.order && hour.Before(b.start)))) {
				r.bestHour = missionHour{mission: m, start: hour, count: n}
			}
		}
	}
	for i, total := range totals {
		if r.total > 0 && total*2 >= r.total {
			r.halfwayAt = runs[i]
			break
		}
	}
	r.weather = summariseWeather(hours, r.start.Truncate(time.Hour), r.end)
	return r, nil
}

func clockHour(t time.Time, loc *time.Location) time.Time {
	local := t.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, loc)
}

func summariseWeather(hours []weather.Hour, from, to time.Time) *finalWeather {
	var w *finalWeather
	for _, h := range hours {
		if h.Start.Before(from) || h.Start.After(to) {
			continue
		}
		if w == nil {
			w = &finalWeather{minTemp: h.Temperature, maxTemp: h.Temperature, hottest: h}
		}
		w.hours++
		w.minTemp = min(w.minTemp, h.Temperature)
		w.maxTemp = max(w.maxTemp, h.Temperature)
		if h.Temperature > w.hottest.Temperature {
			w.hottest = h
		}
		if h.PrecipitationAmount >= rainyHourMM {
			w.rainyHours++
		}
		if h.PrecipitationAmount > w.wettest.PrecipitationAmount {
			w.wettest = h
		}
		w.rainMM += h.PrecipitationAmount
	}
	return w
}

// extremes returns every mission sharing the best value of key among those accepted by include.
func extremes(missions []finalMission, include func(finalMission) bool, key func(finalMission) float64, highest bool) []finalMission {
	var out []finalMission
	for _, m := range missions {
		if !include(m) {
			continue
		}
		switch {
		case len(out) == 0:
			out = []finalMission{m}
		case key(m) == key(out[0]):
			out = append(out, m)
		case (key(m) > key(out[0])) == highest:
			out = []finalMission{m}
		}
	}
	return out
}

func all(finalMission) bool     { return true }
func rated(m finalMission) bool { return m.rated }
func timed(m finalMission) bool { return m.avgDurationMs > 0 }

// lang selects the language of a rendered summary.
type lang int

const (
	english lang = iota
	chinese
)

var chineseWeekdays = [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// code is the language code charts.R and the logs use.
func (l lang) code() string { return l.pick("en", "zh") }

func (l lang) pick(en, zh string) string {
	if l == chinese {
		return zh
	}
	return en
}

func (l lang) day(t time.Time) string {
	if l == chinese {
		return chineseWeekdays[t.Weekday()]
	}
	return t.Format("Mon")
}

func (l lang) dateTime(t time.Time) string {
	if l == chinese {
		return fmt.Sprintf("%d月%d日 %s %s", t.Month(), t.Day(), l.day(t), t.Format("15:04"))
	}
	return t.Format("Mon 02 Jan 15:04")
}

func (l lang) dayTime(t time.Time) string { return l.day(t) + " " + t.Format("15:04") }

func (l lang) hourRange(start time.Time) string {
	return fmt.Sprintf("%s %s–%s", l.day(start), start.Format("15:04"), start.Add(time.Hour).Format("15:04"))
}

func (l lang) duration(ms int64) string {
	minutes := int((time.Duration(ms)*time.Millisecond + 30*time.Second).Minutes())
	if minutes >= 60 {
		return fmt.Sprintf(l.pick("%d h %02d min", "%d 小时 %02d 分钟"), minutes/60, minutes%60)
	}
	return fmt.Sprintf(l.pick("%d min", "%d 分钟"), minutes)
}

func missionNames(ms []finalMission) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = fmt.Sprintf("#%02d %s", m.order, m.name)
	}
	return strings.Join(parts, ", ")
}

func (r finalReport) title(l lang) string {
	title := "🏁 " + l.pick(r.name.EN+" · final summary", r.name.ZH+" · 最终总结")
	if r.preview {
		title += l.pick(" (preview)", "（预览）")
	}
	return title
}

func (r finalReport) totalLine(l lang) (count, rest string) {
	return fmt.Sprint(r.total), fmt.Sprintf(l.pick(" unique completions across %d missions", " 次独立完成，共 %d 个任务"), len(r.missions))
}

func (r finalReport) spanLine(l lang) string {
	return fmt.Sprintf(l.pick("From %s to %s (%.0f h)", "%s 至 %s（%.0f 小时）"),
		l.dateTime(r.start.In(r.loc)), l.dateTime(r.end.In(r.loc)), r.end.Sub(r.start).Hours())
}

func (r finalReport) weatherLine(l lang) string {
	w := r.weather
	return fmt.Sprintf(l.pick("🌦 Weather: %.0f–%.0f°C · rain in %d of %d hours, %.1f mm in total",
		"🌦 天气：%.0f–%.0f°C · %d/%d 小时有雨，累计 %.1f 毫米"),
		w.minTemp, w.maxTemp, w.rainyHours, w.hours, w.rainMM)
}

// highlights lists the notable facts of the event, one line each.
func (r finalReport) highlights(l lang) []string {
	var lines []string
	add := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	completed := func(m finalMission) float64 { return float64(m.completed) }
	rating := func(m finalMission) float64 { return m.rating }
	duration := func(m finalMission) float64 { return float64(m.avgDurationMs) }

	if ms := extremes(r.missions, all, completed, true); len(ms) > 0 {
		add(l.pick("🏆 Most completed: %s — %d", "🏆 完成最多：%s — %d 次"), missionNames(ms), ms[0].completed)
	}
	if ms := extremes(r.missions, all, completed, false); len(ms) > 0 {
		add(l.pick("🌱 Fewest completions: %s — %d", "🌱 完成最少：%s — %d 次"), missionNames(ms), ms[0].completed)
	}
	if ms := extremes(r.missions, rated, rating, true); len(ms) > 0 {
		add(l.pick("⭐ Highest rated: %s — %.1f%%", "⭐ 评分最高：%s — %.1f%%"), missionNames(ms), ms[0].rating)
	}
	if ms := extremes(r.missions, rated, rating, false); len(ms) > 0 {
		add(l.pick("💔 Lowest rated: %s — %.1f%%", "💔 评分最低：%s — %.1f%%"), missionNames(ms), ms[0].rating)
	}
	if ms := extremes(r.missions, timed, duration, false); len(ms) > 0 {
		add(l.pick("⚡ Quickest on average: %s — %s", "⚡ 平均用时最短：%s — %s"), missionNames(ms), l.duration(ms[0].avgDurationMs))
	}
	if ms := extremes(r.missions, timed, duration, true); len(ms) > 0 {
		add(l.pick("🐢 Longest on average: %s — %s", "🐢 平均用时最长：%s — %s"), missionNames(ms), l.duration(ms[0].avgDurationMs))
	}
	if r.peakCount > 0 {
		add(l.pick("🔥 Busiest hour: %s — %d completions", "🔥 最热闹的一小时：%s — %d 次完成"), l.hourRange(r.peakStart), r.peakCount)
	}
	if b := r.bestHour; b.count > 0 {
		add(l.pick("🚀 Biggest hour for one mission: %s, %s — %d", "🚀 单任务单小时最多：%s，%s — %d 次"),
			missionNames([]finalMission{b.mission}), l.hourRange(b.start), b.count)
	}
	if !r.halfwayAt.IsZero() {
		add(l.pick("⏳ Halfway: half of all completions were done by %s", "⏳ 过半：%s 时完成数已达总数一半"),
			l.dayTime(r.halfwayAt.In(r.loc)))
	}
	if w := r.weather; w != nil {
		when := l.dayTime(w.hottest.Start.In(r.loc))
		// The word order differs, so the two languages cannot share one format string.
		var line string
		if l == chinese {
			line = fmt.Sprintf("🌡 最高温：%s %.0f°C", when, w.hottest.Temperature)
		} else {
			line = fmt.Sprintf("🌡 Hottest: %.0f°C at %s", w.hottest.Temperature, when)
		}
		if w.wettest.PrecipitationAmount > 0 {
			line += fmt.Sprintf(l.pick(" · 🌧 Wettest hour: %s, %.1f mm", " · 🌧 雨最大的一小时：%s，%.1f 毫米"),
				l.hourRange(clockHour(w.wettest.Start, r.loc)), w.wettest.PrecipitationAmount)
		}
		add("%s", line)
	}
	return lines
}

func (r finalReport) footer(l lang) string {
	text := l.pick("Δ = rating change since the mission's first rating. Source: Ingress Intel, sampled every 5 min.",
		"Δ = 与该任务首次评分相比的评分变化。数据来源：Ingress Intel，每 5 分钟采样。")
	if r.weather != nil {
		text += " " + l.pick(weatherAttribution, "天气：Apple Weather (weatherkit.apple.com/legal-attribution.html)")
	}
	return text
}

func ratingDelta(m finalMission) string {
	if !m.rated {
		return ""
	}
	return fmt.Sprintf("%+.1f", m.rating-m.firstRating)
}

func (r finalReport) plainText(l lang) string {
	var b strings.Builder
	count, rest := r.totalLine(l)
	fmt.Fprintf(&b, "%s\n%s%s\n%s\n", r.title(l), count, rest, r.spanLine(l))
	if r.weather != nil {
		fmt.Fprintf(&b, "%s\n", r.weatherLine(l))
	}
	fmt.Fprintf(&b, "\n%s\n", l.pick("Highlights", "亮点"))
	for _, line := range r.highlights(l) {
		fmt.Fprintf(&b, "%s\n", line)
	}
	b.WriteByte('\n')
	for _, m := range r.missions {
		fmt.Fprintf(&b, "#%02d %s: %d · %.2f%%", m.order, m.name, m.completed, m.rating)
		if d := ratingDelta(m); d != "" {
			fmt.Fprintf(&b, " (Δ %s)", d)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\n" + r.footer(l))
	return b.String()
}

func (r finalReport) richBlocks(l lang, charts []string) []telegram.RichBlock {
	header := func(text, align string) telegram.TableCell {
		return telegram.TableCell{Text: telegram.Text(text), Header: true, Align: align}
	}
	cell := func(text, align string) telegram.TableCell {
		return telegram.TableCell{Text: telegram.Text(text), Align: align}
	}
	rows := [][]telegram.TableCell{{
		header("#", "right"), header(l.pick("Mission", "任务"), "left"), header(l.pick("Done", "完成"), "right"),
		header(l.pick("Rating", "评分"), "right"), header("Δ", "right"),
	}}
	for _, m := range r.missions {
		rows = append(rows, []telegram.TableCell{
			cell(fmt.Sprintf("%02d", m.order), "right"),
			cell(m.name, "left"),
			cell(fmt.Sprint(m.completed), "right"),
			cell(fmt.Sprintf("%.1f%%", m.rating), "right"),
			cell(ratingDelta(m), "right"),
		})
	}
	count, rest := r.totalLine(l)
	blocks := []telegram.RichBlock{
		telegram.Heading{Text: telegram.Text(r.title(l)), Size: 2},
		telegram.Paragraph{Text: telegram.Join(telegram.Bold(telegram.Text(count)), telegram.Text(rest))},
		telegram.Paragraph{Text: telegram.Text(r.spanLine(l))},
	}
	if r.weather != nil {
		blocks = append(blocks, telegram.Paragraph{Text: telegram.Text(r.weatherLine(l))})
	}
	blocks = append(blocks, telegram.Heading{Text: telegram.Text(l.pick("Highlights", "亮点")), Size: 3})
	for _, line := range r.highlights(l) {
		blocks = append(blocks, telegram.Paragraph{Text: telegram.Text(line)})
	}
	blocks = append(blocks, telegram.Table{Rows: rows, Striped: true, Compact: true})
	if len(charts) > 0 {
		items := make([]telegram.Media, 0, len(charts))
		for _, path := range charts {
			items = append(items, telegram.Media{Path: path, Caption: l.chartCaption(path), Animation: isAnimation(path)})
		}
		blocks = append(blocks, telegram.Slideshow{Items: items})
	}
	return append(blocks, telegram.Footer{Text: telegram.Text(r.footer(l))})
}

// runFinal sends the end-of-event summary. It is run by scripts/stop.sh when collection ends;
// -preview sends it to the private chat only, marked as a preview.
func runFinal(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("final", flag.ContinueOnError)
	flags := addEventFlags(fs)
	chartsScript := fs.String("charts", "", "path to charts.R; when set, the summary includes the replay and charts")
	rscript := fs.String("rscript", "Rscript", "Rscript executable used to render charts")
	preview := fs.Bool("preview", false, "send to the private chat only, marked as a preview")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ev, list, err := loadEventMissions(flags)
	if err != nil {
		return err
	}
	chat, channel, err := loadTelegram(*flags.dir)
	if err != nil {
		return err
	}
	db, err := store.Open(ev.Path("mdtrace.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.Rows(ctx)
	if err != nil {
		return err
	}
	hours, err := db.WeatherHours(ctx)
	if err != nil {
		return err
	}
	report, err := buildFinalReport(rows, hours, namesByGUID(list), ev.Location)
	if err != nil {
		return err
	}
	report.name = ev.Name
	report.preview = *preview

	// Each language gets its own chart set, with all chart text in that language. A failed set only
	// drops the charts from that language's message.
	languages := []lang{english, chinese}
	var errs []error
	charts := make(map[lang][]string, len(languages))
	if *chartsScript != "" {
		path, err := exec.LookPath(*rscript)
		if err != nil {
			return err
		}
		for _, l := range languages {
			r := &rCharts{rscript: path, script: *chartsScript, eventDir: ev.Dir, rows: db, weather: db,
				loc: ev.Location, timeout: finalChartTimeout, final: true, lang: l}
			paths, cleanup, err := r.Render(ctx)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s charts: %w", l.code(), err))
				continue
			}
			defer cleanup()
			charts[l] = paths
			if err := copyFiles(paths, finalChartDir(ev.Dir, l)); err != nil {
				errs = append(errs, fmt.Errorf("save %s final charts: %w", l.code(), err))
			}
		}
	}

	// One complete message per language, so either can be forwarded on its own.
	for _, l := range languages {
		rich, plain := report.richBlocks(l, charts[l]), report.plainText(l)
		if _, err := deliver(ctx, chat, rich, plain); err != nil {
			errs = append(errs, err)
		}
		if channel != nil && !*preview {
			if _, err := deliver(ctx, channel, rich, plain); err != nil {
				errs = append(errs, fmt.Errorf("channel: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

// finalChartDir is where a language's final charts are kept: the event directory's final/, and
// final/zh for Chinese.
func finalChartDir(dir string, l lang) string {
	if l == chinese {
		return filepath.Join(dir, "final", "zh")
	}
	return filepath.Join(dir, "final")
}

// copyFiles copies each file into dir (created if needed), replacing files of the same name.
func copyFiles(paths []string, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, src := range paths {
		if err := copyFile(src, filepath.Join(dir, filepath.Base(src))); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFile(dst, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}
