// Package cty looks callsigns up in a cty.dat country file (the
// country-files.com format used by contest loggers): which DXCC entity a
// callsign belongs to, and its CQ and ITU zone and continent. The CQ finder
// uses it to reject callsign readings with an impossible prefix.
//
// bmrec does not ship the file. The owner places a cty.dat (Big CTY is
// recommended) in the storage directory and Open loads it; without a file
// Open returns a nil *DB, and a nil *DB accepts every callsign.
package cty

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Entity is a DXCC entity (or a WAE/CQ-only entity) with the zones that
// apply to the looked-up callsign: per-callsign overrides in the file win
// over the entity defaults.
type Entity struct {
	Name      string
	Primary   string // primary prefix, without the WAE marker '*'
	CQZone    int
	ITUZone   int
	Continent string // AF, AN, AS, EU, NA, OC or SA
	// WAE marks an entity that only the WAE/CQ country lists count, such as
	// Sicily or European Turkey (a '*' in front of the primary prefix).
	WAE bool
	// ADIF is the DXCC entity number. It is set only by DXCC, and only when
	// the DB was given the numbers of a cty.csv (LoadNumbers).
	ADIF int
}

// DB is a parsed cty.dat. It is read-only after Parse and safe for
// concurrent use.
type DB struct {
	exact    map[string]*Entity // "=CALL" entries, matched only whole
	prefixes map[string]*Entity
	maxPref  int // longest prefix key, bounds the longest-prefix scan

	byPrimary map[string]*Entity // entity header defaults by primary prefix
	numbers   map[string]int     // ADIF entity number by primary prefix, from cty.csv
}

// Open loads <dir>/cty.dat. A missing file is not an error: it returns
// (nil, nil) and callers fall back to shape-only validation.
func Open(dir string) (*DB, error) {
	db, err := LoadFile(filepath.Join(dir, "cty.dat"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return db, err
}

// LoadFile parses the cty.dat at path.
func LoadFile(path string) (*DB, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	db, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("cty: %s: %w", path, err)
	}
	return db, nil
}

// Parse reads the cty.dat format: per entity a header line of ':'
// delimited fields (name, CQ zone, ITU zone, continent, latitude,
// longitude, UTC offset, primary prefix), then alias lines of comma
// separated prefixes that end with ';'. When a key appears twice the first
// occurrence wins.
func Parse(r io.Reader) (*DB, error) {
	db := &DB{exact: map[string]*Entity{}, prefixes: map[string]*Entity{}, byPrimary: map[string]*Entity{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var cur *Entity
	var aliases strings.Builder
	entities, line := 0, 0
	finish := func() {
		if cur != nil {
			db.addAliases(cur, aliases.String())
		}
		cur = nil
		aliases.Reset()
	}
	for sc.Scan() {
		line++
		text := strings.TrimRight(sc.Text(), "\r \t")
		if line == 1 {
			text = strings.TrimPrefix(text, "\ufeff")
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if text[0] != ' ' && text[0] != '\t' && strings.Count(text, ":") >= 7 {
			// a header line starts an entity; an entity without its closing
			// ';' ends here
			finish()
			e, err := parseHeader(text)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			cur = e
			if _, dup := db.byPrimary[e.Primary]; !dup {
				db.byPrimary[e.Primary] = e
			}
			entities++
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d: alias line before any entity header", line)
		}
		aliases.WriteString(strings.TrimSpace(text))
		if strings.HasSuffix(text, ";") {
			finish()
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	finish()
	if entities == 0 {
		return nil, errors.New("no entities found")
	}
	return db, nil
}

func parseHeader(line string) (*Entity, error) {
	f := strings.Split(line, ":")
	if len(f) < 8 {
		return nil, fmt.Errorf("entity header has %d fields, want 8", len(f))
	}
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	cq, err := strconv.Atoi(f[1])
	if err != nil {
		return nil, fmt.Errorf("entity %q: bad CQ zone %q", f[0], f[1])
	}
	itu, err := strconv.Atoi(f[2])
	if err != nil {
		return nil, fmt.Errorf("entity %q: bad ITU zone %q", f[0], f[2])
	}
	return &Entity{
		Name: f[0], CQZone: cq, ITUZone: itu,
		Continent: strings.ToUpper(f[3]),
		Primary:   strings.ToUpper(strings.TrimPrefix(f[7], "*")),
		WAE:       strings.HasPrefix(f[7], "*"),
	}, nil
}

// addAliases registers the alias list "A,B,=C(13)[16]<..>{EU}~1~;" of e.
func (db *DB) addAliases(e *Entity, list string) {
	list = strings.TrimSuffix(strings.TrimSpace(list), ";")
	for _, tok := range strings.Split(list, ",") {
		tok = strings.TrimSpace(tok)
		exact := strings.HasPrefix(tok, "=")
		tok = strings.TrimPrefix(tok, "=")
		key := tok
		if i := strings.IndexAny(tok, "([<{~"); i >= 0 {
			key = tok[:i]
		}
		key = strings.ToUpper(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		ent := e
		if len(key) < len(tok) {
			ent = withOverrides(e, tok[len(key):])
		}
		if exact {
			if _, dup := db.exact[key]; !dup {
				db.exact[key] = ent
			}
			continue
		}
		if _, dup := db.prefixes[key]; !dup {
			db.prefixes[key] = ent
			if len(key) > db.maxPref {
				db.maxPref = len(key)
			}
		}
	}
}

// withOverrides applies the (CQ zone), [ITU zone] and {continent} markers
// of an alias to a copy of e. Latitude/longitude <..> and UTC offset ~..~
// markers are skipped.
func withOverrides(e *Entity, marks string) *Entity {
	c := *e
	for len(marks) > 0 {
		var closer byte
		switch marks[0] {
		case '(':
			closer = ')'
		case '[':
			closer = ']'
		case '<':
			closer = '>'
		case '{':
			closer = '}'
		case '~':
			closer = '~'
		default:
			return &c // not a marker; ignore the rest
		}
		end := strings.IndexByte(marks[1:], closer)
		if end < 0 {
			return &c
		}
		val := strings.TrimSpace(marks[1 : 1+end])
		switch marks[0] {
		case '(':
			if n, err := strconv.Atoi(val); err == nil {
				c.CQZone = n
			}
		case '[':
			if n, err := strconv.Atoi(val); err == nil {
				c.ITUZone = n
			}
		case '{':
			if val != "" {
				c.Continent = strings.ToUpper(val)
			}
		}
		marks = marks[end+2:]
	}
	return &c
}

// ssid is an SSID suffix as the DX cluster appends it: DL1ABC-7, DL1ABC-#.
var ssid = regexp.MustCompile(`-(?:[0-9]{1,2}|#)$`)

// Modifier suffixes that say nothing about the country. A single digit
// (W1ABC/4) only changes the call area. /MM and /AM are handled apart: they
// have no country.
var modifiers = map[string]bool{"P": true, "M": true, "QRP": true, "A": true, "LH": true, "B": true}

// Lookup finds the entity of a callsign.
//
// Order: the whole string as an exact entry; the string without an SSID
// suffix (-7); slash rules; longest prefix. Slash rules follow community
// practice, since cty.dat defines none: the modifiers /P /M /QRP /A /LH /B
// and a single digit are dropped, /MM and /AM have no country (false), and
// for a prefix/suffix form such as OE/DL1ABC the shorter part is the
// prefix, falling back to the call part. A callsign without a digit is not
// matched by prefix, so words like "HELLO" are no callsigns even when they
// start with a valid prefix. Exact-only "=CALL" entries never match a
// truncated string.
func (db *DB) Lookup(call string) (Entity, bool) {
	if db == nil {
		return Entity{}, false
	}
	s := strings.ToUpper(strings.TrimSpace(call))
	if s == "" {
		return Entity{}, false
	}
	if e, ok := db.exact[s]; ok {
		return *e, true
	}
	if loc := ssid.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
		if e, ok := db.exact[s]; ok {
			return *e, true
		}
	}
	parts := strings.Split(s, "/")
	if len(parts) == 1 {
		return db.byCall(s)
	}
	kept := parts[:1]
	for _, p := range parts[1:] {
		switch {
		case p == "MM" || p == "AM":
			return Entity{}, false
		case modifiers[p], len(p) == 1 && p[0] >= '0' && p[0] <= '9':
			// dropped
		default:
			kept = append(kept, p)
		}
	}
	if len(kept) == 1 {
		return db.byCall(kept[0])
	}
	// prefix/suffix form: the shortest part is the prefix, the longest the call
	short, long := kept[0], kept[0]
	for _, p := range kept[1:] {
		if len(p) < len(short) {
			short = p
		}
		if len(p) > len(long) {
			long = p
		}
	}
	if short == long { // equal lengths: first is the prefix, last the call
		long = kept[len(kept)-1]
	}
	if e, ok := db.exact[short]; ok {
		return *e, true
	}
	if e, ok := db.longestPrefix(short); ok {
		return e, true
	}
	return db.byCall(long)
}

// byCall matches one callsign without slashes: exact, then longest prefix.
func (db *DB) byCall(s string) (Entity, bool) {
	if e, ok := db.exact[s]; ok {
		return *e, true
	}
	if !strings.ContainsAny(s, "0123456789") {
		return Entity{}, false
	}
	return db.longestPrefix(s)
}

func (db *DB) longestPrefix(s string) (Entity, bool) {
	n := len(s)
	if n > db.maxPref {
		n = db.maxPref
	}
	for ; n > 0; n-- {
		if e, ok := db.prefixes[s[:n]]; ok {
			return *e, true
		}
	}
	return Entity{}, false
}

// Valid reports whether call has a known country prefix. A nil DB has no
// data and no judgement: it returns true.
func (db *DB) Valid(call string) bool {
	if db == nil {
		return true
	}
	_, ok := db.Lookup(call)
	return ok
}
