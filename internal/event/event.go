// Package event loads the description of one Mission Day event (event.json): its name, time zone,
// weather point and the optional map area to zoom into. Each event lives in its own directory
// together with its mission list, map geometry and database.
package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// FileName is the event description inside an event directory.
const FileName = "event.json"

// Text is a phrase in the two languages the bot and charts use.
type Text struct {
	EN string `json:"en"`
	ZH string `json:"zh"`
}

// Weather is the point WeatherKit is asked about and how the area is named in messages.
type Weather struct {
	Lat  float64 `json:"lat"`
	Lng  float64 `json:"lng"`
	Area Text    `json:"area"`
}

// MapFocus is a dense group of missions the map shows in a second, zoomed-in panel.
type MapFocus struct {
	Label  Text  `json:"label"`
	Orders []int `json:"orders"`
}

type Event struct {
	Slug     string    `json:"slug"`
	Name     Text      `json:"name"`
	Timezone string    `json:"timezone"`
	Weather  *Weather  `json:"weather,omitempty"`
	MapFocus *MapFocus `json:"mapFocus,omitempty"`

	// Dir is the event directory the description was loaded from.
	Dir string `json:"-"`
	// Location is Timezone, loaded.
	Location *time.Location `json:"-"`
}

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Resolve turns an -event argument into a directory: a bare name is looked up under
// configDir/events, anything containing a path separator is used as given.
func Resolve(configDir, arg string) (string, error) {
	if arg == "" {
		return "", errors.New("-event is required (an event name under DIR/events, or a path)")
	}
	if filepath.Base(arg) == arg && arg != "." && arg != ".." {
		return filepath.Join(configDir, "events", arg), nil
	}
	return filepath.Clean(arg), nil
}

// Load reads and validates dir/event.json.
func Load(dir string) (*Event, error) {
	path := filepath.Join(dir, FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read event: %w", err)
	}
	e, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	e.Dir = dir
	return e, nil
}

// Parse validates an event description. Unknown fields are rejected so a typo cannot silently
// drop a setting.
func Parse(raw []byte) (*Event, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var e Event
	if err := decoder.Decode(&e); err != nil {
		return nil, fmt.Errorf("invalid event: %w", err)
	}
	if decoder.More() {
		return nil, errors.New("invalid event: trailing data after the JSON object")
	}
	if !slugPattern.MatchString(e.Slug) {
		return nil, fmt.Errorf("slug %q must be lower-case letters, digits and single hyphens", e.Slug)
	}
	if err := e.Name.validate("name"); err != nil {
		return nil, err
	}
	if e.Timezone == "" {
		return nil, errors.New("timezone is required (an IANA name such as Asia/Shanghai)")
	}
	loc, err := time.LoadLocation(e.Timezone)
	if err != nil {
		return nil, fmt.Errorf("timezone: %w", err)
	}
	e.Location = loc
	if w := e.Weather; w != nil {
		if math.IsNaN(w.Lat) || math.Abs(w.Lat) > 90 || math.IsNaN(w.Lng) || math.Abs(w.Lng) > 180 {
			return nil, fmt.Errorf("weather point %v,%v is out of range", w.Lat, w.Lng)
		}
		if err := w.Area.validate("weather.area"); err != nil {
			return nil, err
		}
	}
	if f := e.MapFocus; f != nil {
		if err := f.Label.validate("mapFocus.label"); err != nil {
			return nil, err
		}
		if len(f.Orders) == 0 {
			return nil, errors.New("mapFocus.orders must list at least one mission")
		}
		seen := map[int]bool{}
		for _, order := range f.Orders {
			if order < 1 || seen[order] {
				return nil, fmt.Errorf("mapFocus.orders: %d is not a new mission order", order)
			}
			seen[order] = true
		}
	}
	return &e, nil
}

func (t Text) validate(field string) error {
	if t.EN == "" || t.ZH == "" {
		return fmt.Errorf("%s needs both en and zh text", field)
	}
	return nil
}

// Path returns a file inside the event directory.
func (e *Event) Path(name ...string) string {
	return filepath.Join(append([]string{e.Dir}, name...)...)
}
