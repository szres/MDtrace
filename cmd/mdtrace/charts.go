package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mdtrace/internal/store"
	"mdtrace/internal/weather"
)

// finalChartFiles is the slideshow of the end-of-event summary (charts.R --final).
var finalChartFiles = []string{"event_anim.mp4", "total.png", "map.png", "pace.png", "completions.png", "ratings.png"}

// chartFiles is the order the charts appear in the Telegram slideshow, with their captions.
// The animated map goes as MP4: Telegram plays it as a GIF without re-encoding a 256-colour GIF.
var chartFiles = []string{"total.png", "map.png", "map_anim.mp4", "pace.png", "completions.png", "ratings.png"}

var chartCaptions = map[string]string{
	"total.png":       "Total completions and hourly pace",
	"map.png":         "Where agents are playing",
	"map_anim.mp4":    "Last 2 hours, read by read",
	"event_anim.mp4":  "Mission Day replay",
	"pace.png":        "Completions per hour by mission",
	"completions.png": "Cumulative completions per mission",
	"ratings.png":     "Rating per mission",
}

var chineseChartCaptions = map[string]string{
	"total.png":       "总完成数与每小时进度",
	"map.png":         "特工们在哪里玩",
	"map_anim.mp4":    "最近 2 小时，逐次读取",
	"event_anim.mp4":  "任务日回放",
	"pace.png":        "各任务每小时完成数",
	"completions.png": "各任务累计完成数",
	"ratings.png":     "各任务评分",
}

func chartCaption(path string) string {
	return chartCaptions[filepath.Base(path)]
}

func (l lang) chartCaption(path string) string {
	if l == chinese {
		return chineseChartCaptions[filepath.Base(path)]
	}
	return chartCaption(path)
}

const maxChartOutput = 600

var chartLocaleEnv = []string{"LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8"}

type rowSource interface {
	Rows(ctx context.Context) ([]store.Row, error)
}

type weatherSource interface {
	WeatherHours(ctx context.Context) ([]weather.Hour, error)
}

// rCharts renders the ggplot charts by exporting the snapshots to CSV and running analysis/charts.R.
type rCharts struct {
	rscript string
	script  string
	geoDir  string // optional; holds routes.json and coastline.json for the map chart
	rows    rowSource
	weather weatherSource // optional; adds the weather panel to total.png
	loc     *time.Location
	timeout time.Duration
	final   bool // render the end-of-event set (whole-event map and replay)
	lang    lang // language of the text inside the charts
}

// Render returns the chart image paths; cleanup removes them and must be called when done.
func (r *rCharts) Render(ctx context.Context) (paths []string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "mdtrace-charts-")
	if err != nil {
		return nil, nil, fmt.Errorf("charts: %w", err)
	}
	removeDir := func() { os.RemoveAll(dir) }
	defer func() {
		if err != nil {
			removeDir()
		}
	}()

	rows, err := r.rows.Rows(ctx)
	if err != nil {
		return nil, nil, err
	}
	csvPath := filepath.Join(dir, "export.csv")
	if err := writeFile(csvPath, func(w io.Writer) error { return writeCSV(w, rows, r.loc) }); err != nil {
		return nil, nil, fmt.Errorf("charts: %w", err)
	}

	outDir := filepath.Join(dir, "png")
	args := []string{r.script, csvPath, outDir}
	if r.geoDir != "" {
		args = append(args, "--geo", r.geoDir)
	}
	files := chartFiles
	if r.final {
		args = append(args, "--final")
		files = finalChartFiles
	}
	if r.lang == chinese {
		args = append(args, "--lang", "zh")
	}
	if r.weather != nil {
		hours, err := r.weather.WeatherHours(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("charts: %w", err)
		}
		if len(hours) > 0 {
			weatherPath := filepath.Join(dir, "weather.csv")
			if err := writeFile(weatherPath, func(w io.Writer) error { return writeWeatherCSV(w, hours, r.loc) }); err != nil {
				return nil, nil, fmt.Errorf("charts: %w", err)
			}
			args = append(args, "--weather", weatherPath)
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, r.rscript, args...)
	// launchd starts jobs without a locale; R then cannot decode the UTF-8 mission titles in the CSV.
	cmd.Env = append(os.Environ(), chartLocaleEnv...)
	// A killed process can leave children holding the output pipe; don't wait on them indefinitely.
	cmd.WaitDelay = 2 * time.Second
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, nil, fmt.Errorf("charts: %s failed: %w: %s", filepath.Base(r.script), err, tail(string(output), maxChartOutput))
	}

	for _, name := range files {
		path := filepath.Join(outDir, name)
		if _, statErr := os.Stat(path); statErr == nil {
			paths = append(paths, path)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("charts: %w", statErr)
		}
	}
	if len(paths) == 0 {
		return nil, nil, errors.New("charts: script produced no images")
	}
	return paths, removeDir, nil
}

func writeFile(path string, write func(io.Writer) error) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	return write(f)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
