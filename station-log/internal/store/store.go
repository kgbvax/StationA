// Package store is station-log's SQLite database (modernc, pure Go): the
// QSO record, with the merge rules of a duplicate import, and a few small
// settings. Callsigns and the log are personal data: the database lives
// under the service's state directory and is never committed.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the database handle. One connection: every write is serialised,
// and an open Tx holds it, so a reader waits for a running import.
type Store struct {
	db  *sql.DB
	Now func() time.Time
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, Now: time.Now}
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
	if version < 2 {
		if _, err := s.db.Exec(schemaV2); err != nil {
			return err
		}
		if _, err := s.db.Exec(`PRAGMA user_version=2`); err != nil {
			return err
		}
	}
	return nil
}

// v2: every source that contributed to a QSO (stored it or merged into it),
// so replace_source removes a QSO only when the replaced source is the sole
// contributor. Existing rows get their first source.
const schemaV2 = `
CREATE TABLE IF NOT EXISTS qso_sources (
    qso_id INTEGER NOT NULL,
    source TEXT NOT NULL,
    PRIMARY KEY (qso_id, source)
);
CREATE INDEX IF NOT EXISTS idx_qso_sources_source ON qso_sources(source);
INSERT OR IGNORE INTO qso_sources (qso_id, source) SELECT id, source FROM qsos;
`

// v1: the QSO table and settings. Times are RFC 3339 UTC text in whole
// seconds, so the text compares in time order. The flags are 0/1;
// confirmed is their OR (any confirmation path).
const schemaV1 = `
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS qsos (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    own_call    TEXT NOT NULL,
    call        TEXT NOT NULL,
    band        TEXT NOT NULL,
    mode        TEXT NOT NULL DEFAULT '',
    submode     TEXT NOT NULL DEFAULT '',
    mode_class  TEXT NOT NULL,
    at_utc      TEXT NOT NULL,
    freq_hz     INTEGER NOT NULL DEFAULT 0,
    dxcc        INTEGER NOT NULL DEFAULT 0,
    state       TEXT NOT NULL DEFAULT '',
    cq_zone     INTEGER NOT NULL DEFAULT 0,
    itu_zone    INTEGER NOT NULL DEFAULT 0,
    grid        TEXT NOT NULL DEFAULT '',
    sota_ref    TEXT NOT NULL DEFAULT '',
    wwff_ref    TEXT NOT NULL DEFAULT '',
    sig         TEXT NOT NULL DEFAULT '',
    sig_info    TEXT NOT NULL DEFAULT '',
    qsl_rcvd    INTEGER NOT NULL DEFAULT 0,
    lotw_rcvd   INTEGER NOT NULL DEFAULT 0,
    eqsl_rcvd   INTEGER NOT NULL DEFAULT 0,
    club_log    INTEGER NOT NULL DEFAULT 0,
    confirmed   INTEGER NOT NULL DEFAULT 0,
    source      TEXT NOT NULL DEFAULT '',
    created_utc TEXT NOT NULL,
    updated_utc TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_qsos_dup ON qsos(own_call, call, band, mode_class, at_utc);
CREATE INDEX IF NOT EXISTS idx_qsos_call ON qsos(call);
CREATE INDEX IF NOT EXISTS idx_qsos_source ON qsos(source);
`

// Setting reads a small value; ok is false when it was never set.
func (s *Store) Setting(key string) (value string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

// SetSetting stores a small value.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
