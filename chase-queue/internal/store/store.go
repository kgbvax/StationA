// Package store is chase-queue's SQLite database (modernc, pure Go): contest
// instances and their DXLog QSOs, uploaded contest files, the operator's
// station log as Log4OM and Club Log report it, Club Log's confirmations, DX
// skips and small settings (the Club Log Most Wanted cache). Callsigns are
// personal data: the database lives under the service's state directory and
// is never committed.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the database handle. One connection: every write is serialised.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrate applies the sequential schema steps (PRAGMA user_version). Append
// new steps; keep each idempotent.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA synchronous=NORMAL;`); err != nil {
		return err
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version < 1 {
		if _, err := s.db.Exec(schemaV1); err != nil {
			return err
		}
		if _, err := s.db.Exec(`PRAGMA user_version=1`); err != nil {
			return err
		}
	}
	return nil
}

// Setting reads a small value; ok is false when it was never set.
func (s *Store) Setting(key string) (value string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// dxTime is the text form of stored times: whole seconds, so the text
// compares in time order.
func dxTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseDXTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// v1: the contest tables (from bmrec's schema step 20), the operator's log,
// Club Log's confirmations per DXCC entity and band, DX skips and settings.
//
// A contest instance is a contest name plus the start date the operator
// confirmed; it points at the uploaded definition and prefill files it uses.
// Foreign keys are off, so deleting an instance removes its QSOs and handled
// stations in code (DeleteContest). Instances untouched for 30 days are
// pruned. log_qsos holds one row per QSO a source reported (source "log4om"
// or "clublog"); confirmations is replaced whole on each Club Log refresh.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS contest_files (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT NOT NULL,
    name         TEXT NOT NULL,
    db_file      TEXT NOT NULL DEFAULT '',
    contest_name TEXT NOT NULL DEFAULT '',
    path         TEXT NOT NULL,
    uploaded_utc TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS contests (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    name               TEXT NOT NULL,
    start_date         TEXT NOT NULL,
    definition_file_id INTEGER NOT NULL DEFAULT 0,
    prefill_file_id    INTEGER NOT NULL DEFAULT 0,
    activated_utc      TEXT NOT NULL,
    UNIQUE (name, start_date)
);
CREATE TABLE IF NOT EXISTS contest_qsos (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    contest_id  INTEGER NOT NULL,
    station_id  TEXT NOT NULL,
    qso_id      TEXT NOT NULL,
    call        TEXT NOT NULL,
    band        TEXT NOT NULL DEFAULT '',
    mode        TEXT NOT NULL DEFAULT '',
    at_utc      TEXT NOT NULL,
    exch1       TEXT NOT NULL DEFAULT '',
    exch2       TEXT NOT NULL DEFAULT '',
    exch3       TEXT NOT NULL DEFAULT '',
    exch4       TEXT NOT NULL DEFAULT '',
    dupe        INTEGER NOT NULL DEFAULT 0,
    invalid     INTEGER NOT NULL DEFAULT 0,
    xqso        INTEGER NOT NULL DEFAULT 0,
    mult1       TEXT NOT NULL DEFAULT '',
    mult2       TEXT NOT NULL DEFAULT '',
    mult3       TEXT NOT NULL DEFAULT '',
    updated_utc TEXT NOT NULL,
    UNIQUE (contest_id, station_id, qso_id)
);
CREATE INDEX IF NOT EXISTS idx_contest_qsos_call ON contest_qsos(contest_id, call);
CREATE TABLE IF NOT EXISTS contest_handled (
    contest_id INTEGER NOT NULL,
    call       TEXT NOT NULL,
    band       TEXT NOT NULL,
    how        TEXT NOT NULL,
    at_utc     TEXT NOT NULL,
    PRIMARY KEY (contest_id, call, band)
);
CREATE TABLE IF NOT EXISTS log_qsos (
    source TEXT NOT NULL,
    call   TEXT NOT NULL,
    band   TEXT NOT NULL,
    mode   TEXT NOT NULL DEFAULT '',
    at_utc TEXT NOT NULL,
    PRIMARY KEY (source, call, band, at_utc)
);
CREATE INDEX IF NOT EXISTS idx_log_qsos_call ON log_qsos(call, band);
CREATE TABLE IF NOT EXISTS confirmations (
    dxcc INTEGER NOT NULL,
    band TEXT NOT NULL,
    PRIMARY KEY (dxcc, band)
);
CREATE TABLE IF NOT EXISTS dx_skips (
    call      TEXT NOT NULL,
    band      TEXT NOT NULL,
    until_utc TEXT NOT NULL,
    PRIMARY KEY (call, band)
);
`

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
