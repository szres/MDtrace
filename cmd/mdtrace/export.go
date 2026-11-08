package main

import (
	"encoding/csv"
	"io"
	"strconv"
	"time"

	"mdtrace/internal/store"
	"mdtrace/internal/weather"
)

var exportHeader = []string{
	"run_id", "run_started_at", "fetched_at", "mission_order", "mission_guid", "title",
	"completed_count", "completed_delta", "rating_percent", "rating_e6",
	"author", "faction", "average_duration_ms",
}

// writeCSV writes snapshots in long format with a UTF-8 BOM so Excel shows Chinese titles correctly.
func writeCSV(w io.Writer, rows []store.Row, loc *time.Location) error {
	if _, err := io.WriteString(w, "\ufeff"); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(exportHeader); err != nil {
		return err
	}
	for _, r := range rows {
		delta := ""
		if r.CompletedDelta.Valid {
			delta = strconv.FormatInt(r.CompletedDelta.Int64, 10)
		}
		record := []string{
			strconv.FormatInt(r.RunID, 10),
			r.RunStartedAt.In(loc).Format(time.RFC3339),
			r.FetchedAt.In(loc).Format(time.RFC3339),
			strconv.Itoa(r.MissionOrder),
			r.MissionGUID,
			r.Title,
			strconv.FormatInt(r.CompletedCount, 10),
			delta,
			strconv.FormatFloat(float64(r.RatingE6)/10000, 'f', 4, 64),
			strconv.FormatInt(r.RatingE6, 10),
			r.Author,
			r.Faction,
			strconv.FormatInt(r.AverageDurationMs, 10),
		}
		if err := cw.Write(record); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

var weatherHeader = []string{"hour_start", "condition", "temperature", "precipitation_chance", "precipitation_amount"}

// writeWeatherCSV writes the stored hourly weather series (temperature in °C, rain in mm).
func writeWeatherCSV(w io.Writer, hours []weather.Hour, loc *time.Location) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(weatherHeader); err != nil {
		return err
	}
	for _, h := range hours {
		record := []string{
			h.Start.In(loc).Format(time.RFC3339),
			h.Condition,
			strconv.FormatFloat(h.Temperature, 'f', 2, 64),
			strconv.FormatFloat(h.PrecipitationChance, 'f', 2, 64),
			strconv.FormatFloat(h.PrecipitationAmount, 'f', 2, 64),
		}
		if err := cw.Write(record); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
