package store

import (
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// Mode classes: the award families a mode belongs to. Duplicate detection and
// the worked view key on the class, not on the mode as logged (SSB, USB and
// LSB are one QSO).
const (
	ClassCW    = "cw"
	ClassPhone = "phone"
	ClassData  = "data"
)

// Event actions.
const (
	ActionAdded   = "added"
	ActionMerged  = "merged"
	ActionDeleted = "deleted"
)

// QSO is one logged contact. Zero values mean "not known".
type QSO struct {
	ID        string    `json:"id"` // the row id, as a string
	OwnCall   string    `json:"own_call"`
	Call      string    `json:"call"`
	Band      string    `json:"band"`
	Mode      string    `json:"mode"` // as logged, upper case
	Submode   string    `json:"submode,omitempty"`
	ModeClass string    `json:"mode_class"`
	At        time.Time `json:"at"`
	FreqHz    int64     `json:"freq_hz,omitempty"`
	DXCC      int       `json:"dxcc,omitempty"` // ADIF entity number
	State     string    `json:"state,omitempty"`
	CQZone    int       `json:"cq_zone,omitempty"`
	ITUZone   int       `json:"itu_zone,omitempty"`
	Grid      string    `json:"grid,omitempty"`
	SOTARef   string    `json:"sota_ref,omitempty"`
	WWFFRef   string    `json:"wwff_ref,omitempty"`
	Sig       string    `json:"sig,omitempty"`
	SigInfo   string    `json:"sig_info,omitempty"`
	QSLRcvd   bool      `json:"qsl_rcvd"`
	LoTWRcvd  bool      `json:"lotw_rcvd"`
	EQSLRcvd  bool      `json:"eqsl_rcvd"`
	ClubLog   bool      `json:"club_log"`
	Confirmed bool      `json:"confirmed"` // any of the four
	Source    string    `json:"source"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

// Event is what a write did to one QSO, for the event topic.
type Event struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	QSO    QSO    `json:"qso"`
}

// Result is the outcome of one Upsert. Action is ActionAdded, ActionMerged
// (the match changed) or "" (a match, nothing new).
type Result struct {
	Action string
	QSO    QSO
}

const qsoCols = `id, own_call, call, band, mode, submode, mode_class, at_utc, freq_hz, dxcc, state, cq_zone, itu_zone,
	grid, sota_ref, wwff_ref, sig, sig_info, qsl_rcvd, lotw_rcvd, eqsl_rcvd, club_log, confirmed, source, created_utc, updated_utc`

type scanner interface{ Scan(dest ...any) error }

func scanQSO(r scanner) (QSO, error) {
	var q QSO
	var id int64
	var at, created, updated string
	var qsl, lotw, eqsl, club, conf int
	err := r.Scan(&id, &q.OwnCall, &q.Call, &q.Band, &q.Mode, &q.Submode, &q.ModeClass, &at, &q.FreqHz, &q.DXCC, &q.State,
		&q.CQZone, &q.ITUZone, &q.Grid, &q.SOTARef, &q.WWFFRef, &q.Sig, &q.SigInfo, &qsl, &lotw, &eqsl, &club, &conf,
		&q.Source, &created, &updated)
	if err != nil {
		return QSO{}, err
	}
	q.ID = strconv.FormatInt(id, 10)
	q.At, q.Created, q.Updated = parseTS(at), parseTS(created), parseTS(updated)
	q.QSLRcvd, q.LoTWRcvd, q.EQSLRcvd, q.ClubLog, q.Confirmed = qsl != 0, lotw != 0, eqsl != 0, club != 0, conf != 0
	return q, nil
}

// Validate reports why a QSO cannot be stored.
func (q QSO) Validate() error {
	switch {
	case q.OwnCall == "":
		return errors.New("no own callsign")
	case q.Call == "":
		return errors.New("no callsign")
	case q.Band == "":
		return errors.New("no band")
	case q.ModeClass == "":
		return errors.New("no mode")
	case q.At.IsZero():
		return errors.New("no QSO time")
	}
	return nil
}

// Merge folds a later arrival of the same QSO into q and reports whether q
// changed. Rules: the earliest time is kept; the flags only turn true;
// an empty value never overwrites a known one; the award references,
// entity, zones and grid are overwritten by a non-empty value (a later
// export carries the corrected one); mode and frequency are only filled in;
// the source stays the first one's.
func (q *QSO) Merge(in QSO) bool {
	old := *q
	if !in.At.IsZero() && in.At.Before(q.At) {
		q.At = in.At
	}
	fill := func(dst *string, v string) {
		if *dst == "" {
			*dst = v
		}
	}
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	fill(&q.Mode, in.Mode)
	fill(&q.Submode, in.Submode)
	if q.FreqHz == 0 {
		q.FreqHz = in.FreqHz
	}
	set(&q.State, in.State)
	set(&q.Grid, in.Grid)
	set(&q.SOTARef, in.SOTARef)
	set(&q.WWFFRef, in.WWFFRef)
	set(&q.Sig, in.Sig)
	set(&q.SigInfo, in.SigInfo)
	if in.DXCC != 0 {
		q.DXCC = in.DXCC
	}
	if in.CQZone != 0 {
		q.CQZone = in.CQZone
	}
	if in.ITUZone != 0 {
		q.ITUZone = in.ITUZone
	}
	q.QSLRcvd = q.QSLRcvd || in.QSLRcvd
	q.LoTWRcvd = q.LoTWRcvd || in.LoTWRcvd
	q.EQSLRcvd = q.EQSLRcvd || in.EQSLRcvd
	q.ClubLog = q.ClubLog || in.ClubLog
	q.Confirmed = q.QSLRcvd || q.LoTWRcvd || q.EQSLRcvd || q.ClubLog
	return *q != old
}

// Tx is an open transaction. It holds the only connection: use no Store
// method until Commit or Rollback.
type Tx struct {
	s  *Store
	tx *sql.Tx
}

// Begin starts a write transaction.
func (s *Store) Begin() (*Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{s: s, tx: tx}, nil
}

func (t *Tx) Commit() error   { return t.tx.Commit() }
func (t *Tx) Rollback() error { return t.tx.Rollback() }

// Upsert stores q, or merges it into the stored QSO of the same own call,
// call, band and mode class whose time is within window of q's (the closest
// one). It sets q.ID/Created/Updated on a new row.
func (t *Tx) Upsert(q QSO, window time.Duration) (Result, error) {
	if err := q.Validate(); err != nil {
		return Result{}, err
	}
	q.Confirmed = q.QSLRcvd || q.LoTWRcvd || q.EQSLRcvd || q.ClubLog
	now := t.s.Now().UTC().Truncate(time.Second)
	cur, found, err := t.findDup(q, window)
	if err != nil {
		return Result{}, err
	}
	if !found {
		q.Created, q.Updated = now, now
		res, err := t.tx.Exec(`INSERT INTO qsos (own_call, call, band, mode, submode, mode_class, at_utc, freq_hz, dxcc, state,
			cq_zone, itu_zone, grid, sota_ref, wwff_ref, sig, sig_info, qsl_rcvd, lotw_rcvd, eqsl_rcvd, club_log, confirmed,
			source, created_utc, updated_utc) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			q.OwnCall, q.Call, q.Band, q.Mode, q.Submode, q.ModeClass, ts(q.At), q.FreqHz, q.DXCC, q.State, q.CQZone, q.ITUZone,
			q.Grid, q.SOTARef, q.WWFFRef, q.Sig, q.SigInfo, boolInt(q.QSLRcvd), boolInt(q.LoTWRcvd), boolInt(q.EQSLRcvd),
			boolInt(q.ClubLog), boolInt(q.Confirmed), q.Source, ts(q.Created), ts(q.Updated))
		if err != nil {
			return Result{}, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return Result{}, err
		}
		q.ID = strconv.FormatInt(id, 10)
		if err := t.addSource(q.ID, q.Source); err != nil {
			return Result{}, err
		}
		return Result{Action: ActionAdded, QSO: q}, nil
	}
	if err := t.addSource(cur.ID, q.Source); err != nil {
		return Result{}, err
	}
	if !cur.Merge(q) {
		return Result{QSO: cur}, nil
	}
	cur.Updated = now
	_, err = t.tx.Exec(`UPDATE qsos SET mode=?, submode=?, at_utc=?, freq_hz=?, dxcc=?, state=?, cq_zone=?, itu_zone=?, grid=?,
		sota_ref=?, wwff_ref=?, sig=?, sig_info=?, qsl_rcvd=?, lotw_rcvd=?, eqsl_rcvd=?, club_log=?, confirmed=?, updated_utc=?
		WHERE id=?`,
		cur.Mode, cur.Submode, ts(cur.At), cur.FreqHz, cur.DXCC, cur.State, cur.CQZone, cur.ITUZone, cur.Grid,
		cur.SOTARef, cur.WWFFRef, cur.Sig, cur.SigInfo, boolInt(cur.QSLRcvd), boolInt(cur.LoTWRcvd), boolInt(cur.EQSLRcvd),
		boolInt(cur.ClubLog), boolInt(cur.Confirmed), ts(cur.Updated), cur.ID)
	if err != nil {
		return Result{}, err
	}
	return Result{Action: ActionMerged, QSO: cur}, nil
}

// findDup returns the stored QSO closest in time to q within the window.
func (t *Tx) findDup(q QSO, window time.Duration) (QSO, bool, error) {
	rows, err := t.tx.Query(`SELECT `+qsoCols+` FROM qsos WHERE own_call=? AND call=? AND band=? AND mode_class=?
		AND at_utc>=? AND at_utc<=? ORDER BY id`, q.OwnCall, q.Call, q.Band, q.ModeClass, ts(q.At.Add(-window)), ts(q.At.Add(window)))
	if err != nil {
		return QSO{}, false, err
	}
	defer rows.Close()
	var best QSO
	var bestD time.Duration
	found := false
	for rows.Next() {
		c, err := scanQSO(rows)
		if err != nil {
			return QSO{}, false, err
		}
		d := c.At.Sub(q.At).Abs()
		if !found || d < bestD {
			best, bestD, found = c, d, true
		}
	}
	return best, found, rows.Err()
}

func (t *Tx) addSource(id, source string) error {
	_, err := t.tx.Exec(`INSERT OR IGNORE INTO qso_sources (qso_id, source) VALUES (?, ?)`, id, source)
	return err
}

// SourceIDs lists the row ids of the QSOs a source contributed to (stored
// or merged into).
func (t *Tx) SourceIDs(source string) ([]string, error) {
	rows, err := t.tx.Query(`SELECT qso_id FROM qso_sources WHERE source=? ORDER BY qso_id`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out, rows.Err()
}

// Release drops a source's claim on a QSO. The row is deleted (and returned
// with deleted true) only when no other source contributed to it; otherwise
// it stays with the other sources' data.
func (t *Tx) Release(id, source string) (q QSO, deleted bool, err error) {
	if _, err = t.tx.Exec(`DELETE FROM qso_sources WHERE qso_id=? AND source=?`, id, source); err != nil {
		return QSO{}, false, err
	}
	var n int
	if err = t.tx.QueryRow(`SELECT COUNT(*) FROM qso_sources WHERE qso_id=?`, id).Scan(&n); err != nil {
		return QSO{}, false, err
	}
	if n > 0 {
		return QSO{}, false, nil
	}
	q, err = t.Delete(id)
	return q, err == nil, err
}

// Delete removes one QSO and returns it for the event.
func (t *Tx) Delete(id string) (QSO, error) {
	q, err := scanQSO(t.tx.QueryRow(`SELECT `+qsoCols+` FROM qsos WHERE id=?`, id))
	if err != nil {
		return QSO{}, err
	}
	if _, err := t.tx.Exec(`DELETE FROM qsos WHERE id=?`, id); err != nil {
		return QSO{}, err
	}
	if _, err := t.tx.Exec(`DELETE FROM qso_sources WHERE qso_id=?`, id); err != nil {
		return QSO{}, err
	}
	return q, nil
}

// SetSetting stores a small value inside the transaction.
func (t *Tx) SetSetting(key, value string) error {
	_, err := t.tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// Upsert is a one-QSO transaction.
func (s *Store) Upsert(q QSO, window time.Duration) (Result, error) {
	tx, err := s.Begin()
	if err != nil {
		return Result{}, err
	}
	r, err := tx.Upsert(q, window)
	if err != nil {
		tx.Rollback()
		return Result{}, err
	}
	return r, tx.Commit()
}

// ByCall returns every QSO of a call (exact match), oldest first, optionally
// of one own call.
func (s *Store) ByCall(call, own string) ([]QSO, error) {
	q := `SELECT ` + qsoCols + ` FROM qsos WHERE call=?`
	args := []any{call}
	if own != "" {
		q += ` AND own_call=?`
		args = append(args, own)
	}
	return s.query(q+` ORDER BY at_utc, id`, args...)
}

func (s *Store) query(q string, args ...any) ([]QSO, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QSO
	for rows.Next() {
		c, err := scanQSO(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Worked is one row of the compact worked view: one per call, band and mode
// class.
type Worked struct {
	Call      string    `json:"call"`
	Band      string    `json:"band"`
	ModeClass string    `json:"mode_class"`
	Mode      string    `json:"mode"` // of the latest QSO
	At        time.Time `json:"at"`   // of the latest QSO
	Confirmed bool      `json:"confirmed"`
	DXCC      int       `json:"dxcc,omitempty"`
	State     string    `json:"state,omitempty"`
	SOTARef   string    `json:"sota_ref,omitempty"`
	WWFFRef   string    `json:"wwff_ref,omitempty"`
}

// WorkedView aggregates the log per call, band and mode class (confirmed is
// OR-ed; mode and time are the latest QSO's; entity, state and references
// the latest non-empty ones). own "" takes every own call. With a non-zero
// since only groups with a QSO added or changed at or after since are
// returned, still aggregated over the whole group, so a client can refresh
// its view from a delta.
func (s *Store) WorkedView(own string, since time.Time) ([]Worked, error) {
	q := `SELECT ` + qsoCols + ` FROM qsos`
	var args []any
	if own != "" {
		q += ` WHERE own_call=?`
		args = append(args, own)
	}
	rows, err := s.query(q+` ORDER BY at_utc, id`, args...)
	if err != nil {
		return nil, err
	}
	type key struct{ call, band, class string }
	idx := map[key]int{}
	var out []Worked
	changed := map[key]bool{}
	for _, r := range rows {
		k := key{r.Call, r.Band, r.ModeClass}
		i, ok := idx[k]
		if !ok {
			idx[k] = len(out)
			i = len(out)
			out = append(out, Worked{Call: r.Call, Band: r.Band, ModeClass: r.ModeClass})
		}
		w := &out[i]
		w.Mode, w.At = r.Mode, r.At
		w.Confirmed = w.Confirmed || r.Confirmed
		if r.DXCC != 0 {
			w.DXCC = r.DXCC
		}
		if r.State != "" {
			w.State = r.State
		}
		if r.SOTARef != "" {
			w.SOTARef = r.SOTARef
		}
		if r.WWFFRef != "" {
			w.WWFFRef = r.WWFFRef
		}
		if !since.IsZero() && !r.Updated.Before(since) {
			changed[k] = true
		}
	}
	if since.IsZero() {
		return out, nil
	}
	kept := out[:0]
	for _, w := range out {
		if changed[key{w.Call, w.Band, w.ModeClass}] {
			kept = append(kept, w)
		}
	}
	return kept, nil
}

// Stats summarises the log.
type Stats struct {
	QSOCount       int            `json:"qso_count"`
	ConfirmedCount int            `json:"confirmed_count"`
	OwnCalls       []string       `json:"own_calls"`
	Sources        map[string]int `json:"sources"`
	FirstAt        *time.Time     `json:"first_at,omitempty"`
	LastAt         *time.Time     `json:"last_at,omitempty"`
}

// Stats reads the summary.
func (s *Store) Stats() (Stats, error) {
	st := Stats{Sources: map[string]int{}, OwnCalls: []string{}}
	var first, last sql.NullString
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(confirmed),0), MIN(at_utc), MAX(at_utc) FROM qsos`).
		Scan(&st.QSOCount, &st.ConfirmedCount, &first, &last); err != nil {
		return st, err
	}
	if first.Valid {
		f, l := parseTS(first.String), parseTS(last.String)
		st.FirstAt, st.LastAt = &f, &l
	}
	var err error
	if st.OwnCalls, err = s.OwnCalls(); err != nil {
		return st, err
	}
	rows, err := s.db.Query(`SELECT source, COUNT(*) FROM qsos GROUP BY source`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var src string
		var n int
		if err := rows.Scan(&src, &n); err != nil {
			return st, err
		}
		st.Sources[src] = n
	}
	return st, rows.Err()
}

// OwnCalls lists the distinct own callsigns of the log, sorted.
func (s *Store) OwnCalls() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT own_call FROM qsos ORDER BY own_call`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Count is the number of QSOs.
func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM qsos`).Scan(&n)
	return n, err
}
