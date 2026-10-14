package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func Run(options Options) error {
	settings, base, err := LoadSettings(options.Config)
	if err != nil {
		return err
	}
	if err := validateSettings(settings); err != nil {
		return err
	}
	lockPath := filepath.Join(base, ".collector.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("collector lock exists at %s; check for an active collector before removing it", lockPath)
		}
		return err
	}
	lock.Close()
	defer os.Remove(lockPath)

	databasePath := filepath.Join(base, settings.Database)
	store, err := OpenStore(databasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	outputDir := filepath.Join(base, settings.OutputDir)
	statusPath := filepath.Join(outputDir, "run-status.json")
	report := map[string]any{
		"started_at": timestamp(), "sources": []map[string]any{}, "events": []map[string]string{}, "command": options.Command,
	}

	if options.Command == "collect" || options.Command == "collect-boards" {
		if err := collectSources(context.Background(), store, settings, base, options, report); err != nil {
			return err
		}
	} else if options.Command == "export" {
		if previous, err := readReport(statusPath); err == nil {
			for key, value := range previous {
				report[key] = value
			}
			report["exported_at"] = timestamp()
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		return fmt.Errorf("unsupported collector command %q", options.Command)
	}
	if report["finished_at"] == nil {
		report["finished_at"] = timestamp()
	}
	count, err := Export(context.Background(), store, settings, base, report)
	if err != nil {
		return err
	}
	fmt.Printf("Exported %d listings to %s\n", count, outputDir)
	if sourceFailed(report) {
		return fmt.Errorf("one or more sources failed or returned partial results")
	}
	return nil
}

func collectSources(ctx context.Context, store *Store, settings Settings, base string, options Options, report map[string]any) error {
	sourcesPath := filepath.Join(base, settings.SourcesFile)
	data, err := os.ReadFile(sourcesPath)
	if err != nil {
		return fmt.Errorf("read sources file: %w", err)
	}
	var sources SourceList
	if err := json.Unmarshal(data, &sources); err != nil {
		return fmt.Errorf("parse sources file: %w", err)
	}
	selected := make(map[string]bool, len(options.Boards))
	for _, board := range options.Boards {
		selected[board] = true
	}
	if options.Command == "collect-boards" && len(selected) > 0 {
		found := make(map[string]bool)
		for _, board := range sources.Boards {
			key := stringValue(board["provider"]) + ":" + stringValue(board["slug"])
			if selected[key] {
				found[key] = true
			}
		}
		for key := range selected {
			if !found[key] {
				return fmt.Errorf("unknown board %q", key)
			}
		}
	}

	client := newHTTPClient(settings.Collection)
	configuredSources := make([]struct {
		config map[string]any
		board  bool
	}, 0, len(sources.Feeds)+len(sources.Boards))
	if options.Command == "collect" {
		for _, feed := range sources.Feeds {
			configuredSources = append(configuredSources, struct {
				config map[string]any
				board  bool
			}{feed, false})
		}
	}
	for _, board := range sources.Boards {
		key := stringValue(board["provider"]) + ":" + stringValue(board["slug"])
		if len(selected) > 0 && !selected[key] {
			continue
		}
		configuredSources = append(configuredSources, struct {
			config map[string]any
			board  bool
		}{board, true})
	}

	sourceReports := make([]map[string]any, 0, len(configuredSources))
	events := make([]map[string]string, 0)
	for _, item := range configuredSources {
		if !enabled(item.config) {
			continue
		}
		name := stringValue(item.config["type"])
		if item.board {
			name = stringValue(item.config["provider"]) + ":" + stringValue(item.config["slug"])
		}
		fmt.Printf("Collecting %s...\n", name)
		result, collectErr := collectConfiguredSource(ctx, client, item.config, item.board, settings.Collection, settings.Preferences)
		if result.Status == "" {
			result.Status = "ok"
		}
		sourceReport := map[string]any{"source": name, "status": result.Status, "count": len(result.Jobs)}
		if result.Reason != "" {
			sourceReport["reason"] = result.Reason
		}
		if result.Coverage != "" {
			sourceReport["coverage"] = result.Coverage
		}
		if collectErr != nil {
			sourceReport["status"] = "failed"
			sourceReport["error"] = collectErr.Error()
		} else if !result.Complete && result.Status == "ok" {
			sourceReport["status"] = "partial"
		}
		if result.Status != "skipped" {
			targeted := !item.board && name == "himalayas" && result.Coverage != ""
			completeSnapshot := result.Complete && !targeted && collectErr == nil
			ingestEvents, err := store.Ingest(name, result.Jobs, completeSnapshot, timestamp())
			if err != nil {
				sourceReport["status"] = "failed"
				sourceReport["error"] = err.Error()
			} else {
				events = append(events, ingestEvents...)
			}
		}
		sourceReports = append(sourceReports, sourceReport)
		if item.board {
			delay := numberSetting(settings.Collection, "board_delay_seconds", 0)
			if delay > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(delay) * time.Second):
				}
			}
		}
	}
	if options.Command == "collect-boards" && len(selected) > 0 {
		if previous, err := readReport(filepath.Join(base, settings.OutputDir, "run-status.json")); err == nil {
			boardKeys := map[string]bool{}
			for _, board := range sources.Boards {
				boardKeys[stringValue(board["provider"])+":"+stringValue(board["slug"])] = true
			}
			for _, previousSource := range mapSlice(previous["sources"]) {
				key := stringValue(previousSource["source"])
				if boardKeys[key] && !selected[key] {
					sourceReports = append([]map[string]any{previousSource}, sourceReports...)
				}
			}
			for _, previousEvent := range mapSlice(previous["events"]) {
				key := stringValue(previousEvent["source"])
				if boardKeys[key] && !selected[key] {
					events = append(events, map[string]string{"source": key, "source_id": stringValue(previousEvent["source_id"]), "change": stringValue(previousEvent["change"])})
				}
			}
		}
	}
	report["sources"], report["events"] = sourceReports, events
	return nil
}

func validateSettings(settings Settings) error {
	for _, key := range []string{"timeout_seconds", "max_pages"} {
		if numberSetting(settings.Collection, key, 1) <= 0 {
			return fmt.Errorf("collection.%s must be positive", key)
		}
	}
	for _, key := range []string{"retries", "page_delay_seconds"} {
		if numberSetting(settings.Collection, key, 0) < 0 {
			return fmt.Errorf("collection.%s cannot be negative", key)
		}
	}
	return nil
}

func sourceFailed(report map[string]any) bool {
	for _, source := range mapSlice(report["sources"]) {
		status := stringValue(source["status"])
		if status != "ok" && status != "skipped" {
			return true
		}
	}
	return false
}

func readReport(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, err
	}
	return report, nil
}

func DefaultConfigPath() string {
	workingDir, err := os.Getwd()
	if err != nil {
		return "config.json"
	}
	for dir := workingDir; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "config.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "config.json"
		}
	}
}

func ParseOptions(args []string) (Options, error) {
	options := Options{Config: DefaultConfigPath()}
	if len(args) == 0 {
		options.Command = "serve"
		return options, nil
	}
	options.Command = args[0]
	for index := 1; index < len(args); index++ {
		switch args[index] {
		case "--config":
			if index+1 >= len(args) {
				return Options{}, fmt.Errorf("--config requires a path")
			}
			index++
			options.Config = args[index]
		case "--board":
			if index+1 >= len(args) {
				return Options{}, fmt.Errorf("--board requires provider:slug")
			}
			index++
			options.Boards = append(options.Boards, args[index])
		default:
			return Options{}, fmt.Errorf("unknown option %q", args[index])
		}
	}
	if options.Command != "collect" && options.Command != "collect-boards" && options.Command != "export" && options.Command != "serve" {
		return Options{}, fmt.Errorf("command must be collect, collect-boards, export, or serve")
	}
	return options, nil
}
