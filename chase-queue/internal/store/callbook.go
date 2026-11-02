package store

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"chase-queue/internal/band"
	"chase-queue/internal/bus"
)

// CallbookEntry is one cached callbook lookup. Data is the published
// bus.CallInfo JSON; Found is false for a call the callbook does not have.
type CallbookEntry struct {
	Call    string
	Found   bool
	Data    string
	Fetched time.Time
}

// Callbook returns the cached lookup of call.
func (s *Store) Callbook(call string) (CallbookEntry, bool, error) {
	var e CallbookEntry
	var found int
	var at string
	err := s.db.QueryRow(`SELECT call, found, data, fetched_utc FROM callbook WHERE call=?`, call).Scan(&e.Call, &found, &e.Data, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return CallbookEntry{}, false, nil
	}
	if err != nil {
		return CallbookEntry{}, false, err
	}
	e.Found, e.Fetched = found != 0, parseDXTime(at)
	return e, true, nil
}

// PutCallbook stores a lookup result, replacing an older one.
func (s *Store) PutCallbook(e CallbookEntry) error {
	_, err := s.db.Exec(`INSERT INTO callbook (call, found, data, fetched_utc) VALUES (?, ?, ?, ?)
		ON CONFLICT(call) DO UPDATE SET found=excluded.found, data=excluded.data, fetched_utc=excluded.fetched_utc`,
		e.Call, boolInt(e.Found), e.Data, dxTime(e.Fetched))
	return err
}

// PruneCallbook deletes lookups fetched before cutoff.
func (s *Store) PruneCallbook(cutoff time.Time) error {
	_, err := s.db.Exec(`DELETE FROM callbook WHERE fetched_utc < ?`, dxTime(cutoff))
	return err
}

// WorkedBefore summarises, per call, the QSOs of the station log (every
// source) and of every stored contest instance (invalid QSOs left out). A
// QSO reported by two sources counts once: QSOs are told apart by band and
// minute. ThisBand is left for the caller. Calls absent from the result were
// never worked.
func (s *Store) WorkedBefore(calls []string) (map[string]*bus.WorkedBefore, error) {
	out := map[string]*bus.WorkedBefore{}
	if len(calls) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(calls)), ",")
	args := make([]any, 0, 2*len(calls))
	for _, c := range calls {
		args = append(args, c)
	}
	args = append(args, args...)
	rows, err := s.db.Query(`SELECT call, band, mode, at_utc FROM log_qsos WHERE call IN (`+ph+`)
		UNION ALL SELECT call, band, mode, at_utc FROM contest_qsos WHERE invalid=0 AND call IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var call, b, mode, at string
		if err := rows.Scan(&call, &b, &mode, &at); err != nil {
			return nil, err
		}
		t := parseDXTime(at)
		k := call + "|" + b + "|" + t.Truncate(time.Minute).Format(time.RFC3339)
		if seen[k] {
			continue
		}
		seen[k] = true
		w := out[call]
		if w == nil {
			w = &bus.WorkedBefore{}
			out[call] = w
		}
		w.QSOs++
		if b != "" && !slices.Contains(w.Bands, b) {
			w.Bands = append(w.Bands, b)
		}
		if m := strings.ToUpper(strings.TrimSpace(mode)); m != "" && !slices.Contains(w.Modes, m) {
			w.Modes = append(w.Modes, m)
		}
		if t.After(w.Last) {
			w.Last = t
		}
	}
	for _, w := range out {
		slices.SortFunc(w.Bands, func(a, b string) int { return bandIndex(a) - bandIndex(b) })
		slices.Sort(w.Modes)
	}
	return out, rows.Err()
}

func bandIndex(name string) int {
	for i, b := range band.Bands {
		if b.Name == name {
			return i
		}
	}
	return len(band.Bands)
}
