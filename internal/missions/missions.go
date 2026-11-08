// Package missions loads the reviewed mission list exported by the mission-links tool.
package missions

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

type Mission struct {
	Order       int    `json:"order"`
	DisplayName string `json:"displayName"`
	GUID        string `json:"missionGuid"`
	Title       string `json:"title"`
}

type linksFile struct {
	Kind     string    `json:"kind"`
	Missions []Mission `json:"missions"`
}

// Load reads a mission-links.json file and returns its missions sorted by order.
func Load(path string) ([]Mission, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read missions file: %w", err)
	}
	return Parse(raw)
}

func Parse(raw []byte) ([]Mission, error) {
	var file linksFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse missions file: %w", err)
	}
	if file.Kind != "mission-links" {
		return nil, fmt.Errorf("missions file kind is %q, want \"mission-links\"", file.Kind)
	}
	if len(file.Missions) == 0 {
		return nil, fmt.Errorf("missions file contains no missions")
	}

	seen := make(map[string]bool, len(file.Missions))
	for i := range file.Missions {
		m := &file.Missions[i]
		m.GUID = strings.TrimSpace(m.GUID)
		if m.GUID == "" {
			return nil, fmt.Errorf("mission #%d (%q) has no missionGuid", i+1, m.Title)
		}
		if seen[m.GUID] {
			return nil, fmt.Errorf("duplicate missionGuid %s", m.GUID)
		}
		seen[m.GUID] = true
	}

	sort.SliceStable(file.Missions, func(a, b int) bool {
		return file.Missions[a].Order < file.Missions[b].Order
	})
	return file.Missions, nil
}
