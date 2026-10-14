package collector

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

var adzunaCountries = map[string]string{"AT": "at", "BE": "be", "DE": "de", "ES": "es", "FR": "fr", "IT": "it", "NL": "nl", "PL": "pl"}

func missingCredentials(source map[string]any) []string {
	switch stringValue(source["type"]) {
	case "adzuna":
		idEnv := firstNonEmpty(stringValue(source["app_id_env"]), "ADZUNA_APP_ID")
		keyEnv := firstNonEmpty(stringValue(source["app_key_env"]), "ADZUNA_APP_KEY")
		var missing []string
		for _, name := range []string{idEnv, keyEnv} {
			if os.Getenv(name) == "" {
				missing = append(missing, name)
			}
		}
		return missing
	case "jooble":
		keys := objectValue(source["api_key_envs"])
		domains := objectValue(source["domains"])
		var missing []string
		present := 0
		for country, envValue := range keys {
			env := stringValue(envValue)
			if os.Getenv(env) == "" {
				missing = append(missing, env)
				continue
			}
			present++
			if stringValue(domains[strings.ToUpper(country)]) == "" {
				missing = append(missing, "Jooble domain for "+strings.ToUpper(country))
			}
		}
		if present == 0 {
			missing = append(missing, "JOOBLE_API_KEY_<EU_COUNTRY_CODE> (country-specific keys)")
		}
		return missing
	case "google_cse":
		keyEnv := firstNonEmpty(stringValue(source["api_key_env"]), "GOOGLE_CSE_API_KEY")
		engineEnv := firstNonEmpty(stringValue(source["engine_id_env"]), "GOOGLE_CSE_ENGINE_ID")
		var missing []string
		if os.Getenv(keyEnv) == "" {
			missing = append(missing, keyEnv)
		}
		if os.Getenv(engineEnv) == "" {
			missing = append(missing, engineEnv)
		}
		if source["terms_allow_automated_access"] != true {
			missing = append(missing, "terms_allow_automated_access=true after checking each target site's rules")
		}
		if len(stringList(source["allowed_domains"])) == 0 {
			missing = append(missing, "allowed_domains")
		}
		return missing
	default:
		return nil
	}
}

func collectAdzuna(ctx context.Context, client *http.Client, source, settings, preferences map[string]any) (SourceResult, error) {
	appID := os.Getenv(firstNonEmpty(stringValue(source["app_id_env"]), "ADZUNA_APP_ID"))
	appKey := os.Getenv(firstNonEmpty(stringValue(source["app_key_env"]), "ADZUNA_APP_KEY"))
	terms := stringList(preferences["technologies"])
	if len(terms) == 0 {
		terms = stringList(preferences["roles"])
	}
	if len(terms) == 0 {
		return SourceResult{Jobs: []map[string]any{}, Complete: true, Status: "ok"}, nil
	}
	countries := stringList(source["countries"])
	if len(countries) == 0 {
		countries = []string{"AT", "BE", "DE", "ES", "FR", "IT", "NL", "PL"}
	}
	maxRequests := numberSetting(source, "max_requests_per_run", 120)
	maxPages := numberSetting(source, "max_pages_per_query", 3)
	perPage := min(max(1, numberSetting(source, "results_per_page", 50)), 50)
	maxAge, hasMaxAge := number(preferences["posted_within_days"])
	requestCount := 0
	jobs := make([]map[string]any, 0)
	for _, country := range countries {
		domain, ok := adzunaCountries[strings.ToUpper(country)]
		if !ok {
			continue
		}
		for _, term := range terms {
			for page := 1; page <= maxPages; page++ {
				if requestCount >= maxRequests {
					return SourceResult{Jobs: deduplicateJobs(jobs), Complete: false, Status: "partial"}, nil
				}
				query := url.Values{"app_id": {appID}, "app_key": {appKey}, "results_per_page": {strconv.Itoa(perPage)}, "what": {term}, "content-type": {"application/json"}}
				if hasMaxAge {
					query.Set("max_days_old", strconv.Itoa(int(maxAge)))
				}
				endpoint := fmt.Sprintf("https://api.adzuna.com/v1/api/jobs/%s/search/%d?%s", domain, page, query.Encode())
				payload, err := fetchJSON(ctx, client, settings, endpoint)
				requestCount++
				if err != nil {
					message := strings.ReplaceAll(strings.ReplaceAll(err.Error(), appID, "<redacted>"), appKey, "<redacted>")
					return SourceResult{Jobs: deduplicateJobs(jobs), Complete: false, Status: "partial"}, fmt.Errorf("Adzuna request failed: %s", message)
				}
				rows := rowObjects(listValue(objectValue(payload)["results"]))
				for _, row := range rows {
					if job, err := normalizeJob("adzuna", row); err == nil {
						jobs = append(jobs, job)
					}
				}
				if len(rows) < perPage || page*perPage >= numberSetting(objectValue(payload), "count", 0) {
					break
				}
			}
		}
	}
	return SourceResult{Jobs: deduplicateJobs(jobs), Complete: true, Status: "ok", Coverage: "supported EU countries: " + strings.Join(countries, ", ")}, nil
}

func collectJooble(ctx context.Context, client *http.Client, source, settings, preferences map[string]any) (SourceResult, error) {
	keys := objectValue(source["api_key_envs"])
	domains := objectValue(source["domains"])
	terms := stringList(preferences["technologies"])
	if len(terms) == 0 {
		terms = stringList(preferences["roles"])
	}
	if len(terms) == 0 {
		return SourceResult{Jobs: []map[string]any{}, Complete: true, Status: "ok"}, nil
	}
	maxRequests := numberSetting(source, "max_requests_per_run", 120)
	maxPages := numberSetting(source, "max_pages_per_query", 3)
	requests := 0
	jobs := make([]map[string]any, 0)
	for country, envValue := range keys {
		countryCode := strings.ToUpper(country)
		if !containsString(euCodes, countryCode) {
			continue
		}
		key := os.Getenv(stringValue(envValue))
		domain := stringValue(domains[countryCode])
		if key == "" || domain == "" {
			continue
		}
		for _, term := range terms {
			for page := 1; page <= maxPages; page++ {
				if requests >= maxRequests {
					return SourceResult{Jobs: deduplicateJobs(jobs), Complete: false, Status: "partial"}, nil
				}
				countryName := euCountryNamesForCode(countryCode)
				payload, err := postJSON(ctx, client, settings, "https://"+domain+"/api/"+url.PathEscape(key), map[string]any{"keywords": term, "location": countryName, "page": page})
				requests++
				if err != nil {
					message := strings.ReplaceAll(err.Error(), key, "<redacted>")
					return SourceResult{Jobs: deduplicateJobs(jobs), Complete: false, Status: "partial"}, fmt.Errorf("Jooble request failed: %s", message)
				}
				rows := rowObjects(listValue(objectValue(payload)["jobs"]))
				for _, row := range rows {
					if job, err := normalizeJob("jooble", row); err == nil {
						jobs = append(jobs, job)
					}
				}
				total := numberSetting(objectValue(payload), "totalCount", numberSetting(objectValue(payload), "total", 0))
				if len(rows) == 0 || (total > 0 && page*len(rows) >= total) {
					break
				}
			}
		}
	}
	return SourceResult{Jobs: deduplicateJobs(jobs), Complete: true, Status: "ok"}, nil
}

func credentialSkip(source map[string]any) (string, bool) {
	missing := missingCredentials(source)
	if len(missing) == 0 {
		return "", false
	}
	return "Missing credentials or source settings: " + strings.Join(missing, ", "), true
}

func euCountryNamesForCode(code string) string {
	names := map[string]string{"AT": "Austria", "BE": "Belgium", "BG": "Bulgaria", "HR": "Croatia", "CY": "Cyprus", "CZ": "Czechia", "DK": "Denmark", "EE": "Estonia", "FI": "Finland", "FR": "France", "DE": "Germany", "GR": "Greece", "HU": "Hungary", "IE": "Ireland", "IT": "Italy", "LV": "Latvia", "LT": "Lithuania", "LU": "Luxembourg", "MT": "Malta", "NL": "Netherlands", "PL": "Poland", "PT": "Portugal", "RO": "Romania", "SK": "Slovakia", "SI": "Slovenia", "ES": "Spain", "SE": "Sweden"}
	return names[code]
}

func parseInt(value any, fallback int) int {
	if n, ok := number(value); ok {
		return int(n)
	}
	if text, ok := value.(string); ok {
		if n, err := strconv.Atoi(text); err == nil {
			return n
		}
	}
	return fallback
}
