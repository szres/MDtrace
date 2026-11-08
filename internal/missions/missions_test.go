package missions

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSortsByOrder(t *testing.T) {
	got, err := Parse([]byte(`{"kind":"mission-links","missions":[
		{"order":2,"missionGuid":"b.1c","title":"B"},
		{"order":1,"missionGuid":" a.1c ","title":"A"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].GUID != "a.1c" || got[1].GUID != "b.1c" {
		t.Fatalf("unexpected missions: %+v", got)
	}
}

func TestParseRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"wrong kind": `{"kind":"other","missions":[{"order":1,"missionGuid":"a"}]}`,
		"empty":      `{"kind":"mission-links","missions":[]}`,
		"no guid":    `{"kind":"mission-links","missions":[{"order":1,"title":"x"}]}`,
		"duplicate":  `{"kind":"mission-links","missions":[{"order":1,"missionGuid":"a"},{"order":2,"missionGuid":"a"}]}`,
		"bad json":   `{`,
	}
	for name, input := range cases {
		if _, err := Parse([]byte(input)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestLoadShippedEventMissionFiles(t *testing.T) {
	paths, err := filepath.Glob("../../events/*/missions.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no events/*/missions.json found")
	}
	for _, path := range paths {
		got, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			t.Errorf("%s has no missions", path)
		}
		for _, m := range got {
			if !strings.HasSuffix(m.GUID, ".1c") {
				t.Errorf("%s: unexpected guid format %q", path, m.GUID)
			}
		}
	}
	zhuhai, err := Load("../../events/md2026-zhuhai/missions.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(zhuhai) != 18 {
		t.Fatalf("got %d Zhuhai missions, want 18", len(zhuhai))
	}
}
