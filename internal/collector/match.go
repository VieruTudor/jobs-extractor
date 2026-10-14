package collector

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	boundaryCache  = map[string]*regexp.Regexp{}
	euCountryNames = []string{"Austria", "Belgium", "Bulgaria", "Croatia", "Cyprus", "Czechia", "Czech Republic", "Denmark", "Estonia", "Finland", "France", "Germany", "Deutschland", "Greece", "Hungary", "Ireland", "Italy", "Latvia", "Lithuania", "Luxembourg", "Malta", "Netherlands", "Poland", "Portugal", "Romania", "Slovakia", "Slovenia", "Spain", "Sweden"}
	nonEUCountries = []string{"Albania", "Andorra", "Armenia", "Azerbaijan", "Belarus", "Bosnia and Herzegovina", "Georgia", "Iceland", "Kosovo", "Liechtenstein", "Moldova", "Monaco", "Montenegro", "North Macedonia", "Norway", "Russia", "San Marino", "Serbia", "Switzerland", "Turkey", "Türkiye", "Ukraine", "United Kingdom", "Vatican"}
	outsideRegions = []string{"united states", "usa", "canada", "north america", "latin america", "asia", "apac", "australia", "india", "africa", "middle east", "china", "japan", "singapore", "brazil", "argentina", "new zealand", "south korea", "south africa", "mexico", "thailand", "indonesia", "philippines", "pakistan", "vietnam", "malaysia", "israel", "nigeria", "kenya", "egypt", "saudi arabia", "hong kong", "taiwan", "sri lanka", "bangladesh", "colombia", "chile", "peru"}
)

type Assessment struct {
	Status       string   `json:"status"`
	Reasons      []string `json:"reasons"`
	MatchedTerms []string `json:"matched_terms"`
}

func assessJob(job map[string]any, preferences map[string]any, geocodedCode string) Assessment {
	text := stringValue(job["title"]) + " " + stringValue(job["description"])
	var rejected, review, matched []string
	for _, field := range []struct{ name, haystack string }{{"roles", stringValue(job["title"])}, {"technologies", text}} {
		terms := stringList(preferences[field.name])
		var hits []string
		for _, term := range terms {
			if containsTerm(field.haystack, term) {
				hits = append(hits, term)
			}
		}
		if len(terms) > 0 && len(hits) == 0 {
			rejected = append(rejected, "No requested "+field.name+" found")
		}
		matched = append(matched, hits...)
	}
	for _, term := range stringList(preferences["required_keywords"]) {
		if !containsTerm(text, term) {
			rejected = append(rejected, "Missing required keyword: "+term)
		}
	}
	for _, term := range stringList(preferences["excluded_keywords"]) {
		if containsTerm(text, term) {
			rejected = append(rejected, "Excluded keyword: "+term)
		}
	}
	workModes := stringList(preferences["work_modes"])
	if len(workModes) > 0 && !containsString(workModes, stringValue(job["work_mode"])) {
		if stringValue(job["work_mode"]) == "unknown" {
			review = append(review, "Work mode does not establish a match")
		} else {
			rejected = append(rejected, "Work mode does not establish a match")
		}
	}
	contractTypes := stringList(preferences["contract_types"])
	if len(contractTypes) > 0 && !intersects(contractTypes, stringList(job["contract_types"])) {
		review = append(review, "Requested contract type not confirmed")
	}
	if countryFilter, ok := preferences["country_filter"].(string); ok && strings.TrimSpace(countryFilter) != "" {
		locationStatus := matchLocation(stringValue(job["location"]), stringList(job["location_restrictions"]), countryFilter, geocodedCode)
		switch locationStatus {
		case "mismatch":
			rejected = append(rejected, "Listing location does not match country filter: "+countryFilter)
		case "unknown":
			review = append(review, "Listing location does not confirm a country match: "+countryFilter)
		}
	}
	if maxAge, exists := preferences["posted_within_days"]; exists && maxAge != nil {
		days, ok := number(maxAge)
		postedAt := parseDate(job["published_at"])
		if !ok || postedAt.IsZero() {
			review = append(review, "Posting date is missing or unrecognized")
		} else {
			age := time.Since(postedAt).Hours() / 24
			if age > days {
				rejected = append(rejected, fmt.Sprintf("Posted more than %v days ago", maxAge))
			} else if age < 0 {
				review = append(review, "Posting date is in the future")
			}
		}
	}
	assessment := Assessment{Status: "match", Reasons: append(rejected, review...), MatchedTerms: uniqueStrings(matched)}
	if len(rejected) > 0 {
		assessment.Status = "excluded"
	} else if len(review) > 0 {
		assessment.Status = "review"
	}
	return assessment
}

func matchLocation(location string, restrictions []string, countryFilter, geocodedCode string) string {
	parts := append([]string(nil), restrictions...)
	if location != "" {
		parts = append(parts, location)
	}
	text := strings.ToLower(strings.Join(parts, " | "))
	if strings.TrimSpace(text) == "" {
		return "unknown"
	}
	filter := strings.ToLower(strings.TrimSpace(countryFilter))
	if filter == "eu" {
		if anyTerm(text, nonEUCountries) || anyTerm(text, outsideRegions) || hasCode(text, []string{"AL", "AD", "AM", "AZ", "BY", "BA", "GE", "IS", "XK", "LI", "MD", "MC", "ME", "MK", "NO", "RU", "SM", "RS", "CH", "TR", "UA", "GB", "UK", "VA", "US", "CA", "MX", "BR", "AR", "IN", "CN", "JP", "SG", "AU", "NZ", "KR", "ZA", "TH", "ID", "PH", "PK", "VN", "MY", "AE", "IL", "NG", "KE", "EG", "SA", "HK", "TW", "LK", "BD", "CO", "CL", "PE"}) {
			return "mismatch"
		}
		if containsTerm(text, "eu") || containsTerm(text, "european union") {
			return "match"
		}
		if anyTerm(text, euCountryNames) || hasCode(text, euCodes) {
			return "match"
		}
		if geocodedCode != "" {
			if containsString(euCodes, strings.ToUpper(geocodedCode)) {
				return "match"
			}
			return "mismatch"
		}
		return "unknown"
	}
	if filter == "europe" || filter == "european" {
		if anyTerm(text, []string{"worldwide", "anywhere in the world", "global remote", "europe", "eu", "european union", "eu/eea", "eea", "schengen"}) {
			return "match"
		}
		if anyTerm(text, outsideRegions) {
			return "mismatch"
		}
		if anyTerm(text, append(append([]string{}, euCountryNames...), nonEUCountries...)) {
			return "match"
		}
		return "unknown"
	}
	if containsTerm(text, countryFilter) {
		return "match"
	}
	return "mismatch"
}

func canonicalURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" {
		return ""
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	query := parsed.Query()
	for key := range query {
		if strings.HasPrefix(strings.ToLower(key), "utm_") {
			query.Del(key)
		}
	}
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	return parsed.String()
}

func containsTerm(text, term string) bool {
	term = strings.TrimSpace(term)
	if term == "" {
		return false
	}
	pattern, ok := boundaryCache[term]
	if !ok {
		pattern = regexp.MustCompile(fmt.Sprintf(`(?i)(^|[^\pL\pN_])%s([^\pL\pN_]|$)`, regexp.QuoteMeta(term)))
		boundaryCache[term] = pattern
	}
	return pattern.MatchString(text)
}

func anyTerm(text string, terms []string) bool {
	for _, term := range terms {
		if containsTerm(text, term) {
			return true
		}
	}
	return false
}

func hasCode(text string, codes []string) bool {
	for _, code := range codes {
		if containsTerm(text, code) {
			return true
		}
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func intersects(left, right []string) bool {
	for _, a := range left {
		if containsString(right, a) {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func number(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

func parseDate(value any) time.Time {
	switch v := value.(type) {
	case float64:
		return unixDate(v)
	case int64:
		return unixDate(float64(v))
	case int:
		return unixDate(float64(v))
	case string:
		if v == "" {
			return time.Time{}
		}
		if numeric, err := strconv.ParseFloat(v, 64); err == nil {
			return unixDate(numeric)
		}
		if year, err := time.Parse("2006", v); err == nil {
			return year
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02", "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, v); err == nil {
				return parsed.UTC()
			}
		}
	}
	return time.Time{}
}

func unixDate(value float64) time.Time {
	if value > 100_000_000_000 {
		value /= 1000
	}
	return time.Unix(int64(value), 0).UTC()
}
