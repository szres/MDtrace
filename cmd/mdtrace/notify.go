package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"mdtrace/internal/intel"
	"mdtrace/internal/store"
	"mdtrace/internal/telegram"
	"mdtrace/internal/weather"
)

const (
	alertRunFailure = "run_failure"
	alertWeather    = "weather_failure"

	failureRepeat = 2 * time.Hour
	// Updates are sent at most this often and report the change over this window.
	updateEvery = 30 * time.Minute
	// Runs start a few seconds after their scheduled minute, so exact 30-minute comparisons would
	// miss the run scheduled 30 minutes earlier and shift the window by one interval.
	updateSlack        = 2 * time.Minute
	maxErrorTextLength = 300

	weatherRepeat = 12 * time.Hour
	// Conditions older than this are left out of an update rather than shown as current.
	weatherMaxAge        = 90 * time.Minute
	weatherUpcomingHours = 3
	weatherAttribution   = "Weather: Apple Weather (weatherkit.apple.com/legal-attribution.html)"
)

type messageSender interface {
	Send(ctx context.Context, text string) error
	SendRich(ctx context.Context, blocks []telegram.RichBlock) error
}

type chartRenderer interface {
	Render(ctx context.Context) (paths []string, cleanup func(), err error)
}

type alertStore interface {
	ChangesSince(ctx context.Context, runID, baselineRunID int64) ([]store.MissionChange, error)
	BaselineRun(ctx context.Context, runID int64, ago time.Duration) (baselineID int64, startedAt time.Time, ok bool, err error)
	LastUpdateSentAt(ctx context.Context) (time.Time, bool, error)
	MarkUpdateSent(ctx context.Context, runID int64, at time.Time) error
	AlertSentAt(ctx context.Context, key string) (time.Time, bool, error)
	MarkAlertSent(ctx context.Context, key string, at time.Time) error
	ClearAlert(ctx context.Context, key string) error
	LatestWeather(ctx context.Context, upcoming int) (weather.Report, bool, error)
}

// notifier decides which Telegram messages a collection run produces.
// Problem alerts repeat at most once per interval, so a broken session does not spam every run.
type notifier struct {
	sender  messageSender // private chat: alerts and updates
	channel messageSender // optional public channel: updates only
	charts  chartRenderer // optional; charts are attached to updates
	store   alertStore
	loc     *time.Location
	// eventName titles the updates, e.g. "MD 2026 Zhuhai".
	eventName string
	now       func() time.Time
	names     map[string]string // mission GUID -> short display name
}

func (n *notifier) afterRun(ctx context.Context, res collectResult, runErr error) error {
	var errs []error
	if runErr != nil {
		errs = append(errs, n.alert(ctx, alertRunFailure, failureRepeat, failureMessage(runErr)))
	} else {
		errs = append(errs, n.resolve(ctx, alertRunFailure, "✅ MDTrace recovered: collection is working again."))
	}
	if res.OK > 0 {
		errs = append(errs, n.sendChanges(ctx, res.RunID))
	}
	return errors.Join(errs...)
}

// weatherStatus alerts when WeatherKit fails and reports recovery; collection is unaffected.
func (n *notifier) weatherStatus(ctx context.Context, fetchErr error) error {
	if fetchErr == nil {
		return n.resolve(ctx, alertWeather, "✅ MDTrace: weather (WeatherKit) is working again.")
	}
	text := fetchErr.Error()
	if len([]rune(text)) > maxErrorTextLength {
		text = string([]rune(text)[:maxErrorTextLength]) + "…"
	}
	msg := "⚠️ MDTrace: weather (WeatherKit) failed; updates go out without weather.\n\n" + text
	if errors.Is(fetchErr, weather.ErrAuth) {
		msg = "⚠️ MDTrace: WeatherKit rejected the key. Check the Team ID, Key ID and Services ID in ~/.config/mdtrace/weatherkit.env and that the key has WeatherKit enabled.\n\n" + text
	}
	return n.alert(ctx, alertWeather, weatherRepeat, msg)
}

// attachWeather adds the latest conditions to the report when they are recent enough.
func (n *notifier) attachWeather(ctx context.Context, report *updateReport) error {
	latest, ok, err := n.store.LatestWeather(ctx, weatherUpcomingHours)
	if err != nil || !ok {
		return err
	}
	if age := report.at.Sub(latest.Current.AsOf); age > weatherMaxAge || age < -weatherMaxAge {
		return nil
	}
	report.weather = &latest
	return nil
}

func (n *notifier) alert(ctx context.Context, key string, repeat time.Duration, msg string) error {
	sentAt, active, err := n.store.AlertSentAt(ctx, key)
	if err != nil {
		return err
	}
	now := n.now()
	if active && now.Sub(sentAt) < repeat {
		return nil
	}
	if err := n.sender.Send(ctx, msg); err != nil {
		return err
	}
	return n.store.MarkAlertSent(ctx, key, now)
}

func (n *notifier) resolve(ctx context.Context, key, msg string) error {
	_, active, err := n.store.AlertSentAt(ctx, key)
	if err != nil || !active {
		return err
	}
	if err := n.sender.Send(ctx, msg); err != nil {
		return err
	}
	return n.store.ClearAlert(ctx, key)
}

// sendChanges sends at most one update per updateEvery, reporting the change since the run
// about updateEvery before this one.
func (n *notifier) sendChanges(ctx context.Context, runID int64) error {
	now := n.now()
	lastSent, sentBefore, err := n.store.LastUpdateSentAt(ctx)
	if err != nil {
		return err
	}
	if sentBefore && now.Sub(lastSent) < updateEvery-updateSlack {
		return nil
	}
	baseline, baselineAt, ok, err := n.store.BaselineRun(ctx, runID, updateEvery-updateSlack)
	if err != nil || !ok {
		return err
	}
	changes, err := n.store.ChangesSince(ctx, runID, baseline)
	if err != nil {
		return err
	}
	report, changed := n.buildReport(changes, now, baselineAt)
	if !changed {
		return nil
	}
	// A weather lookup problem must not hold back the completion update.
	weatherErr := n.attachWeather(ctx, &report)
	return errors.Join(weatherErr, n.publishUpdate(ctx, runID, report))
}

// previewReport builds the update for runID as sendChanges would, even if nothing changed.
func (n *notifier) previewReport(ctx context.Context, runID int64) (updateReport, error) {
	baseline, baselineAt, ok, err := n.store.BaselineRun(ctx, runID, updateEvery-updateSlack)
	if err != nil {
		return updateReport{}, err
	}
	if !ok {
		return updateReport{}, errors.New("not enough history for a comparison yet")
	}
	changes, err := n.store.ChangesSince(ctx, runID, baseline)
	if err != nil {
		return updateReport{}, err
	}
	report, _ := n.buildReport(changes, n.now(), baselineAt)
	return report, n.attachWeather(ctx, &report)
}

// publishUpdate sends the update to the private chat and the channel. The update counts as sent
// once any destination received it; otherwise both would get a duplicate on the next run.
func (n *notifier) publishUpdate(ctx context.Context, runID int64, report updateReport) error {
	var chartErr error
	var charts []string
	if n.charts != nil {
		paths, cleanup, err := n.charts.Render(ctx)
		if err != nil {
			chartErr = err
		} else {
			defer cleanup()
			charts = paths
		}
	}
	rich := report.richBlocks(charts)
	plain := report.plainText()

	delivered, chatErr := deliver(ctx, n.sender, rich, plain)
	var channelErr error
	if n.channel != nil {
		var ok bool
		if ok, channelErr = deliver(ctx, n.channel, rich, plain); channelErr != nil {
			channelErr = fmt.Errorf("channel: %w", channelErr)
		}
		delivered = delivered || ok
	}
	if !delivered {
		return errors.Join(chartErr, chatErr, channelErr)
	}
	return errors.Join(chartErr, chatErr, channelErr, n.store.MarkUpdateSent(ctx, runID, report.at))
}

// deliver sends the rich message, falling back to plain text if Telegram rejects it.
// The returned error reports the rich failure even when the fallback succeeded.
func deliver(ctx context.Context, to messageSender, rich []telegram.RichBlock, plain string) (bool, error) {
	richErr := to.SendRich(ctx, rich)
	if richErr == nil {
		return true, nil
	}
	if err := to.Send(ctx, plain); err != nil {
		return false, errors.Join(richErr, err)
	}
	return true, fmt.Errorf("sent as plain text: %w", richErr)
}

type reportRow struct {
	order          int
	name           string
	completed      int64
	completedDelta int64
	ratingPercent  float64
	ratingDelta    float64
}

type updateReport struct {
	eventName  string
	at         time.Time
	since      time.Time
	loc        *time.Location
	total      int64
	totalDelta int64
	rows       []reportRow
	weather    *weather.Report // nil when weather is not configured or stale
}

// buildReport summarises the change against the baseline; it reports false when nothing changed.
func (n *notifier) buildReport(changes []store.MissionChange, at, since time.Time) (updateReport, bool) {
	r := updateReport{eventName: n.eventName, at: at, since: since, loc: n.loc}
	anyChanged := false
	for _, c := range changes {
		anyChanged = anyChanged || c.Changed()
		row := reportRow{order: c.Order, name: n.names[c.GUID], completed: c.Completed,
			ratingPercent: float64(c.RatingE6) / 10000}
		if row.name == "" {
			row.name = c.Title
		}
		if c.PrevCompleted.Valid {
			row.completedDelta = c.Completed - c.PrevCompleted.Int64
			row.ratingDelta = float64(c.RatingE6-c.PrevRatingE6.Int64) / 10000
		}
		r.total += c.Completed
		r.totalDelta += row.completedDelta
		r.rows = append(r.rows, row)
	}
	return r, anyChanged
}

func (r updateReport) title() string {
	return "📊 " + r.eventName + " · " + r.at.In(r.loc).Format("01-02 15:04")
}

func (r updateReport) sinceLabel() string {
	return "since " + r.since.In(r.loc).Format("15:04")
}

func (r updateReport) plainText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", r.title())
	fmt.Fprintf(&b, "Total completions: %d (%+d %s)\n", r.total, r.totalDelta, r.sinceLabel())
	if r.weather != nil {
		fmt.Fprintf(&b, "%s\n%s\n", weatherSummary(r.weather.Current), weatherOutlook(r.weather.Hours, r.loc))
	}
	b.WriteByte('\n')
	for _, row := range r.rows {
		fmt.Fprintf(&b, "#%02d %s: %d", row.order, row.name, row.completed)
		if row.completedDelta != 0 {
			fmt.Fprintf(&b, " (%+d)", row.completedDelta)
		}
		fmt.Fprintf(&b, " · %.2f%%", row.ratingPercent)
		if row.ratingDelta != 0 {
			fmt.Fprintf(&b, " (%+.2f)", row.ratingDelta)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func (r updateReport) richBlocks(charts []string) []telegram.RichBlock {
	header := func(text, align string) telegram.TableCell {
		return telegram.TableCell{Text: telegram.Text(text), Header: true, Align: align}
	}
	cell := func(text telegram.RichText, align string) telegram.TableCell {
		return telegram.TableCell{Text: text, Align: align}
	}
	rows := [][]telegram.TableCell{{
		header("#", "right"), header("Mission", "left"), header("Done", "right"),
		header("Δ", "right"), header("Rating", "right"), header("Δ", "right"),
	}}
	for _, row := range r.rows {
		delta := telegram.Text("")
		if row.completedDelta != 0 {
			delta = telegram.Bold(telegram.Text(fmt.Sprintf("%+d", row.completedDelta)))
		}
		ratingDelta := ""
		if row.ratingDelta != 0 {
			ratingDelta = fmt.Sprintf("%+.1f", row.ratingDelta)
		}
		rows = append(rows, []telegram.TableCell{
			cell(telegram.Text(fmt.Sprintf("%02d", row.order)), "right"),
			cell(telegram.Text(row.name), "left"),
			cell(telegram.Text(fmt.Sprint(row.completed)), "right"),
			cell(delta, "right"),
			cell(telegram.Text(fmt.Sprintf("%.1f%%", row.ratingPercent)), "right"),
			cell(telegram.Text(ratingDelta), "right"),
		})
	}

	blocks := []telegram.RichBlock{
		telegram.Heading{Text: telegram.Text(r.title()), Size: 2},
		telegram.Paragraph{Text: telegram.Join(
			telegram.Text("Total completions "),
			telegram.Bold(telegram.Text(fmt.Sprint(r.total))),
			telegram.Text(" · "),
			telegram.Bold(telegram.Text(fmt.Sprintf("%+d", r.totalDelta))),
			telegram.Text(" "+r.sinceLabel()),
		)},
	}
	footer := "Δ = change " + r.sinceLabel() + ". Source: Ingress Intel; each mission refreshes about hourly."
	if r.weather != nil {
		blocks = append(blocks,
			telegram.Paragraph{Text: telegram.Text(weatherSummary(r.weather.Current))},
			telegram.Paragraph{Text: telegram.Text(weatherOutlook(r.weather.Hours, r.loc))})
		footer += " " + weatherAttribution
	}
	blocks = append(blocks, telegram.Table{Rows: rows, Striped: true, Compact: true})
	if len(charts) > 0 {
		items := make([]telegram.Media, 0, len(charts))
		for _, path := range charts {
			items = append(items, telegram.Media{Path: path, Caption: chartCaption(path), Animation: isAnimation(path)})
		}
		blocks = append(blocks, telegram.Slideshow{Items: items})
	}
	return append(blocks, telegram.Footer{Text: telegram.Text(footer)})
}

// isAnimation reports whether a chart file is sent as a Telegram animation rather than a photo.
func isAnimation(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".gif", ".mp4":
		return true
	}
	return false
}

// weatherSummary renders current conditions, e.g. "☁️ 21°C (feels 23°C) · Mostly cloudy · humidity 83% · wind 15 km/h".
func weatherSummary(c weather.Current) string {
	var b strings.Builder
	if icon := weather.Icon(c.Condition); icon != "" {
		b.WriteString(icon + " ")
	}
	fmt.Fprintf(&b, "%.0f°C (feels %.0f°C) · %s · humidity %.0f%% · wind %.0f km/h",
		c.Temperature, c.ApparentTemperature, weather.Describe(c.Condition), c.Humidity*100, c.WindSpeed)
	if c.PrecipitationIntensity > 0 {
		fmt.Fprintf(&b, " · rain %.1f mm/h", c.PrecipitationIntensity)
	}
	return b.String()
}

// weatherOutlook renders the coming hours, e.g. "Next hours: 16:00 22°C ☁️ · 17:00 21°C 🌧️ ☔60%".
func weatherOutlook(hours []weather.Hour, loc *time.Location) string {
	if len(hours) == 0 {
		return "Next hours: no forecast"
	}
	parts := make([]string, 0, len(hours))
	for _, h := range hours {
		part := fmt.Sprintf("%s %.0f°C", h.Start.In(loc).Format("15:04"), h.Temperature)
		if icon := weather.Icon(h.Condition); icon != "" {
			part += " " + icon
		}
		if h.PrecipitationChance >= 0.1 {
			part += fmt.Sprintf(" ☔%.0f%%", h.PrecipitationChance*100)
		}
		parts = append(parts, part)
	}
	return "Next hours: " + strings.Join(parts, " · ")
}

func failureMessage(err error) string {
	text := err.Error()
	if len([]rune(text)) > maxErrorTextLength {
		text = string([]rune(text)[:maxErrorTextLength]) + "…"
	}
	switch {
	case errors.Is(err, intel.ErrAuth):
		return "⚠️ MDTrace: Intel session expired or invalid.\nCopy a fresh Cookie header from a logged-in https://intel.ingress.com tab into the cookie file (default ~/.config/mdtrace/intel.cookie).\n\n" + text
	default:
		return "⚠️ MDTrace collection failed.\n\n" + text
	}
}
