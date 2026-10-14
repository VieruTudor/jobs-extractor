package collector

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Settings struct {
	Preferences map[string]any `json:"preferences"`
	Collection  map[string]any `json:"collection"`
	Geocoding   map[string]any `json:"geocoding"`
	SourcesFile string         `json:"sources_file"`
	Database    string         `json:"database"`
	OutputDir   string         `json:"output_directory"`
}

type SourceList struct {
	Feeds  []map[string]any `json:"feeds"`
	Boards []map[string]any `json:"boards"`
}

type Options struct {
	Command string
	Config  string
	Boards  []string
}

type RunReport struct {
	StartedAt  string              `json:"started_at"`
	FinishedAt string              `json:"finished_at"`
	Command    string              `json:"command"`
	Sources    []map[string]any    `json:"sources"`
	Events     []map[string]string `json:"events"`
	Geocoding  map[string]any      `json:"geocoding,omitempty"`
}

func LoadSettings(configPath string) (Settings, string, error) {
	absolutePath, err := filepath.Abs(configPath)
	if err != nil {
		return Settings{}, "", fmt.Errorf("resolve config path: %w", err)
	}
	data, err := os.ReadFile(absolutePath)
	if err != nil {
		return Settings{}, "", fmt.Errorf("read config: %w", err)
	}
	var settings Settings
	if err := json.Unmarshal(data, &settings); err != nil {
		return Settings{}, "", fmt.Errorf("parse config: %w", err)
	}
	if settings.SourcesFile == "" || settings.Database == "" || settings.OutputDir == "" {
		return Settings{}, "", fmt.Errorf("config must define sources_file, database, and output_directory")
	}
	return settings, filepath.Dir(absolutePath), nil
}

func numberSetting(values map[string]any, key string, fallback int) int {
	value, ok := values[key]
	if !ok {
		return fallback
	}
	switch n := value.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return fallback
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func listValue(value any) []any {
	items, _ := value.([]any)
	return items
}

func stringList(value any) []string {
	items := listValue(value)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func objectValue(value any) map[string]any {
	object, _ := value.(map[string]any)
	if object == nil {
		return map[string]any{}
	}
	return object
}

func enabled(source map[string]any) bool {
	value, exists := source["enabled"]
	if !exists {
		return true
	}
	active, ok := value.(bool)
	return !ok || active
}
