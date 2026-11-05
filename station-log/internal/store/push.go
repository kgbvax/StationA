package store

import (
	"strconv"
	"strings"
	"time"
)

// SaveRaw keeps the ADIF record a QSO first arrived as (from a non-Wavelog
// source); a later record of the same QSO does not replace it.
func (t *Tx) SaveRaw(qsoID, source, adifText string) error {
	_, err := t.tx.Exec(`INSERT OR IGNORE INTO qso_raw (qso_id, source, adif) VALUES (?, ?, ?)`, qsoID, source, adifText)
	return err
}

// PushQuery selects the QSOs to push to Wavelog for one station profile.
type PushQuery struct {
	// OwnCalls restricts to these own calls; with NotOwn it excludes them.
	OwnCalls []string
	NotOwn   bool
	// Since: only QSOs at or after this time (history is not bulk-pushed).
	Since time.Time
	// Prefixes: at least one source of the QSO must start with one of these.
	Prefixes []string
	// ExcludeSource: a QSO this source contributed to (it came from or merged
	// with the remote) is never a candidate: the structural loop guard.
	ExcludeSource string
	ProfileID     string
	// MaxAttempts: a failed QSO is skipped after this many attempts until its
	// fingerprint changes.
	MaxAttempts int
	// Limit caps the result (0 = no cap).
	Limit int
}

// PushCandidate is a QSO to push with its raw ADIF record (may be empty).
type PushCandidate struct {
	QSO         QSO
	Raw         string
	Pushed      bool
	Fingerprint string // of the last push attempt
	Attempts    int
}

func prefixed(cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = "q." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// PushCandidates lists what is still to be pushed, oldest id first.
func (s *Store) PushCandidates(pq PushQuery) ([]PushCandidate, error) {
	var args []any
	args = append(args, pq.ProfileID)
	where := []string{"1=1"}
	if len(pq.OwnCalls) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(pq.OwnCalls)), ",")
		op := "IN"
		if pq.NotOwn {
			op = "NOT IN"
		}
		where = append(where, "q.own_call "+op+" ("+ph+")")
		for _, c := range pq.OwnCalls {
			args = append(args, c)
		}
	} else if !pq.NotOwn {
		return nil, nil
	}
	if !pq.Since.IsZero() {
		where = append(where, "q.at_utc >= ?")
		args = append(args, ts(pq.Since))
	}
	if pq.ExcludeSource != "" {
		where = append(where, "NOT EXISTS (SELECT 1 FROM qso_sources x WHERE x.qso_id=q.id AND x.source=?)")
		args = append(args, pq.ExcludeSource)
	}
	if len(pq.Prefixes) > 0 {
		var ors []string
		for _, p := range pq.Prefixes {
			ors = append(ors, "substr(x.source,1,?)=?")
			args = append(args, len(p), p)
		}
		where = append(where, "EXISTS (SELECT 1 FROM qso_sources x WHERE x.qso_id=q.id AND ("+strings.Join(ors, " OR ")+"))")
	}
	// pushed once: Wavelog skips duplicates and has its own LoTW/QRZ sync, so
	// a QSO that gained confirmations or fields is not sent again
	where = append(where, "(p.qso_id IS NULL OR p.pushed_utc='')")
	rows, err := s.db.Query(`SELECT `+prefixed(qsoCols)+`, COALESCE(r.adif,''), COALESCE(p.pushed_utc,''), COALESCE(p.fingerprint,''),
		COALESCE(p.attempts,0) FROM qsos q LEFT JOIN qso_raw r ON r.qso_id=q.id
		LEFT JOIN wavelog_push p ON p.qso_id=q.id AND p.profile_id=?
		WHERE `+strings.Join(where, " AND ")+` ORDER BY q.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushCandidate
	for rows.Next() {
		var c PushCandidate
		var pushedAt string
		// scanQSO reads the 26 QSO columns; the extra four follow
		q, extra, err := scanQSOExtra(rows)
		if err != nil {
			return nil, err
		}
		c.QSO = q
		c.Raw, pushedAt, c.Fingerprint, c.Attempts = extra.raw, extra.pushed, extra.fp, extra.attempts
		c.Pushed = pushedAt != ""
		if !c.Pushed && pq.MaxAttempts > 0 && c.Attempts >= pq.MaxAttempts && c.Fingerprint == q.Fingerprint() {
			continue
		}
		out = append(out, c)
		if pq.Limit > 0 && len(out) >= pq.Limit {
			break
		}
	}
	return out, rows.Err()
}

type pushExtra struct {
	raw, pushed, fp string
	attempts        int
}

func scanQSOExtra(r scanner) (QSO, pushExtra, error) {
	var q QSO
	var e pushExtra
	var id int64
	var at, created, updated string
	var qsl, lotw, eqsl, club, conf int
	err := r.Scan(&id, &q.OwnCall, &q.Call, &q.Band, &q.Mode, &q.Submode, &q.ModeClass, &at, &q.FreqHz, &q.DXCC, &q.State,
		&q.CQZone, &q.ITUZone, &q.Grid, &q.SOTARef, &q.WWFFRef, &q.Sig, &q.SigInfo, &qsl, &lotw, &eqsl, &club, &conf,
		&q.Source, &created, &updated, &e.raw, &e.pushed, &e.fp, &e.attempts)
	if err != nil {
		return QSO{}, e, err
	}
	q.ID = strconv.FormatInt(id, 10)
	q.At, q.Created, q.Updated = parseTS(at), parseTS(created), parseTS(updated)
	q.QSLRcvd, q.LoTWRcvd, q.EQSLRcvd, q.ClubLog, q.Confirmed = qsl != 0, lotw != 0, eqsl != 0, club != 0, conf != 0
	return q, e, nil
}

// MarkPushed records a successful push (note: "" or e.g. "duplicate").
func (s *Store) MarkPushed(qsoID, profileID, fingerprint, note string) error {
	_, err := s.db.Exec(`INSERT INTO wavelog_push (qso_id, profile_id, fingerprint, pushed_utc, attempts, last_error)
		VALUES (?,?,?,?,0,?) ON CONFLICT(qso_id, profile_id) DO UPDATE SET fingerprint=excluded.fingerprint,
		pushed_utc=excluded.pushed_utc, attempts=0, last_error=excluded.last_error`,
		qsoID, profileID, fingerprint, ts(s.Now()), note)
	return err
}

// MarkFailed counts a failed push attempt; the counter restarts when the
// fingerprint changed since the last attempt.
func (s *Store) MarkFailed(qsoID, profileID, fingerprint, msg string) error {
	_, err := s.db.Exec(`INSERT INTO wavelog_push (qso_id, profile_id, fingerprint, pushed_utc, attempts, last_error)
		VALUES (?,?,?,'',1,?) ON CONFLICT(qso_id, profile_id) DO UPDATE SET
		attempts=CASE WHEN wavelog_push.fingerprint=excluded.fingerprint AND wavelog_push.pushed_utc='' THEN wavelog_push.attempts+1 ELSE 1 END,
		fingerprint=excluded.fingerprint, pushed_utc='', last_error=excluded.last_error`,
		qsoID, profileID, fingerprint, msg)
	return err
}

// PushedCount is the number of QSOs successfully pushed for a profile.
func (s *Store) PushedCount(profileID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM wavelog_push WHERE profile_id=? AND pushed_utc<>''`, profileID).Scan(&n)
	return n, err
}

// PushError returns the last error of a QSO's push row.
func (s *Store) PushError(qsoID, profileID string) (string, error) {
	var e string
	err := s.db.QueryRow(`SELECT last_error FROM wavelog_push WHERE qso_id=? AND profile_id=?`, qsoID, profileID).Scan(&e)
	return e, err
}

// ResetPushAttempts gives failed, not yet pushed QSOs (at or above minAttempts
// attempts) a fresh set of attempts; it returns how many rows changed. profileID
// "" = all profiles.
func (s *Store) ResetPushAttempts(profileID string, minAttempts int) (int, error) {
	res, err := s.db.Exec(`UPDATE wavelog_push SET attempts=0 WHERE pushed_utc='' AND attempts>=? AND (?='' OR profile_id=?)`,
		minAttempts, profileID, profileID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ResetWavelogState forgets everything tied to one Wavelog instance: the push
// ledger and every setting whose key starts with one of settingPrefixes (pull
// cursors, full-resync marks).
func (s *Store) ResetWavelogState(settingPrefixes ...string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM wavelog_push`); err != nil {
		return err
	}
	for _, p := range settingPrefixes {
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(p)
		if _, err := tx.Exec(`DELETE FROM settings WHERE key LIKE ? ESCAPE '\'`, esc+"%"); err != nil {
			return err
		}
	}
	return tx.Commit()
}
