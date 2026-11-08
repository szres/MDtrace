package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mdtrace/internal/event"
	"mdtrace/internal/missions"
)

// copyEvent copies the shipped Zhuhai event into a temporary directory the test may break.
func copyEvent(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "events", "md2026-zhuhai")
	dst := t.TempDir()
	for _, name := range []string{"event.json", "missions.json", "geo/routes.json", "geo/coastline.json"} {
		raw, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dst, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func loadForCheck(t *testing.T, dir string) (*event.Event, []missions.Mission) {
	t.Helper()
	ev, err := event.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := missions.Load(ev.Path("missions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return ev, list
}

func TestCheckShippedEvent(t *testing.T) {
	ev, list := loadForCheck(t, copyEvent(t))
	var out bytes.Buffer
	if err := checkEvent(&out, ev, list); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"md2026-zhuhai · MD 2026 Zhuhai / MD 2026 珠海", "time zone  Asia/Shanghai",
		"missions   18", "weather    22.2738, 113.5790", "map        18 routes, 184 coastline ways, zoom on 情侣路 cluster (#01"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary missing %q:\n%s", want, out.String())
		}
	}
}

func TestCheckWithoutGeoTurnsMapsOff(t *testing.T) {
	dir := copyEvent(t)
	if err := os.RemoveAll(filepath.Join(dir, "geo")); err != nil {
		t.Fatal(err)
	}
	ev, list := loadForCheck(t, dir)
	var out bytes.Buffer
	if err := checkEvent(&out, ev, list); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "map        off") {
		t.Fatalf("expected the map to be off:\n%s", out.String())
	}
}

func TestCheckRejectsInconsistentEvents(t *testing.T) {
	cases := map[string]func(dir string) error{
		"half of geo": func(dir string) error { return os.Remove(filepath.Join(dir, "geo", "coastline.json")) },
		"missing route": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "geo", "routes.json"), []byte(`{"routes": []}`), 0o600)
		},
		"focus order not a mission": func(dir string) error {
			path := filepath.Join(dir, "event.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(path, bytes.Replace(raw, []byte("[1, 2, 3,"), []byte("[99, 2, 3,"), 1), 0o600)
		},
	}
	for name, breakIt := range cases {
		dir := copyEvent(t)
		if err := breakIt(dir); err != nil {
			t.Fatal(err)
		}
		ev, list := loadForCheck(t, dir)
		if err := checkEvent(&bytes.Buffer{}, ev, list); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
