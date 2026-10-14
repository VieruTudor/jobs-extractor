package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func Export(ctx context.Context, store *Store, settings Settings, base string, report map[string]any) (int, error) {
	outputDir := filepath.Join(base, settings.OutputDir)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return 0, fmt.Errorf("create output directory: %w", err)
	}
	stored, err := store.ActiveJobs()
	if err != nil {
		return 0, err
	}
	groups := make(map[string]map[string]any)
	groupOrder := make([]string, 0, len(stored))
	geoSettings := settings.Geocoding
	geoEnabled, ok := geoSettings["enabled"].(bool)
	if !ok {
		geoEnabled = true
	}
	resolver := newGeocodeResolver(store, geoSettings)
	for _, item := range stored {
		job := item.Job
		if raw, ok := job["raw"].(map[string]any); ok {
			source := stringValue(job["source"])
			if provider, slug, ok := splitBoardSource(source); ok {
				job, err = normalizeBoard(provider, slug, stringValue(job["company"]), raw)
			} else {
				job, err = normalizeJob(source, raw)
			}
			if err != nil {
				continue
			}
		}
		delete(job, "raw")
		job["first_seen"], job["last_seen"] = item.FirstSeen, item.LastSeen
		key := canonicalURL(stringValue(job["url"]))
		if key == "" {
			key = stringValue(job["source"]) + ":" + stringValue(job["source_id"])
		}
		sourceURL := firstNonEmpty(stringValue(job["source_url"]), stringValue(job["url"]))
		reference := map[string]any{"source": job["source"], "source_id": job["source_id"], "url": sourceURL}
		if existing, found := groups[key]; found {
			refs := existing["sources"].([]map[string]any)
			existing["sources"] = append(refs, reference)
			continue
		}
		job["sources"] = []map[string]any{reference}
		assessment := assessJob(job, settings.Preferences, "")
		if assessment.Status != "excluded" && geoEnabled && strings.EqualFold(stringValue(settings.Preferences["country_filter"]), "EU") &&
			matchLocation(stringValue(job["location"]), stringList(job["location_restrictions"]), "EU", "") == "unknown" {
			countryCode := resolver.Resolve(ctx, stringValue(job["location"]), stringList(job["location_restrictions"]))
			if countryCode != "" {
				job["location_country_code"] = countryCode
				job["location_country_source"] = "Open-Meteo / GeoNames"
			}
			assessment = assessJob(job, settings.Preferences, countryCode)
		}
		job["assessment"] = assessment
		groups[key] = job
		groupOrder = append(groupOrder, key)
	}

	jobs := make([]map[string]any, 0, len(groups))
	for _, key := range groupOrder {
		job := groups[key]
		assessment, _ := job["assessment"].(Assessment)
		if assessment.Status != "excluded" {
			jobs = append(jobs, job)
		}
	}
	report["geocoding"] = resolver.Stats()
	matches := filterByAssessment(jobs, "match")
	review := filterByAssessment(jobs, "review")
	for name, value := range map[string]any{"jobs.json": jobs, "matches.json": matches, "review.json": review, "run-status.json": report} {
		if err := writeJSONAtomic(filepath.Join(outputDir, name), value); err != nil {
			return 0, err
		}
	}
	if err := writeDaily(filepath.Join(outputDir, "daily.txt"), jobs, report); err != nil {
		return 0, err
	}
	if err := writeHTML(filepath.Join(outputDir, "jobs.html"), jobs, report); err != nil {
		return 0, err
	}
	return len(jobs), nil
}

func filterByAssessment(jobs []map[string]any, status string) []map[string]any {
	result := make([]map[string]any, 0)
	for _, job := range jobs {
		assessment, _ := job["assessment"].(Assessment)
		if assessment.Status == status {
			result = append(result, job)
		}
	}
	return result
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return atomicWrite(path, data)
}

func atomicWrite(path string, data []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return err
	}
	return nil
}

func writeDaily(path string, jobs []map[string]any, report map[string]any) error {
	finished := stringValue(report["finished_at"])
	var lines []string
	lines = append(lines, "Job collector: "+finished, fmt.Sprintf("Current listings: %d", len(jobs)), "")
	for _, source := range mapSlice(report["sources"]) {
		lines = append(lines, fmt.Sprintf("%s: %s (%v listings) %s", source["source"], source["status"], source["count"], source["error"]))
	}
	lines = append(lines, "", "New and changed matches / review candidates:")
	changes := map[string]string{}
	for _, event := range mapSlice(report["events"]) {
		changes[stringValue(event["source"])+"\x00"+stringValue(event["source_id"])] = stringValue(event["change"])
	}
	for _, job := range jobs {
		refs := mapSlice(job["sources"])
		var jobChanges []string
		for _, ref := range refs {
			if change := changes[stringValue(ref["source"])+"\x00"+stringValue(ref["source_id"])]; change != "" {
				jobChanges = append(jobChanges, change)
			}
		}
		if len(jobChanges) == 0 {
			continue
		}
		assessment, _ := job["assessment"].(Assessment)
		lines = append(lines, fmt.Sprintf("\n[%s] %s — %s", assessment.Status, job["title"], job["company"]), stringValue(job["url"]), strings.Join(assessment.Reasons, "; "))
		for _, ref := range refs {
			lines = append(lines, fmt.Sprintf("Source: %s — %s", ref["source"], ref["url"]))
		}
	}
	return atomicWrite(path, []byte(strings.Join(lines, "\n")))
}

var jobsHTML = template.Must(template.New("jobs").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Job listings</title>
<style>body{font:15px system-ui,sans-serif;margin:2rem;color:#17202a}table{border-collapse:collapse;width:100%}th,td{border:1px solid #ccd1d1;padding:.6rem;text-align:left}th{background:#f2f4f4}.review{color:#8a5a00}.match{color:#137547}</style></head>
<body><h1>Job listings</h1><p>{{.Count}} active listings · updated {{.Updated}}</p><table><thead><tr><th>Status</th><th>Job</th><th>Company</th><th>Location</th><th>Sources</th><th>Reasons</th></tr></thead><tbody>
{{range .Jobs}}<tr><td class="{{.Status}}">{{.Status}}</td><td><a href="{{.URL}}">{{.Title}}</a></td><td>{{.Company}}</td><td>{{.Location}}</td><td>{{.Sources}}</td><td>{{.Reasons}}</td></tr>{{end}}
</tbody></table><footer>Location lookups use <a href="https://open-meteo.com/en/docs/geocoding-api">Open-Meteo</a> and GeoNames data (CC BY 4.0).</footer></body></html>`))

func writeHTML(path string, jobs []map[string]any, report map[string]any) error {
	type row struct{ Status, URL, Title, Company, Location, Sources, Reasons string }
	data := struct {
		Count   int
		Updated string
		Jobs    []row
	}{Count: len(jobs), Updated: stringValue(report["finished_at"])}
	for _, job := range jobs {
		assessment, _ := job["assessment"].(Assessment)
		var sources []string
		for _, source := range mapSlice(job["sources"]) {
			sources = append(sources, stringValue(source["source"]))
		}
		data.Jobs = append(data.Jobs, row{assessment.Status, safeHTTPURL(stringValue(job["url"])), stringValue(job["title"]), stringValue(job["company"]), stringValue(job["location"]), strings.Join(sources, ", "), strings.Join(assessment.Reasons, "; ")})
	}
	var output strings.Builder
	if err := jobsHTML.Execute(&output, data); err != nil {
		return err
	}
	return atomicWrite(path, []byte(output.String()))
}

func safeHTTPURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "#"
	}
	return parsed.String()
}

func mapSlice(value any) []map[string]any {
	switch items := value.(type) {
	case []map[string]any:
		return items
	case []map[string]string:
		result := make([]map[string]any, 0, len(items))
		for _, item := range items {
			object := make(map[string]any, len(item))
			for key, value := range item {
				object[key] = value
			}
			result = append(result, object)
		}
		return result
	case []any:
		result := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if object, ok := item.(map[string]any); ok {
				result = append(result, object)
			}
		}
		return result
	}
	return nil
}

func splitBoardSource(source string) (provider, slug string, ok bool) {
	index := strings.IndexByte(source, ':')
	if index < 0 {
		return "", "", false
	}
	return source[:index], source[index+1:], true
}
