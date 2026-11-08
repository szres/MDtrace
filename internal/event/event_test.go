package event

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const valid = `{
  "slug": "md2026-zhuhai",
  "name": {"en": "MD 2026 Zhuhai", "zh": "MD 2026 珠海"},
  "timezone": "Asia/Shanghai",
  "weather": {"lat": 22.27, "lng": 113.58, "area": {"en": "the 情侣路 cluster", "zh": "情侣路一带"}},
  "mapFocus": {"label": {"en": "情侣路 cluster", "zh": "情侣路片区"}, "orders": [1, 2, 3]}
}`

func TestParseValidEvent(t *testing.T) {
	e, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if e.Slug != "md2026-zhuhai" || e.Name.ZH != "MD 2026 珠海" || e.Location.String() != "Asia/Shanghai" {
		t.Fatalf("unexpected event %+v", e)
	}
	if e.Weather.Lat != 22.27 || len(e.MapFocus.Orders) != 3 {
		t.Fatalf("unexpected weather or focus %+v %+v", e.Weather, e.MapFocus)
	}
}

func TestOptionalSectionsMayBeLeftOut(t *testing.T) {
	e, err := Parse([]byte(`{"slug": "md2027-x", "name": {"en": "X", "zh": "X"}, "timezone": "UTC"}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.Weather != nil || e.MapFocus != nil {
		t.Fatalf("expected no weather or focus, got %+v", e)
	}
}

func TestParseRejectsBadEvents(t *testing.T) {
	cases := map[string]string{
		"unknown field":   strings.Replace(valid, `"slug"`, `"slgu": "x", "slug"`, 1),
		"bad slug":        strings.Replace(valid, "md2026-zhuhai", "MD 2026", 1),
		"missing zh name": strings.Replace(valid, `"zh": "MD 2026 珠海"`, `"zh": ""`, 1),
		"bad timezone":    strings.Replace(valid, "Asia/Shanghai", "Mars/Olympus", 1),
		"no timezone":     strings.Replace(valid, `"timezone": "Asia/Shanghai",`, ``, 1),
		"latitude":        strings.Replace(valid, "22.27", "122.27", 1),
		"area text":       strings.Replace(valid, `"zh": "情侣路一带"`, `"zh": ""`, 1),
		"duplicate order": strings.Replace(valid, "[1, 2, 3]", "[1, 2, 2]", 1),
		"zero order":      strings.Replace(valid, "[1, 2, 3]", "[0]", 1),
		"empty focus":     strings.Replace(valid, "[1, 2, 3]", "[]", 1),
		"trailing data":   valid + "{}",
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestResolve(t *testing.T) {
	got, err := Resolve("/cfg", "md2026-zhuhai")
	if err != nil || got != filepath.Join("/cfg", "events", "md2026-zhuhai") {
		t.Fatalf("bare name: %q %v", got, err)
	}
	if got, _ := Resolve("/cfg", "./events/x/"); got != "events/x" {
		t.Fatalf("path: %q", got)
	}
	if _, err := Resolve("/cfg", ""); err == nil {
		t.Fatal("empty -event must fail")
	}
}

func TestLoadKeepsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e.Path("geo", "routes.json") != filepath.Join(dir, "geo", "routes.json") {
		t.Fatalf("path: %s", e.Path("geo", "routes.json"))
	}
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("a directory without event.json must fail")
	}
}
