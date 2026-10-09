"""Compatibility entry point for the modular job extractor."""
from job_extractor.cli import main
from job_extractor.exporter import atomic, export
from job_extractor.model import (
    EU_CODES, assess, canonical, contains, geocode_query, match_location_filter,
    normalize, normalize_board, parse_posted_at, plain,
)
from job_extractor.sources import (
    collect, collect_board, collect_himalayas_search, fetch, resolve_location_country,
)
from job_extractor.storage import connect, ingest

if __name__ == "__main__":
    raise SystemExit(main())
