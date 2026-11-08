package intel

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MissionDetail holds the fields of Intel's positional getMissionDetails result:
// [0]=guid [1]=title [2]=description [3]=author [4]=faction [5]=ratingE6
// [6]=averageDurationMs [7]=completedCount ...
type MissionDetail struct {
	GUID              string
	Title             string
	Author            string
	Faction           string
	RatingE6          int64
	AverageDurationMs int64
	CompletedCount    int64
}

// RatingPercent converts the millionths-based rating into a percentage (1000000 -> 100).
func (d MissionDetail) RatingPercent() float64 {
	return float64(d.RatingE6) / 10000
}

const minResultFields = 8

func ParseMissionDetail(body []byte) (MissionDetail, error) {
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return MissionDetail{}, fmt.Errorf("decode response: %w (body: %s)", err, preview(body))
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return MissionDetail{}, fmt.Errorf("intel error: %s", string(envelope.Error))
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return MissionDetail{}, errors.New("intel returned an empty result")
	}

	var fields []json.RawMessage
	if err := json.Unmarshal(envelope.Result, &fields); err != nil {
		return MissionDetail{}, fmt.Errorf("result is not an array: %w", err)
	}
	if len(fields) < minResultFields {
		return MissionDetail{}, fmt.Errorf("result has %d fields, want at least %d", len(fields), minResultFields)
	}

	var d MissionDetail
	var err error
	if d.GUID, err = stringField(fields, 0, "guid"); err != nil {
		return MissionDetail{}, err
	}
	if d.Title, err = stringField(fields, 1, "title"); err != nil {
		return MissionDetail{}, err
	}
	if d.Author, err = stringField(fields, 3, "author"); err != nil {
		return MissionDetail{}, err
	}
	if d.Faction, err = stringField(fields, 4, "faction"); err != nil {
		return MissionDetail{}, err
	}
	d.Faction = normalizeFaction(d.Faction)
	if d.RatingE6, err = intField(fields, 5, "ratingE6"); err != nil {
		return MissionDetail{}, err
	}
	if d.AverageDurationMs, err = intField(fields, 6, "averageDurationMs"); err != nil {
		return MissionDetail{}, err
	}
	if d.CompletedCount, err = intField(fields, 7, "completedCount"); err != nil {
		return MissionDetail{}, err
	}
	if d.GUID == "" {
		return MissionDetail{}, errors.New("result has an empty mission guid")
	}
	if d.RatingE6 < 0 || d.RatingE6 > 1_000_000 {
		return MissionDetail{}, fmt.Errorf("ratingE6 %d out of range", d.RatingE6)
	}
	if d.CompletedCount < 0 {
		return MissionDetail{}, fmt.Errorf("completedCount %d is negative", d.CompletedCount)
	}
	return d, nil
}

func stringField(fields []json.RawMessage, index int, name string) (string, error) {
	var value *string
	if err := json.Unmarshal(fields[index], &value); err != nil {
		return "", fmt.Errorf("result[%d] (%s): %w", index, name, err)
	}
	if value == nil {
		return "", nil
	}
	return *value, nil
}

func intField(fields []json.RawMessage, index int, name string) (int64, error) {
	var value json.Number
	if err := json.Unmarshal(fields[index], &value); err != nil {
		return 0, fmt.Errorf("result[%d] (%s): %w", index, name, err)
	}
	n, err := value.Int64()
	if err != nil {
		f, ferr := value.Float64()
		if ferr != nil {
			return 0, fmt.Errorf("result[%d] (%s): %q is not a number", index, name, value)
		}
		n = int64(f)
	}
	return n, nil
}

func normalizeFaction(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "R", "RES", "RESISTANCE":
		return "RESISTANCE"
	case "E", "ENL", "ENLIGHTENED":
		return "ENLIGHTENED"
	case "M", "MACHINA":
		return "MACHINA"
	case "N", "NEUTRAL", "NONE":
		return "NEUTRAL"
	default:
		return strings.ToUpper(strings.TrimSpace(value))
	}
}
