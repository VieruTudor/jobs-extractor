"""Source HTTP clients and provider adapters."""
import json
import re
import time
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, urlopen
from .common import now
from .model import EU_CODES, normalize, normalize_board

def collect_board(board, settings):
    provider, slug = board["provider"], board["slug"]
    if provider == "greenhouse":
        url = f"https://boards-api.greenhouse.io/v1/boards/{slug}/jobs?content=true"
    elif provider == "lever":
        base = "https://api.eu.lever.co" if board.get("region") == "eu" else "https://api.lever.co"
        url = f"{base}/v0/postings/{slug}?mode=json"
    elif provider == "ashby":
        url = f"https://api.ashbyhq.com/posting-api/job-board/{slug}?includeCompensation=true"
    else:
        raise ValueError(f"Unsupported ATS: {provider}")
    data = fetch(url, settings)
    rows = data.get("jobs", []) if provider in ("greenhouse", "ashby") else data
    if not isinstance(rows, list):
        raise ValueError("Invalid ATS job list")
    company = board.get("name") or slug
    return [normalize_board(provider, slug, company, row) for row in rows]

def resolve_location_country(db, query, settings, run_cache, stats):
    """Resolve and cache a single location's ISO country code with Open-Meteo."""
    # Version the cache key when the disambiguation policy changes.
    key = "v2:" + re.sub(r"\s+", " ", query).strip().casefold()
    if key in run_cache:
        return run_cache[key]
    cached = db.execute("SELECT country_code, resolution FROM location_geocodes WHERE query=?", (key,)).fetchone()
    if cached:
        code = cached[0] if cached[1] == "resolved" else None
        run_cache[key] = code
        stats["cache_hits"] += 1
        return code
    if stats["requests"] >= settings.get("max_lookups_per_run", 50):
        stats["limit_skips"] += 1
        run_cache[key] = None
        return None

    url = "https://geocoding-api.open-meteo.com/v1/search?" + urlencode({
        "name": query, "count": 20, "language": "en", "format": "json",
    })
    request = Request(url, headers={"User-Agent": "JobsListingExtractor/1.0 (personal EU location filtering)"})
    stats["requests"] += 1
    try:
        with urlopen(request, timeout=settings.get("timeout_seconds", 8)) as response:
            data = json.loads(response.read().decode("utf-8"))
        results = data.get("results", [])
        codes = {str(item.get("country_code", "")).upper() for item in results if item.get("country_code")}
    except Exception:
        stats["errors"] += 1
        run_cache[key] = None
        return None

    code = None
    if len(codes) == 1:
        code = next(iter(codes))
    elif results:
        # Open-Meteo returns relevance-ranked results. A large city or capital
        # may safely disambiguate a common bare name; weak ties stay in review.
        top = results[0]
        top_code = str(top.get("country_code", "")).upper()
        top_name = str(top.get("name", "")).casefold()
        base_name = query.split(",", 1)[0].strip().casefold()
        top_population = int(top.get("population") or 0)
        other_populations = [int(item.get("population") or 0) for item in results
                             if str(item.get("country_code", "")).upper() != top_code]
        strongest_other = max(other_populations, default=0)
        primary_feature = top.get("feature_code") in {"PPLC", "PPLA", "PPLA2"}
        exact_name = top_name == base_name
        if top_code and exact_name and top.get("feature_code") == "PPLC":
            code = top_code
        elif top_code and exact_name and primary_feature and top_population >= max(100_000, 5 * strongest_other):
            code = top_code

    if code:
        resolution = "resolved"
        stats["resolved"] += 1
    elif results:
        code = None
        resolution = "ambiguous"
        stats["ambiguous"] += 1
    else:
        code = None
        resolution = "not_found"
        stats["not_found"] += 1
    with db:
        db.execute("INSERT OR REPLACE INTO location_geocodes(query,country_code,resolution,resolved_at) VALUES(?,?,?,?)",
                   (key, code, resolution, now()))
    run_cache[key] = code
    return code

def fetch(url, settings):
    for attempt in range(settings["retries"] + 1):
        try:
            request = Request(url, headers={"User-Agent": "PersonalJobCollector/0.1", "Accept": "application/json"})
            with urlopen(request, timeout=settings["timeout_seconds"]) as response:
                return json.load(response)
        except Exception as error:
            if isinstance(error, HTTPError) and error.code in (400, 401, 403, 404):
                raise
            if attempt == settings["retries"]:
                raise
            time.sleep(min(30, 2 ** (attempt + 1)))

def collect_himalayas_search(settings, preferences):
    """Fetch targeted Himalayas results by configured terms and EU countries."""
    terms = preferences.get("technologies") or preferences.get("roles") or []
    query_terms, seen_terms = [], set()
    for term in terms:
        # Himalayas indexes .NET and Dotnet as the same search term.
        value = ".NET" if term.casefold() in (".net", "dotnet") else term
        if value.casefold() not in seen_terms:
            seen_terms.add(value.casefold())
            query_terms.append(value)
    if not query_terms:
        return None

    max_requests = settings.get("max_himalayas_search_requests", 250)
    request_count, rows_by_id = 0, {}
    for country_code in sorted(EU_CODES):
        for term in query_terms:
            page, total_count = 1, None
            while True:
                if request_count >= max_requests:
                    print(f"  himalayas: filtered search reached request cap ({max_requests}); results may be partial", flush=True)
                    return list(rows_by_id.values()), False
                url = "https://himalayas.app/jobs/api/search?" + urlencode({
                    "country": country_code, "exclude_worldwide": "true", "q": term,
                    "sort": "recent", "page": page,
                })
                try:
                    data = fetch(url, settings)
                except Exception as error:
                    print(f"  himalayas filtered search stopped after {request_count} requests: {error}", flush=True)
                    return list(rows_by_id.values()), False
                request_count += 1
                batch = data.get("jobs", [])
                if not isinstance(batch, list):
                    raise ValueError("Invalid Himalayas search job list")
                for row in batch:
                    job = normalize("himalayas", row)
                    rows_by_id[job["source_id"]] = job
                if total_count is None:
                    total_count = int(data.get("totalCount") or 0)
                if not batch or page * 20 >= total_count:
                    break
                page += 1
                if settings["page_delay_seconds"]:
                    time.sleep(settings["page_delay_seconds"])
    print(f"  himalayas: searched {len(EU_CODES)} EU countries and {len(query_terms)} configured terms; "
          f"{request_count} API requests, {len(rows_by_id)} unique listings", flush=True)
    return list(rows_by_id.values()), True

def collect(kind, settings, preferences=None, source_config=None):
    source_config = source_config or {"type": kind}
    if kind == "adzuna":
        from .providers import collect_adzuna
        return collect_adzuna(source_config, settings, preferences or {})
    if kind == "jooble":
        from .providers import collect_jooble
        return collect_jooble(source_config, settings, preferences or {})
    if kind == "jsonld":
        from .scraping import collect_jsonld
        return collect_jsonld(source_config, settings)
    if kind == "google_cse":
        from .scraping import collect_google_cse
        return collect_google_cse(source_config, settings, preferences or {})
    if kind == "remoteok":
        tags = source_config.get("tags") or ["dev"]
        url = "https://remoteok.com/api?" + urlencode({"tags": ",".join(tags)})
        data = fetch(url, settings)
        if not isinstance(data, list):
            raise ValueError("Invalid Remote OK job feed")
        # The first array item contains feed-level legal metadata, not a job.
        rows = [row for row in data if isinstance(row, dict) and (row.get("id") or row.get("slug"))]
        unique = {}
        for row in rows:
            job = normalize("remoteok", row)
            unique[job["source_id"]] = job
        return list(unique.values()), True
    if kind == "himalayas" and preferences and str(preferences.get("country_filter") or "").casefold() == "eu":
        targeted = collect_himalayas_search(settings, preferences)
        if targeted is not None:
            return targeted
    rows, cursor, seen = [], None, set()
    for page in range(1, settings["max_pages"] + 1):
        if kind == "arbeitnow":
            url = f"https://www.arbeitnow.com/api/job-board-api?page={page}"
        elif kind == "himalayas":
            url = "https://himalayas.app/jobs/api?" + urlencode({"limit": 20, **({"cursor": cursor} if cursor else {})})
        elif kind == "remotive":
            url = "https://remotive.com/api/remote-jobs"
        else:
            raise ValueError(f"Unsupported source: {kind}")
        data = fetch(url, settings)
        batch = data["data"] if kind == "arbeitnow" else data["jobs"]
        if not isinstance(batch, list):
            raise ValueError("Invalid job list")
        rows.extend(normalize(kind, row) for row in batch)
        if page % 10 == 0:
            print(f"  {kind}: page {page}, {len(rows)} listings", flush=True)
        if kind == "remotive":
            return rows, True
        if kind == "arbeitnow":
            more = data.get("links", {}).get("next")
        else:
            cursor = data.get("nextCursor")
            more = cursor
        if not more:
            return rows, True
        if str(more) in seen:
            raise ValueError("Repeated pagination token")
        seen.add(str(more))
        time.sleep(settings["page_delay_seconds"])
    return rows, False
