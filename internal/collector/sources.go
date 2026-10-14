package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var euCodes = []string{"AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR", "DE", "GR", "HU", "IE", "IT", "LV", "LT", "LU", "MT", "NL", "PL", "PT", "RO", "SK", "SI", "ES", "SE"}

type SourceResult struct {
	Jobs     []map[string]any
	Complete bool
	Status   string
	Reason   string
	Coverage string
}

func collectConfiguredSource(ctx context.Context, client *http.Client, source map[string]any, isBoard bool, settings map[string]any, preferences map[string]any) (SourceResult, error) {
	if isBoard {
		jobs, err := collectBoard(ctx, client, source, settings)
		return SourceResult{Jobs: jobs, Complete: true, Status: "ok"}, err
	}
	kind := stringValue(source["type"])
	if kind == "adzuna" || kind == "jooble" || kind == "google_cse" {
		if reason, skip := credentialSkip(source); skip {
			return SourceResult{Complete: true, Status: "skipped", Reason: reason}, nil
		}
		switch kind {
		case "adzuna":
			return collectAdzuna(ctx, client, source, settings, preferences)
		case "jooble":
			return collectJooble(ctx, client, source, settings, preferences)
		case "google_cse":
			return collectGoogleCSE(ctx, client, source, settings, preferences)
		}
	}
	if kind == "jsonld" {
		return collectJSONLD(ctx, client, source, settings)
	}
	if kind == "himalayas" && strings.EqualFold(stringValue(preferences["country_filter"]), "EU") && (len(stringList(preferences["technologies"])) > 0 || len(stringList(preferences["roles"])) > 0) {
		return collectHimalayasSearch(ctx, client, settings, preferences)
	}

	jobs := make([]map[string]any, 0)
	maxPages := numberSetting(settings, "max_pages", 250)
	pageDelay := time.Duration(numberSetting(settings, "page_delay_seconds", 1)) * time.Second
	seenTokens := map[string]bool{}
	var cursor string
	for page := 1; page <= maxPages; page++ {
		requestURL, err := feedPageURL(kind, source, page, cursor)
		if err != nil {
			return SourceResult{}, err
		}
		payload, err := fetchJSON(ctx, client, settings, requestURL)
		if err != nil {
			return SourceResult{}, err
		}
		data := objectValue(payload)
		rows, next, err := feedRows(kind, payload, data)
		if err != nil {
			return SourceResult{}, err
		}
		for _, raw := range rows {
			job, err := normalizeJob(kind, raw)
			if err != nil {
				continue
			}
			jobs = append(jobs, job)
		}
		if kind == "remotive" || kind == "remoteok" || next == "" {
			return SourceResult{Jobs: deduplicateJobs(jobs), Complete: true, Status: "ok"}, nil
		}
		if seenTokens[next] {
			return SourceResult{Jobs: deduplicateJobs(jobs), Complete: false, Status: "partial"}, fmt.Errorf("%s returned a repeated pagination token", kind)
		}
		seenTokens[next] = true
		cursor = next
		if page < maxPages && pageDelay > 0 {
			select {
			case <-ctx.Done():
				return SourceResult{Jobs: deduplicateJobs(jobs), Complete: false, Status: "partial"}, ctx.Err()
			case <-time.After(pageDelay):
			}
		}
	}
	return SourceResult{Jobs: deduplicateJobs(jobs), Complete: false, Status: "partial"}, nil
}

func feedPageURL(kind string, source map[string]any, page int, cursor string) (string, error) {
	switch kind {
	case "arbeitnow":
		return fmt.Sprintf("https://www.arbeitnow.com/api/job-board-api?page=%d", page), nil
	case "himalayas":
		query := url.Values{"limit": {"20"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		return fetchURL("https://himalayas.app/jobs/api", query), nil
	case "remotive":
		return "https://remotive.com/api/remote-jobs", nil
	case "remoteok":
		tags := stringList(source["tags"])
		if len(tags) == 0 {
			tags = []string{"dev"}
		}
		return fetchURL("https://remoteok.com/api", url.Values{"tags": {strings.Join(tags, ",")}}), nil
	default:
		return "", fmt.Errorf("unsupported feed type %q", kind)
	}
}

func feedRows(kind string, payload any, data map[string]any) ([]map[string]any, string, error) {
	var rows []any
	switch kind {
	case "arbeitnow":
		rows = listValue(data["data"])
		next := stringValue(objectValue(data["links"])["next"])
		return rowObjects(rows), next, nil
	case "himalayas":
		rows = listValue(data["jobs"])
		return rowObjects(rows), stringValue(data["nextCursor"]), nil
	case "remotive":
		rows = listValue(data["jobs"])
		return rowObjects(rows), "", nil
	case "remoteok":
		rows = listValue(payload)
		jobs := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			job := objectValue(row)
			if job["id"] != nil || job["slug"] != nil {
				jobs = append(jobs, job)
			}
		}
		return jobs, "", nil
	default:
		return nil, "", fmt.Errorf("unsupported feed type %q", kind)
	}
	return nil, "", fmt.Errorf("unsupported feed type %q", kind)
}

func collectBoard(ctx context.Context, client *http.Client, board, settings map[string]any) ([]map[string]any, error) {
	provider, slug := stringValue(board["provider"]), stringValue(board["slug"])
	if slug == "" {
		return nil, fmt.Errorf("board configuration is missing slug")
	}
	var endpoint string
	switch provider {
	case "greenhouse":
		endpoint = fmt.Sprintf("https://boards-api.greenhouse.io/v1/boards/%s/jobs?content=true", url.PathEscape(slug))
	case "lever":
		base := "https://api.lever.co"
		if strings.EqualFold(stringValue(board["region"]), "eu") {
			base = "https://api.eu.lever.co"
		}
		endpoint = fmt.Sprintf("%s/v0/postings/%s?mode=json", base, url.PathEscape(slug))
	case "ashby":
		endpoint = fmt.Sprintf("https://api.ashbyhq.com/posting-api/job-board/%s?includeCompensation=true", url.PathEscape(slug))
	default:
		return nil, fmt.Errorf("unsupported board provider %q", provider)
	}
	payload, err := fetchJSON(ctx, client, settings, endpoint)
	if err != nil {
		return nil, err
	}
	var rows []any
	if provider == "lever" {
		rows = listValue(payload)
	} else {
		rows = listValue(objectValue(payload)["jobs"])
	}
	company := firstNonEmpty(stringValue(board["name"]), slug)
	jobs := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		job, err := normalizeBoard(provider, slug, company, objectValue(row))
		if err != nil {
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func collectHimalayasSearch(ctx context.Context, client *http.Client, settings, preferences map[string]any) (SourceResult, error) {
	terms := stringList(preferences["technologies"])
	if len(terms) == 0 {
		terms = stringList(preferences["roles"])
	}
	if len(terms) == 0 {
		return SourceResult{}, fmt.Errorf("Himalayas targeted search needs role or technology terms")
	}
	uniqueTerms := make([]string, 0, len(terms))
	seenTerms := map[string]bool{}
	for _, term := range terms {
		if strings.EqualFold(term, ".NET") || strings.EqualFold(term, "Dotnet") {
			term = ".NET"
		}
		key := strings.ToLower(term)
		if !seenTerms[key] {
			seenTerms[key] = true
			uniqueTerms = append(uniqueTerms, term)
		}
	}
	limit := numberSetting(settings, "max_himalayas_search_requests", 250)
	maxPages := numberSetting(settings, "max_pages", 250)
	pageDelay := time.Duration(numberSetting(settings, "page_delay_seconds", 1)) * time.Second
	uniqueJobs := map[string]map[string]any{}
	requestCount := 0
	for _, country := range euCodes {
		for _, term := range uniqueTerms {
			for page := 1; page <= maxPages; page++ {
				if requestCount >= limit {
					return SourceResult{Jobs: mapJobs(uniqueJobs), Complete: false, Status: "partial", Coverage: "filtered by configured terms and EU country"}, nil
				}
				query := url.Values{"country": {country}, "exclude_worldwide": {"true"}, "q": {term}, "sort": {"recent"}, "page": {strconv.Itoa(page)}}
				payload, err := fetchJSON(ctx, client, settings, fetchURL("https://himalayas.app/jobs/api/search", query))
				if err != nil {
					return SourceResult{Jobs: mapJobs(uniqueJobs), Complete: false, Status: "partial", Coverage: "filtered by configured terms and EU country"}, err
				}
				requestCount++
				data := objectValue(payload)
				rows := rowObjects(listValue(data["jobs"]))
				for _, row := range rows {
					job, err := normalizeJob("himalayas", row)
					if err == nil {
						uniqueJobs[stringValue(job["source_id"])] = job
					}
				}
				total := numberSetting(data, "totalCount", 0)
				if len(rows) == 0 || page*20 >= total {
					break
				}
				if pageDelay > 0 {
					select {
					case <-ctx.Done():
						return SourceResult{Jobs: mapJobs(uniqueJobs), Complete: false, Status: "partial"}, ctx.Err()
					case <-time.After(pageDelay):
					}
				}
			}
		}
	}
	return SourceResult{Jobs: mapJobs(uniqueJobs), Complete: true, Status: "ok", Coverage: "filtered by configured terms and EU country"}, nil
}

func rowObjects(rows []any) []map[string]any {
	result := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if object, ok := row.(map[string]any); ok {
			result = append(result, object)
		}
	}
	return result
}

func deduplicateJobs(jobs []map[string]any) []map[string]any {
	unique := make(map[string]map[string]any, len(jobs))
	for _, job := range jobs {
		unique[stringValue(job["source_id"])] = job
	}
	return mapJobs(unique)
}

func mapJobs(jobs map[string]map[string]any) []map[string]any {
	keys := make([]string, 0, len(jobs))
	for key := range jobs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		result = append(result, jobs[key])
	}
	return result
}

func postJSON(ctx context.Context, client *http.Client, settings map[string]any, endpoint string, value any) (any, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	retries := numberSetting(settings, "retries", 2)
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", "PersonalJobCollector/0.1 (Go)")
		response, err := client.Do(request)
		if err != nil {
			lastErr = err
		} else {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 25<<20))
			response.Body.Close()
			if readErr != nil {
				lastErr = readErr
			} else if response.StatusCode >= 200 && response.StatusCode < 300 {
				var result any
				if err := json.Unmarshal(data, &result); err != nil {
					return nil, err
				}
				return result, nil
			} else {
				lastErr = fmt.Errorf("POST %s returned HTTP %d: %s", endpoint, response.StatusCode, truncate(string(data), 300))
				if response.StatusCode == 400 || response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 404 {
					return nil, lastErr
				}
			}
		}
		if attempt < retries {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<min(attempt+1, 5)) * time.Second):
			}
		}
	}
	return nil, lastErr
}
