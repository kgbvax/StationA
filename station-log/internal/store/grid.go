package store

import (
	"database/sql"
	"errors"
	"strings"
)

// GridForCall returns the most precise (longest, then latest) locator any
// stored QSO with this exact call holds; "" when none. It backs the working
// station feed's position lookup before QRZ.
func (s *Store) GridForCall(call string) (string, error) {
	var g string
	err := s.db.QueryRow(`SELECT grid FROM qsos WHERE call = ? AND grid <> '' ORDER BY length(grid) DESC, at_utc DESC LIMIT 1`,
		strings.ToUpper(strings.TrimSpace(call))).Scan(&g)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return g, nil
}
