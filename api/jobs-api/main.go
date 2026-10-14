package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"jobs-listing-extractor/internal/collector"
)

const (
	defaultAddress  = "127.0.0.1:8000"
	defaultDataDir  = "output"
	defaultPageSize = 100
	maximumPageSize = 500
)

type server struct {
	jobsFile string
	config   string
	mu       sync.Mutex
	running  bool
	lastRun  map[string]any
}

type jobPage struct {
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
	Jobs   []map[string]any `json:"jobs"`
}

type apiError struct {
	Error string `json:"error"`
}

func main() {
	options, err := collector.ParseOptions(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	if options.Command != "serve" {
		if err := collector.Run(options); err != nil {
			log.Fatal(err)
		}
		return
	}

	jobsFile, err := resolveJobsFile()
	if err != nil {
		log.Fatalf("resolve jobs file: %v", err)
	}
	address := environmentOrDefault("JOBS_API_ADDR", defaultAddress)

	app := server{
		jobsFile: jobsFile,
		config:   collector.DefaultConfigPath(),
	}
	server := &http.Server{
		Addr:              address,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("Jobs API listening on http://%s (data: %s)", address, jobsFile)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("start jobs API: %v", err)
	}
}

func resolveJobsFile() (string, error) {
	if configuredFile := strings.TrimSpace(os.Getenv("JOBS_FILE")); configuredFile != "" {
		return filepath.Abs(configuredFile)
	}

	if configuredDir := strings.TrimSpace(os.Getenv("JOBS_OUTPUT_DIR")); configuredDir != "" {
		return filepath.Abs(filepath.Join(configuredDir, "jobs.json"))
	}

	workingDir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for dir := workingDir; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "output", "jobs.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect %q: %w", candidate, err)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}

	return filepath.Abs(filepath.Join(defaultDataDir, "jobs.json"))
}

func (s server) routes() http.Handler {
	router := chi.NewRouter()
	router.Use(
		middleware.RequestID,
		middleware.RealIP,
		middleware.Logger,
		middleware.Recoverer,
	)
	router.Use(cors)

	router.Get("/api/health", s.health)
	router.Get("/api/jobs", s.listJobs)
	router.Get("/api/collector", s.collectorStatus)
	router.Post("/api/collector/runs", s.startCollection)
	router.Get("/api/config", s.getConfig)
	router.Put("/api/config", s.putConfig)
	router.Get("/api/sources", s.getSources)
	router.Put("/api/sources", s.putSources)
	router.Get("/openapi.json", s.openapi)
	router.Get("/docs", s.docs)
	router.Get("/docs/", s.docs)

	return router
}

func (s *server) getConfig(w http.ResponseWriter, _ *http.Request) {
	data, err := os.ReadFile(s.config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unable to read config")
		return
	}
	writeJSONBytes(w, http.StatusOK, data)
}

func (s *server) putConfig(w http.ResponseWriter, r *http.Request) {
	data, err := readRequestJSON(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var settings collector.Settings
	if err := json.Unmarshal(data, &settings); err != nil || settings.Preferences == nil || settings.Collection == nil || settings.SourcesFile == "" || settings.Database == "" || settings.OutputDir == "" {
		writeError(w, http.StatusBadRequest, "config must be valid JSON with preferences, collection, sources_file, database, and output_directory")
		return
	}
	if err := validateConfigPaths(settings); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.writeConfigFile(s.config, data); err != nil {
		if err.Error() == "collector is running" {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		log.Printf("write config: %v", err)
		writeError(w, http.StatusInternalServerError, "unable to save config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *server) getSources(w http.ResponseWriter, _ *http.Request) {
	path, err := s.sourcesPath()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unable to resolve sources config")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unable to read sources config")
		return
	}
	writeJSONBytes(w, http.StatusOK, data)
}

func (s *server) putSources(w http.ResponseWriter, r *http.Request) {
	data, err := readRequestJSON(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var sources collector.SourceList
	if err := json.Unmarshal(data, &sources); err != nil || sources.Feeds == nil || sources.Boards == nil {
		writeError(w, http.StatusBadRequest, "sources must be valid JSON with feeds and boards arrays")
		return
	}
	if err := validateSources(sources); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	path, err := s.sourcesPath()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unable to resolve sources config")
		return
	}
	if err := s.writeConfigFile(path, data); err != nil {
		if err.Error() == "collector is running" {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		log.Printf("write sources config: %v", err)
		writeError(w, http.StatusInternalServerError, "unable to save sources config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *server) sourcesPath() (string, error) {
	settings, base, err := collector.LoadSettings(s.config)
	if err != nil {
		return "", err
	}
	path := filepath.Clean(filepath.Join(base, settings.SourcesFile))
	relative, err := filepath.Rel(base, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("sources_file must be inside the config directory")
	}
	return path, nil
}

func (s *server) writeConfigFile(path string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return errors.New("collector is running")
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, data, "", "  "); err != nil {
		return err
	}
	formatted.WriteByte('\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(formatted.Bytes()); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func readRequestJSON(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		return nil, errors.New("request body must be valid JSON under 2 MiB")
	}
	if !json.Valid(data) {
		return nil, errors.New("request body must be valid JSON")
	}
	return data, nil
}

func validateConfigPaths(settings collector.Settings) error {
	for _, value := range []string{settings.SourcesFile, settings.Database, settings.OutputDir} {
		if filepath.IsAbs(value) {
			return errors.New("config paths must be relative")
		}
		cleaned := filepath.Clean(value)
		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			return errors.New("config paths must stay inside the config directory")
		}
	}
	return nil
}

func validateSources(sources collector.SourceList) error {
	seen := map[string]bool{}
	for _, feed := range sources.Feeds {
		typeName := strings.TrimSpace(stringValue(feed["type"]))
		if typeName == "" {
			return errors.New("every feed must define a type")
		}
		key := "feed:" + typeName
		if seen[key] {
			return fmt.Errorf("duplicate feed type %q", typeName)
		}
		seen[key] = true
	}
	for _, board := range sources.Boards {
		provider, slug := strings.TrimSpace(stringValue(board["provider"])), strings.TrimSpace(stringValue(board["slug"]))
		if provider == "" || slug == "" {
			return errors.New("every board must define provider and slug")
		}
		key := "board:" + strings.ToLower(provider) + ":" + strings.ToLower(slug)
		if seen[key] {
			return fmt.Errorf("duplicate board %s:%s", provider, slug)
		}
		seen[key] = true
	}
	return nil
}

func writeJSONBytes(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (s *server) collectorStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	running := s.running
	lastRun := s.lastRun
	s.mu.Unlock()

	if lastRun == nil {
		if report, err := s.readCollectorReport(); err == nil {
			lastRun = report
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Printf("read collector report: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"running": running, "last_run": lastRun})
}

func (s *server) startCollection(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Command string   `json:"command"`
		Boards  []string `json:"boards,omitempty"`
	}
	if r.Body != nil {
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid JSON request body")
			return
		}
	}
	if request.Command == "" {
		request.Command = "collect"
	}
	if request.Command != "collect" && request.Command != "collect-boards" {
		writeError(w, http.StatusBadRequest, "command must be collect or collect-boards")
		return
	}
	if request.Command == "collect" && len(request.Boards) > 0 {
		writeError(w, http.StatusBadRequest, "boards can only be used with collect-boards")
		return
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "a collector run is already in progress")
		return
	}
	s.running = true
	s.lastRun = map[string]any{"command": request.Command, "started_at": time.Now().UTC().Format(time.RFC3339), "status": "running"}
	s.mu.Unlock()

	options := collector.Options{Command: request.Command, Config: s.config, Boards: request.Boards}
	go s.runCollection(options)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "running", "command": request.Command})
}

func (s *server) runCollection(options collector.Options) {
	err := collector.Run(options)
	report, reportErr := s.readCollectorReport()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	if reportErr == nil {
		s.lastRun = report
		if err != nil {
			s.lastRun["error"] = err.Error()
		}
		return
	}
	finished := map[string]any{"command": options.Command, "finished_at": time.Now().UTC().Format(time.RFC3339)}
	if err != nil {
		finished["status"] = "failed"
		finished["error"] = err.Error()
	} else {
		finished["status"] = "ok"
	}
	s.lastRun = finished
}

func (s *server) readCollectorReport() (map[string]any, error) {
	settings, base, err := collector.LoadSettings(s.config)
	if err != nil {
		return nil, err
	}
	return readJSONFile(filepath.Join(base, settings.OutputDir, "run-status.json"))
}

func readJSONFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func (s server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s server) listJobs(w http.ResponseWriter, r *http.Request) {
	filters, err := parseJobQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	jobs, err := s.loadJobs()
	if err != nil {
		log.Printf("load jobs: %v", err)
		writeError(w, http.StatusInternalServerError, "unable to read job export")
		return
	}

	filteredJobs := filterJobs(jobs, filters)
	page := paginateJobs(filteredJobs, filters.limit, filters.offset)
	writeJSON(w, http.StatusOK, page)
}

func (s server) loadJobs() ([]map[string]any, error) {
	data, err := os.ReadFile(s.jobsFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("jobs file %q does not exist", s.jobsFile)
	}
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", s.jobsFile, err)
	}

	var jobs []map[string]any
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, fmt.Errorf("parse %q: %w", s.jobsFile, err)
	}
	if jobs == nil {
		jobs = []map[string]any{}
	}
	return jobs, nil
}

type jobFilters struct {
	limit    int
	offset   int
	status   string
	source   string
	company  string
	location string
	workMode string
	search   string
}

func parseJobQuery(r *http.Request) (jobFilters, error) {
	query := r.URL.Query()
	filters := jobFilters{
		limit:    defaultPageSize,
		offset:   0,
		status:   strings.TrimSpace(query.Get("status")),
		source:   strings.TrimSpace(query.Get("source")),
		company:  strings.TrimSpace(query.Get("company")),
		location: strings.TrimSpace(query.Get("location")),
		workMode: strings.TrimSpace(query.Get("work_mode")),
		search:   strings.TrimSpace(query.Get("q")),
	}

	if rawLimit := query.Get("limit"); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 1 || limit > maximumPageSize {
			return jobFilters{}, fmt.Errorf("limit must be an integer from 1 to %d", maximumPageSize)
		}
		filters.limit = limit
	}

	if rawOffset := query.Get("offset"); rawOffset != "" {
		offset, err := strconv.Atoi(rawOffset)
		if err != nil || offset < 0 {
			return jobFilters{}, errors.New("offset must be a non-negative integer")
		}
		filters.offset = offset
	}

	return filters, nil
}

func filterJobs(jobs []map[string]any, filters jobFilters) []map[string]any {
	filtered := make([]map[string]any, 0, len(jobs))
	for _, job := range jobs {
		if !matchesExact(filters.status, nestedString(job, "assessment", "status")) ||
			!matchesExact(filters.source, stringValue(job["source"])) ||
			!matchesExact(filters.company, stringValue(job["company"])) ||
			!matchesExact(filters.location, stringValue(job["location"])) ||
			!matchesExact(filters.workMode, stringValue(job["work_mode"])) {
			continue
		}

		if filters.search != "" && !matchesSearch(job, filters.search) {
			continue
		}
		filtered = append(filtered, job)
	}

	return filtered
}

func matchesSearch(job map[string]any, search string) bool {
	searchableText := strings.ToLower(strings.Join([]string{
		stringValue(job["title"]),
		stringValue(job["company"]),
		stringValue(job["description"]),
		stringValue(job["location"]),
	}, " "))
	return strings.Contains(searchableText, strings.ToLower(search))
}

func paginateJobs(jobs []map[string]any, limit, offset int) jobPage {
	total := len(jobs)
	if offset > total {
		offset = total
	}

	end := min(offset+limit, total)
	return jobPage{
		Total:  total,
		Limit:  limit,
		Offset: offset,
		Jobs:   jobs[offset:end],
	}
}

func (s server) openapi(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(openAPISpec))
}

func (s server) docs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(swaggerHTML))
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func matchesExact(expected, actual string) bool {
	return expected == "" || strings.EqualFold(expected, actual)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func nestedString(value map[string]any, keys ...string) string {
	var current any = value
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[key]
	}
	return stringValue(current)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, apiError{Error: message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write JSON response: %v", err)
	}
}

func environmentOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

const swaggerHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Jobs API — Swagger UI</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    SwaggerUIBundle({ url: "/openapi.json", dom_id: "#swagger-ui" });
  </script>
</body>
</html>`

const openAPISpec = `{
  "openapi": "3.0.3",
  "info": {
    "title": "Jobs Listing API",
    "version": "1.0.0",
    "description": "Read-only access to the latest exported job listings."
  },
  "paths": {
    "/api/health": {
      "get": {
        "summary": "API health",
        "responses": {
          "200": {
            "description": "API is running",
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/Health" }
              }
            }
          }
        }
      }
    },
    "/api/jobs": {
      "get": {
        "summary": "List job listings",
        "description": "Applies filters before pagination.",
        "parameters": [
          { "name": "limit", "in": "query", "schema": { "type": "integer", "minimum": 1, "maximum": 500, "default": 100 } },
          { "name": "offset", "in": "query", "schema": { "type": "integer", "minimum": 0, "default": 0 } },
          { "name": "status", "in": "query", "schema": { "type": "string", "description": "Case-insensitive exact match." } },
          { "name": "source", "in": "query", "schema": { "type": "string", "description": "Case-insensitive exact match." } },
          { "name": "company", "in": "query", "schema": { "type": "string", "description": "Case-insensitive exact match." } },
          { "name": "location", "in": "query", "schema": { "type": "string", "description": "Case-insensitive exact match." } },
          { "name": "work_mode", "in": "query", "schema": { "type": "string", "description": "Case-insensitive exact match." } },
          { "name": "q", "in": "query", "schema": { "type": "string", "description": "Search title, company, description, and location." } }
        ],
        "responses": {
          "200": {
            "description": "Filtered page of listings.",
            "content": {
              "application/json": {
                "schema": { "$ref": "#/components/schemas/JobPage" }
              }
            }
          },
          "400": { "description": "Invalid pagination parameters." },
          "500": { "description": "Unable to read the job export." }
        }
      }
    }
    ,"/api/collector": {
      "get": {
        "summary": "Get collector status",
        "responses": {"200": {"description": "Current execution state and latest run report."}}
      }
    },
    "/api/collector/runs": {
      "post": {
        "summary": "Start a collector run",
        "description": "Runs the collector in the background. Poll /api/collector for status.",
        "requestBody": {
          "required": false,
          "content": {"application/json": {"schema": {"type": "object", "properties": {
            "command": {"type": "string", "enum": ["collect", "collect-boards"], "default": "collect"},
            "boards": {"type": "array", "items": {"type": "string", "description": "provider:slug"}}
          }}}}
        },
        "responses": {
          "202": {"description": "Collector run accepted."},
          "400": {"description": "Invalid request."},
          "409": {"description": "A collector run is already in progress."}
        }
      }
    },
    "/api/config": {
      "get": {
        "summary": "Read collector configuration",
        "responses": {"200": {"description": "Current preferences and collector settings."}}
      },
      "put": {
        "summary": "Replace collector configuration",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object"}}}},
        "responses": {"200": {"description": "Configuration saved."}, "400": {"description": "Invalid configuration."}, "409": {"description": "Collector is running."}}
      }
    },
    "/api/sources": {
      "get": {
        "summary": "Read configured feeds and boards",
        "responses": {"200": {"description": "Current sources configuration."}}
      },
      "put": {
        "summary": "Replace configured feeds and boards",
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object", "required": ["feeds", "boards"], "properties": {"feeds": {"type": "array", "items": {"type": "object"}}, "boards": {"type": "array", "items": {"type": "object"}}}}}}},
        "responses": {"200": {"description": "Sources saved."}, "400": {"description": "Invalid sources configuration."}, "409": {"description": "Collector is running."}}
      }
    }
  },
  "components": {
    "schemas": {
      "Health": {
        "type": "object",
        "properties": { "status": { "type": "string", "example": "ok" } }
      },
      "JobPage": {
        "type": "object",
        "properties": {
          "total": { "type": "integer" },
          "limit": { "type": "integer" },
          "offset": { "type": "integer" },
          "jobs": { "type": "array", "items": { "type": "object" } }
        }
      }
    }
  }
}`
