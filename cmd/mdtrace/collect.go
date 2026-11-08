package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"mdtrace/internal/intel"
	"mdtrace/internal/missions"
)

type missionFetcher interface {
	LoadVersion(ctx context.Context) error
	MissionDetails(ctx context.Context, guid string) (intel.MissionDetail, error)
}

type snapshotStore interface {
	StartRun(ctx context.Context, missionsTotal int, startedAt time.Time) (int64, error)
	FinishRun(ctx context.Context, runID int64, missionsOK int, runErr error) error
	SaveSnapshot(ctx context.Context, runID int64, order int, d intel.MissionDetail) error
	SaveFailure(ctx context.Context, runID int64, guid string, failure error) error
	LastCompletedCount(ctx context.Context, guid string) (int64, bool, error)
}

type collector struct {
	fetcher missionFetcher
	store   snapshotStore
	logger  *log.Logger
	// delay spaces out requests so Intel is not hammered; also used before a retry.
	delay time.Duration
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
}

type collectResult struct {
	RunID   int64
	OK      int
	Failed  int
	Skipped int // readings rejected as Intel glitches; not failures
}

// run fetches every mission once and records the outcome as a single run.
// An authentication failure aborts the run immediately since every later request would fail too.
func (c *collector) run(ctx context.Context, list []missions.Mission) (collectResult, error) {
	runID, err := c.store.StartRun(ctx, len(list), c.now())
	if err != nil {
		return collectResult{}, err
	}
	result := collectResult{RunID: runID}

	runErr := c.fetchAll(ctx, runID, list, &result)
	if runErr == nil && result.Failed > 0 {
		runErr = fmt.Errorf("%d of %d missions failed", result.Failed, len(list))
	}
	if err := c.store.FinishRun(ctx, runID, result.OK, runErr); err != nil {
		return result, errors.Join(runErr, err)
	}
	return result, runErr
}

func (c *collector) fetchAll(ctx context.Context, runID int64, list []missions.Mission, result *collectResult) error {
	if err := c.fetcher.LoadVersion(ctx); err != nil {
		return err
	}
	for i, m := range list {
		if i > 0 {
			if err := c.sleep(ctx, c.delay); err != nil {
				return err
			}
		}
		detail, err := c.fetchWithRetry(ctx, m.GUID)
		if err != nil {
			result.Failed++
			c.logger.Printf("FAIL  #%02d %s: %v", m.Order, m.GUID, err)
			if saveErr := c.store.SaveFailure(ctx, runID, m.GUID, err); saveErr != nil {
				return saveErr
			}
			if errors.Is(err, intel.ErrAuth) {
				return err
			}
			continue
		}
		glitch, err := c.zeroedStats(ctx, detail)
		if err != nil {
			return err
		}
		if glitch != nil {
			result.Skipped++
			c.logger.Printf("skip  #%02d %v", m.Order, glitch)
			if err := c.store.SaveFailure(ctx, runID, m.GUID, glitch); err != nil {
				return err
			}
			continue
		}
		if err := c.store.SaveSnapshot(ctx, runID, m.Order, detail); err != nil {
			return err
		}
		result.OK++
		c.logger.Printf("ok    #%02d completed=%-6d rating=%.2f%%  %s",
			m.Order, detail.CompletedCount, detail.RatingPercent(), detail.Title)
	}
	return nil
}

// zeroedStats detects Intel briefly serving all-zero statistics for a mission that already has
// completions (seen for about an hour on 2026-11-07). Unique completions never go back to zero,
// so such a reading is reported instead of stored.
func (c *collector) zeroedStats(ctx context.Context, d intel.MissionDetail) (error, error) {
	if d.CompletedCount != 0 {
		return nil, nil
	}
	last, ok, err := c.store.LastCompletedCount(ctx, d.GUID)
	if err != nil || !ok || last == 0 {
		return nil, err
	}
	return fmt.Errorf("intel returned zeroed stats (completed 0 after %d); reading skipped", last), nil
}

func (c *collector) fetchWithRetry(ctx context.Context, guid string) (intel.MissionDetail, error) {
	detail, err := c.fetcher.MissionDetails(ctx, guid)
	if err == nil || errors.Is(err, intel.ErrAuth) || ctx.Err() != nil {
		return detail, err
	}
	c.logger.Printf("retry %s after error: %v", guid, err)
	if sleepErr := c.sleep(ctx, c.delay); sleepErr != nil {
		return intel.MissionDetail{}, errors.Join(err, sleepErr)
	}
	return c.fetcher.MissionDetails(ctx, guid)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
