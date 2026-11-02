package store

import (
	"time"
)

// Log sources.
const (
	SourceLog4OM  = "log4om"
	SourceClubLog = "clublog"
)

// LogQSO is one QSO of the operator's station log.
type LogQSO struct {
	Source string
	Call   string // normalised by the caller
	Band   string
	Mode   string
	At     time.Time
}

// AddLogQSOs stores QSOs; a QSO already stored (same source, call, band and
// time) is kept once.
func (s *Store) AddLogQSOs(qs []LogQSO) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`INSERT OR IGNORE INTO log_qsos (source, call, band, mode, at_utc) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, q := range qs {
		if _, err := st.Exec(q.Source, q.Call, q.Band, q.Mode, dxTime(q.At)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceLogQSOs replaces every QSO of one source (a full Club Log download).
func (s *Store) ReplaceLogQSOs(source string, qs []LogQSO) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM log_qsos WHERE source=?`, source); err != nil {
		return err
	}
	st, err := tx.Prepare(`INSERT OR IGNORE INTO log_qsos (source, call, band, mode, at_utc) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, q := range qs {
		if _, err := st.Exec(source, q.Call, q.Band, q.Mode, dxTime(q.At)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// WorkedKey is one callsign worked on one band.
type WorkedKey struct{ Call, Band string }

// WorkedSet returns every callsign and band of the log, whatever the source
// and mode.
func (s *Store) WorkedSet() (map[WorkedKey]bool, error) {
	rows, err := s.db.Query(`SELECT DISTINCT call, band FROM log_qsos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[WorkedKey]bool{}
	for rows.Next() {
		var k WorkedKey
		if err := rows.Scan(&k.Call, &k.Band); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// Confirmation is one DXCC entity (ADIF number) confirmed on one band.
type Confirmation struct {
	DXCC int
	Band string
}

// ReplaceConfirmations replaces the confirmation set (one Club Log refresh).
func (s *Store) ReplaceConfirmations(cs []Confirmation) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM confirmations`); err != nil {
		return err
	}
	st, err := tx.Prepare(`INSERT OR IGNORE INTO confirmations (dxcc, band) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, c := range cs {
		if _, err := st.Exec(c.DXCC, c.Band); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Confirmations returns the stored confirmation set.
func (s *Store) Confirmations() ([]Confirmation, error) {
	rows, err := s.db.Query(`SELECT dxcc, band FROM confirmations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Confirmation
	for rows.Next() {
		var c Confirmation
		if err := rows.Scan(&c.DXCC, &c.Band); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DXSkip is a station the operator skipped in DX mode, hidden until Until.
type DXSkip struct {
	Call, Band string
	Until      time.Time
}

func (s *Store) SetDXSkip(k DXSkip) error {
	_, err := s.db.Exec(`INSERT INTO dx_skips (call, band, until_utc) VALUES (?, ?, ?)
		ON CONFLICT(call, band) DO UPDATE SET until_utc=excluded.until_utc`, k.Call, k.Band, dxTime(k.Until))
	return err
}

func (s *Store) ClearDXSkip(call, band string) error {
	_, err := s.db.Exec(`DELETE FROM dx_skips WHERE call=? AND band=?`, call, band)
	return err
}

// DXSkips returns the skips still in force at now and deletes the rest.
func (s *Store) DXSkips(now time.Time) ([]DXSkip, error) {
	if _, err := s.db.Exec(`DELETE FROM dx_skips WHERE until_utc <= ?`, dxTime(now)); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT call, band, until_utc FROM dx_skips`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DXSkip
	for rows.Next() {
		var k DXSkip
		var until string
		if err := rows.Scan(&k.Call, &k.Band, &until); err != nil {
			return nil, err
		}
		k.Until = parseDXTime(until)
		out = append(out, k)
	}
	return out, rows.Err()
}
