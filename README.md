# Jobs listing extractor

Local job collection with Python 3.11+ and no third-party packages. Configuration and source switches are JSON files.

## Run

In PowerShell, from this folder:

```powershell
python jobs.py collect
python jobs.py collect-boards
python jobs.py export
python -m unittest -v
```

`collect` downloads all enabled feeds and employer boards, updates SQLite history, then exports. `collect-boards` refreshes only the direct Greenhouse and Ashby boards. `export` reapplies your current preferences to stored active records; it may make cached-once geocoding requests for unresolved EU locations. It preserves the last collection status and events. Paths in config are relative to the config file; an alternate config can be supplied with `--config PATH`.

## Preferences

Edit `config.json`. Empty lists mean unrestricted; set the country to `null` to remove the country filter. The default country filter is `EU`.

- `roles`: case-insensitive whole-term matching against titles; any listed role suffices.
- `technologies`: any listed term must appear in the title or description. Exact terms, not semantic aliases.
- `required_keywords`: all terms must appear.
- `excluded_keywords`: any term excludes the job.
- `work_modes`: currently `remote` can be confirmed from feeds; otherwise the job goes to review. A false remote flag is not assumed to mean on-site.
- `contract_types`: `permanent`, `contract`, `b2b`, `freelance`. Missing or unmatched structured types go to review. Full-time does not establish permanent employment. Description mentions alone are not treated as confirmation (for example, "no B2B").
- `country_filter`: defaults to `"EU"`, which matches explicitly named EU member countries (including ISO codes) or the European Union, and excludes listings that also name non-EU countries or regions. Broad or unclear locations such as `Europe` or `worldwide` go to review because they can include non-EU locations. Set to `"Europe"` for the wider geographical region, or use a specific country name.
- `posted_within_days`: defaults to `30`, so the matches export only includes listings posted within the rolling last 30 days. Older listings are excluded; missing or unrecognized dates go to review. Set to `null` to disable this date filter.

Examples: `"roles": ["engineer", "developer"]`, `"technologies": ["Python", ".NET"]`, `"work_modes": ["remote"]`, `"country_filter": "EU"`.

When `country_filter` is `EU`, the collector uses the Open-Meteo Geocoding API for ambiguous city/state-only locations. Results are cached in SQLite, so each unique location is looked up once; up to 50 new lookups are made per run. Ambiguous or unresolved locations remain in review. The free API is for non-commercial use, has daily request limits, and uses GeoNames data under CC BY 4.0; attribution is included in the HTML output. Set `geocoding.enabled` to `false` to turn this off.

## Outputs

- `output/jobs.json`: active matches and review candidates, with excluded listings omitted.
- `output/matches.json`: matches for configured filters.
- `output/review.json`: ambiguous candidates.
- `output/daily.txt`: new/changed/reopened candidates and source health from the latest collection.
- `output/jobs.html`: plain escaped table with links, source attribution, and a filter for each column. Excluded listings are omitted.
- `output/run-status.json`: source status, counts, errors and change events.
- `data/jobs.sqlite3`: original source payloads and first/last-seen metadata. Keep this file to retain history.

Matching canonical URLs are grouped in exports. Distinct URLs with similar titles are intentionally not merged yet. Deleted jobs remain in SQLite. Two complete successful scans missing a job mark it inactive/possibly closed. Source failure or a page limit never marks jobs closed. Disabled-source historical listings remain in the output until their source is scanned again; disabling only stops collection.

`max_pages` caps each ordinary paginated source per run; the configured cap is 250 pages. Himalayas uses its filtered search endpoint for the configured technology terms across EU countries, with a separate 250-request budget, rather than crawling its 117k+ global listings. Hitting either cap reports `partial` and exits with code 1, while retaining/exporting fetched records. Filtered Himalayas scans do not mark unrelated historical jobs closed. `failed` sources also produce exit code 1. Inspect source status rather than assuming an empty report means no jobs. An interrupted process can leave `.collector.lock`; remove it only after confirming no collector process remains.

## Daily Windows schedule

To install a daily 09:00 task in your Windows local timezone:

```powershell
powershell -ExecutionPolicy Bypass -File .\schedule.ps1 -Time '09:00'
```

This runs as your current user while logged in. The PC must be available; missed runs start when available. The script does not register anything until you run it. Change the time by rerunning the command. Remove with `powershell -ExecutionPolicy Bypass -File .\schedule.ps1 -Remove`.

## Project layout

- `job_extractor/model.py`: normalization, location rules, and matching.
- `job_extractor/sources.py`: feed/board dispatch and HTTP access.
- `job_extractor/providers.py`: optional Adzuna and Jooble API adapters.
- `job_extractor/scraping.py`: Google Custom Search and robots-aware JSON-LD extraction.
- `job_extractor/storage.py`: SQLite history and lifecycle tracking.
- `job_extractor/exporter.py`: JSON, text, and HTML exports.
- `job_extractor/cli.py`: command-line orchestration. `jobs.py` remains the entry point.

## Source documentation

- Arbeitnow: https://www.arbeitnow.com/blog/job-board-api
- Himalayas: https://himalayas.app/api
- Remotive: https://github.com/remotive-com/remote-jobs-api

Keep attribution and original source links when using exports. Remotive's public feed is delayed by 24 hours. These feeds overlap and do not cover all EU opportunities.

The configured feeds—Arbeitnow, Himalayas, Remotive, and Remote OK—use public structured endpoints. Greenhouse, Ashby, and Lever boards use published-job endpoints; they do not need API keys. This collector requests structured data rather than scraping rendered HTML. Sources still have their own rate, attribution, and reuse requirements. Himalayas requires attribution and says not to submit its listings to third-party job sites; the local export keeps the original listing link and source name. Remote OK's free public API supports tag filters; the collector keeps the Remote OK job URL and source attribution as its FAQ requests.

## Additional sources

`sources.json` enables the Adzuna, Jooble, and Google Custom Search adapters, but skips them cleanly until credentials and settings are supplied. Secrets are read from environment variables and are never stored in the JSON configuration or run report.

- **Adzuna:** set `ADZUNA_APP_ID` and `ADZUNA_APP_KEY`. The current country set is AT, BE, DE, ES, FR, IT, NL, and PL, which is not all EU countries. It searches the configured technology terms, with request and page caps.
- **Jooble:** each country domain requires its own key. Fill `api_key_envs` and `domains` in `sources.json` only for the regional APIs for which you obtained credentials. The current empty maps mean it is skipped.
- **Google Custom Search:** set `GOOGLE_CSE_API_KEY` and `GOOGLE_CSE_ENGINE_ID` for a Programmable Search Engine. The engine returns discovery links, not complete job records. The adapter only visits domains in `allowed_domains`, respects each domain's `robots.txt`, and imports pages with schema.org `JobPosting` JSON-LD. Add only domains whose terms permit this use, then set `terms_allow_automated_access` to `true`. Google lists the JSON API at $5 per 1,000 queries; check its current limits and pricing before enabling it ([Google API setup](https://developers.google.com/custom-search/v1/introduction), [pricing/limits](https://developers.google.com/custom-search/docs/overview)).
- **Other HTML pages:** a `jsonld` feed can be added with explicit `urls`, `allowed_domains`, and `terms_allow_automated_access: true`. It reads a configured page or sitemap, extracts only structured `JobPosting` data, follows sitemap entries only on the seed or explicitly allowed domains, checks `robots.txt`, and applies the configured delay. It does not defeat blocks or log in.

No generic permission is inferred from a page being visible in a browser. Eurojobs.com currently prohibits automated scraping in its [Acceptable Use Policy](https://www.eurojobs.com/acceptable-use), so it is not configured for scraping. EURES likewise restricts API extraction to recognized partners; it is not a general-purpose source for this collector ([EURES terms](https://europa.eu/eures/portal/jv-se/home?lang=en&pageCode=find_a_job#openDisclaimerQualityDialog)). SmartRecruiters, Workable, and Teamtailor have authenticated APIs; they are not anonymous public feeds and need appropriate employer/API credentials ([SmartRecruiters](https://developers.smartrecruiters.com/docs/posting-api), [Workable](https://workable.readme.io/reference/jobs), [Teamtailor](https://docs.teamtailor.com/)).

Some requested job platforms are not configured because their current published rules do not permit this collector's automated retrieval or storage: No Fluff Jobs blocks its API paths in `robots.txt` and restricts copying site content; Just Join IT prohibits automatic downloading without written permission; GermanTechJobs prohibits automated access and third-party collection without explicit agreement; Welcome to the Jungle prohibits automated scraping/copying; and We Work Remotely prohibits saving/storing its feed data and job-search services under its API terms. These sources can be revisited if their operators provide permission or a compatible partner feed. See [No Fluff Jobs terms](https://nofluffjobs.com/static/No_Fluff_Jobs_Terms_and_Conditions.pdf), [Just Join IT terms](https://justjoin.it/terms-and-privacy-policies), [GermanTechJobs terms](https://static.germantechjobs.de/documents/Terms-And-Conditions-2026.pdf), [Welcome to the Jungle terms](https://www.welcometothejungle.com/en/pages/terms), and [We Work Remotely API terms](https://weworkremotely.com/api-terms-and-guidelines).

## Direct employer boards

`sources.json` contains 53 public employer boards: 25 Greenhouse, 25 Ashby, and three Lever boards. Their published-job APIs provide direct listings; `country_filter` and the 30-day date window are applied just like the feeds. Run `python jobs.py collect-boards` to refresh boards only, or `python jobs.py collect` for all enabled sources. Greenhouse and Ashby board identifiers were selected from a public CC0 directory of API-verified boards. Add Lever boards with their exact career-page slug and `provider: "lever"`; use `region: "eu"` for a `jobs.eu.lever.co` board. Disable or add boards by editing the `boards` list.

## Current scope

The collector handles collection, conservative filters, history, URL deduplication and file exports. Salary is preserved as supplied; currency conversion and automatic employer-board discovery are not included.
