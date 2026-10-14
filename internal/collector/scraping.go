package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var (
	jsonLDScriptPattern = regexp.MustCompile(`(?is)<script[^>]*type=["']application/ld\+json["'][^>]*>(.*?)</script>`)
	sitemapLocation     = regexp.MustCompile(`(?is)<loc>\s*(.*?)\s*</loc>`)
	robotsLine          = regexp.MustCompile(`(?i)^\s*(user-agent|allow|disallow)\s*:\s*(.*?)\s*$`)
)

func collectGoogleCSE(ctx context.Context, client *http.Client, source, settings, preferences map[string]any) (SourceResult, error) {
	keyEnv := firstNonEmpty(stringValue(source["api_key_env"]), "GOOGLE_CSE_API_KEY")
	engineEnv := firstNonEmpty(stringValue(source["engine_id_env"]), "GOOGLE_CSE_ENGINE_ID")
	terms := uniqueStrings(append(stringList(preferences["roles"]), stringList(preferences["technologies"])...))
	if len(terms) == 0 {
		return SourceResult{Complete: true, Status: "ok"}, nil
	}
	maxQueries := numberSetting(source, "max_queries_per_run", 20)
	maxResults := min(max(1, numberSetting(source, "max_results_per_query", 10)), 100)
	allowedDomains := stringList(source["allowed_domains"])
	jobs := make([]map[string]any, 0)
	requests := 0
	complete := true
	for _, term := range terms {
		searchQuery := fmt.Sprintf("\"%s\" jobs Europe", term)
		for start := 1; start <= maxResults; start += 10 {
			if requests >= maxQueries {
				complete = false
				break
			}
			pageSize := min(10, maxResults-start+1)
			query := url.Values{"key": {os.Getenv(keyEnv)}, "cx": {os.Getenv(engineEnv)}, "q": {searchQuery}, "num": {fmt.Sprint(pageSize)}, "start": {fmt.Sprint(start)}}
			payload, err := fetchJSON(ctx, client, settings, fetchURL("https://www.googleapis.com/customsearch/v1", query))
			requests++
			if err != nil {
				message := strings.ReplaceAll(err.Error(), os.Getenv(keyEnv), "<redacted>")
				return SourceResult{Jobs: deduplicateJobs(jobs), Complete: false, Status: "partial"}, fmt.Errorf("Google CSE request failed: %s", message)
			}
			items := rowObjects(listValue(objectValue(payload)["items"]))
			if len(items) == 0 {
				break
			}
			for _, item := range items {
				pageURL := stringValue(item["link"])
				if !allowedURL(pageURL, allowedDomains) {
					continue
				}
				pageJobs, failures := scrapeJobPostings(ctx, client, settings, []string{pageURL}, allowedDomains, true)
				jobs = append(jobs, pageJobs...)
				if len(failures) > 0 {
					complete = false
				}
				if delay := numberSetting(settings, "page_delay_seconds", 0); delay > 0 {
					select {
					case <-ctx.Done():
						return SourceResult{Jobs: deduplicateJobs(jobs), Complete: false, Status: "partial"}, ctx.Err()
					case <-time.After(time.Duration(delay) * time.Second):
					}
				}
			}
		}
		if requests >= maxQueries && term != terms[len(terms)-1] {
			complete = false
			break
		}
	}
	status := "ok"
	if !complete {
		status = "partial"
	}
	return SourceResult{Jobs: deduplicateJobs(jobs), Complete: complete, Status: status}, nil
}

func collectJSONLD(ctx context.Context, client *http.Client, source, settings map[string]any) (SourceResult, error) {
	if source["terms_allow_automated_access"] != true {
		return SourceResult{Complete: true, Status: "skipped", Reason: "set terms_allow_automated_access=true only after checking the target site's terms"}, nil
	}
	allowedDomains := stringList(source["allowed_domains"])
	seeds := stringList(source["urls"])
	if len(seeds) == 0 {
		return SourceResult{Complete: true, Status: "skipped", Reason: "no seed URLs configured"}, nil
	}
	for _, seed := range seeds {
		if parsed, err := url.Parse(seed); err == nil && parsed.Hostname() != "" && !containsStringCaseFold(allowedDomains, parsed.Hostname()) {
			allowedDomains = append(allowedDomains, parsed.Hostname())
		}
	}
	partial := false
	pageURLs := make([]string, 0)
	for _, seed := range seeds {
		if !allowedURL(seed, allowedDomains) {
			partial = true
			continue
		}
		if strings.HasSuffix(strings.ToLower(strings.Split(seed, "?")[0]), ".xml") {
			body, err := fetchText(ctx, client, settings, seed)
			if err != nil {
				partial = true
				continue
			}
			locations := sitemapLocation.FindAllStringSubmatch(body, -1)
			if len(locations) == 0 {
				partial = true
			}
			for _, match := range locations {
				candidate := strings.TrimSpace(html.UnescapeString(match[1]))
				if allowedURL(candidate, allowedDomains) {
					pageURLs = append(pageURLs, candidate)
				} else {
					partial = true
				}
			}
		} else {
			pageURLs = append(pageURLs, seed)
		}
	}
	pageURLs = uniqueStrings(pageURLs)
	maxPages := numberSetting(source, "max_pages", numberSetting(settings, "max_pages", 250))
	if len(pageURLs) > maxPages {
		pageURLs = pageURLs[:maxPages]
		partial = true
	}
	jobs, failures := scrapeJobPostings(ctx, client, settings, pageURLs, allowedDomains, false)
	if len(failures) > 0 {
		partial = true
	}
	status := "ok"
	if partial {
		status = "partial"
	}
	return SourceResult{Jobs: deduplicateJobs(jobs), Complete: !partial, Status: status}, nil
}

func scrapeJobPostings(ctx context.Context, client *http.Client, settings map[string]any, seeds, allowedDomains []string, google bool) ([]map[string]any, []string) {
	jobs := make([]map[string]any, 0)
	seenURLs := make(map[string]bool)
	robotsCache := make(map[string][]robotRule)
	var failures []string
	for _, pageURL := range seeds {
		if seenURLs[pageURL] || !allowedURL(pageURL, allowedDomains) {
			continue
		}
		seenURLs[pageURL] = true
		allowed, err := allowedByRobots(ctx, client, settings, pageURL, robotsCache)
		if err != nil {
			failures = append(failures, pageURL+": robots check: "+err.Error())
			continue
		}
		if !allowed {
			failures = append(failures, pageURL+": robots.txt disallows access")
			continue
		}
		body, err := fetchText(ctx, client, settings, pageURL)
		if err != nil {
			failures = append(failures, pageURL+": "+err.Error())
			continue
		}
		for _, posting := range extractJobPostings(body) {
			postingURL := firstNonEmpty(stringValue(posting["url"]), pageURL)
			if baseURL, baseErr := url.Parse(pageURL); baseErr == nil {
				if relativeURL, relativeErr := url.Parse(postingURL); relativeErr == nil {
					postingURL = baseURL.ResolveReference(relativeURL).String()
				}
			}
			if !allowedURL(postingURL, allowedDomains) {
				continue
			}
			posting["url"] = postingURL
			kind := "jsonld"
			if google {
				kind = "google_cse"
			}
			job, err := normalizeJob(kind, posting)
			if err == nil {
				jobs = append(jobs, job)
			}
		}
		if delay := numberSetting(settings, "page_delay_seconds", 0); delay > 0 {
			select {
			case <-ctx.Done():
				return jobs, append(failures, ctx.Err().Error())
			case <-time.After(time.Duration(delay) * time.Second):
			}
		}
	}
	return jobs, failures
}

func fetchText(ctx context.Context, client *http.Client, settings map[string]any, pageURL string) (string, error) {
	retries := numberSetting(settings, "retries", 2)
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
		if err != nil {
			return "", err
		}
		request.Header.Set("User-Agent", "JobsListingExtractor/1.0 (robots-aware structured data collector)")
		request.Header.Set("Accept", "text/html,application/xml,application/xhtml+xml,*/*")
		response, err := client.Do(request)
		if err != nil {
			lastErr = err
		} else {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 10<<20))
			response.Body.Close()
			if readErr != nil {
				lastErr = readErr
			} else if response.StatusCode >= 200 && response.StatusCode < 300 {
				return string(body), nil
			} else {
				lastErr = fmt.Errorf("HTTP %d", response.StatusCode)
				if response.StatusCode == 400 || response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 404 {
					return "", lastErr
				}
			}
		}
		if attempt < retries {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(1<<min(attempt+1, 5)) * time.Second):
			}
		}
	}
	return "", lastErr
}

func extractJobPostings(document string) []map[string]any {
	var postings []map[string]any
	for _, match := range jsonLDScriptPattern.FindAllStringSubmatch(document, -1) {
		var value any
		if json.Unmarshal([]byte(html.UnescapeString(match[1])), &value) != nil {
			continue
		}
		postings = append(postings, postingObjects(value)...)
	}
	return postings
}

func postingObjects(value any) []map[string]any {
	switch item := value.(type) {
	case []any:
		var result []map[string]any
		for _, nested := range item {
			result = append(result, postingObjects(nested)...)
		}
		return result
	case map[string]any:
		var result []map[string]any
		if typesContainJobPosting(item["@type"]) {
			result = append(result, item)
		}
		if graph, ok := item["@graph"]; ok {
			result = append(result, postingObjects(graph)...)
		}
		return result
	default:
		return nil
	}
}

func typesContainJobPosting(value any) bool {
	switch item := value.(type) {
	case string:
		return strings.EqualFold(item, "JobPosting") || strings.HasSuffix(strings.ToLower(item), "/jobposting")
	case []any:
		for _, child := range item {
			if typesContainJobPosting(child) {
				return true
			}
		}
	}
	return false
}

type robotRule struct {
	allow bool
	path  string
}

func allowedByRobots(ctx context.Context, client *http.Client, settings map[string]any, pageURL string, cache map[string][]robotRule) (bool, error) {
	parsed, err := url.Parse(pageURL)
	if err != nil {
		return false, err
	}
	robotsURL := parsed.Scheme + "://" + parsed.Host + "/robots.txt"
	rules, found := cache[robotsURL]
	if !found {
		text, err := fetchText(ctx, client, settings, robotsURL)
		if err != nil {
			if strings.Contains(err.Error(), "HTTP 404") {
				cache[robotsURL] = nil
				return true, nil
			}
			return false, err
		}
		rules = parseRobots(text)
		cache[robotsURL] = rules
	}
	requestPath := parsed.EscapedPath()
	if parsed.RawQuery != "" {
		requestPath += "?" + parsed.RawQuery
	}
	bestLength, allowed := -1, true
	for _, rule := range rules {
		if rule.path == "" || !strings.HasPrefix(requestPath, rule.path) {
			continue
		}
		if len(rule.path) >= bestLength {
			bestLength, allowed = len(rule.path), rule.allow
		}
	}
	return allowed, nil
}

func parseRobots(text string) []robotRule {
	var rules []robotRule
	active := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.SplitN(line, "#", 2)[0]
		match := robotsLine.FindStringSubmatch(line)
		if len(match) != 3 {
			continue
		}
		key, value := strings.ToLower(match[1]), strings.TrimSpace(match[2])
		switch key {
		case "user-agent":
			active = value == "*" || strings.Contains(strings.ToLower(value), "jobslistingextractor")
		case "allow", "disallow":
			if active {
				rules = append(rules, robotRule{allow: key == "allow", path: value})
			}
		}
	}
	return rules
}

func allowedURL(raw string, allowedDomains []string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, domain := range allowedDomains {
		domain = strings.ToLower(strings.TrimPrefix(domain, "*."))
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func containsStringCaseFold(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.Trim(value, ".*"), expected) {
			return true
		}
	}
	return false
}
