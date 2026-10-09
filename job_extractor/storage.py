"""SQLite persistence and lifecycle tracking."""
import hashlib
import json
import sqlite3
from pathlib import Path

def connect(path):
    path.parent.mkdir(parents=True, exist_ok=True)
    db = sqlite3.connect(path)
    db.execute("CREATE TABLE IF NOT EXISTS jobs (source TEXT, id TEXT, payload TEXT, hash TEXT, first_seen TEXT, last_seen TEXT, active INTEGER, missing INTEGER DEFAULT 0, PRIMARY KEY(source,id))")
    db.execute("CREATE TABLE IF NOT EXISTS location_geocodes (query TEXT PRIMARY KEY, country_code TEXT, resolution TEXT NOT NULL, resolved_at TEXT NOT NULL)")
    return db

def ingest(db, source, jobs, complete, stamp):
    events, seen = [], set()
    with db:
        for job in jobs:
            ident = job["source_id"]
            seen.add(ident)
            payload = json.dumps(job, sort_keys=True, ensure_ascii=False)
            digest = hashlib.sha256(payload.encode()).hexdigest()
            old = db.execute("SELECT hash, first_seen, active FROM jobs WHERE source=? AND id=?", (source, ident)).fetchone()
            change = "new" if not old else "reopened" if not old[2] else "changed" if old[0] != digest else None
            db.execute("INSERT OR REPLACE INTO jobs VALUES (?,?,?,?,?,?,1,0)",
                       (source, ident, payload, digest, old[1] if old else stamp, stamp))
            if change:
                events.append({"source": source, "source_id": ident, "change": change})
        if complete:
            for ident, missing in db.execute("SELECT id, missing FROM jobs WHERE source=? AND active=1", (source,)).fetchall():
                if ident not in seen:
                    db.execute("UPDATE jobs SET missing=?, active=? WHERE source=? AND id=?", (missing + 1, int(missing + 1 < 2), source, ident))
                    if missing + 1 >= 2:
                        events.append({"source": source, "source_id": ident, "change": "possibly_closed"})
    return events
