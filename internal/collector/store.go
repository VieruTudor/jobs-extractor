package collector

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	statements := []string{
		`CREATE TABLE IF NOT EXISTS jobs (source TEXT, id TEXT, payload TEXT, hash TEXT, first_seen TEXT, last_seen TEXT, active INTEGER, missing INTEGER DEFAULT 0, PRIMARY KEY(source,id))`,
		`CREATE TABLE IF NOT EXISTS location_geocodes (query TEXT PRIMARY KEY, country_code TEXT, resolution TEXT NOT NULL, resolved_at TEXT NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize SQLite database: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ingest(source string, jobs []map[string]any, complete bool, stamp string) ([]map[string]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	events := make([]map[string]string, 0)
	seen := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		identifier := stringValue(job["source_id"])
		if identifier == "" {
			continue
		}
		seen[identifier] = true
		payloadBytes, err := json.Marshal(job)
		if err != nil {
			return nil, fmt.Errorf("encode job %s/%s: %w", source, identifier, err)
		}
		digestBytes := sha256.Sum256(payloadBytes)
		digest := hex.EncodeToString(digestBytes[:])
		var oldHash, firstSeen string
		var active int
		err = tx.QueryRow(`SELECT hash,first_seen,active FROM jobs WHERE source=? AND id=?`, source, identifier).Scan(&oldHash, &firstSeen, &active)
		change := ""
		if err == sql.ErrNoRows {
			change = "new"
			firstSeen = stamp
		} else if err != nil {
			return nil, err
		} else if active == 0 {
			change = "reopened"
		} else if oldHash != digest {
			change = "changed"
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO jobs(source,id,payload,hash,first_seen,last_seen,active,missing) VALUES(?,?,?,?,?,?,1,0)`, source, identifier, string(payloadBytes), digest, firstSeen, stamp); err != nil {
			return nil, err
		}
		if change != "" {
			events = append(events, map[string]string{"source": source, "source_id": identifier, "change": change})
		}
	}

	if complete {
		rows, err := tx.Query(`SELECT id,missing FROM jobs WHERE source=? AND active=1`, source)
		if err != nil {
			return nil, err
		}
		type missingJob struct {
			id      string
			missing int
		}
		var missingJobs []missingJob
		for rows.Next() {
			var item missingJob
			if err := rows.Scan(&item.id, &item.missing); err != nil {
				rows.Close()
				return nil, err
			}
			missingJobs = append(missingJobs, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		for _, item := range missingJobs {
			if seen[item.id] {
				continue
			}
			missing := item.missing + 1
			active := 1
			if missing >= 2 {
				active = 0
				events = append(events, map[string]string{"source": source, "source_id": item.id, "change": "possibly_closed"})
			}
			if _, err := tx.Exec(`UPDATE jobs SET missing=?,active=? WHERE source=? AND id=?`, missing, active, source, item.id); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}

func (s *Store) ActiveJobs() ([]storedJob, error) {
	rows, err := s.db.Query(`SELECT payload,first_seen,last_seen FROM jobs WHERE active=1 ORDER BY first_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []storedJob
	for rows.Next() {
		var payload, firstSeen, lastSeen string
		if err := rows.Scan(&payload, &firstSeen, &lastSeen); err != nil {
			return nil, err
		}
		var job map[string]any
		if err := json.Unmarshal([]byte(payload), &job); err != nil {
			return nil, fmt.Errorf("decode stored job: %w", err)
		}
		result = append(result, storedJob{Job: job, FirstSeen: firstSeen, LastSeen: lastSeen})
	}
	return result, rows.Err()
}

func (s *Store) Geocode(key string) (country, resolution string, found bool, err error) {
	err = s.db.QueryRow(`SELECT country_code,resolution FROM location_geocodes WHERE query=?`, key).Scan(&country, &resolution)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return country, resolution, err == nil, err
}

func (s *Store) SaveGeocode(key, country, resolution string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO location_geocodes(query,country_code,resolution,resolved_at) VALUES(?,?,?,?)`, key, country, resolution, timestamp())
	return err
}

type storedJob struct {
	Job       map[string]any
	FirstSeen string
	LastSeen  string
}

func timestamp() string { return time.Now().UTC().Format(time.RFC3339) }
