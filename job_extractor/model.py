"""Job normalization and matching rules."""
import re
from datetime import datetime, timezone
from html.parser import HTMLParser
from urllib.parse import urlencode, urlsplit, urlunsplit, parse_qsl

class TextParser(HTMLParser):
    def __init__(self):
        super().__init__()
        self.parts = []

    def handle_data(self, data):
        self.parts.append(data)

def plain(value):
    parser = TextParser()
    parser.feed(value or "")
    return re.sub(r"\s+", " ", " ".join(parser.parts)).strip()

def canonical(url):
    p = urlsplit(url or "")
    query = [(k, v) for k, v in parse_qsl(p.query) if not k.lower().startswith("utm_")]
    return urlunsplit((p.scheme.lower(), p.netloc.lower(), p.path.rstrip("/"), urlencode(query), ""))

def contains(text, term):
    return bool(re.search(r"(?<!\w)" + re.escape(term.lower()) + r"(?!\w)", text.lower()))

EUROPE_COUNTRIES = (
    "Albania", "Andorra", "Armenia", "Austria", "Azerbaijan", "Belarus", "Belgium",
    "Bosnia and Herzegovina", "Bulgaria", "Croatia", "Cyprus", "Czechia", "Czech Republic",
    "Denmark", "Estonia", "Finland", "France", "Georgia", "Germany", "Greece", "Hungary",
    "Iceland", "Ireland", "Italy", "Kazakhstan", "Kosovo", "Latvia", "Liechtenstein",
    "Lithuania", "Luxembourg", "Malta", "Moldova", "Monaco", "Montenegro", "Netherlands",
    "North Macedonia", "Norway", "Poland", "Portugal", "Romania", "Russia", "San Marino",
    "Serbia", "Slovakia", "Slovenia", "Spain", "Sweden", "Switzerland", "Turkey",
    "Türkiye", "Ukraine", "United Kingdom", "Vatican City", "Vatican",
)

EUROPE_CODES = set("AL AD AM AT AZ BY BE BA BG HR CY CZ DK EE FI FR GE DE GR HU IS IE IT KZ XK LV LI LT LU MT MD MC ME NL MK NO PL PT RO RU SM RS SK SI ES SE CH TR UA GB UK VA".split())

EU_COUNTRIES = (
    "Austria", "Belgium", "Bulgaria", "Croatia", "Cyprus", "Czechia", "Czech Republic",
    "Denmark", "Estonia", "Finland", "France", "Germany", "Deutschland", "Greece", "Hungary",
    "Ireland", "Italy", "Latvia", "Lithuania", "Luxembourg", "Malta", "Netherlands",
    "Poland", "Portugal", "Romania", "Slovakia", "Slovenia", "Spain", "Sweden",
)

EU_CODES = set("AT BE BG HR CY CZ DK EE FI FR DE GR HU IE IT LV LT LU MT NL PL PT RO SK SI ES SE".split())

NON_EU_EUROPE_COUNTRIES = tuple(name for name in EUROPE_COUNTRIES if name not in EU_COUNTRIES)

NON_EU_EUROPE_CODES = EUROPE_CODES - EU_CODES

ELIGIBLE_REGIONS = ("worldwide", "anywhere in the world", "global remote", "europe", "eu", "european union", "eu/eea", "eea", "schengen")

EU_ONLY_REGIONS = ("eu", "european union")

OUTSIDE_EUROPE_REGIONS = (
    "united states", "united states of america", "usa", "canada", "north america",
    "latin america", "latam", "asia", "apac", "australia", "india", "africa",
    "middle east", "china", "japan", "singapore", "brazil", "argentina", "new zealand",
    "south korea", "south africa", "mexico", "thailand", "indonesia", "philippines",
    "pakistan", "vietnam", "malaysia", "united arab emirates", "uae", "israel",
    "turkey", "türkiye", "russia", "nigeria", "kenya", "egypt", "saudi arabia",
    "hong kong", "taiwan", "sri lanka", "bangladesh", "colombia", "chile", "peru",
)

OUTSIDE_EUROPE_CODES = set("US CA MX BR AR IN CN JP SG AU NZ KR ZA TH ID PH PK VN MY AE IL RU NG KE EG SA HK TW LK BD CO CL PE".split())

def match_location_filter(location, restrictions, country_filter, geocoded_country_code=None):
    """Compare a listing's stated locations with its configured country filter."""
    values = [str(value) for value in restrictions if value]
    if location:
        values.append(str(location))
    raw_text = " | ".join(values)
    text = raw_text.lower()
    if not text:
        return "unknown"
    filter_name = country_filter.strip().casefold()
    if filter_name == "eu":
        # This is a strict country filter: mixed or broad regions do not count
        # as EU-only because they can include non-EU locations.
        tokens = set(re.findall(r"(?<!\w)[A-Z]{2}(?!\w)", raw_text))
        if any(contains(text, name) for name in NON_EU_EUROPE_COUNTRIES):
            return "mismatch"
        if tokens & NON_EU_EUROPE_CODES:
            return "mismatch"
        if any(contains(text, phrase) for phrase in OUTSIDE_EUROPE_REGIONS):
            return "mismatch"
        if tokens & OUTSIDE_EUROPE_CODES:
            return "mismatch"
        if any(contains(text, phrase) for phrase in EU_ONLY_REGIONS):
            return "match"
        if any(contains(text, name) for name in EU_COUNTRIES):
            return "match"
        if tokens & EU_CODES:
            return "match"
        if geocoded_country_code:
            return "match" if geocoded_country_code.upper() in EU_CODES else "mismatch"
        return "unknown"
    if filter_name in ("europe", "european"):
        if any(contains(text, phrase) for phrase in ELIGIBLE_REGIONS):
            return "match"
        if any(contains(text, phrase) for phrase in OUTSIDE_EUROPE_REGIONS):
            return "mismatch"
        if any(contains(text, name) for name in EUROPE_COUNTRIES):
            return "match"
        tokens = set(re.findall(r"(?<!\w)[A-Z]{2}(?!\w)", raw_text))
        if tokens & EUROPE_CODES:
            return "match"
        if tokens & OUTSIDE_EUROPE_CODES:
            return "mismatch"
        return "unknown"
    if contains(text, country_filter):
        return "match"
    return "mismatch" if text else "unknown"

def parse_posted_at(value):
    if value is None or value == "":
        return None
    if isinstance(value, (int, float)) or (isinstance(value, str) and value.strip().isdigit()):
        seconds = float(value)
        if seconds > 100_000_000_000:
            seconds /= 1000
        try:
            return datetime.fromtimestamp(seconds, tz=timezone.utc)
        except (OverflowError, OSError, ValueError):
            return None
    try:
        parsed = datetime.fromisoformat(str(value).strip().replace("Z", "+00:00"))
        if parsed.tzinfo is None:
            parsed = parsed.replace(tzinfo=timezone.utc)
        return parsed.astimezone(timezone.utc)
    except (TypeError, ValueError, OverflowError):
        return None

def normalize(kind, row):
    if kind == "arbeitnow":
        ident, title, company = row["slug"], row["title"], row["company_name"]
        url, location = row["url"], row.get("location", "")
        description, published = row.get("description", ""), row.get("created_at")
        remote, employment = row.get("remote"), row.get("job_types", [])
        restrictions, salary = [], {"text": None}
    elif kind == "himalayas":
        ident, title, company = row["guid"], row["title"], row["companyName"]
        url = row.get("applicationLink") or row["guid"]
        restrictions = row.get("locationRestrictions") or []
        restrictions = [str(item.get("name") or item.get("alpha2") or "") if isinstance(item, dict) else str(item)
                        for item in restrictions]
        restrictions = [item for item in restrictions if item]
        location = ", ".join(restrictions)
        description, published = row.get("description", ""), row.get("pubDate")
        remote, employment = True, [row.get("employmentType", "")]
        salary = {k: row.get(k) for k in ("minSalary", "maxSalary", "currency", "salaryPeriod")}
    elif kind == "remotive":
        ident, title, company = str(row["id"]), row["title"], row["company_name"]
        url, location = row["url"], row.get("candidate_required_location", "")
        description, published = row.get("description", ""), row.get("publication_date")
        remote, employment = True, [row.get("job_type", "")]
        restrictions, salary = ([location] if location else []), {"text": row.get("salary")}
    elif kind == "adzuna":
        ident, title = str(row["id"]), row["title"]
        company = (row.get("company") or {}).get("display_name", "")
        url = row.get("redirect_url") or row.get("url", "")
        location = (row.get("location") or {}).get("display_name", "")
        description, published = row.get("description", ""), row.get("created")
        remote, employment = None, [row.get("contract_type", ""), row.get("contract_time", "")]
        restrictions = [location] if location else []
        salary = {key: row.get(key) for key in ("salary_min", "salary_max", "salary_is_predicted")}
        salary["currency"] = row.get("currency", "")
    elif kind == "jooble":
        ident, title, company = str(row.get("id") or row.get("link") or ""), row["title"], row.get("company", "")
        url, location = row.get("link", ""), row.get("location", "")
        description, published = row.get("snippet", row.get("description", "")), row.get("updated")
        remote, employment = None, [row.get("type", "")]
        restrictions, salary = ([location] if location else []), {"text": row.get("salary")}
    elif kind in ("jsonld", "google_cse"):
        url = row.get("url", "")
        identifier = row.get("identifier")
        if isinstance(identifier, dict):
            identifier = identifier.get("value") or identifier.get("name")
        ident = str(identifier or url)
        if not ident:
            raise ValueError("JSON-LD JobPosting has no stable identifier or URL")
        title = row.get("title", "")
        company = (row.get("hiringOrganization") or {}).get("name", "")
        def address_text(place):
            address = place.get("address", {}) if isinstance(place, dict) else {}
            if isinstance(address, str):
                return address
            return ", ".join(str(address.get(key, "")) for key in
                              ("addressLocality", "addressRegion", "addressCountry") if address.get(key))
        places = row.get("jobLocation") or []
        if isinstance(places, dict):
            places = [places]
        restrictions = [address_text(place) for place in places if address_text(place)]
        applicant_places = row.get("applicantLocationRequirements") or []
        if isinstance(applicant_places, dict):
            applicant_places = [applicant_places]
        restrictions.extend(str(place.get("name") or "") if isinstance(place, dict) else str(place)
                            for place in applicant_places)
        location = ", ".join(dict.fromkeys(item for item in restrictions if item))
        description, published = row.get("description", ""), row.get("datePosted")
        remote = str(row.get("jobLocationType", "")).upper() == "TELECOMMUTE"
        employment = row.get("employmentType", [])
        if isinstance(employment, str):
            employment = [employment]
        salary = row.get("baseSalary") or {"text": None}
        if isinstance(salary, dict):
            salary = {**salary, "currency": salary.get("currency") or salary.get("currencyCode")}
    else:
        raise ValueError(f"Unsupported source: {kind}")
    text = plain(description)
    employment_text = " ".join(employment).lower().replace("_", " ")
    types = []
    for pattern, label in [("contract", "contract"), ("freelance", "freelance"), ("b2b", "b2b")]:
        if pattern in employment_text:
            types.append(label)
    if "permanent" in employment_text:
        types.append("permanent")
    return {"source": kind, "source_id": str(ident), "title": title, "company": company,
            "url": url, "source_url": str(ident) if kind == "himalayas" else url,
            "description": text, "location": location,
            "location_restrictions": restrictions, "published_at": published,
            "work_mode": "remote" if remote is True else "unknown",
            "contract_types": sorted(set(types)), "employment_type_raw": employment,
            "compensation": salary, "raw": row}

def normalize_board(provider, slug, company, row):
    if provider == "greenhouse":
        ident = str(row["id"])
        title = row["title"]
        loc = row.get("location") or {}
        location = loc.get("name", "") if isinstance(loc, dict) else str(loc)
        url, description = row.get("absolute_url", ""), row.get("content", "")
        published = row.get("first_published") or row.get("published_at")
        remote, employment = None, []
    elif provider == "lever":
        ident, title = str(row["id"]), row["text"]
        categories = row.get("categories") or {}
        location = categories.get("location", "")
        url, description, published = row.get("hostedUrl", ""), row.get("description", ""), row.get("createdAt")
        remote = row.get("workplaceType", "").casefold() == "remote"
        employment = [categories.get("commitment", "")]
    elif provider == "ashby":
        ident, title = str(row.get("id") or row.get("jobUrl", "")), row["title"]
        location = row.get("location", "")
        url = row.get("jobUrl", "")
        description = row.get("descriptionPlain") or row.get("descriptionHtml", "")
        published = row.get("publishedAt")
        remote = row.get("isRemote") is True or row.get("workplaceType") == "Remote"
        employment = [row.get("employmentType", "")]
    else:
        raise ValueError(f"Unsupported ATS: {provider}")
    return {"source": f"{provider}:{slug}", "source_id": ident, "title": title,
            "company": company, "url": url, "source_url": url,
            "description": plain(description), "location": location,
            "location_restrictions": [location] if location else [], "published_at": published,
            "work_mode": "remote" if remote is True else "unknown",
            "contract_types": [], "employment_type_raw": employment,
            "compensation": {"text": None}, "raw": row}

def assess(job, prefs, location_country_code=None):
    text = job["title"] + " " + job["description"]
    rejected, review, matched = [], [], []
    for field, haystack in [("roles", job["title"]), ("technologies", text)]:
        values = prefs.get(field, [])
        hits = [v for v in values if contains(haystack, v)]
        if values and not hits:
            rejected.append(f"No requested {field} found")
        matched.extend(hits)
    for word in prefs.get("required_keywords", []):
        if not contains(text, word):
            rejected.append(f"Missing required keyword: {word}")
    for word in prefs.get("excluded_keywords", []):
        if contains(text, word):
            rejected.append(f"Excluded keyword: {word}")
    modes = prefs.get("work_modes", [])
    if modes and job["work_mode"] not in modes:
        (review if job["work_mode"] == "unknown" else rejected).append("Work mode does not establish a match")
    types = prefs.get("contract_types", [])
    if types and not set(types).intersection(job["contract_types"]):
        review.append("Requested contract type not confirmed")
    country_filter = prefs.get("country_filter")
    if country_filter:
        location_match = match_location_filter(job["location"], job.get("location_restrictions", []), country_filter, location_country_code)
        if location_match == "mismatch":
            rejected.append(f"Listing location does not match country filter: {country_filter}")
        elif location_match == "unknown":
            review.append(f"Listing location does not confirm a country match: {country_filter}")
    posted_within_days = prefs.get("posted_within_days")
    if posted_within_days is not None:
        posted_at = parse_posted_at(job.get("published_at"))
        if posted_at is None:
            review.append("Posting date is missing or unrecognized")
        else:
            age_days = (datetime.now(timezone.utc) - posted_at).total_seconds() / 86400
            if age_days > posted_within_days:
                rejected.append(f"Posted more than {posted_within_days} days ago")
            elif age_days < 0:
                review.append("Posting date is in the future")
    return {"status": "excluded" if rejected else "review" if review else "match",
            "reasons": rejected + review, "matched_terms": matched}

def geocode_query(location, restrictions):
    """Return one specific place query, or None for broad/multi-region locations."""
    value = str(location or "").strip()
    if not value:
        values = [str(item).strip() for item in restrictions if item]
        value = values[0] if len(set(values)) == 1 else ""
    value = plain(value)
    value = re.sub(r"\b(?:fully\s+)?remote\b|\bhybrid\b|\bon[ -]?site\b|\bwork from home\b", "", value, flags=re.I)
    value = re.sub(r"\s+", " ", value).strip(" ,;|-/()")
    if not value or re.search(r"[;|/]", value):
        return None
    if value.casefold() in {"europe", "eu", "european union", "worldwide", "global", "anywhere", "remote"}:
        return None
    alias_key = value.casefold().translate(str.maketrans({"ä": "a", "ö": "o", "ü": "u", "ß": "ss"}))
    aliases = {"koln": "Cologne, Germany", "munchen": "Munich, Germany", "nurnberg": "Nuremberg, Germany"}
    return aliases.get(alias_key, value)
