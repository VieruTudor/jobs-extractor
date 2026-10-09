"""Optional API-backed aggregators that use user-supplied environment secrets."""
import json
import os
import time
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, urlopen

from .model import EU_CODES, normalize


ADZUNA_EU_COUNTRIES = {
    "AT": "at", "BE": "be", "DE": "de", "ES": "es",
    "FR": "fr", "IT": "it", "NL": "nl", "PL": "pl",
}
EU_CODE_TO_NAME = {
    "AT": "Austria", "BE": "Belgium", "BG": "Bulgaria", "HR": "Croatia",
    "CY": "Cyprus", "CZ": "Czechia", "DK": "Denmark", "EE": "Estonia",
    "FI": "Finland", "FR": "France", "DE": "Germany", "GR": "Greece",
    "HU": "Hungary", "IE": "Ireland", "IT": "Italy", "LV": "Latvia",
    "LT": "Lithuania", "LU": "Luxembourg", "MT": "Malta", "NL": "Netherlands",
    "PL": "Poland", "PT": "Portugal", "RO": "Romania", "SK": "Slovakia",
    "SI": "Slovenia", "ES": "Spain", "SE": "Sweden",
}


def missing_credentials(source):
    """Describe absent credentials without ever returning secret values."""
    provider = source["type"]
    if provider == "adzuna":
        names = (source.get("app_id_env", "ADZUNA_APP_ID"), source.get("app_key_env", "ADZUNA_APP_KEY"))
        return [name for name in names if not os.environ.get(name)]
    if provider == "jooble":
        key_envs = source.get("api_key_envs", {})
        present = [code for code, name in key_envs.items() if os.environ.get(name)]
        missing = [name for code, name in key_envs.items() if code.upper() in EU_CODES and not os.environ.get(name)]
        missing.extend(f"Jooble domain for {code.upper()}" for code in present if code.upper() not in source.get("domains", {}))
        if not present:
            missing.append("JOOBLE_API_KEY_<EU_COUNTRY_CODE> (country-specific keys)")
        return missing
    if provider == "google_cse":
        names = (source.get("api_key_env", "GOOGLE_CSE_API_KEY"), source.get("engine_id_env", "GOOGLE_CSE_ENGINE_ID"))
        missing = [name for name in names if not os.environ.get(name)]
        if not source.get("terms_allow_automated_access"):
            missing.append("terms_allow_automated_access=true after checking each target site's rules")
        if not source.get("allowed_domains"):
            missing.append("allowed_domains")
        return missing
    return []


def _post_json(url, payload, settings):
    request = Request(url, data=json.dumps(payload).encode("utf-8"), headers={
        "User-Agent": "PersonalJobCollector/0.1",
        "Accept": "application/json",
        "Content-Type": "application/json",
    }, method="POST")
    for attempt in range(settings["retries"] + 1):
        try:
            with urlopen(request, timeout=settings["timeout_seconds"]) as response:
                return json.load(response)
        except Exception as error:
            if isinstance(error, HTTPError) and error.code in (400, 401, 403, 404):
                raise
            if attempt == settings["retries"]:
                raise
            time.sleep(min(30, 2 ** (attempt + 1)))


def collect_adzuna(source, settings, preferences):
    """Query Adzuna for configured terms in the EU countries it supports."""
    from .sources import fetch

    app_id = os.environ[source.get("app_id_env", "ADZUNA_APP_ID")]
    app_key = os.environ[source.get("app_key_env", "ADZUNA_APP_KEY")]
    terms = preferences.get("technologies") or preferences.get("roles") or []
    if not terms:
        return [], True
    countries = [code for code in source.get("countries", ADZUNA_EU_COUNTRIES) if code in ADZUNA_EU_COUNTRIES]
    max_requests = int(source.get("max_requests_per_run", 120))
    max_pages = int(source.get("max_pages_per_query", 3))
    per_page = min(50, int(source.get("results_per_page", 50)))
    results, seen, requests = {}, set(), 0
    for code in countries:
        for term in terms:
            for page in range(1, max_pages + 1):
                if requests >= max_requests:
                    print(f"  adzuna: request cap reached ({max_requests}); results may be partial", flush=True)
                    return list(results.values()), False
                params = {"app_id": app_id, "app_key": app_key, "what": term, "results_per_page": per_page}
                if preferences.get("posted_within_days") is not None:
                    params["max_days_old"] = preferences["posted_within_days"]
                query = urlencode(params)
                url = f"https://api.adzuna.com/v1/api/jobs/{ADZUNA_EU_COUNTRIES[code]}/search/{page}?{query}"
                try:
                    payload = fetch(url, settings)
                except Exception as error:
                    message = str(error).replace(app_id, "<redacted>").replace(app_key, "<redacted>")
                    raise RuntimeError(message) from None
                requests += 1
                batch = payload.get("results", [])
                if not isinstance(batch, list):
                    raise ValueError("Invalid Adzuna results list")
                for row in batch:
                    job = normalize("adzuna", row)
                    results[job["source_id"]] = job
                total = int(payload.get("count") or 0)
                if len(batch) < per_page or page * per_page >= total:
                    break
                if settings["page_delay_seconds"]:
                    time.sleep(settings["page_delay_seconds"])
    print(f"  adzuna: queried {len(countries)} supported EU countries and {len(terms)} terms; "
          f"{requests} API requests, {len(results)} unique listings", flush=True)
    return list(results.values()), True


def collect_jooble(source, settings, preferences):
    """Query only Jooble country domains with explicitly configured API keys."""
    terms = preferences.get("technologies") or preferences.get("roles") or []
    key_envs = source.get("api_key_envs", {})
    configured = [(code.upper(), os.environ.get(env_name)) for code, env_name in key_envs.items()
                  if code.upper() in EU_CODES and os.environ.get(env_name)]
    if not terms or not configured:
        return [], True
    max_requests = int(source.get("max_requests_per_run", 120))
    max_pages = int(source.get("max_pages_per_query", 3))
    results, requests = {}, 0
    for code, api_key in configured:
        country = EU_CODE_TO_NAME[code]
        for term in terms:
            for page in range(1, max_pages + 1):
                if requests >= max_requests:
                    print(f"  jooble: request cap reached ({max_requests}); results may be partial", flush=True)
                    return list(results.values()), False
                domain = source.get("domains", {}).get(code)
                if not domain:
                    raise ValueError(f"Jooble domain is not configured for {code}")
                try:
                    payload = _post_json(f"https://{domain}/api/{api_key}",
                                         {"keywords": term, "location": country, "page": page}, settings)
                except Exception as error:
                    raise RuntimeError(str(error).replace(api_key, "<redacted>")) from None
                requests += 1
                batch = payload.get("jobs", [])
                if not isinstance(batch, list):
                    raise ValueError("Invalid Jooble results list")
                for row in batch:
                    job = normalize("jooble", row)
                    results[job["source_id"]] = job
                total = int(payload.get("totalCount") or payload.get("total") or 0)
                if not batch or (total and page * len(batch) >= total):
                    break
                if settings["page_delay_seconds"]:
                    time.sleep(settings["page_delay_seconds"])
    print(f"  jooble: queried {len(configured)} configured EU country keys and {len(terms)} terms; "
          f"{requests} API requests, {len(results)} unique listings", flush=True)
    return list(results.values()), True
