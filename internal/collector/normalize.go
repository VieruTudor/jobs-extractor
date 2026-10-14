package collector

import (
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
)

var (
	tagPattern       = regexp.MustCompile(`<[^>]*>`)
	spacePattern     = regexp.MustCompile(`\s+`)
	contractPatterns = []struct{ token, label string }{{"contract", "contract"}, {"freelance", "freelance"}, {"b2b", "b2b"}, {"permanent", "permanent"}}
)

func plainText(value any) string {
	text := stringValue(value)
	text = tagPattern.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	return strings.TrimSpace(spacePattern.ReplaceAllString(text, " "))
}

func normalizeJob(kind string, row map[string]any) (map[string]any, error) {
	identifier, title, company, url, location := "", "", "", "", ""
	description, published := any(""), any(nil)
	remote := false
	var employment []string
	var restrictions []string
	salary := map[string]any{"text": nil}

	switch kind {
	case "arbeitnow":
		identifier = stringValue(row["slug"])
		title = stringValue(row["title"])
		company = stringValue(row["company_name"])
		url = stringValue(row["url"])
		location = stringValue(row["location"])
		description, published = row["description"], row["created_at"]
		remote, _ = row["remote"].(bool)
		employment = stringList(row["job_types"])
	case "himalayas":
		identifier = stringValue(row["guid"])
		title = stringValue(row["title"])
		company = stringValue(row["companyName"])
		url = firstNonEmpty(stringValue(row["applicationLink"]), identifier)
		for _, item := range listValue(row["locationRestrictions"]) {
			place := objectValue(item)
			value := firstNonEmpty(stringValue(place["name"]), stringValue(place["alpha2"]))
			if value == "" {
				value = stringValue(item)
			}
			if value != "" {
				restrictions = append(restrictions, value)
			}
		}
		location = strings.Join(restrictions, ", ")
		description, published = row["description"], row["pubDate"]
		remote = true
		employment = []string{stringValue(row["employmentType"])}
		salary = map[string]any{"min": row["minSalary"], "max": row["maxSalary"], "currency": row["currency"], "period": row["salaryPeriod"]}
	case "remotive":
		identifier = fmt.Sprint(row["id"])
		title, company = stringValue(row["title"]), stringValue(row["company_name"])
		url, location = stringValue(row["url"]), stringValue(row["candidate_required_location"])
		description, published = row["description"], row["publication_date"]
		remote = true
		employment = []string{stringValue(row["job_type"])}
		if location != "" {
			restrictions = []string{location}
		}
		salary = map[string]any{"text": row["salary"]}
	case "remoteok":
		identifier = firstNonEmpty(fmt.Sprint(row["id"]), stringValue(row["slug"]), stringValue(row["url"]))
		title, company = stringValue(row["position"]), stringValue(row["company"])
		url = firstNonEmpty(stringValue(row["url"]), stringValue(row["apply_url"]))
		location = stringValue(row["location"])
		var tags []string
		for _, tag := range listValue(row["tags"]) {
			tags = append(tags, fmt.Sprint(tag))
		}
		description = strings.TrimSpace(stringValue(row["description"]) + " " + strings.Join(tags, " "))
		published = firstNonNil(row["date"], row["epoch"])
		remote = true
		if location != "" {
			restrictions = []string{location}
		}
		salary = map[string]any{"min": nonZero(row["salary_min"]), "max": nonZero(row["salary_max"]), "currency": firstNonEmpty(stringValue(row["salary_currency"]), stringValue(row["currency"]))}
	case "adzuna":
		identifier = fmt.Sprint(row["id"])
		title = stringValue(row["title"])
		company = stringValue(objectValue(row["company"])["display_name"])
		url = firstNonEmpty(stringValue(row["redirect_url"]), stringValue(row["url"]))
		location = stringValue(objectValue(row["location"])["display_name"])
		description, published = row["description"], row["created"]
		employment = []string{stringValue(row["contract_type"]), stringValue(row["contract_time"])}
		restrictions = nonEmptyStrings(location)
		salary = map[string]any{"min": row["salary_min"], "max": row["salary_max"], "predicted": row["salary_is_predicted"], "currency": row["currency"]}
	case "jooble":
		identifier = firstNonEmpty(stringValue(row["id"]), stringValue(row["link"]))
		title, company = stringValue(row["title"]), stringValue(row["company"])
		url, location = stringValue(row["link"]), stringValue(row["location"])
		description, published = firstNonNil(row["snippet"], row["description"]), row["updated"]
		employment = []string{stringValue(row["type"])}
		restrictions = nonEmptyStrings(location)
		salary = map[string]any{"text": row["salary"]}
	case "jsonld", "google_cse":
		url = stringValue(row["url"])
		identifierValue := row["identifier"]
		if object, ok := identifierValue.(map[string]any); ok {
			identifierValue = firstNonEmpty(stringValue(object["value"]), stringValue(object["name"]))
		}
		identifier = firstNonEmpty(fmt.Sprint(identifierValue), url)
		if identifier == "<nil>" {
			identifier = url
		}
		title = stringValue(row["title"])
		company = stringValue(objectValue(row["hiringOrganization"])["name"])
		for _, place := range asObjects(row["jobLocation"]) {
			address := place["address"]
			if addressText, ok := address.(string); ok {
				restrictions = append(restrictions, addressText)
				continue
			}
			addressObject := objectValue(address)
			value := strings.Join(nonEmptyStrings(stringValue(addressObject["addressLocality"]), stringValue(addressObject["addressRegion"]), stringValue(addressObject["addressCountry"])), ", ")
			if value != "" {
				restrictions = append(restrictions, value)
			}
		}
		for _, place := range asObjects(row["applicantLocationRequirements"]) {
			restrictions = append(restrictions, firstNonEmpty(stringValue(place["name"]), stringValue(place["addressCountry"])))
		}
		restrictions = uniqueStrings(nonEmptyStrings(restrictions...))
		location = strings.Join(restrictions, ", ")
		description, published = row["description"], row["datePosted"]
		remote = strings.EqualFold(stringValue(row["jobLocationType"]), "TELECOMMUTE")
		employment = asStrings(row["employmentType"])
		salary = objectValue(row["baseSalary"])
		if len(salary) == 0 {
			salary = map[string]any{"text": nil}
		}
		if salary["currency"] == nil {
			salary["currency"] = salary["currencyCode"]
		}
	default:
		return nil, fmt.Errorf("unsupported source type %q", kind)
	}

	if kind == "remoteok" && (identifier == "" || title == "" || url == "") {
		return nil, fmt.Errorf("Remote OK listing is missing an id, title, or job URL")
	}
	if kind == "himalayas" && identifier == "" {
		return nil, fmt.Errorf("Himalayas listing is missing guid")
	}

	contractTypes := classifyContractTypes(employment)
	sourceURL := url
	if kind == "himalayas" {
		sourceURL = identifier
	}
	return map[string]any{
		"source": kind, "source_id": identifier, "title": title, "company": company,
		"url": url, "source_url": sourceURL, "description": plainText(description),
		"location": location, "location_restrictions": restrictions, "published_at": published,
		"work_mode": workMode(remote), "contract_types": contractTypes,
		"employment_type_raw": employment, "compensation": salary, "raw": row,
	}, nil
}

func normalizeBoard(provider, slug, company string, row map[string]any) (map[string]any, error) {
	identifier, title, location, url, description := "", "", "", "", ""
	var published any
	remote := false
	var employment []string
	switch provider {
	case "greenhouse":
		identifier = fmt.Sprint(row["id"])
		title = stringValue(row["title"])
		place := row["location"]
		if obj, ok := place.(map[string]any); ok {
			location = stringValue(obj["name"])
		} else {
			location = stringValue(place)
		}
		url, description = stringValue(row["absolute_url"]), stringValue(row["content"])
		published = firstNonNil(row["first_published"], row["published_at"])
	case "lever":
		identifier = fmt.Sprint(row["id"])
		title = stringValue(row["text"])
		categories := objectValue(row["categories"])
		location = stringValue(categories["location"])
		url, description, published = stringValue(row["hostedUrl"]), stringValue(row["description"]), row["createdAt"]
		remote = strings.EqualFold(stringValue(row["workplaceType"]), "remote")
		employment = []string{stringValue(categories["commitment"])}
	case "ashby":
		identifier = firstNonEmpty(stringValue(row["id"]), stringValue(row["jobUrl"]))
		title, location = stringValue(row["title"]), stringValue(row["location"])
		url = stringValue(row["jobUrl"])
		description = firstNonEmpty(stringValue(row["descriptionPlain"]), stringValue(row["descriptionHtml"]))
		published = row["publishedAt"]
		remote = row["isRemote"] == true || strings.EqualFold(stringValue(row["workplaceType"]), "remote")
		employment = []string{stringValue(row["employmentType"])}
	default:
		return nil, fmt.Errorf("unsupported board provider %q", provider)
	}

	return map[string]any{
		"source": provider + ":" + slug, "source_id": identifier, "title": title,
		"company": company, "url": url, "source_url": url,
		"description": plainText(description), "location": location,
		"location_restrictions": nonEmptyStrings(location), "published_at": published,
		"work_mode": workMode(remote), "contract_types": classifyContractTypes(employment),
		"employment_type_raw": employment, "compensation": map[string]any{"text": nil}, "raw": row,
	}, nil
}

func classifyContractTypes(values []string) []string {
	text := strings.ToLower(strings.ReplaceAll(strings.Join(values, " "), "_", " "))
	var result []string
	for _, pattern := range contractPatterns {
		if strings.Contains(text, pattern.token) {
			result = append(result, pattern.label)
		}
	}
	sort.Strings(result)
	return result
}

func workMode(remote bool) string {
	if remote {
		return "remote"
	}
	return "unknown"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" && value != "<nil>" {
			return value
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil && value != "" {
			return value
		}
	}
	return nil
}

func nonEmptyStrings(values ...string) []string {
	var result []string
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func asObjects(value any) []map[string]any {
	switch item := value.(type) {
	case []any:
		return rowObjects(item)
	case map[string]any:
		return []map[string]any{item}
	default:
		return nil
	}
}

func asStrings(value any) []string {
	switch item := value.(type) {
	case []any:
		return stringList(item)
	case string:
		return []string{item}
	default:
		return nil
	}
}

func nonZero(value any) any {
	if value == nil || value == float64(0) || value == "0" {
		return nil
	}
	return value
}
