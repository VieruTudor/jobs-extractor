"""JSON, text, and HTML exports."""
import html
import json
from pathlib import Path
from urllib.parse import urlsplit
from .model import assess, canonical, geocode_query, match_location_filter, normalize, normalize_board
from .sources import resolve_location_country

def atomic(path, content):
    temp = path.with_suffix(path.suffix + ".tmp")
    temp.write_text(content, encoding="utf-8")
    temp.replace(path)

def export(db, config, base, report):
    output = base / config["output_directory"]
    output.mkdir(parents=True, exist_ok=True)
    geocoding = config.get("geocoding", {})
    geocode_stats = {"provider": "Open-Meteo Geocoding API / GeoNames", "requests": 0, "cache_hits": 0,
                     "resolved": 0, "ambiguous": 0, "not_found": 0, "errors": 0, "limit_skips": 0}
    geocode_run_cache = {}
    groups = {}
    for payload, first, last in db.execute("SELECT payload, first_seen, last_seen FROM jobs WHERE active=1 ORDER BY first_seen DESC"):
        job = json.loads(payload)
        if "raw" in job:
            if ":" in job["source"]:
                provider, slug = job["source"].split(":", 1)
                job = normalize_board(provider, slug, job["company"], job["raw"])
            else:
                job = normalize(job["source"], job["raw"])
        job.pop("raw", None)
        job.update(first_seen=first, last_seen=last)
        key = canonical(job["url"]) or job["source"] + ":" + job["source_id"]
        ref = {"source": job["source"], "source_id": job["source_id"], "url": job.get("source_url", job["url"])}
        if key in groups:
            groups[key]["sources"].append(ref)
            continue
        job["sources"] = [ref]
        prefs = config["preferences"]
        country_code = None
        assessment = assess(job, prefs)
        if assessment["status"] != "excluded" and geocoding.get("enabled", True) and str(prefs.get("country_filter") or "").casefold() == "eu":
            current_location = match_location_filter(job["location"], job.get("location_restrictions", []), "EU")
            if current_location == "unknown":
                query = geocode_query(job["location"], job.get("location_restrictions", []))
                if query:
                    country_code = resolve_location_country(db, query, geocoding, geocode_run_cache, geocode_stats)
                    if country_code:
                        job["location_country_code"] = country_code
                        job["location_country_source"] = "Open-Meteo / GeoNames"
        job["assessment"] = assess(job, prefs, country_code)
        groups[key] = job
    report["geocoding"] = geocode_stats
    jobs = [job for job in groups.values() if job["assessment"]["status"] != "excluded"]
    for filename, value in [("jobs.json", jobs), ("matches.json", [j for j in jobs if j["assessment"]["status"] == "match"]),
                            ("review.json", [j for j in jobs if j["assessment"]["status"] == "review"]), ("run-status.json", report)]:
        atomic(output / filename, json.dumps(value, indent=2, ensure_ascii=False))
    changed = {(e["source"], e["source_id"]): e["change"] for e in report.get("events", [])}
    lines = [f"Job collector: {report['finished_at']}", f"Current listings: {len(jobs)}", ""]
    for source in report.get("sources", []):
        lines.append(f"{source['source']}: {source['status']} ({source.get('count', 0)} listings) {source.get('error', '')}")
    lines.append("\nNew and changed matches / review candidates:")
    for job in jobs:
        changes = [changed.get((s["source"], s["source_id"])) for s in job["sources"]]
        if any(changes) and job["assessment"]["status"] != "excluded":
            lines.extend([f"\n[{job['assessment']['status']}] {job['title']} — {job['company']}", job["url"], "; ".join(job["assessment"]["reasons"])])
            lines.extend(f"Source: {s['source']} — {s['url']}" for s in job["sources"])
    atomic(output / "daily.txt", "\n".join(lines))
    esc = html.escape
    rows = []
    for job in jobs:
        url = job["url"] if urlsplit(job["url"]).scheme in ("http", "https") else "#"
        source_links = ", ".join(f"<a href='{esc(s['url'], quote=True)}'>{esc(s['source'])}</a>" for s in job["sources"] if urlsplit(s["url"]).scheme in ("http", "https"))
        status = job["assessment"]["status"]
        badge = f"<span class='status-badge status-{esc(status, quote=True)}'>{esc(status)}</span>"
        rows.append(f"<tr><td>{badge}</td><td><a href='{esc(url, quote=True)}'>{esc(job['title'])}</a></td><td>{esc(job['company'])}</td><td>{esc(job['location'])}</td><td>{source_links}</td><td>{esc('; '.join(job['assessment']['reasons']))}</td></tr>")
    headers = ("Status", "Job", "Company", "Location", "Source", "Reasons")
    head = "<thead><tr>" + "".join(f"<th scope='col'>{esc(label)}</th>" for label in headers) + "</tr><tr class='filters'>" + "".join(f"<th><input type='search' data-column='{i}' aria-label='Filter {esc(label, quote=True)}' placeholder='Filter {esc(label, quote=True)}'></th>" for i, label in enumerate(headers)) + "</tr></thead>"
    script = "<script>const filters=[...document.querySelectorAll('.filters input')];const rows=[...document.querySelectorAll('tbody tr')];function applyFilters(){for(const row of rows){const cells=[...row.cells];row.hidden=!filters.every(input=>!input.value||cells[Number(input.dataset.column)].textContent.toLocaleLowerCase().includes(input.value.trim().toLocaleLowerCase()))}}filters.forEach(input=>input.addEventListener('input',applyFilters));</script>"
    stylesheet = (Path(__file__).parent / "assets" / "jobs.css").read_text(encoding="utf-8")
    document = (
        "<!doctype html><html lang='en'><head><meta charset='utf-8'>"
        "<meta name='viewport' content='width=device-width,initial-scale=1'>"
        "<title>Job listings</title><style>" + stylesheet + "</style></head><body>"
        "<main class='page-shell'><header class='page-header'><div><p class='eyebrow'>DAILY JOB COLLECTOR</p>"
        "<h1>Job listings</h1><p class='subtitle'>" + str(len(jobs)) + " active listings in this view</p></div>"
        "<div class='generated'>Updated<br><strong>" + esc(report["finished_at"]) + "</strong></div></header>"
        "<p class='note'>Excluded listings are omitted. See <code>run-status.json</code> for source health.</p>"
        "<section class='table-card' aria-label='Job listings'><div class='table-scroll'><table>" + head +
        "<tbody>" + "".join(rows) + "</tbody></table></div></section>"
        "<footer>Location checks use <a href='https://open-meteo.com/en/docs/geocoding-api'>Open-Meteo</a> and GeoNames data (CC BY 4.0)."
        "</footer></main>" + script + "</body></html>"
    )
    atomic(output / "jobs.html", document)
    return len(jobs)
