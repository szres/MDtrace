// Package store persists mission snapshots in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"mdtrace/internal/intel"
	"mdtrace/internal/weather"
)

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	started_at     TEXT    NOT NULL,
	finished_at    TEXT,
	missions_total INTEGER NOT NULL,
	missions_ok    INTEGER NOT NULL DEFAULT 0,
	error          TEXT
);
CREATE TABLE IF NOT EXISTS snapshots (
	run_id              INTEGER NOT NULL REFERENCES runs(id),
	mission_guid        TEXT    NOT NULL,
	mission_order       INTEGER NOT NULL,
	title               TEXT    NOT NULL,
	author              TEXT    NOT NULL,
	faction             TEXT    NOT NULL,
	rating_e6           INTEGER NOT NULL,
	completed_count     INTEGER NOT NULL,
	average_duration_ms INTEGER NOT NULL,
	fetched_at          TEXT    NOT NULL,
	PRIMARY KEY (run_id, mission_guid)
);
CREATE INDEX IF NOT EXISTS snapshots_by_mission ON snapshots (mission_guid, fetched_at);
CREATE INDEX IF NOT EXISTS snapshots_by_mission_run ON snapshots (mission_guid, run_id);
CREATE TABLE IF NOT EXISTS failures (
	run_id       INTEGER NOT NULL REFERENCES runs(id),
	mission_guid TEXT    NOT NULL,
	error        TEXT    NOT NULL,
	failed_at    TEXT    NOT NULL
);
CREATE TABLE IF NOT EXISTS alerts (
	key     TEXT PRIMARY KEY,
	sent_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS weather_current (
	run_id                  INTEGER PRIMARY KEY REFERENCES runs(id),
	as_of                   TEXT NOT NULL,
	condition               TEXT NOT NULL,
	temperature             REAL NOT NULL,
	apparent_temperature    REAL NOT NULL,
	humidity                REAL NOT NULL,
	precipitation_intensity REAL NOT NULL,
	wind_speed              REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS weather_hours (
	hour_start           TEXT PRIMARY KEY,
	condition            TEXT NOT NULL,
	temperature          REAL NOT NULL,
	precipitation_chance REAL NOT NULL,
	precipitation_amount REAL NOT NULL,
	fetched_at           TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS last_update (
	id      INTEGER PRIMARY KEY CHECK (id = 1),
	run_id  INTEGER NOT NULL REFERENCES runs(id),
	sent_at TEXT    NOT NULL
);
`

type Store struct {
	db  *sql.DB
	now func() time.Time
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialise schema: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) timestamp() string { return s.now().UTC().Format(time.RFC3339) }

func (s *Store) StartRun(ctx context.Context, missionsTotal int, startedAt time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (started_at, missions_total) VALUES (?, ?)`,
		startedAt.UTC().Format(time.RFC3339), missionsTotal)
	if err != nil {
		return 0, fmt.Errorf("start run: %w", err)
	}
	return res.LastInsertId()
}

// FinishRun records the outcome; runErr is the error that aborted the run, if any.
func (s *Store) FinishRun(ctx context.Context, runID int64, missionsOK int, runErr error) error {
	var errText sql.NullString
	if runErr != nil {
		errText = sql.NullString{String: runErr.Error(), Valid: true}
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET finished_at = ?, missions_ok = ?, error = ? WHERE id = ?`,
		s.timestamp(), missionsOK, errText, runID)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	return nil
}

func (s *Store) SaveSnapshot(ctx context.Context, runID int64, order int, d intel.MissionDetail) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO snapshots (run_id, mission_guid, mission_order, title, author, faction,
			rating_e6, completed_count, average_duration_ms, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, d.GUID, order, d.Title, d.Author, d.Faction,
		d.RatingE6, d.CompletedCount, d.AverageDurationMs, s.timestamp())
	if err != nil {
		return fmt.Errorf("save snapshot %s: %w", d.GUID, err)
	}
	return nil
}

// LastCompletedCount returns the most recently stored completion count of a mission, if any.
func (s *Store) LastCompletedCount(ctx context.Context, guid string) (int64, bool, error) {
	var count int64
	err := s.db.QueryRowContext(ctx,
		`SELECT completed_count FROM snapshots WHERE mission_guid = ? ORDER BY run_id DESC LIMIT 1`, guid).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("last count of %s: %w", guid, err)
	}
	return count, true, nil
}

func (s *Store) SaveFailure(ctx context.Context, runID int64, guid string, failure error) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO failures (run_id, mission_guid, error, failed_at) VALUES (?, ?, ?, ?)`,
		runID, guid, failure.Error(), s.timestamp())
	if err != nil {
		return fmt.Errorf("save failure %s: %w", guid, err)
	}
	return nil
}

// SaveWeather stores the conditions observed during runID and upserts the hourly series,
// so later fetches replace forecasts with WeatherKit's revised history.
func (s *Store) SaveWeather(ctx context.Context, runID int64, r weather.Report) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save weather: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	c := r.Current
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO weather_current (run_id, as_of, condition, temperature, apparent_temperature,
			humidity, precipitation_intensity, wind_speed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET as_of = excluded.as_of, condition = excluded.condition,
			temperature = excluded.temperature, apparent_temperature = excluded.apparent_temperature,
			humidity = excluded.humidity, precipitation_intensity = excluded.precipitation_intensity,
			wind_speed = excluded.wind_speed`,
		runID, c.AsOf.UTC().Format(time.RFC3339), c.Condition, c.Temperature, c.ApparentTemperature,
		c.Humidity, c.PrecipitationIntensity, c.WindSpeed); err != nil {
		return fmt.Errorf("save current weather: %w", err)
	}
	fetchedAt := s.timestamp()
	for _, h := range r.Hours {
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO weather_hours (hour_start, condition, temperature, precipitation_chance,
				precipitation_amount, fetched_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(hour_start) DO UPDATE SET condition = excluded.condition,
				temperature = excluded.temperature, precipitation_chance = excluded.precipitation_chance,
				precipitation_amount = excluded.precipitation_amount, fetched_at = excluded.fetched_at`,
			h.Start.UTC().Format(time.RFC3339), h.Condition, h.Temperature, h.PrecipitationChance,
			h.PrecipitationAmount, fetchedAt); err != nil {
			return fmt.Errorf("save weather hour: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("save weather: %w", err)
	}
	return nil
}

// LatestWeather returns the most recently observed conditions and up to upcoming hourly
// forecasts starting after them.
func (s *Store) LatestWeather(ctx context.Context, upcoming int) (weather.Report, bool, error) {
	var r weather.Report
	var asOf string
	err := s.db.QueryRowContext(ctx, `
		SELECT as_of, condition, temperature, apparent_temperature, humidity, precipitation_intensity, wind_speed
		FROM weather_current ORDER BY as_of DESC, run_id DESC LIMIT 1`).Scan(
		&asOf, &r.Current.Condition, &r.Current.Temperature, &r.Current.ApparentTemperature,
		&r.Current.Humidity, &r.Current.PrecipitationIntensity, &r.Current.WindSpeed)
	if errors.Is(err, sql.ErrNoRows) {
		return weather.Report{}, false, nil
	}
	if err != nil {
		return weather.Report{}, false, fmt.Errorf("latest weather: %w", err)
	}
	if r.Current.AsOf, err = time.Parse(time.RFC3339, asOf); err != nil {
		return weather.Report{}, false, fmt.Errorf("parse weather time: %w", err)
	}
	r.Hours, err = s.weatherHours(ctx, `WHERE hour_start > ? ORDER BY hour_start LIMIT ?`, asOf, upcoming)
	if err != nil {
		return weather.Report{}, false, err
	}
	return r, true, nil
}

// WeatherHours returns the whole stored hourly series, oldest first.
func (s *Store) WeatherHours(ctx context.Context) ([]weather.Hour, error) {
	return s.weatherHours(ctx, `ORDER BY hour_start`)
}

func (s *Store) weatherHours(ctx context.Context, clause string, args ...any) ([]weather.Hour, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT hour_start, condition, temperature, precipitation_chance, precipitation_amount
		FROM weather_hours `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("weather hours: %w", err)
	}
	defer rows.Close()
	var hours []weather.Hour
	for rows.Next() {
		var h weather.Hour
		var start string
		if err := rows.Scan(&start, &h.Condition, &h.Temperature, &h.PrecipitationChance, &h.PrecipitationAmount); err != nil {
			return nil, fmt.Errorf("weather hours: %w", err)
		}
		if h.Start, err = time.Parse(time.RFC3339, start); err != nil {
			return nil, fmt.Errorf("parse weather hour: %w", err)
		}
		hours = append(hours, h)
	}
	return hours, rows.Err()
}

// Row is one mission snapshot with the change in completions since that mission's previous snapshot.
type Row struct {
	RunID             int64
	RunStartedAt      time.Time
	FetchedAt         time.Time
	MissionOrder      int
	MissionGUID       string
	Title             string
	Author            string
	Faction           string
	RatingE6          int64
	CompletedCount    int64
	CompletedDelta    sql.NullInt64
	AverageDurationMs int64
}

func (s *Store) Rows(ctx context.Context) ([]Row, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.run_id, r.started_at, s.fetched_at, s.mission_order, s.mission_guid, s.title,
			s.author, s.faction, s.rating_e6, s.completed_count,
			s.completed_count - LAG(s.completed_count) OVER (
				PARTITION BY s.mission_guid ORDER BY s.fetched_at, s.run_id),
			s.average_duration_ms
		FROM snapshots s JOIN runs r ON r.id = s.run_id
		ORDER BY s.run_id, s.mission_order`)
	if err != nil {
		return nil, fmt.Errorf("query snapshots: %w", err)
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		var r Row
		var started, fetched string
		if err := rows.Scan(&r.RunID, &started, &fetched, &r.MissionOrder, &r.MissionGUID, &r.Title,
			&r.Author, &r.Faction, &r.RatingE6, &r.CompletedCount, &r.CompletedDelta,
			&r.AverageDurationMs); err != nil {
			return nil, fmt.Errorf("scan snapshot: %w", err)
		}
		if r.RunStartedAt, err = time.Parse(time.RFC3339, started); err != nil {
			return nil, fmt.Errorf("parse run time: %w", err)
		}
		if r.FetchedAt, err = time.Parse(time.RFC3339, fetched); err != nil {
			return nil, fmt.Errorf("parse fetch time: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MissionChange compares a mission's snapshot in one run with a baseline snapshot.
type MissionChange struct {
	Order         int
	GUID          string
	Title         string
	Completed     int64
	RatingE6      int64
	PrevCompleted sql.NullInt64
	PrevRatingE6  sql.NullInt64
}

// Changed reports whether the snapshot differs from an earlier one; a first snapshot is not a change.
func (m MissionChange) Changed() bool {
	return m.PrevCompleted.Valid && (m.Completed != m.PrevCompleted.Int64 || m.RatingE6 != m.PrevRatingE6.Int64)
}

// ChangesSince compares each mission in runID with its latest snapshot taken at or before
// baselineRunID (which must be earlier than runID).
func (s *Store) ChangesSince(ctx context.Context, runID, baselineRunID int64) ([]MissionChange, error) {
	if baselineRunID >= runID {
		return nil, fmt.Errorf("baseline run %d is not before run %d", baselineRunID, runID)
	}
	// Each mission's latest snapshot up to runID, so a mission skipped or failed in runID still
	// counts with its last known values instead of dropping out of the totals.
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.mission_order, s.mission_guid, s.title, s.completed_count, s.rating_e6,
			p.completed_count, p.rating_e6
		FROM snapshots s
		LEFT JOIN snapshots p ON p.mission_guid = s.mission_guid AND p.run_id = (
			SELECT MAX(run_id) FROM snapshots WHERE mission_guid = s.mission_guid AND run_id <= ?)
		WHERE s.run_id = (SELECT MAX(run_id) FROM snapshots WHERE mission_guid = s.mission_guid AND run_id <= ?)
		ORDER BY s.mission_order`, baselineRunID, runID)
	if err != nil {
		return nil, fmt.Errorf("query run changes: %w", err)
	}
	defer rows.Close()
	var out []MissionChange
	for rows.Next() {
		var m MissionChange
		if err := rows.Scan(&m.Order, &m.GUID, &m.Title, &m.Completed, &m.RatingE6,
			&m.PrevCompleted, &m.PrevRatingE6); err != nil {
			return nil, fmt.Errorf("scan run change: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// BaselineRun returns the latest run with snapshots that started at least `ago` before runID.
// While the history is shorter than that, it falls back to the earliest run with snapshots.
func (s *Store) BaselineRun(ctx context.Context, runID int64, ago time.Duration) (baselineID int64, startedAt time.Time, ok bool, err error) {
	var current string
	if err := s.db.QueryRowContext(ctx, `SELECT started_at FROM runs WHERE id = ?`, runID).Scan(&current); err != nil {
		return 0, time.Time{}, false, fmt.Errorf("read run %d: %w", runID, err)
	}
	currentAt, err := time.Parse(time.RFC3339, current)
	if err != nil {
		return 0, time.Time{}, false, fmt.Errorf("parse run %d time: %w", runID, err)
	}
	// Timestamps are stored as fixed-width UTC RFC3339, so string order is time order.
	threshold := currentAt.Add(-ago).UTC().Format(time.RFC3339)
	const withSnapshots = `id < ? AND EXISTS (SELECT 1 FROM snapshots WHERE run_id = runs.id)`
	var at string
	err = s.db.QueryRowContext(ctx,
		`SELECT id, started_at FROM runs WHERE `+withSnapshots+` AND started_at <= ? ORDER BY id DESC LIMIT 1`,
		runID, threshold).Scan(&baselineID, &at)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.db.QueryRowContext(ctx,
			`SELECT id, started_at FROM runs WHERE `+withSnapshots+` ORDER BY id ASC LIMIT 1`,
			runID).Scan(&baselineID, &at)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, fmt.Errorf("find baseline run: %w", err)
	}
	if startedAt, err = time.Parse(time.RFC3339, at); err != nil {
		return 0, time.Time{}, false, fmt.Errorf("parse baseline time: %w", err)
	}
	return baselineID, startedAt, true, nil
}

// LatestRun returns the most recent run that stored snapshots.
func (s *Store) LatestRun(ctx context.Context) (int64, bool, error) {
	var runID sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(run_id) FROM snapshots`).Scan(&runID); err != nil {
		return 0, false, fmt.Errorf("latest run: %w", err)
	}
	return runID.Int64, runID.Valid, nil
}

// LastUpdateSentAt returns when the most recent Telegram update was sent, if any.
func (s *Store) LastUpdateSentAt(ctx context.Context) (time.Time, bool, error) {
	var at string
	err := s.db.QueryRowContext(ctx, `SELECT sent_at FROM last_update WHERE id = 1`).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read last update: %w", err)
	}
	sentAt, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse last update time: %w", err)
	}
	return sentAt, true, nil
}

func (s *Store) MarkUpdateSent(ctx context.Context, runID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO last_update (id, run_id, sent_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET run_id = excluded.run_id, sent_at = excluded.sent_at`,
		runID, at.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("mark update sent: %w", err)
	}
	return nil
}

// AlertSentAt returns when the alert with key was last sent, if it is active.
func (s *Store) AlertSentAt(ctx context.Context, key string) (time.Time, bool, error) {
	var sentAt string
	err := s.db.QueryRowContext(ctx, `SELECT sent_at FROM alerts WHERE key = ?`, key).Scan(&sentAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read alert %s: %w", key, err)
	}
	t, err := time.Parse(time.RFC3339, sentAt)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse alert %s time: %w", key, err)
	}
	return t, true, nil
}

func (s *Store) MarkAlertSent(ctx context.Context, key string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO alerts (key, sent_at) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET sent_at = excluded.sent_at`,
		key, at.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("mark alert %s: %w", key, err)
	}
	return nil
}

func (s *Store) ClearAlert(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM alerts WHERE key = ?`, key); err != nil {
		return fmt.Errorf("clear alert %s: %w", key, err)
	}
	return nil
}
