// Package contest models a DXLog.net contest well enough to rank the
// stations the CQ finder hears: it reads the contest definition and the
// prefill database, predicts what a callsign would be worth as a multiplier,
// derives the multipliers the log already holds, and orders the queue. It
// does no I/O of its own beyond the readers it is handed.
package contest

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// SlotKind is what a multiplier slot counts, as far as bmrec can evaluate it.
type SlotKind string

const (
	KindDXCC   SlotKind = "DXCC"
	KindCQZone SlotKind = "CQZONE"
	KindITU    SlotKind = "ITUZONE"
	KindWPX    SlotKind = "WPX"
	KindCustom SlotKind = "CUSTOM" // a name from the received exchange, checked against the definition's list
)

// Scope is how a slot counts: once per band, once per mode (always SSB
// here), once per band and mode, or once in the whole contest.
type Scope string

const (
	ScopeBand  Scope = "band"
	ScopeAll   Scope = "all"
	ScopeMode  Scope = "mode"
	ScopeBandM Scope = "band+mode"
)

// Exception swaps what a slot evaluates for callsigns whose DXCC entity
// matches Dest: MULTn_EXCEPTION=DEST->DXCC:^G$|^EI$;CUSTOM_MULT_LIST.
type Exception struct {
	Negate bool
	Dest   *regexp.Regexp // matched against the entity's primary prefix
	// Def is the replacement: "NONE" (the slot does not apply),
	// "CUSTOM_MULT_LIST" or "WPX".
	Def string
}

// Slot is one of the up to three multiplier slots of a contest.
type Slot struct {
	Index      int // 1..3
	Kind       SlotKind
	Field      string // MULTn_FIELD as written
	Scope      Scope
	Exceptions []Exception
	// Unsupported says why bmrec cannot evaluate the slot (an FX expression,
	// an unknown type); every call is then unknown for it.
	Unsupported string
}

// DBFile is a prefill database a definition names.
type DBFile struct {
	Name    string
	RCVDCol int // 1-based column among the info columns, 0 = not given
}

// Definition is what bmrec reads of a DXLog contest definition file.
type Definition struct {
	Name      string // CONTESTNAME
	DoubleQSO string // DOUBLE_QSO, "" = per band
	Slots     []Slot
	DBFiles   []DBFile
	// List maps every name and alias of the custom multiplier list to its
	// canonical name (the head of "NK{NK;NR}=Norwich" is NK).
	List map[string]string
	// CountsWAE reports whether the contest counts the WAE country list
	// (CQ WW), so Sicily or European Turkey are multipliers of their own.
	CountsWAE bool
	// Notes carries what the parser skipped, for the status line.
	Notes []string
}

var (
	exceptionRe = regexp.MustCompile(`^(!?)DEST->DXCC:(.+)$`)
	keyedRe     = regexp.MustCompile(`^MULT([1-3])_(TYPE|FIELD|COUNT|EXCEPTION)$`)
	dbKeyRe     = regexp.MustCompile(`^DB_FILE([1-5])?$`)
	dbColRe     = regexp.MustCompile(`^DB_FILE([1-5])?_COL_RCVD$`)
)

// ParseDefinition reads a DXLog contest definition: KEY=VALUE lines, a
// custom multiplier list between [MULTIPLIERS START] and [MULTIPLIERS END].
// Keys bmrec does not use are ignored.
func ParseDefinition(r io.Reader) (*Definition, error) {
	d := &Definition{List: map[string]string{}}
	slots := map[int]*Slot{}
	slot := func(n int) *Slot {
		if slots[n] == nil {
			slots[n] = &Slot{Index: n, Scope: ScopeBand}
		}
		return slots[n]
	}
	dbs := map[int]*DBFile{}
	db := func(n int) *DBFile {
		if dbs[n] == nil {
			dbs[n] = &DBFile{}
		}
		return dbs[n]
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	inList, seen := false, false
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "[MULTIPLIERS START"):
			inList = true
			continue
		case strings.HasPrefix(up, "[MULTIPLIERS END"):
			inList = false
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if inList {
			d.addListEntry(strings.TrimSpace(key))
			continue
		}
		key, val = strings.ToUpper(strings.TrimSpace(key)), strings.TrimSpace(val)
		seen = true
		switch {
		case key == "CONTESTNAME":
			d.Name = val
		case key == "DOUBLE_QSO":
			d.DoubleQSO = strings.ToUpper(val)
		case dbKeyRe.MatchString(key):
			db(dbIndex(dbKeyRe.FindStringSubmatch(key)[1])).Name = val
		case dbColRe.MatchString(key):
			if n, err := strconv.Atoi(val); err == nil {
				db(dbIndex(dbColRe.FindStringSubmatch(key)[1])).RCVDCol = n
			}
		case keyedRe.MatchString(key):
			m := keyedRe.FindStringSubmatch(key)
			n, _ := strconv.Atoi(m[1])
			s := slot(n)
			switch m[2] {
			case "TYPE":
				s.setType(val)
			case "FIELD":
				s.Field = strings.ToUpper(val)
			case "COUNT":
				s.setScope(val)
			case "EXCEPTION":
				s.addException(val)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !seen {
		return nil, fmt.Errorf("contest definition: no KEY=VALUE lines found")
	}
	for n := 1; n <= 3; n++ {
		if s, ok := slots[n]; ok {
			if s.Kind == "" && s.Unsupported == "" {
				s.Unsupported = "no type"
			}
			d.Slots = append(d.Slots, *s)
		}
	}
	for n := 0; n <= 5; n++ {
		if f, ok := dbs[n]; ok && f.Name != "" {
			d.DBFiles = append(d.DBFiles, *f)
		}
	}
	if len(d.Slots) == 0 {
		d.Notes = append(d.Notes, "the definition has no multiplier slots")
	}
	norm := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToUpper(d.Name))
	d.CountsWAE = strings.Contains(norm, "CQWW")
	return d, nil
}

func dbIndex(s string) int {
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

func (d *Definition) addListEntry(key string) {
	head := key
	var aliases []string
	if i := strings.IndexByte(key, '{'); i >= 0 && strings.HasSuffix(key, "}") {
		head = strings.TrimSpace(key[:i])
		for _, a := range strings.Split(key[i+1:len(key)-1], ";") {
			aliases = append(aliases, strings.TrimSpace(a))
		}
	}
	head = strings.ToUpper(head)
	if head == "" {
		return
	}
	d.List[head] = head
	for _, a := range aliases {
		if a != "" {
			d.List[strings.ToUpper(a)] = head
		}
	}
}

func (s *Slot) setType(v string) {
	switch strings.ToUpper(v) {
	case "DXCC":
		s.Kind = KindDXCC
	case "CQZONE":
		s.Kind = KindCQZone
	case "ITUZONE":
		s.Kind = KindITU
	case "WPX":
		s.Kind = KindWPX
	case "CUSTOM":
		s.Kind = KindCustom
	default:
		s.Kind = ""
		s.Unsupported = "type " + strings.ToUpper(v)
	}
}

func (s *Slot) setScope(v string) {
	switch strings.ToUpper(v) {
	case "PER_BAND":
		s.Scope = ScopeBand
	case "ALL":
		s.Scope = ScopeAll
	case "PER_MODE":
		s.Scope = ScopeMode
	case "PER_BAND_MODE":
		s.Scope = ScopeBandM
	default:
		if s.Unsupported == "" {
			s.Unsupported = "count " + strings.ToUpper(v)
		}
	}
}

func (s *Slot) addException(v string) {
	cond, def, ok := strings.Cut(v, ";")
	if !ok {
		if s.Unsupported == "" {
			s.Unsupported = "exception " + v
		}
		return
	}
	m := exceptionRe.FindStringSubmatch(strings.TrimSpace(cond))
	def = strings.ToUpper(strings.TrimSpace(def))
	if m == nil || (def != "NONE" && def != "CUSTOM_MULT_LIST" && def != "WPX") {
		if s.Unsupported == "" {
			s.Unsupported = "exception " + v
		}
		return
	}
	re, err := regexp.Compile("(?i)" + m[2])
	if err != nil {
		if s.Unsupported == "" {
			s.Unsupported = "exception " + v
		}
		return
	}
	s.Exceptions = append(s.Exceptions, Exception{Negate: m[1] == "!", Dest: re, Def: def})
}

// UnsupportedSlots lists the slots bmrec cannot evaluate, for the status
// line: "slot 2: type FX".
func (d *Definition) UnsupportedSlots() []string {
	var out []string
	for _, s := range d.Slots {
		if s.Unsupported != "" {
			out = append(out, fmt.Sprintf("slot %d: %s", s.Index, s.Unsupported))
		}
	}
	return out
}
