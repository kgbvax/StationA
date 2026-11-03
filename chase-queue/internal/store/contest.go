package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Contest is a contest instance: a contest name plus the start date the
// operator confirmed. It points at the uploaded definition and prefill
// files it uses (0 = none).
type Contest struct {
	ID               int64
	Name             string
	StartDate        string // YYYY-MM-DD
	DefinitionFileID int64
	PrefillFileID    int64
	Activated        time.Time
}

// ContestQSO is one QSO DXLog logged, keyed by the station that logged it
// and its QSO id there. Exch holds DXLog's exch1..exch4; Mults its
// mult1..mult3 as sent.
type ContestQSO struct {
	StationID string
	QSOID     string
	Call      string
	Band      string
	Mode      string
	At        time.Time
	Exch      [4]string
	Mults     [3]string
	Dupe      bool
	Invalid   bool
	XQSO      bool
}

// ContestHandled is a station the operator dealt with on a band: how is
// "spot", "skip" or "qsy".
type ContestHandled struct {
	Call string
	Band string
	How  string
	At   time.Time
}

// ContestFile is an uploaded contest definition or prefill database. The
// file lives under the storage directory at Path, a name the server chose;
// Name is the name it was uploaded under and DBFile the lowercase base name
// a prefill is matched to a definition's DB_FILE by.
type ContestFile struct {
	ID          int64
	Kind        string // "definition" or "prefill"
	Name        string
	DBFile      string
	ContestName string
	Path        string
	Uploaded    time.Time
}

const (
	ContestFileDefinition = "definition"
	ContestFilePrefill    = "prefill"
)

// CreateContest returns the instance with this name and start date,
// creating it when there is none yet. The definition and prefill ids of an
// existing instance are updated when given.
func (s *Store) CreateContest(name, startDate string, definitionFileID, prefillFileID int64, now time.Time) (Contest, error) {
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO contests (name, start_date, definition_file_id, prefill_file_id, activated_utc)
		VALUES (?, ?, ?, ?, ?)`, name, startDate, definitionFileID, prefillFileID, dxTime(now)); err != nil {
		return Contest{}, err
	}
	if definitionFileID != 0 {
		if _, err := s.db.Exec(`UPDATE contests SET definition_file_id=? WHERE name=? AND start_date=?`, definitionFileID, name, startDate); err != nil {
			return Contest{}, err
		}
	}
	if prefillFileID != 0 {
		if _, err := s.db.Exec(`UPDATE contests SET prefill_file_id=? WHERE name=? AND start_date=?`, prefillFileID, name, startDate); err != nil {
			return Contest{}, err
		}
	}
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM contests WHERE name=? AND start_date=?`, name, startDate).Scan(&id); err != nil {
		return Contest{}, err
	}
	c, _, err := s.GetContest(id)
	return c, err
}

const contestCols = `id, name, start_date, definition_file_id, prefill_file_id, activated_utc`

func scanContest(sc interface{ Scan(...any) error }) (Contest, error) {
	var c Contest
	var act string
	if err := sc.Scan(&c.ID, &c.Name, &c.StartDate, &c.DefinitionFileID, &c.PrefillFileID, &act); err != nil {
		return Contest{}, err
	}
	c.Activated = parseDXTime(act)
	return c, nil
}

// GetContest returns an instance; ok is false when there is none.
func (s *Store) GetContest(id int64) (Contest, bool, error) {
	c, err := scanContest(s.db.QueryRow(`SELECT `+contestCols+` FROM contests WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Contest{}, false, nil
	}
	return c, err == nil, err
}

// ListContests returns all instances, newest first.
func (s *Store) ListContests() ([]Contest, error) {
	rows, err := s.db.Query(`SELECT ` + contestCols + ` FROM contests ORDER BY start_date DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Contest
	for rows.Next() {
		c, err := scanContest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteContest removes an instance with its QSOs and handled stations,
// and the definition and prefill file rows no other instance uses. It
// returns the storage paths of the removed file rows, which the caller
// deletes from disk.
func (s *Store) DeleteContest(id int64) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	paths, err := deleteContestTx(tx, id)
	if err != nil {
		return nil, err
	}
	return paths, tx.Commit()
}

func deleteContestTx(tx *sql.Tx, id int64) ([]string, error) {
	var def, pre int64
	if err := tx.QueryRow(`SELECT definition_file_id, prefill_file_id FROM contests WHERE id=?`, id).Scan(&def, &pre); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	for _, q := range []string{
		`DELETE FROM contest_qsos WHERE contest_id=?`,
		`DELETE FROM contest_handled WHERE contest_id=?`,
		`DELETE FROM contests WHERE id=?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return nil, err
		}
	}
	// only the files this instance used and nobody else uses go with it; a
	// file uploaded but not yet attached to any instance stays
	return unreferencedFilesTx(tx, "", def, pre)
}

// unreferencedFilesTx deletes the file rows no instance points at and
// returns their paths. With ids it considers only those rows; otherwise
// only rows uploaded before the cutoff.
func unreferencedFilesTx(tx *sql.Tx, cutoff string, ids ...int64) ([]string, error) {
	q := `SELECT id, path FROM contest_files
		WHERE id NOT IN (SELECT definition_file_id FROM contests)
		  AND id NOT IN (SELECT prefill_file_id FROM contests)`
	var args []any
	if cutoff != "" {
		q += ` AND uploaded_utc < ?`
		args = append(args, cutoff)
	} else {
		q += ` AND id IN (?, ?)`
		args = append(args, ids[0], ids[1])
	}
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	var gone []int64
	var paths []string
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return nil, err
		}
		gone = append(gone, id)
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range gone {
		if _, err := tx.Exec(`DELETE FROM contest_files WHERE id=?`, id); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

// PruneContests deletes the instances untouched since the cutoff, never the
// active one (0 = none), and returns the storage paths of the file rows that
// went with them and how many instances were removed. An instance is
// touched by its activation, a QSO upsert or a handled mark.
func (s *Store) PruneContests(cutoff time.Time, activeID int64) ([]string, int, error) {
	c := dxTime(cutoff)
	rows, err := s.db.Query(`SELECT c.id FROM contests c
		WHERE c.id <> ?
		  AND c.activated_utc < ?
		  AND COALESCE((SELECT MAX(updated_utc) FROM contest_qsos WHERE contest_id=c.id), '') < ?
		  AND COALESCE((SELECT MAX(at_utc) FROM contest_handled WHERE contest_id=c.id), '') < ?`,
		activeID, c, c, c)
	if err != nil {
		return nil, 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var paths []string
	for _, id := range ids {
		p, err := s.DeleteContest(id)
		if err != nil {
			return paths, 0, err
		}
		paths = append(paths, p...)
	}
	return paths, len(ids), nil
}

// UpsertContestQSOs stores QSOs in one transaction, replacing a QSO already
// stored under the same station and QSO id.
func (s *Store) UpsertContestQSOs(contestID int64, qsos []ContestQSO, now time.Time) error {
	if len(qsos) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO contest_qsos
		(contest_id, station_id, qso_id, call, band, mode, at_utc, exch1, exch2, exch3, exch4, dupe, invalid, xqso, mult1, mult2, mult3, updated_utc)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(contest_id, station_id, qso_id) DO UPDATE SET
		 call=excluded.call, band=excluded.band, mode=excluded.mode, at_utc=excluded.at_utc,
		 exch1=excluded.exch1, exch2=excluded.exch2, exch3=excluded.exch3, exch4=excluded.exch4,
		 dupe=excluded.dupe, invalid=excluded.invalid, xqso=excluded.xqso,
		 mult1=excluded.mult1, mult2=excluded.mult2, mult3=excluded.mult3, updated_utc=excluded.updated_utc`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, q := range qsos {
		if _, err := stmt.Exec(contestID, q.StationID, q.QSOID, q.Call, q.Band, q.Mode, dxTime(q.At),
			q.Exch[0], q.Exch[1], q.Exch[2], q.Exch[3], boolInt(q.Dupe), boolInt(q.Invalid), boolInt(q.XQSO),
			q.Mults[0], q.Mults[1], q.Mults[2], dxTime(now)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListContestQSOs returns the QSOs of an instance in time order.
func (s *Store) ListContestQSOs(contestID int64) ([]ContestQSO, error) {
	rows, err := s.db.Query(`SELECT station_id, qso_id, call, band, mode, at_utc, exch1, exch2, exch3, exch4,
		dupe, invalid, xqso, mult1, mult2, mult3 FROM contest_qsos WHERE contest_id=? ORDER BY at_utc, id`, contestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContestQSO
	for rows.Next() {
		var q ContestQSO
		var at string
		var dupe, invalid, xqso int
		if err := rows.Scan(&q.StationID, &q.QSOID, &q.Call, &q.Band, &q.Mode, &at,
			&q.Exch[0], &q.Exch[1], &q.Exch[2], &q.Exch[3], &dupe, &invalid, &xqso,
			&q.Mults[0], &q.Mults[1], &q.Mults[2]); err != nil {
			return nil, err
		}
		q.At = parseDXTime(at)
		q.Dupe, q.Invalid, q.XQSO = dupe != 0, invalid != 0, xqso != 0
		out = append(out, q)
	}
	return out, rows.Err()
}

// DeleteContestQSO removes one QSO and returns how many rows went.
func (s *Store) DeleteContestQSO(contestID int64, stationID, qsoID string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM contest_qsos WHERE contest_id=? AND station_id=? AND qso_id=?`, contestID, stationID, qsoID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountStationQSOs returns how many QSOs of an instance one station logged.
func (s *Store) CountStationQSOs(contestID int64, stationID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM contest_qsos WHERE contest_id=? AND station_id=?`, contestID, stationID).Scan(&n)
	return n, err
}

// DeleteStationQSOsNotIn removes, in one transaction, the QSOs one station
// logged in an instance whose QSO ids are not in keep, and returns how many.
// Other stations' QSOs are left alone.
func (s *Store) DeleteStationQSOsNotIn(contestID int64, stationID string, keep []string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TEMP TABLE IF NOT EXISTS keep_qso (id TEXT PRIMARY KEY)`); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM keep_qso`); err != nil {
		return 0, err
	}
	for _, id := range keep {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO keep_qso (id) VALUES (?)`, id); err != nil {
			return 0, err
		}
	}
	res, err := tx.Exec(`DELETE FROM contest_qsos WHERE contest_id=? AND station_id=? AND qso_id NOT IN (SELECT id FROM keep_qso)`, contestID, stationID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

// MarkContestHandled records that the operator dealt with a station on a
// band; a later mark of the same station and band replaces the earlier.
func (s *Store) MarkContestHandled(contestID int64, call, band, how string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO contest_handled (contest_id, call, band, how, at_utc) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(contest_id, call, band) DO UPDATE SET how=excluded.how, at_utc=excluded.at_utc`,
		contestID, call, band, how, dxTime(at))
	return err
}

// ClearContestHandled removes a handled mark.
func (s *Store) ClearContestHandled(contestID int64, call, band string) error {
	_, err := s.db.Exec(`DELETE FROM contest_handled WHERE contest_id=? AND call=? AND band=?`, contestID, call, band)
	return err
}

// ListContestHandled returns the handled stations of an instance.
func (s *Store) ListContestHandled(contestID int64) ([]ContestHandled, error) {
	rows, err := s.db.Query(`SELECT call, band, how, at_utc FROM contest_handled WHERE contest_id=? ORDER BY at_utc`, contestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContestHandled
	for rows.Next() {
		var h ContestHandled
		var at string
		if err := rows.Scan(&h.Call, &h.Band, &h.How, &at); err != nil {
			return nil, err
		}
		h.At = parseDXTime(at)
		out = append(out, h)
	}
	return out, rows.Err()
}

const contestFileCols = `id, kind, name, db_file, contest_name, path, uploaded_utc`

func scanContestFile(sc interface{ Scan(...any) error }) (ContestFile, error) {
	var f ContestFile
	var up string
	if err := sc.Scan(&f.ID, &f.Kind, &f.Name, &f.DBFile, &f.ContestName, &f.Path, &up); err != nil {
		return ContestFile{}, err
	}
	f.Uploaded = parseDXTime(up)
	return f, nil
}

// AddContestFile records an uploaded file. An earlier file of the same kind
// and matching name (a prefill by DBFile, a definition by ContestName
// or, when that is empty, by Name; compared case-insensitively) is replaced
// in the same transaction; the storage paths of the replaced rows are
// returned for the caller to delete from disk. Instances that pointed at
// a replaced file are repointed at the new one.
func (s *Store) AddContestFile(f ContestFile) (ContestFile, []string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return ContestFile{}, nil, err
	}
	defer tx.Rollback()
	key, col := strings.ToLower(f.Name), "lower(name)"
	if f.Kind == ContestFilePrefill && f.DBFile != "" {
		key, col = strings.ToLower(f.DBFile), "lower(db_file)"
	} else if f.Kind == ContestFileDefinition && f.ContestName != "" {
		key, col = strings.ToLower(f.ContestName), "lower(contest_name)"
	}
	rows, err := tx.Query(`SELECT id, path FROM contest_files WHERE kind=? AND `+col+`=?`, f.Kind, key)
	if err != nil {
		return ContestFile{}, nil, err
	}
	var oldIDs []int64
	var paths []string
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return ContestFile{}, nil, err
		}
		oldIDs = append(oldIDs, id)
		paths = append(paths, p)
	}
	rows.Close()
	res, err := tx.Exec(`INSERT INTO contest_files (kind, name, db_file, contest_name, path, uploaded_utc) VALUES (?, ?, ?, ?, ?, ?)`,
		f.Kind, f.Name, f.DBFile, f.ContestName, f.Path, dxTime(f.Uploaded))
	if err != nil {
		return ContestFile{}, nil, err
	}
	f.ID, _ = res.LastInsertId()
	for _, id := range oldIDs {
		colRef := "definition_file_id"
		if f.Kind == ContestFilePrefill {
			colRef = "prefill_file_id"
		}
		if _, err := tx.Exec(`UPDATE contests SET `+colRef+`=? WHERE `+colRef+`=?`, f.ID, id); err != nil {
			return ContestFile{}, nil, err
		}
		if _, err := tx.Exec(`DELETE FROM contest_files WHERE id=?`, id); err != nil {
			return ContestFile{}, nil, err
		}
	}
	return f, paths, tx.Commit()
}

// ContestFileByID returns a file row.
func (s *Store) ContestFileByID(id int64) (ContestFile, bool, error) {
	f, err := scanContestFile(s.db.QueryRow(`SELECT `+contestFileCols+` FROM contest_files WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ContestFile{}, false, nil
	}
	return f, err == nil, err
}

// ListContestFiles returns all file rows of a kind ("" = all), newest first.
func (s *Store) ListContestFiles(kind string) ([]ContestFile, error) {
	rows, err := s.db.Query(`SELECT `+contestFileCols+` FROM contest_files WHERE ?='' OR kind=? ORDER BY uploaded_utc DESC, id DESC`, kind, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContestFile
	for rows.Next() {
		f, err := scanContestFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FindContestPrefill returns the prefill file a definition's DB_FILE names:
// the base name compared case-insensitively, a Windows path separator
// treated as '/'.
func (s *Store) FindContestPrefill(dbFile string) (ContestFile, bool, error) {
	key := strings.ToLower(strings.ReplaceAll(dbFile, `\`, "/"))
	if i := strings.LastIndex(key, "/"); i >= 0 {
		key = key[i+1:]
	}
	f, err := scanContestFile(s.db.QueryRow(`SELECT `+contestFileCols+` FROM contest_files
		WHERE kind=? AND lower(db_file)=? ORDER BY uploaded_utc DESC, id DESC LIMIT 1`, ContestFilePrefill, key))
	if errors.Is(err, sql.ErrNoRows) {
		return ContestFile{}, false, nil
	}
	return f, err == nil, err
}

// PruneContestFiles deletes the file rows no instance uses that were
// uploaded before the cutoff and returns their storage paths.
func (s *Store) PruneContestFiles(cutoff time.Time) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	paths, err := unreferencedFilesTx(tx, dxTime(cutoff))
	if err != nil {
		return nil, err
	}
	return paths, tx.Commit()
}
