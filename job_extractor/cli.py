"""Command-line interface and run orchestration."""
import argparse
import json
import os
from pathlib import Path
import sys
import time
from .common import now
from .exporter import export
from .sources import collect, collect_board
from .storage import connect, ingest

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["collect", "collect-boards", "export"])
    parser.add_argument("--config", default=str(Path(__file__).resolve().parent.parent / "config.json"))
    parser.add_argument("--board", action="append", help="With collect-boards, refresh only this provider:slug (repeatable).")
    args = parser.parse_args()
    path = Path(args.config).resolve()
    base, config = path.parent, json.loads(path.read_text(encoding="utf-8"))
    allowed = {"roles", "technologies", "excluded_keywords", "country_filter", "posted_within_days", "work_modes", "contract_types", "required_keywords"}
    unknown = set(config["preferences"]) - allowed
    if unknown:
        parser.error(f"Unsupported preferences: {', '.join(sorted(unknown))}")
    for key, value in config["preferences"].items():
        if key == "country_filter":
            if value is not None and not isinstance(value, str):
                parser.error("country_filter must be a string or null")
        elif key == "posted_within_days":
            if value is not None and (not isinstance(value, int) or isinstance(value, bool) or value <= 0):
                parser.error("posted_within_days must be a positive integer or null")
        elif not isinstance(value, list) or not all(isinstance(term, str) and term.strip() for term in value):
            parser.error(f"{key} must be a list of non-empty strings")
    for key in ("timeout_seconds", "max_pages"):
        if config["collection"][key] <= 0:
            parser.error(f"{key} must be positive")
    for key in ("retries", "page_delay_seconds"):
        if config["collection"][key] < 0:
            parser.error(f"{key} cannot be negative")
    lock = base / ".collector.lock"
    try:
        fd = os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY)
    except FileExistsError:
        parser.error(f"Collector lock exists: {lock}. If no run is active, remove this stale lock.")
    os.close(fd)
    db = None
    try:
        db = connect(base / config["database"])
        report = {"started_at": now(), "sources": [], "events": [], "command": args.command}
        if args.command in ("collect", "collect-boards"):
            source_config = json.loads((base / config["sources_file"]).read_text(encoding="utf-8"))
            sources = [] if args.command == "collect-boards" else [(feed, False) for feed in source_config.get("feeds", [])]
            configured_boards = source_config.get("boards", [])
            if args.board:
                wanted = set(args.board)
                configured_boards = [board for board in configured_boards if f"{board['provider']}:{board['slug']}" in wanted]
                missing = wanted - {f"{board['provider']}:{board['slug']}" for board in configured_boards}
                if missing:
                    parser.error("Unknown board(s): " + ", ".join(sorted(missing)))
            sources.extend((board, True) for board in configured_boards)
            for feed, is_board in sources:
                if not feed.get("enabled", True):
                    continue
                kind = f"{feed['provider']}:{feed['slug']}" if is_board else feed["type"]
                if not is_board and kind in {"adzuna", "jooble", "google_cse"}:
                    from .providers import missing_credentials
                    missing = missing_credentials(feed)
                    if missing:
                        report["sources"].append({"source": kind, "status": "skipped",
                                                  "reason": "Missing credentials: " + ", ".join(missing)})
                        print(f"Skipping {kind}: configure credentials to enable this source", flush=True)
                        continue
                print(f"Collecting {kind}...", flush=True)
                try:
                    if is_board:
                        jobs, complete = collect_board(feed, config["collection"]), True
                    else:
                        jobs, complete = collect(kind, config["collection"], config["preferences"], feed)
                    targeted = (not is_board and kind == "himalayas" and
                                str(config["preferences"].get("country_filter") or "").casefold() == "eu" and
                                bool(config["preferences"].get("technologies") or config["preferences"].get("roles")))
                    # Search results are complete for the configured query, not a full
                    # source snapshot; don't mark unrelated historical records closed.
                    snapshot_complete = complete and not targeted
                    report["events"].extend(ingest(db, kind, jobs, snapshot_complete, now()))
                    source_report = {"source": kind, "count": len(jobs), "status": "ok" if complete else "partial"}
                    if targeted:
                        source_report["coverage"] = "filtered by configured terms and EU country"
                    if kind == "adzuna":
                        source_report["coverage"] = "supported EU countries: " + ", ".join(feed.get("countries", []))
                    report["sources"].append(source_report)
                except Exception as error:
                    report["sources"].append({"source": kind, "status": "failed", "error": str(error)})
                if is_board and config["collection"].get("board_delay_seconds", 0) > 0:
                    time.sleep(config["collection"]["board_delay_seconds"])
            if args.command == "collect-boards" and args.board:
                status_path = base / config["output_directory"] / "run-status.json"
                if status_path.exists():
                    previous = json.loads(status_path.read_text(encoding="utf-8"))
                    valid_keys = {f"{b['provider']}:{b['slug']}" for b in source_config.get("boards", [])}
                    selected = set(args.board)
                    retained = [s for s in previous.get("sources", []) if s["source"] in valid_keys and s["source"] not in selected]
                    retained_events = [e for e in previous.get("events", []) if e.get("source") in valid_keys and e.get("source") not in selected]
                    report["sources"] = retained + report["sources"]
                    report["events"] = retained_events + report["events"]
        else:
            status_path = base / config["output_directory"] / "run-status.json"
            if status_path.exists():
                report = json.loads(status_path.read_text(encoding="utf-8"))
                report["exported_at"] = now()
        report.setdefault("finished_at", now())
        count = export(db, config, base, report)
        print(f"Exported {count} listings to {base / config['output_directory']}")
        return int(any(s["status"] not in {"ok", "skipped"} for s in report["sources"]))
    finally:
        if db:
            db.close()
        lock.unlink(missing_ok=True)
