package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mdtrace/internal/intel"
	"mdtrace/internal/missions"
	"mdtrace/internal/store"
)

type fakeFetcher struct {
	versionErr error
	// errs maps guid to the errors returned on successive calls; nil entries mean success.
	errs map[string][]error
	// counts maps guid to the completion counts returned on successive calls (default 7).
	counts map[string][]int64
	calls  map[string]int
}

func (f *fakeFetcher) LoadVersion(context.Context) error { return f.versionErr }

func (f *fakeFetcher) MissionDetails(_ context.Context, guid string) (intel.MissionDetail, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	n := f.calls[guid]
	f.calls[guid]++
	if seq := f.errs[guid]; n < len(seq) && seq[n] != nil {
		return intel.MissionDetail{}, seq[n]
	}
	count := int64(7)
	if seq := f.counts[guid]; n < len(seq) {
		count = seq[n]
	}
	return intel.MissionDetail{GUID: guid, Title: "T " + guid, RatingE6: 950000, CompletedCount: count}, nil
}

func newTestCollector(t *testing.T, f *fakeFetcher) (*collector, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &collector{
		fetcher: f, store: db, logger: log.New(io.Discard, "", 0),
		delay: time.Millisecond, sleep: sleepContext, now: time.Now,
	}, db
}

var testMissions = []missions.Mission{{Order: 1, GUID: "a.1c"}, {Order: 2, GUID: "b.1c"}, {Order: 3, GUID: "c.1c"}}

func TestCollectorRetriesTransientErrors(t *testing.T) {
	f := &fakeFetcher{errs: map[string][]error{"b.1c": {errors.New("timeout")}}}
	c, db := newTestCollector(t, f)

	res, err := c.run(context.Background(), testMissions)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK != 3 || res.Failed != 0 || f.calls["b.1c"] != 2 {
		t.Fatalf("res=%+v calls=%v", res, f.calls)
	}
	rows, err := db.Rows(context.Background())
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
}

func TestCollectorSkipsZeroedStats(t *testing.T) {
	f := &fakeFetcher{counts: map[string][]int64{"a.1c": {0, 204, 0, 206}, "b.1c": {0, 0, 0, 0}}}
	c, db := newTestCollector(t, f)
	ctx := context.Background()
	var results []collectResult
	for i := 0; i < 4; i++ {
		res, err := c.run(ctx, testMissions)
		if err != nil {
			t.Fatalf("run %d: a zeroed reading is not a run failure: %v", i, err)
		}
		results = append(results, res)
	}
	if results[2].Skipped != 1 || results[2].OK != 2 || results[2].Failed != 0 {
		t.Fatalf("run 3 should skip a.1c's zeroed reading: %+v", results[2])
	}
	for _, i := range []int{0, 1, 3} {
		if results[i].Skipped != 0 {
			t.Fatalf("run %d: a zero before any completion (or for b.1c, always zero) is real: %+v", i+1, results[i])
		}
	}

	rows, err := db.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var history []string
	for _, r := range rows {
		if r.MissionGUID == "a.1c" {
			history = append(history, fmt.Sprintf("%d:%d", r.RunID, r.CompletedCount))
			if r.CompletedDelta.Valid && r.CompletedDelta.Int64 < 0 {
				t.Errorf("negative delta at run %d", r.RunID)
			}
		}
	}
	if got := strings.Join(history, ","); got != "1:0,2:204,4:206" {
		t.Fatalf("a.1c history = %s", got)
	}

	changes, err := db.ChangesSince(ctx, results[2].RunID, results[1].RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 || changes[0].GUID != "a.1c" || changes[0].Completed != 204 {
		t.Fatalf("run 3 changes should carry a.1c's last good count: %+v", changes)
	}
}

func TestCollectorContinuesAfterPersistentFailure(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeFetcher{errs: map[string][]error{"a.1c": {boom, boom}}}
	c, _ := newTestCollector(t, f)

	res, err := c.run(context.Background(), testMissions)
	if err == nil || !strings.Contains(err.Error(), "1 of 3") {
		t.Fatalf("expected partial failure error, got %v", err)
	}
	if res.OK != 2 || res.Failed != 1 {
		t.Fatalf("res=%+v", res)
	}
}

func TestCollectorAbortsOnAuthError(t *testing.T) {
	authErr := fmt.Errorf("%w: expired", intel.ErrAuth)
	f := &fakeFetcher{errs: map[string][]error{"a.1c": {authErr}}}
	c, _ := newTestCollector(t, f)

	res, err := c.run(context.Background(), testMissions)
	if !errors.Is(err, intel.ErrAuth) {
		t.Fatalf("expected ErrAuth, got %v", err)
	}
	if f.calls["a.1c"] != 1 || f.calls["b.1c"] != 0 || res.OK != 0 {
		t.Fatalf("should not retry or continue after auth error: calls=%v res=%+v", f.calls, res)
	}
}

func TestCollectorVersionFailureAbortsRun(t *testing.T) {
	f := &fakeFetcher{versionErr: fmt.Errorf("%w: login page", intel.ErrAuth)}
	c, _ := newTestCollector(t, f)
	if _, err := c.run(context.Background(), testMissions); !errors.Is(err, intel.ErrAuth) {
		t.Fatalf("expected ErrAuth, got %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("no mission should be fetched, calls=%v", f.calls)
	}
}

func TestWriteCSV(t *testing.T) {
	rows := []store.Row{
		{RunID: 1, RunStartedAt: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), FetchedAt: time.Date(2026, 11, 7, 0, 0, 5, 0, time.UTC),
			MissionOrder: 1, MissionGUID: "a.1c", Title: "MD 2026: Zhuhai, 海滨泳场", RatingE6: 987654, CompletedCount: 10},
		{RunID: 2, RunStartedAt: time.Date(2026, 11, 7, 0, 30, 0, 0, time.UTC), FetchedAt: time.Date(2026, 11, 7, 0, 30, 5, 0, time.UTC),
			MissionOrder: 1, MissionGUID: "a.1c", Title: "x", RatingE6: 1000000, CompletedCount: 30,
			CompletedDelta: sql.NullInt64{Int64: 20, Valid: true}},
	}
	var buf bytes.Buffer
	if err := writeCSV(&buf, rows, time.FixedZone("CST", 8*3600)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "\ufeff") {
		t.Fatal("missing BOM")
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(buf.String(), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records", len(records))
	}
	first, second := records[1], records[2]
	if first[1] != "2026-11-07T08:00:00+08:00" || first[5] != "MD 2026: Zhuhai, 海滨泳场" || first[7] != "" || first[8] != "98.7654" {
		t.Errorf("unexpected first record %q", first)
	}
	if second[6] != "30" || second[7] != "20" || second[8] != "100.0000" {
		t.Errorf("unexpected second record %q", second)
	}
}
