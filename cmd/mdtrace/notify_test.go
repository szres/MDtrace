package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mdtrace/internal/intel"
	"mdtrace/internal/store"
	"mdtrace/internal/telegram"
	"mdtrace/internal/weather"
)

// recordingSender records plain messages as-is and rich messages flattened to text (see flatten),
// so tests can assert on both through messages.
type recordingSender struct {
	messages []string
	rich     [][]telegram.RichBlock
	err      error // fails every send
	richErr  error // fails only rich sends, exercising the plain-text fallback
}

func (r *recordingSender) Send(_ context.Context, text string) error {
	if r.err != nil {
		return r.err
	}
	r.messages = append(r.messages, text)
	return nil
}

func (r *recordingSender) SendRich(_ context.Context, blocks []telegram.RichBlock) error {
	if r.err != nil {
		return r.err
	}
	if r.richErr != nil {
		return r.richErr
	}
	r.rich = append(r.rich, blocks)
	r.messages = append(r.messages, flatten(blocks))
	return nil
}

// flatten renders rich blocks as lines of text; table rows become "cell|cell|...".
func flatten(blocks []telegram.RichBlock) string {
	var lines []string
	for _, b := range blocks {
		switch b := b.(type) {
		case telegram.Heading:
			lines = append(lines, richString(b.Text))
		case telegram.Paragraph:
			lines = append(lines, richString(b.Text))
		case telegram.Footer:
			lines = append(lines, richString(b.Text))
		case telegram.Table:
			for _, row := range b.Rows {
				cells := make([]string, len(row))
				for i, c := range row {
					cells[i] = richString(c.Text)
				}
				lines = append(lines, strings.Join(cells, "|"))
			}
		case telegram.Slideshow:
			var names []string
			for _, m := range b.Items {
				name := filepath.Base(m.Path) + "=" + m.Caption
				if m.Animation {
					name += " (animation)"
				}
				names = append(names, name)
			}
			lines = append(lines, "[slideshow "+strings.Join(names, ", ")+"]")
		default:
			lines = append(lines, fmt.Sprintf("[%T]", b))
		}
	}
	return strings.Join(lines, "\n")
}

func richString(t telegram.RichText) string {
	raw, _ := json.Marshal(t)
	var v any
	json.Unmarshal(raw, &v)
	var walk func(any) string
	walk = func(v any) string {
		switch v := v.(type) {
		case string:
			return v
		case []any:
			var b strings.Builder
			for _, part := range v {
				b.WriteString(walk(part))
			}
			return b.String()
		case map[string]any:
			return walk(v["text"])
		}
		return ""
	}
	return walk(v)
}

type notifyFixture struct {
	n      *notifier
	sender *recordingSender
	db     *store.Store
	clock  time.Time
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &notifyFixture{sender: &recordingSender{}, db: db, clock: time.Date(2026, 11, 7, 2, 0, 0, 0, time.UTC)}
	f.n = &notifier{
		sender: f.sender, store: db, loc: time.FixedZone("CST", 8*3600), eventName: "MD 2026 Zhuhai",
		now:   func() time.Time { return f.clock },
		names: map[string]string{"a.1c": "海滨泳场"},
	}
	return f
}

func (f *notifyFixture) saveRun(t *testing.T, counts map[string]int64, rating int64) collectResult {
	t.Helper()
	ctx := context.Background()
	runID, err := f.db.StartRun(ctx, len(counts), f.clock)
	if err != nil {
		t.Fatal(err)
	}
	for i, guid := range []string{"a.1c", "b.1c"} {
		d := intel.MissionDetail{GUID: guid, Title: "MD " + guid, RatingE6: rating, CompletedCount: counts[guid]}
		if err := f.db.SaveSnapshot(ctx, runID, i+1, d); err != nil {
			t.Fatal(err)
		}
	}
	return collectResult{RunID: runID, OK: 2}
}

func TestUpdateSentOnlyWhenNumbersChange(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()

	for _, res := range []collectResult{
		f.saveRun(t, map[string]int64{"a.1c": 0, "b.1c": 0}, 0),
		f.saveRun(t, map[string]int64{"a.1c": 0, "b.1c": 0}, 0),
	} {
		if err := f.n.afterRun(ctx, res, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.sender.messages) != 0 {
		t.Fatalf("unchanged runs must stay quiet, got %q", f.sender.messages)
	}

	res := f.saveRun(t, map[string]int64{"a.1c": 12, "b.1c": 0}, 985000)
	if err := f.n.afterRun(ctx, res, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.sender.messages) != 1 {
		t.Fatalf("got %d messages", len(f.sender.messages))
	}
	msg := f.sender.messages[0]
	for _, want := range []string{"📊 MD 2026 Zhuhai · 11-07 10:00", "Total completions 12 · +12 since 10:00",
		"#|Mission|Done|Δ|Rating|Δ", "01|海滨泳场|12|+12|98.5%|+98.5", "02|MD b.1c|0||98.5%|+98.5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestFailureAlertThrottledAndRecovered(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	authErr := fmt.Errorf("%w: login page", intel.ErrAuth)

	for i := 0; i < 4; i++ { // four 15-minute runs within the 2h repeat window
		if err := f.n.afterRun(ctx, collectResult{}, authErr); err != nil {
			t.Fatal(err)
		}
		f.clock = f.clock.Add(15 * time.Minute)
	}
	if len(f.sender.messages) != 1 || !strings.Contains(f.sender.messages[0], "fresh Cookie header") {
		t.Fatalf("want one auth alert, got %q", f.sender.messages)
	}

	f.clock = f.clock.Add(2 * time.Hour)
	if err := f.n.afterRun(ctx, collectResult{}, authErr); err != nil {
		t.Fatal(err)
	}
	if len(f.sender.messages) != 2 {
		t.Fatalf("want a reminder after 2h, got %d messages", len(f.sender.messages))
	}

	if err := f.n.afterRun(ctx, f.saveRun(t, map[string]int64{}, 0), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.sender.messages) != 3 || !strings.Contains(f.sender.messages[2], "recovered") {
		t.Fatalf("want recovery message, got %q", f.sender.messages)
	}
	if err := f.n.afterRun(ctx, f.saveRun(t, map[string]int64{}, 0), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.sender.messages) != 3 {
		t.Fatal("recovery must be announced only once")
	}
}

func TestFailedSendIsRetriedNextRun(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	f.sender.err = errors.New("network down")
	if err := f.n.afterRun(ctx, collectResult{}, errors.New("boom")); err == nil {
		t.Fatal("expected send error")
	}
	f.sender.err = nil
	if err := f.n.afterRun(ctx, collectResult{}, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if len(f.sender.messages) != 1 {
		t.Fatalf("alert should be sent once the network is back, got %q", f.sender.messages)
	}
}

func TestFailureMessageKinds(t *testing.T) {
	if !strings.Contains(failureMessage(intel.ErrAuth), "fresh Cookie header") {
		t.Error("auth error should ask for a fresh cookie")
	}
	long := failureMessage(errors.New(strings.Repeat("x", 1000)))
	if len(long) > 400 {
		t.Errorf("error text not truncated: %d bytes", len(long))
	}
}

func TestUpdatesCappedAtOnePerInterval(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	run := func(a int64) {
		t.Helper()
		if err := f.n.afterRun(ctx, f.saveRun(t, map[string]int64{"a.1c": a, "b.1c": 0}, 990000), nil); err != nil {
			t.Fatal(err)
		}
		f.clock = f.clock.Add(5*time.Minute + 3*time.Second) // scheduled 5-min runs start a little late
	}

	run(0)  // 10:00 first snapshot, nothing to compare
	run(10) // 10:05 change -> first update (+10)
	for a := int64(11); a <= 16; a++ {
		run(a) // 10:10 .. 10:35, changes but inside the 30-min window except the last
	}
	if len(f.sender.messages) != 2 {
		t.Fatalf("want 2 updates (10:05 and ~10:35), got %d: %q", len(f.sender.messages), f.sender.messages)
	}
	if !strings.Contains(f.sender.messages[0], "Total completions 10 · +10 since 10:00\n") {
		t.Errorf("first update: %q", f.sender.messages[0])
	}
	// The second update spans every run since the first message: 16 - 10 = +6.
	if !strings.Contains(f.sender.messages[1], "Total completions 16 · +6 since 10:05") ||
		!strings.Contains(f.sender.messages[1], "01|海滨泳场|16|+6|") {
		t.Errorf("second update: %q", f.sender.messages[1])
	}

	// Quiet runs after the window do not send or move the baseline.
	f.clock = f.clock.Add(time.Hour)
	run(16)
	if len(f.sender.messages) != 2 {
		t.Fatalf("unchanged run must not send, got %q", f.sender.messages[2:])
	}
}

func TestChannelGetsUpdatesButNotAlerts(t *testing.T) {
	f := newNotifyFixture(t)
	channel := &recordingSender{}
	f.n.channel = channel
	ctx := context.Background()

	if err := f.n.afterRun(ctx, collectResult{}, fmt.Errorf("%w: expired", intel.ErrAuth)); err != nil {
		t.Fatal(err)
	}
	f.n.afterRun(ctx, f.saveRun(t, map[string]int64{"a.1c": 1}, 0), nil) // recovery, first snapshot
	if err := f.n.afterRun(ctx, f.saveRun(t, map[string]int64{"a.1c": 5}, 0), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.sender.messages) != 3 {
		t.Fatalf("private chat wants alert, recovery, update; got %q", f.sender.messages)
	}
	if len(channel.messages) != 1 || !strings.Contains(channel.messages[0], "01|海滨泳场|5|+4|") {
		t.Fatalf("channel should get only the update, got %q", channel.messages)
	}
}

func TestUpdateMarkedSentWhenOnlyChannelSucceeds(t *testing.T) {
	f := newNotifyFixture(t)
	channel := &recordingSender{}
	f.n.channel = channel
	ctx := context.Background()

	f.n.afterRun(ctx, f.saveRun(t, map[string]int64{"a.1c": 1}, 0), nil)
	f.sender.err = errors.New("private chat down")
	res := f.saveRun(t, map[string]int64{"a.1c": 3}, 0)
	if err := f.n.afterRun(ctx, res, nil); err == nil || !strings.Contains(err.Error(), "private chat down") {
		t.Fatalf("expected the private-chat error to be reported, got %v", err)
	}
	if sent, ok, _ := f.db.LastUpdateSentAt(ctx); !ok || !sent.Equal(f.clock) {
		t.Fatal("update delivered to the channel must be marked sent to avoid a duplicate")
	}
	markedAt := f.clock

	channel.err = errors.New("channel down")
	f.clock = f.clock.Add(time.Hour)
	res = f.saveRun(t, map[string]int64{"a.1c": 9}, 0)
	if err := f.n.afterRun(ctx, res, nil); err == nil {
		t.Fatal("expected error when nothing was delivered")
	}
	if sent, _, _ := f.db.LastUpdateSentAt(ctx); !sent.Equal(markedAt) {
		t.Fatal("undelivered update must not be marked sent")
	}
}

func TestUpdateComparesWithLast30Minutes(t *testing.T) {
	f := newNotifyFixture(t) // clock starts 10:00 CST
	ctx := context.Background()
	start := f.clock
	run := func(i int, a int64) {
		t.Helper()
		f.clock = start.Add(time.Duration(i)*5*time.Minute + time.Duration(i%4)*time.Second)
		if err := f.n.afterRun(ctx, f.saveRun(t, map[string]int64{"a.1c": a}, 0), nil); err != nil {
			t.Fatal(err)
		}
	}

	run(0, 0)
	run(1, 5) // 10:05 first update
	for i := 2; i < 24; i++ {
		run(i, 5) // quiet until 11:55
	}
	run(24, 9) // 12:00: last update was 10:05, but the comparison covers only the last 30 minutes
	if len(f.sender.messages) != 2 {
		t.Fatalf("got %d messages: %q", len(f.sender.messages), f.sender.messages)
	}
	if msg := f.sender.messages[1]; !strings.Contains(msg, "Total completions 9 · +4 since 11:30") {
		t.Errorf("want a 30-minute comparison, got:\n%s", msg)
	}
}

type fakeCharts struct {
	paths   []string
	err     error
	cleaned int
}

func (c *fakeCharts) Render(context.Context) ([]string, func(), error) {
	if c.err != nil {
		return nil, nil, c.err
	}
	return c.paths, func() { c.cleaned++ }, nil
}

func TestUpdateCarriesChartSlideshow(t *testing.T) {
	f := newNotifyFixture(t)
	channel := &recordingSender{}
	charts := &fakeCharts{paths: []string{"/tmp/x/total.png", "/tmp/x/map_anim.mp4", "/tmp/x/pace.png"}}
	f.n.channel, f.n.charts = channel, charts
	ctx := context.Background()

	f.n.afterRun(ctx, f.saveRun(t, map[string]int64{"a.1c": 1}, 0), nil)
	if err := f.n.afterRun(ctx, f.saveRun(t, map[string]int64{"a.1c": 4}, 0), nil); err != nil {
		t.Fatal(err)
	}
	want := "[slideshow total.png=Total completions and hourly pace, map_anim.mp4=Last 2 hours, read by read (animation), pace.png=Completions per hour by mission]"
	for name, s := range map[string]*recordingSender{"chat": f.sender, "channel": channel} {
		if len(s.messages) != 1 || !strings.Contains(s.messages[0], want) {
			t.Errorf("%s: want one rich update with charts, got %q", name, s.messages)
		}
	}
	if charts.cleaned != 1 {
		t.Errorf("charts rendered once per update and cleaned up, cleaned=%d", charts.cleaned)
	}
}

func TestChartFailureStillSendsUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	f.n.charts = &fakeCharts{err: errors.New("Rscript not found")}
	ctx := context.Background()

	f.n.afterRun(ctx, f.saveRun(t, map[string]int64{"a.1c": 1}, 0), nil)
	err := f.n.afterRun(ctx, f.saveRun(t, map[string]int64{"a.1c": 4}, 0), nil)
	if err == nil || !strings.Contains(err.Error(), "Rscript") {
		t.Fatalf("chart error should be reported, got %v", err)
	}
	if len(f.sender.messages) != 1 || strings.Contains(f.sender.messages[0], "slideshow") {
		t.Fatalf("update without charts expected, got %q", f.sender.messages)
	}
	if _, ok, _ := f.db.LastUpdateSentAt(ctx); !ok {
		t.Fatal("update was delivered and must be marked sent")
	}
}

func TestRichFailureFallsBackToPlainText(t *testing.T) {
	f := newNotifyFixture(t)
	f.sender.richErr = errors.New("Bad Request: rich messages unsupported")
	ctx := context.Background()

	f.n.afterRun(ctx, f.saveRun(t, map[string]int64{"a.1c": 1}, 0), nil)
	err := f.n.afterRun(ctx, f.saveRun(t, map[string]int64{"a.1c": 4}, 0), nil)
	if err == nil || !strings.Contains(err.Error(), "sent as plain text") {
		t.Fatalf("fallback should be reported, got %v", err)
	}
	if len(f.sender.messages) != 1 || !strings.Contains(f.sender.messages[0], "#01 海滨泳场: 4 (+3)") {
		t.Fatalf("plain fallback expected, got %q", f.sender.messages)
	}
	if _, ok, _ := f.db.LastUpdateSentAt(ctx); !ok {
		t.Fatal("fallback delivery must mark the update sent")
	}
}

func (f *notifyFixture) saveWeather(t *testing.T, runID int64, asOf time.Time) {
	t.Helper()
	report := weather.Report{
		Current: weather.Current{AsOf: asOf, Condition: "MostlyCloudy", Temperature: 21.4, ApparentTemperature: 23.2,
			Humidity: 0.83, PrecipitationIntensity: 0.4, WindSpeed: 14.6},
		Hours: []weather.Hour{
			{Start: asOf.Truncate(time.Hour).Add(time.Hour), Condition: "Rain", Temperature: 21, PrecipitationChance: 0.6},
			{Start: asOf.Truncate(time.Hour).Add(2 * time.Hour), Condition: "Cloudy", Temperature: 20, PrecipitationChance: 0.05},
		},
	}
	if err := f.db.SaveWeather(context.Background(), runID, report); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateIncludesCurrentWeather(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	f.n.afterRun(ctx, f.saveRun(t, map[string]int64{"a.1c": 1}, 0), nil)
	f.clock = f.clock.Add(30 * time.Minute)
	res := f.saveRun(t, map[string]int64{"a.1c": 4}, 0)
	f.saveWeather(t, res.RunID, f.clock.Add(-5*time.Minute))
	if err := f.n.afterRun(ctx, res, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.sender.messages) != 1 {
		t.Fatalf("want one update, got %q", f.sender.messages)
	}
	msg := f.sender.messages[0]
	for _, want := range []string{
		"☁️ 21°C (feels 23°C) · Mostly cloudy · humidity 83% · wind 15 km/h · rain 0.4 mm/h",
		"Next hours: 11:00 21°C 🌧️ ☔60% · 12:00 20°C ☁️",
		"Weather: Apple Weather",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("update missing %q:\n%s", want, msg)
		}
	}

	plain := f.sender.messages[0]
	f.sender.richErr = errors.New("rich rejected")
	f.clock = f.clock.Add(30 * time.Minute)
	res = f.saveRun(t, map[string]int64{"a.1c": 6}, 0)
	f.saveWeather(t, res.RunID, f.clock)
	f.n.afterRun(ctx, res, nil)
	if plain = f.sender.messages[len(f.sender.messages)-1]; !strings.Contains(plain, "Mostly cloudy") || !strings.Contains(plain, "Next hours:") {
		t.Errorf("plain-text fallback should carry the weather too:\n%s", plain)
	}
}

func TestStaleWeatherIsLeftOut(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	first := f.saveRun(t, map[string]int64{"a.1c": 1}, 0)
	f.saveWeather(t, first.RunID, f.clock)
	f.n.afterRun(ctx, first, nil)
	f.clock = f.clock.Add(3 * time.Hour)
	if err := f.n.afterRun(ctx, f.saveRun(t, map[string]int64{"a.1c": 4}, 0), nil); err != nil {
		t.Fatal(err)
	}
	if msg := f.sender.messages[0]; strings.Contains(msg, "°C") || strings.Contains(msg, "Apple Weather") {
		t.Errorf("3-hour-old weather must not be shown as current:\n%s", msg)
	}
}

func TestWeatherFailureAlertsAndRecovers(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	authErr := fmt.Errorf("%w (HTTP 401)", weather.ErrAuth)
	f.n.weatherStatus(ctx, authErr)
	f.n.weatherStatus(ctx, authErr) // repeats are throttled
	if len(f.sender.messages) != 1 || !strings.Contains(f.sender.messages[0], "WeatherKit rejected the key") {
		t.Fatalf("want one auth alert, got %q", f.sender.messages)
	}
	f.n.weatherStatus(ctx, nil)
	f.n.weatherStatus(ctx, nil)
	if len(f.sender.messages) != 2 || !strings.Contains(f.sender.messages[1], "working again") {
		t.Fatalf("want one recovery message, got %q", f.sender.messages)
	}
	f.n.weatherStatus(ctx, errors.New("weatherkit: HTTP 500: boom"))
	if len(f.sender.messages) != 3 || !strings.Contains(f.sender.messages[2], "updates go out without weather") {
		t.Fatalf("want a generic weather alert, got %q", f.sender.messages)
	}
}
