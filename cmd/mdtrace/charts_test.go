package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mdtrace/internal/store"
	"mdtrace/internal/weather"
)

type staticRows struct {
	rows []store.Row
	err  error
}

func (s staticRows) Rows(context.Context) ([]store.Row, error) { return s.rows, s.err }

// fakeScript stands in for charts.R: it is run as `sh script <csv> <outdir>`.
func fakeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "charts.sh")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestCharts(t *testing.T, script string) *rCharts {
	rows := []store.Row{{RunID: 1, MissionOrder: 1, MissionGUID: "a.1c", Title: "海滨泳场", CompletedCount: 3,
		RunStartedAt: time.Unix(0, 0), FetchedAt: time.Unix(0, 0)}}
	return &rCharts{rscript: "/bin/sh", script: script, eventDir: "/events/test", rows: staticRows{rows: rows},
		loc: time.UTC, timeout: 10 * time.Second}
}

func TestRenderChartsInAlbumOrder(t *testing.T) {
	script := fakeScript(t, `set -e
grep -q 海滨泳场 "$1"
mkdir -p "$2"
for f in ratings completions total; do echo png > "$2/$f.png"; done
echo mp4 > "$2/map_anim.mp4"
echo gif > "$2/map_anim.gif"
`)
	paths, cleanup, err := newTestCharts(t, script).Render(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	if got := strings.Join(names, ","); got != "total.png,map_anim.mp4,completions.png,ratings.png" {
		t.Fatalf("album order = %s (missing pace.png must be skipped)", got)
	}
	dir := filepath.Dir(filepath.Dir(paths[0]))
	cleanup()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup should remove %s", dir)
	}
}

func TestRenderChartsPassesEventDir(t *testing.T) {
	script := fakeScript(t, `set -e
test "$3" = --event
test "$4" = /events/md2027-x
test "$#" = 4
mkdir -p "$2"
echo png > "$2/map.png"; echo png > "$2/total.png"
`)
	c := newTestCharts(t, script)
	c.eventDir = "/events/md2027-x"
	paths, cleanup, err := c.Render(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(paths) != 2 || filepath.Base(paths[1]) != "map.png" || chartCaption(paths[1]) != "Where agents are playing" {
		t.Fatalf("map should follow total in the slideshow: %v", paths)
	}
}

func TestRenderChartsForcesUTF8Locale(t *testing.T) {
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANG", "")
	os.Unsetenv("LANG")
	script := fakeScript(t, `set -e
test "$LC_ALL" = en_US.UTF-8
test "$LANG" = en_US.UTF-8
mkdir -p "$2"
echo png > "$2/total.png"
`)
	_, cleanup, err := newTestCharts(t, script).Render(context.Background())
	if err != nil {
		t.Fatalf("R must run under a UTF-8 locale even when the caller has none: %v", err)
	}
	cleanup()
}

type staticWeather []weather.Hour

func (s staticWeather) WeatherHours(context.Context) ([]weather.Hour, error) { return s, nil }

func TestRenderChartsPassesWeatherCSV(t *testing.T) {
	script := fakeScript(t, `set -e
test "$5" = --weather
grep -q '^hour_start,condition,temperature,precipitation_chance,precipitation_amount$' "$6"
grep -q '^2026-11-07T07:00:00+08:00,Rain,21.50,0.60,1.20$' "$6"
mkdir -p "$2"
echo png > "$2/total.png"
`)
	c := newTestCharts(t, script)
	c.loc = time.FixedZone("CST", 8*3600)
	c.weather = staticWeather{{Start: time.Date(2026, 11, 6, 23, 0, 0, 0, time.UTC), Condition: "Rain",
		Temperature: 21.5, PrecipitationChance: 0.6, PrecipitationAmount: 1.2}}
	_, cleanup, err := c.Render(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cleanup()

	// No stored weather yet: the option is left out so the chart renders without the panel.
	c = newTestCharts(t, fakeScript(t, "set -e\ntest \"$#\" = 4\nmkdir -p \"$2\"\necho png > \"$2/total.png\"\n"))
	c.weather = staticWeather{}
	_, cleanup, err = c.Render(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
}

func TestRenderChartsNeedsEventDir(t *testing.T) {
	c := newTestCharts(t, fakeScript(t, "exit 0"))
	c.eventDir = ""
	if _, _, err := c.Render(context.Background()); err == nil {
		t.Fatal("rendering without an event directory must fail")
	}
}

func TestRenderChartsReportsScriptFailure(t *testing.T) {
	c := newTestCharts(t, fakeScript(t, "echo 'there is no package called ggplot2' >&2; exit 1\n"))
	_, cleanup, err := c.Render(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ggplot2") {
		t.Fatalf("expected script output in error, got %v", err)
	}
	if cleanup != nil {
		t.Fatal("failed render must clean up itself")
	}

	c = newTestCharts(t, fakeScript(t, "mkdir -p \"$2\"\n"))
	if _, _, err := c.Render(context.Background()); err == nil {
		t.Fatal("expected error when no images were produced")
	}

	c = newTestCharts(t, fakeScript(t, "sleep 5\n"))
	c.timeout = 100 * time.Millisecond
	if _, _, err := c.Render(context.Background()); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestEveryChartHasCaptionsInBothLanguages(t *testing.T) {
	for _, name := range append(append([]string{}, chartFiles...), finalChartFiles...) {
		path := filepath.Join("/out", name)
		if english.chartCaption(path) == "" || chinese.chartCaption(path) == "" {
			t.Errorf("%s is missing a caption: en %q, zh %q", name, english.chartCaption(path), chinese.chartCaption(path))
		}
	}
}

func TestRenderChartsPassesLanguage(t *testing.T) {
	script := fakeScript(t, `set -e
mkdir -p "$2"
echo "$*" > "$2/args"
echo png > "$2/total.png"
`)
	for l, want := range map[lang]string{english: " --final", chinese: " --final --lang zh"} {
		c := newTestCharts(t, script)
		c.final, c.lang = true, l
		paths, cleanup, err := c.Render(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		args, err := os.ReadFile(filepath.Join(filepath.Dir(paths[0]), "args"))
		cleanup()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(args)); !strings.HasSuffix(got, want) {
			t.Errorf("lang %s: args %q should end with %q", l.code(), got, want)
		}
	}
}
