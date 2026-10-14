package collector

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var (
	geocodeSpace = regexp.MustCompile(`\s+`)
	remoteWords  = regexp.MustCompile(`(?i)\b(?:fully\s+remote|remote|hybrid|on[ -]?site|work from home)\b`)
)

type geocodeStats struct {
	Provider   string `json:"provider"`
	Requests   int    `json:"requests"`
	CacheHits  int    `json:"cache_hits"`
	Resolved   int    `json:"resolved"`
	Ambiguous  int    `json:"ambiguous"`
	NotFound   int    `json:"not_found"`
	Errors     int    `json:"errors"`
	LimitSkips int    `json:"limit_skips"`
}

type geocodeResolver struct {
	store    *Store
	client   *http.Client
	settings map[string]any
	stats    geocodeStats
	runCache map[string]string
	known    map[string]bool
}

func newGeocodeResolver(store *Store, settings map[string]any) *geocodeResolver {
	client := newHTTPClient(settings)
	return &geocodeResolver{
		store:    store,
		client:   client,
		settings: settings,
		stats:    geocodeStats{Provider: "Open-Meteo Geocoding API / GeoNames"},
		runCache: make(map[string]string),
		known:    make(map[string]bool),
	}
}

func (g *geocodeResolver) Resolve(ctx context.Context, location string, restrictions []string) string {
	query := geocodeQuery(location, restrictions)
	if query == "" {
		return ""
	}
	key := "v2:" + strings.ToLower(geocodeSpace.ReplaceAllString(strings.TrimSpace(query), " "))
	if g.known[key] {
		return g.runCache[key]
	}
	if code, resolution, found, err := g.store.Geocode(key); err == nil && found {
		g.known[key] = true
		if resolution == "resolved" {
			g.runCache[key] = code
		}
		g.stats.CacheHits++
		return g.runCache[key]
	}
	if g.stats.Requests >= numberSetting(g.settings, "max_lookups_per_run", 50) {
		g.stats.LimitSkips++
		g.known[key] = true
		return ""
	}
	queryValues := url.Values{"name": {query}, "count": {"20"}, "language": {"en"}, "format": {"json"}}
	endpoint := fetchURL("https://geocoding-api.open-meteo.com/v1/search", queryValues)
	g.stats.Requests++
	payload, err := fetchJSON(ctx, g.client, g.settings, endpoint)
	if err != nil {
		g.stats.Errors++
		g.known[key] = true
		return ""
	}
	results := rowObjects(listValue(objectValue(payload)["results"]))
	codes := make(map[string]bool)
	for _, result := range results {
		if code := strings.ToUpper(stringValue(result["country_code"])); code != "" {
			codes[code] = true
		}
	}
	code := ""
	if len(codes) == 1 {
		for value := range codes {
			code = value
		}
	} else if len(results) > 0 {
		top := results[0]
		topCode := strings.ToUpper(stringValue(top["country_code"]))
		topName := strings.ToLower(stringValue(top["name"]))
		baseName := strings.ToLower(strings.TrimSpace(strings.SplitN(query, ",", 2)[0]))
		topPopulation, _ := number(top["population"])
		strongestOther := 0.0
		for _, item := range results[1:] {
			if strings.ToUpper(stringValue(item["country_code"])) != topCode {
				population, _ := number(item["population"])
				if population > strongestOther {
					strongestOther = population
				}
			}
		}
		feature := stringValue(top["feature_code"])
		primaryFeature := feature == "PPLC" || feature == "PPLA" || feature == "PPLA2"
		if topCode != "" && topName == baseName && feature == "PPLC" {
			code = topCode
		} else if topCode != "" && topName == baseName && primaryFeature && topPopulation >= max(100_000, 5*strongestOther) {
			code = topCode
		}
	}
	resolution := "not_found"
	if len(results) > 0 {
		resolution = "ambiguous"
	}
	if code != "" {
		resolution = "resolved"
		g.stats.Resolved++
	} else if len(results) > 0 {
		g.stats.Ambiguous++
	} else {
		g.stats.NotFound++
	}
	if err := g.store.SaveGeocode(key, code, resolution); err != nil {
		g.stats.Errors++
	}
	g.known[key] = true
	g.runCache[key] = code
	return code
}

func (g *geocodeResolver) Stats() geocodeStats { return g.stats }

func geocodeQuery(location string, restrictions []string) string {
	value := strings.TrimSpace(location)
	if value == "" {
		if len(restrictions) == 1 {
			value = strings.TrimSpace(restrictions[0])
		}
	}
	value = plainText(value)
	value = remoteWords.ReplaceAllString(value, " ")
	value = strings.Trim(geocodeSpace.ReplaceAllString(value, " "), " ,;|-/()")
	if value == "" || strings.ContainsAny(value, ";|/") {
		return ""
	}
	switch strings.ToLower(value) {
	case "europe", "eu", "european union", "worldwide", "global", "anywhere", "remote":
		return ""
	}
	if strings.EqualFold(value, "Koln") {
		return "Cologne, Germany"
	}
	if strings.EqualFold(value, "Munchen") {
		return "Munich, Germany"
	}
	if strings.EqualFold(value, "Nurnberg") {
		return "Nuremberg, Germany"
	}
	return value
}

func (g geocodeStats) String() string {
	return fmt.Sprintf("%d requests, %d resolved, %d ambiguous", g.Requests, g.Resolved, g.Ambiguous)
}
