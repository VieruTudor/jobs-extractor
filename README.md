# Jobs listing extractor

A Go 1.26+ job collector, SQLite history store, and Chi REST API. `config.json` controls matching preferences and collector limits. `sources.json` controls public feeds and employer boards.

## Run

```powershell
go mod download
go run ./api collect
go run ./api collect-boards
go run ./api collect-boards --board ashby:1password
go run ./api export
go run ./api serve
```

With no command, the process starts the API. The API serves the latest `output/jobs.json` at `http://127.0.0.1:8000`. Use `JOBS_FILE` to select a specific export file, `JOBS_OUTPUT_DIR` to select an output directory, and `JOBS_API_ADDR` to change the listen address.

## Collector behavior

- `collect` fetches enabled feeds and employer boards, records job changes and history in `data/jobs.sqlite3`, and writes the filtered exports.
- `collect-boards` refreshes the configured employer boards. Repeat `--board provider:slug` to restrict it to selected boards.
- `export` reapplies the current preferences to stored active records without fetching sources.
- `output/jobs.json` contains match and review listings. `matches.json`, `review.json`, `daily.txt`, `run-status.json`, and `jobs.html` are also generated.
- Listings are marked possibly closed only after two complete scans miss them. Partial scans do not close records.
- City-only EU locations can be resolved with Open-Meteo. Geocoding results are cached in SQLite; disable it with `geocoding.enabled: false`.
- Adzuna, Jooble, and Google Custom Search require the credentials and terms settings documented in `sources.json`; they are skipped with a reason when those settings are absent.

The Go collector supports the configured Arbeitnow, Himalayas, Remotive, Remote OK, Greenhouse, Ashby, and Lever sources, along with the configured Adzuna, Jooble, Google CSE, and JSON-LD adapters.

## API

- `GET /api/health`
- `GET /api/jobs` — pagination via `limit` and `offset`; exact filters for `status`, `source`, `company`, `location`, and `work_mode`; free-text `q` over title, company, description, and location.
- `GET /api/collector` — current run state and latest collector report.
- `POST /api/collector/runs` — start a background run. The optional JSON body is `{"command":"collect"}` or `{"command":"collect-boards","boards":["ashby:1password"]}`. Poll `GET /api/collector` for completion. The endpoint returns `409 Conflict` if a run is already active.
- `GET` and `PUT /api/config` — read or replace collector preferences and settings. `PUT` accepts the complete `config.json` document.
- `GET` and `PUT /api/sources` — read or replace configured feeds and employer boards. `PUT` accepts the complete `sources.json` document.
- `GET /openapi.json`
- `GET /docs` — Swagger UI.

The API reads `output/jobs.json`; run `collect` or `export` to refresh it.


