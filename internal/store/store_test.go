package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"mdtrace/internal/intel"
	"mdtrace/internal/weather"
)

func TestStoreRoundTripWithDeltas(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	clock := time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }

	record := func(counts map[string]int64) {
		t.Helper()
		runID, err := s.StartRun(ctx, len(counts), clock)
		if err != nil {
			t.Fatal(err)
		}
		order := 0
		for _, guid := range []string{"a.1c", "b.1c"} {
			order++
			count, ok := counts[guid]
			if !ok {
				if err := s.SaveFailure(ctx, runID, guid, errors.New("boom")); err != nil {
					t.Fatal(err)
				}
				continue
			}
			d := intel.MissionDetail{GUID: guid, Title: guid, RatingE6: 990000, CompletedCount: count}
			if err := s.SaveSnapshot(ctx, runID, order, d); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.FinishRun(ctx, runID, len(counts), nil); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(30 * time.Minute)
	}

	record(map[string]int64{"a.1c": 10, "b.1c": 5})
	record(map[string]int64{"a.1c": 25})
	record(map[string]int64{"a.1c": 40, "b.1c": 12})

	rows, err := s.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		run  int64
		guid string
	}
	got := map[key]Row{}
	for _, r := range rows {
		got[key{r.RunID, r.MissionGUID}] = r
	}
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5", len(rows))
	}
	if got[key{1, "a.1c"}].CompletedDelta.Valid {
		t.Error("first snapshot should have no delta")
	}
	if d := got[key{2, "a.1c"}].CompletedDelta; !d.Valid || d.Int64 != 15 {
		t.Errorf("run 2 a.1c delta = %+v, want 15", d)
	}
	if d := got[key{3, "b.1c"}].CompletedDelta; !d.Valid || d.Int64 != 7 {
		t.Errorf("run 3 b.1c delta spans the failed run, got %+v, want 7", d)
	}
	if r := got[key{3, "a.1c"}]; !r.FetchedAt.Equal(time.Date(2026, 11, 7, 2, 0, 0, 0, time.UTC)) {
		t.Errorf("unexpected fetched_at %v", r.FetchedAt)
	}

	// Reopening must keep existing data (schema creation is idempotent).
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rows, err = s2.Rows(ctx)
	if err != nil || len(rows) != 5 {
		t.Fatalf("after reopen: %d rows, err %v", len(rows), err)
	}
}

func TestRunChangesAndAlerts(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	save := func(counts map[string]int64, rating int64) int64 {
		t.Helper()
		runID, err := s.StartRun(ctx, len(counts), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		order := 0
		for _, guid := range []string{"a.1c", "b.1c"} {
			order++
			if c, ok := counts[guid]; ok {
				d := intel.MissionDetail{GUID: guid, Title: guid, RatingE6: rating, CompletedCount: c}
				if err := s.SaveSnapshot(ctx, runID, order, d); err != nil {
					t.Fatal(err)
				}
			}
		}
		return runID
	}

	first := save(map[string]int64{"a.1c": 1, "b.1c": 2}, 900000)
	changes, err := s.ChangesSince(ctx, first, first-1)
	if err != nil || len(changes) != 2 || changes[0].Changed() || changes[0].PrevCompleted.Valid {
		t.Fatalf("first run: %+v err %v", changes, err)
	}
	second := save(map[string]int64{"a.1c": 5}, 900000)
	third := save(map[string]int64{"a.1c": 5, "b.1c": 2}, 950000)
	if _, err := s.ChangesSince(ctx, third, third); err == nil {
		t.Fatal("baseline equal to run must be rejected")
	}
	if since1, err := s.ChangesSince(ctx, third, first); err != nil || since1[0].PrevCompleted.Int64 != 1 {
		t.Fatalf("a.1c against run 1 baseline: %+v %v", since1, err)
	}
	changes, err = s.ChangesSince(ctx, third, second)
	if err != nil {
		t.Fatal(err)
	}
	if a := changes[0]; !a.Changed() || a.PrevCompleted.Int64 != 5 || a.PrevRatingE6.Int64 != 900000 {
		t.Errorf("a.1c change: %+v", a)
	}
	if b := changes[1]; !b.Changed() || b.PrevCompleted.Int64 != 2 {
		t.Errorf("b.1c should compare with run 1 and see the rating change: %+v", b)
	}

	if _, ok, err := s.AlertSentAt(ctx, "k"); ok || err != nil {
		t.Fatalf("unexpected alert: %v %v", ok, err)
	}
	at := time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC)
	if err := s.MarkAlertSent(ctx, "k", at); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAlertSent(ctx, "k", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.AlertSentAt(ctx, "k"); !ok || err != nil || !got.Equal(at.Add(time.Hour)) {
		t.Fatalf("alert: %v %v %v", got, ok, err)
	}
	if _, ok, err := s.LastUpdateSentAt(ctx); ok || err != nil {
		t.Fatalf("unexpected last update: %v %v", ok, err)
	}
	if err := s.MarkUpdateSent(ctx, second, at); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUpdateSent(ctx, third, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if sent, ok, err := s.LastUpdateSentAt(ctx); !ok || err != nil || !sent.Equal(at.Add(time.Hour)) {
		t.Fatalf("last update: %v %v %v", sent, ok, err)
	}
	if err := s.ClearAlert(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.AlertSentAt(ctx, "k"); ok {
		t.Fatal("alert should be cleared")
	}
}

func TestBaselineRun(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok, err := s.LatestRun(ctx); ok || err != nil {
		t.Fatalf("empty store has no latest run: %v %v", ok, err)
	}

	start := time.Date(2026, 11, 7, 2, 0, 0, 0, time.UTC)
	var ids []int64
	for i := 0; i < 8; i++ { // runs every 5 minutes, starting a few seconds late
		at := start.Add(time.Duration(i)*5*time.Minute + time.Duration(i%3)*time.Second)
		id, err := s.StartRun(ctx, 1, at)
		if err != nil {
			t.Fatal(err)
		}
		if i != 1 { // run 1 failed and has no snapshots
			if err := s.SaveSnapshot(ctx, id, 1, intel.MissionDetail{GUID: "a.1c"}); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, id)
	}

	if latest, ok, err := s.LatestRun(ctx); err != nil || !ok || latest != ids[7] {
		t.Fatalf("LatestRun = %d %v %v", latest, ok, err)
	}
	if _, _, ok, err := s.BaselineRun(ctx, ids[0], 28*time.Minute); ok || err != nil {
		t.Fatalf("first run has no baseline: %v %v", ok, err)
	}
	if id, _, ok, _ := s.BaselineRun(ctx, ids[3], 28*time.Minute); !ok || id != ids[0] {
		t.Fatalf("short history should fall back to the earliest run, got %d", id)
	}
	id, at, ok, err := s.BaselineRun(ctx, ids[6], 28*time.Minute) // 02:30:00 -> 02:00:00
	if err != nil || !ok || id != ids[0] || !at.Equal(start) {
		t.Fatalf("baseline for 02:30 = %d %v %v %v", id, at, ok, err)
	}
	if id, _, _, _ := s.BaselineRun(ctx, ids[7], 28*time.Minute); id != ids[0] {
		t.Fatalf("02:35 should skip snapshot-less run 1 (02:05) and use 02:00, got %d", id)
	}
}

func TestChangesSinceUsesLastKnownSnapshot(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	save := func(counts map[string]int64) int64 {
		runID, _ := s.StartRun(ctx, 2, at)
		at = at.Add(5 * time.Minute)
		for i, guid := range []string{"a.1c", "b.1c"} {
			if n, ok := counts[guid]; ok {
				if err := s.SaveSnapshot(ctx, runID, i+1, intel.MissionDetail{GUID: guid, CompletedCount: n}); err != nil {
					t.Fatal(err)
				}
			}
		}
		return runID
	}
	first := save(map[string]int64{"a.1c": 10, "b.1c": 5})
	second := save(map[string]int64{"b.1c": 6}) // a.1c skipped this run

	if n, ok, err := s.LastCompletedCount(ctx, "a.1c"); err != nil || !ok || n != 10 {
		t.Fatalf("LastCompletedCount = %d %v %v", n, ok, err)
	}
	if _, ok, err := s.LastCompletedCount(ctx, "zzz"); err != nil || ok {
		t.Fatalf("unknown mission: ok=%v err=%v", ok, err)
	}
	changes, err := s.ChangesSince(ctx, second, first)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Completed != 10 || changes[0].Changed() || changes[1].Completed != 6 {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestWeatherRoundTripAndUpsert(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok, err := s.LatestWeather(ctx, 3); err != nil || ok {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}

	base := time.Date(2026, 11, 6, 22, 0, 0, 0, time.UTC)
	hour := func(i int, cond string, mm float64) weather.Hour {
		return weather.Hour{Start: base.Add(time.Duration(i) * time.Hour), Condition: cond, Temperature: 20 + float64(i),
			PrecipitationChance: 0.1 * float64(i), PrecipitationAmount: mm}
	}
	run1, _ := s.StartRun(ctx, 1, base)
	first := weather.Report{
		Current: weather.Current{AsOf: base.Add(10 * time.Minute), Condition: "Cloudy", Temperature: 20.5,
			ApparentTemperature: 21, Humidity: 0.8, PrecipitationIntensity: 0, WindSpeed: 10},
		Hours: []weather.Hour{hour(0, "Cloudy", 0), hour(1, "Rain", 1.2), hour(2, "Rain", 0.5), hour(3, "Cloudy", 0)},
	}
	if err := s.SaveWeather(ctx, run1, first); err != nil {
		t.Fatal(err)
	}
	run2, _ := s.StartRun(ctx, 1, base.Add(time.Hour))
	second := first
	second.Current.AsOf = base.Add(70 * time.Minute)
	second.Current.Condition = "Rain"
	second.Hours = []weather.Hour{hour(1, "Drizzle", 0.3)} // revised history for 23:00
	if err := s.SaveWeather(ctx, run2, second); err != nil {
		t.Fatal(err)
	}

	latest, ok, err := s.LatestWeather(ctx, 2)
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if latest.Current.Condition != "Rain" || !latest.Current.AsOf.Equal(second.Current.AsOf) {
		t.Errorf("current = %+v", latest.Current)
	}
	// After 23:10 the upcoming hours are 00:00 and 01:00.
	if len(latest.Hours) != 2 || !latest.Hours[0].Start.Equal(base.Add(2*time.Hour)) || latest.Hours[1].Condition != "Cloudy" {
		t.Errorf("upcoming hours = %+v", latest.Hours)
	}

	all, err := s.WeatherHours(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 || all[1].Condition != "Drizzle" || all[1].PrecipitationAmount != 0.3 {
		t.Errorf("hours after upsert = %+v", all)
	}
}
