package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mdtrace/internal/event"
	"mdtrace/internal/store"
	"mdtrace/internal/weather"
)

func finalRows() []store.Row {
	base := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC) // 16:00 CST
	row := func(run int64, minutes int, order int, guid string, count, delta, ratingE6, durationMs int64) store.Row {
		at := base.Add(time.Duration(minutes) * time.Minute)
		r := store.Row{RunID: run, RunStartedAt: at, FetchedAt: at, MissionOrder: order, MissionGUID: guid,
			Title: "MD " + guid, CompletedCount: count, RatingE6: ratingE6, AverageDurationMs: durationMs}
		if run > 1 {
			r.CompletedDelta = sql.NullInt64{Int64: delta, Valid: true}
		}
		return r
	}
	return []store.Row{
		row(1, 0, 1, "a.1c", 0, 0, 0, 0), row(1, 0, 2, "b.1c", 0, 0, 0, 0), row(1, 0, 3, "c.1c", 0, 0, 0, 0),
		// 16:30: first completions
		row(2, 30, 1, "a.1c", 3, 3, 1000000, 600000), row(2, 30, 2, "b.1c", 0, 0, 0, 0), row(2, 30, 3, "c.1c", 1, 1, 0, 900000),
		// 17:30: busiest hour (12), a.1c's biggest hour (7); halfway (21 of 20) is reached here
		row(3, 90, 1, "a.1c", 10, 7, 800000, 720000), row(3, 90, 2, "b.1c", 4, 4, 500000, 4500000), row(3, 90, 3, "c.1c", 2, 1, 0, 900000),
		// 18:30
		row(4, 150, 1, "a.1c", 12, 2, 850000, 690000), row(4, 150, 2, "b.1c", 6, 2, 600000, 3900000), row(4, 150, 3, "c.1c", 2, 0, 0, 900000),
	}
}

func TestBuildFinalReport(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	hours := []weather.Hour{
		{Start: time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC), Temperature: 30, PrecipitationAmount: 5}, // before the event
		{Start: time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC), Temperature: 26, PrecipitationAmount: 0.4},
		{Start: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC), Temperature: 24, PrecipitationAmount: 0.05},
		{Start: time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC), Temperature: 25, PrecipitationAmount: 1.1},
	}
	r, err := buildFinalReport(finalRows(), hours, map[string]string{"a.1c": "海滨泳场"}, loc)
	if err != nil {
		t.Fatal(err)
	}
	r.name = event.Text{EN: "MD 2026 Zhuhai", ZH: "MD 2026 珠海"}
	if r.total != 20 || len(r.missions) != 3 {
		t.Fatalf("total=%d missions=%d", r.total, len(r.missions))
	}
	a, b := r.missions[0], r.missions[1]
	if a.name != "海滨泳场" || b.name != "MD b.1c" || a.firstRating != 100 || a.rating != 85 || b.firstRating != 50 {
		t.Fatalf("missions = %+v", r.missions)
	}

	en := strings.Join(r.highlights(english), "\n")
	for _, want := range []string{
		"🏆 Most completed: #01 海滨泳场 — 12",
		"🌱 Fewest completions: #03 MD c.1c — 2",
		"⭐ Highest rated: #01 海滨泳场 — 85.0%",
		"💔 Lowest rated: #02 MD b.1c — 60.0%",
		"⚡ Quickest on average: #01 海滨泳场 — 12 min",
		"🐢 Longest on average: #02 MD b.1c — 1 h 05 min",
		"🔥 Busiest hour: Fri 17:00–18:00 — 12 completions",
		"🚀 Biggest hour for one mission: #01 海滨泳场, Fri 17:00–18:00 — 7",
		"⏳ Halfway: half of all completions were done by Fri 17:30",
		"🌡 Hottest: 26°C at Fri 16:00 · 🌧 Wettest hour: Fri 18:00–19:00, 1.1 mm",
	} {
		if !strings.Contains(en, want) {
			t.Errorf("English highlights missing %q:\n%s", want, en)
		}
	}
	zh := strings.Join(r.highlights(chinese), "\n")
	for _, want := range []string{
		"🏆 完成最多：#01 海滨泳场 — 12 次",
		"🐢 平均用时最长：#02 MD b.1c — 1 小时 05 分钟",
		"🔥 最热闹的一小时：周五 17:00–18:00 — 12 次完成",
		"⏳ 过半：周五 17:30 时完成数已达总数一半",
		"🌡 最高温：周五 16:00 26°C · 🌧 雨最大的一小时：周五 18:00–19:00，1.1 毫米",
	} {
		if !strings.Contains(zh, want) {
			t.Errorf("Chinese highlights missing %q:\n%s", want, zh)
		}
	}

	if got := r.spanLine(english); got != "From Fri 06 Nov 16:30 to Fri 06 Nov 18:30 (2 h)" {
		t.Errorf("span = %q", got)
	}
	if got := r.spanLine(chinese); got != "11月6日 周五 16:30 至 11月6日 周五 18:30（2 小时）" {
		t.Errorf("Chinese span = %q", got)
	}
	// Weather from 16:00 (the start's hour) to 18:30: three hours, 24–26°C, two rainy hours.
	if got := r.weatherLine(english); got != "🌦 Weather: 24–26°C · rain in 2 of 3 hours, 1.6 mm in total" {
		t.Errorf("weather = %q", got)
	}

	charts := []string{"/tmp/f/event_anim.mp4", "/tmp/f/total.png"}
	for l, wants := range map[lang][]string{
		english: {"🏁 MD 2026 Zhuhai · final summary", "20 unique completions across 3 missions", "Highlights",
			"#|Mission|Done|Rating|Δ", "01|海滨泳场|12|85.0%|-15.0", "02|MD b.1c|6|60.0%|+10.0", "03|MD c.1c|2|0.0%|",
			"[slideshow event_anim.mp4=Mission Day replay (animation), total.png=Total completions and hourly pace]",
			"Weather: Apple Weather"},
		chinese: {"🏁 MD 2026 珠海 · 最终总结", "20 次独立完成，共 3 个任务", "亮点", "#|任务|完成|评分|Δ",
			"[slideshow event_anim.mp4=任务日回放 (animation), total.png=总完成数与每小时进度]", "天气：Apple Weather"},
	} {
		msg := flatten(r.richBlocks(l, charts))
		for _, want := range wants {
			if !strings.Contains(msg, want) {
				t.Errorf("lang %d rich summary missing %q:\n%s", l, want, msg)
			}
		}
	}
	r.preview = true
	for l, wants := range map[lang][]string{
		english: {"final summary (preview)", "#02 MD b.1c: 6 · 60.00% (Δ +10.0)", "🔥 Busiest hour"},
		chinese: {"最终总结（预览）", "亮点", "🔥 最热闹的一小时"},
	} {
		plain := r.plainText(l)
		for _, want := range wants {
			if !strings.Contains(plain, want) {
				t.Errorf("lang %d plain summary missing %q:\n%s", l, want, plain)
			}
		}
	}
}

func TestFinalHighlightsListTies(t *testing.T) {
	ms := []finalMission{{order: 16, name: "励骏庞都广场", completed: 46}, {order: 17, name: "横琴芒洲湿地公园", completed: 46},
		{order: 1, name: "海滨泳场", completed: 231}}
	least := extremes(ms, all, func(m finalMission) float64 { return float64(m.completed) }, false)
	if got := missionNames(least); got != "#16 励骏庞都广场, #17 横琴芒洲湿地公园" {
		t.Fatalf("tie = %q", got)
	}
}

func TestBuildFinalReportWithoutWeatherOrData(t *testing.T) {
	if _, err := buildFinalReport(nil, nil, nil, time.UTC); err == nil {
		t.Fatal("empty database should be an error")
	}
	r, err := buildFinalReport(finalRows(), nil, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []lang{english, chinese} {
		if text := r.plainText(l); r.weather != nil || strings.Contains(text, "°C") {
			t.Fatalf("no weather lines without weather data:\n%s", text)
		}
	}
}

func TestRenderFinalChartSet(t *testing.T) {
	script := fakeScript(t, `set -e
test "$5" = --final
mkdir -p "$2"
for f in total.png map.png event_anim.mp4 map_anim.mp4; do echo x > "$2/$f"; done
`)
	c := newTestCharts(t, script)
	c.final = true
	paths, cleanup, err := c.Render(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var names []string
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	if got := strings.Join(names, ","); got != "event_anim.mp4,total.png,map.png" {
		t.Fatalf("final slideshow = %s", got)
	}

	dest := filepath.Join(t.TempDir(), "final")
	if err := copyFiles(paths, dest); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "event_anim.mp4")); err != nil || string(b) != "x\n" {
		t.Fatalf("copied replay: %q %v", b, err)
	}
	if english.code() != "en" || chinese.code() != "zh" ||
		finalChartDir("/d", english) != "/d/final" || finalChartDir("/d", chinese) != "/d/final/zh" {
		t.Error("each language needs its own code and chart folder")
	}
}
