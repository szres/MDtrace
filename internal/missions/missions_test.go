package missions

import (
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

func TestLoadRepositoryMissionFile(t *testing.T) {
	got, err := Load("../../missions.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 18 {
		t.Fatalf("got %d missions, want 18", len(got))
	}
	for _, m := range got {
		if !strings.HasSuffix(m.GUID, ".1c") {
			t.Errorf("unexpected guid format %q", m.GUID)
		}
	}
}
