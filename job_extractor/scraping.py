"""Robots-aware extraction of schema.org JobPosting JSON-LD from public pages."""
from html.parser import HTMLParser
import json
import os
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode, urljoin, urlsplit
from urllib.request import Request, urlopen
from urllib.robotparser import RobotFileParser
import xml.etree.ElementTree as ET

from .model import normalize


USER_AGENT = "JobsListingExtractor/1.0 (personal job search)"


class JsonLdParser(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.in_jsonld = False
        self.parts = []
        self.scripts = []

    def handle_starttag(self, tag, attrs):
        if tag.lower() == "script":
            attributes = {key.lower(): value for key, value in attrs}
            self.in_jsonld = "ld+json" in (attributes.get("type") or "").lower()
            self.parts = []

    def handle_data(self, data):
        if self.in_jsonld:
            self.parts.append(data)

    def handle_endtag(self, tag):
        if tag.lower() == "script" and self.in_jsonld:
            self.scripts.append("".join(self.parts))
            self.in_jsonld = False


def _job_postings(value):
    if isinstance(value, list):
        for item in value:
            yield from _job_postings(item)
    elif isinstance(value, dict):
        types = value.get("@type", [])
        if isinstance(types, str):
            types = [types]
        if "JobPosting" in types:
            yield value
        for key in ("@graph", "mainEntity", "itemListElement"):
            if key in value:
                yield from _job_postings(value[key])


def _read_url(url, settings):
    request = Request(url, headers={"User-Agent": USER_AGENT, "Accept": "text/html,application/xml;q=0.9,*/*;q=0.5"})
    with urlopen(request, timeout=settings["timeout_seconds"]) as response:
        return response.read().decode(response.headers.get_content_charset() or "utf-8", errors="replace")


def _robots_parser(url, settings, cache):
    parsed = urlsplit(url)
    origin = f"{parsed.scheme}://{parsed.netloc}"
    if origin not in cache:
        robots_url = origin + "/robots.txt"
        parser = RobotFileParser(robots_url)
        try:
            parser.parse(_read_url(robots_url, settings).splitlines())
        except HTTPError as error:
            if error.code == 404:
                parser.parse([])
            else:
                raise PermissionError(f"Cannot verify robots.txt for {origin}: HTTP {error.code}") from None
        except (URLError, TimeoutError) as error:
            raise PermissionError(f"Cannot verify robots.txt for {origin}: {error}") from None
        cache[origin] = parser
    return cache[origin]


def _allowed(url, settings, cache):
    if urlsplit(url).scheme not in ("http", "https"):
        return False
    return _robots_parser(url, settings, cache).can_fetch(USER_AGENT, url)


def _extract_page(url, settings):
    content = _read_url(url, settings)
    parser = JsonLdParser()
    parser.feed(content)
    found = []
    for script in parser.scripts:
        try:
            found.extend(_job_postings(json.loads(script)))
        except (json.JSONDecodeError, TypeError):
            continue
    return found


def collect_jsonld(source, settings):
    """Scrape only explicitly configured pages, and only where robots.txt permits it."""
    if source.get("terms_allow_automated_access") is not True:
        raise PermissionError("Enable only after confirming the target site's terms permit automated access")
    seed_urls = source.get("urls", [])
    if not seed_urls:
        raise ValueError("JSON-LD source needs at least one URL")
    max_pages = int(source.get("max_pages", settings["max_pages"]))
    robots, page_urls, found = {}, [], {}
    allowed_domains = {str(domain).casefold().strip(".") for domain in source.get("allowed_domains", [])}
    allowed_domains.update((urlsplit(seed).hostname or "").casefold() for seed in seed_urls)

    def allowed_host(url):
        host = (urlsplit(url).hostname or "").casefold()
        return any(host == domain or host.endswith("." + domain) for domain in allowed_domains)

    complete = True
    for seed in seed_urls:
        if not allowed_host(seed):
            complete = False
            continue
        if not _allowed(seed, settings, robots):
            print(f"  jsonld: robots.txt disallows {seed}", flush=True)
            complete = False
            continue
        try:
            content = _read_url(seed, settings)
        except Exception as error:
            print(f"  jsonld: could not read {seed}: {error}", flush=True)
            complete = False
            continue
        if "<urlset" in content[:1000].lower() or "<sitemapindex" in content[:1000].lower():
            try:
                root = ET.fromstring(content)
                page_urls.extend(element.text.strip() for element in root.iter()
                                 if element.tag.rsplit("}", 1)[-1] == "loc" and element.text)
            except ET.ParseError:
                complete = False
        else:
            page_urls.append(seed)
        if settings["page_delay_seconds"]:
            import time
            time.sleep(settings["page_delay_seconds"])

    unique_urls = list(dict.fromkeys(page_urls))[:max_pages]
    if len(unique_urls) < len(set(page_urls)):
        complete = False
    for index, page_url in enumerate(unique_urls):
        if not allowed_host(page_url):
            complete = False
            continue
        if not _allowed(page_url, settings, robots):
            complete = False
            continue
        try:
            postings = _extract_page(page_url, settings)
        except Exception as error:
            print(f"  jsonld: page failed: {error}", flush=True)
            complete = False
            continue
        for posting in postings:
            posting_url = urljoin(page_url, str(posting.get("url") or page_url))
            posting["url"] = posting_url
            job = normalize("jsonld", posting)
            found[job["source_id"]] = job
        if index + 1 < len(unique_urls) and settings["page_delay_seconds"]:
            import time
            time.sleep(settings["page_delay_seconds"])
    print(f"  jsonld: checked {len(unique_urls)} permitted pages; found {len(found)} JobPosting records", flush=True)
    return list(found.values()), complete


def collect_google_cse(source, settings, preferences):
    """Use Google's JSON search API for discovery, then read allowed JSON-LD pages."""
    api_key = os.environ[source.get("api_key_env", "GOOGLE_CSE_API_KEY")]
    engine_id = os.environ[source.get("engine_id_env", "GOOGLE_CSE_ENGINE_ID")]
    domains = [str(domain).casefold().strip(".") for domain in source.get("allowed_domains", [])]
    terms = preferences.get("technologies") or preferences.get("roles") or []
    max_queries = int(source.get("max_queries_per_run", 20))
    max_results = min(100, max(1, int(source.get("max_results_per_query", 10))))
    found, robots, query_count, complete = {}, {}, 0, True
    for term in terms:
        query = f'"{term}" jobs Europe'
        for start in range(1, max_results + 1, 10):
            if query_count >= max_queries:
                complete = False
                break
            endpoint = "https://www.googleapis.com/customsearch/v1?" + urlencode({
                "key": api_key, "cx": engine_id, "q": query,
                "num": min(10, max_results - start + 1), "start": start,
            })
            try:
                from .sources import fetch
                payload = fetch(endpoint, settings)
            except Exception as error:
                message = str(error).replace(api_key, "<redacted>")
                raise RuntimeError(message) from None
            query_count += 1
            batch = payload.get("items", [])
            if not batch:
                break
            for result in batch:
                page_url = result.get("link", "")
                host = urlsplit(page_url).hostname or ""
                if not any(host.casefold() == domain or host.casefold().endswith("." + domain) for domain in domains):
                    continue
                if not _allowed(page_url, settings, robots):
                    complete = False
                    continue
                try:
                    for posting in _extract_page(page_url, settings):
                        posting["url"] = urljoin(page_url, str(posting.get("url") or page_url))
                        job = normalize("google_cse", posting)
                        found[job["source_id"]] = job
                except Exception as error:
                    print(f"  google_cse: page failed: {error}", flush=True)
                    complete = False
                if settings["page_delay_seconds"]:
                    import time
                    time.sleep(settings["page_delay_seconds"])
        if query_count >= max_queries:
            break
    print(f"  google_cse: ran {query_count} Google API queries; found {len(found)} structured job postings", flush=True)
    return list(found.values()), complete
