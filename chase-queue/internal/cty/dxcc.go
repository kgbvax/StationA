package cty

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// waeParent maps the WAE-only entities to the DXCC entity they belong to.
// IG9 (African Italy) is a DXCC entity of its own and is not listed.
var waeParent = map[string]string{
	"IT9":  "I",
	"GM/S": "GM",
	"JW/B": "JW",
	"TA1":  "TA",
	"4U1V": "OE",
}

// OpenContest loads <dir>/cty.dat for the contest model, with the DXCC
// entity numbers of <dir>/cty.csv when that file is present. It returns a
// DB of its own, apart from the one Open gives the CQ finder, so the finder
// never depends on cty.csv. A missing cty.dat gives (nil, nil).
func OpenContest(dir string) (*DB, error) {
	db, err := Open(dir)
	if db == nil || err != nil {
		return nil, err
	}
	if err := db.LoadNumbers(filepath.Join(dir, "cty.csv")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return db, nil
}

// LoadNumbers reads the DXCC entity numbers from an AD1C cty.csv (primary
// prefix, name, ADIF number, ...). Call it before the DB is shared.
func (db *DB) LoadNumbers(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	nums := map[string]int{}
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("cty: %s: %w", path, err)
		}
		if len(rec) < 3 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(rec[2]))
		if err != nil || n <= 0 {
			continue
		}
		key := strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(rec[0]), "*"))
		if _, dup := nums[key]; !dup {
			nums[key] = n
		}
	}
	if len(nums) == 0 {
		return fmt.Errorf("cty: %s: no entity numbers found", path)
	}
	db.numbers = nums
	return nil
}

// DXCC finds the DXCC entity of a callsign: Lookup, with a WAE-only entity
// folded into its parent DXCC entity, and the ADIF number filled in when
// the DB has numbers. The zones are those of the parent entity's defaults.
func (db *DB) DXCC(call string) (Entity, bool) {
	e, ok := db.Lookup(call)
	if !ok {
		return Entity{}, false
	}
	if e.WAE {
		if p, found := db.byPrimary[waeParent[e.Primary]]; found {
			e = *p
		}
	}
	e.ADIF = db.numbers[e.Primary]
	return e, true
}
