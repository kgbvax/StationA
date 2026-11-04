package award

import (
	"slices"
	"strconv"
	"strings"
)

// Entry is one row of the station log as the evaluator needs it: a call
// worked on a band in a mode class (station-log's /v1/worked row).
type Entry struct {
	Call      string // normalized; may be "" for a legacy confirmation
	DXCC      int    // ADIF entity number, 0 = unknown
	State     string // upper case, "" = unknown
	Band      string
	ModeClass string // "" = unknown (a legacy confirmation without mode)
	Confirmed bool
}

// Log is the history the evaluator reads: station-log's entries and, for
// DXCC, Club Log's confirmation matrix.
type Log struct {
	// Loaded: the station-log view has been loaded at least once.
	Loaded  bool
	Entries []Entry
	// ClubLoaded: Club Log's matrix has been loaded at least once.
	ClubLoaded bool
	// Club maps ADIF -> bands confirmed (any mode).
	Club map[int]map[string]bool
}

type slotKey struct{ key, band, mode string }

// Index is an award's slots in one Log, built once per log change.
type Index struct {
	def         Definition
	loaded      bool
	slots       map[slotKey]Status
	everConfirm map[string]bool   // key confirmed in any slice
	everWorked  map[string]bool   // key worked (confirmed or not) in any slice
	callState   map[string]string // normalized call -> state, from the log
}

// Build indexes the log for the award.
func (d Definition) Build(l *Log) *Index {
	ix := &Index{def: d, slots: map[slotKey]Status{}, everConfirm: map[string]bool{}, everWorked: map[string]bool{}, callState: map[string]string{}}
	switch d.Kind {
	case KindDXCC:
		ix.loaded = l.Loaded || l.ClubLoaded
	default:
		ix.loaded = l.Loaded
	}
	anyMode := len(d.Modes) == 0 && !d.PerMode
	for _, e := range l.Entries {
		if e.Call != "" && e.State != "" {
			ix.callState[e.Call] = e.State
		}
		if !d.inBands(e.Band) {
			continue
		}
		if !anyMode {
			if e.ModeClass == "" || (len(d.Modes) > 0 && !slices.Contains(d.Modes, e.ModeClass)) {
				continue
			}
		}
		key, ok := d.entryKey(e)
		if !ok {
			continue
		}
		ix.note(key, e.Band, e.ModeClass, e.Confirmed)
	}
	if d.Kind == KindDXCC && anyMode {
		for adif, bands := range l.Club {
			for b, ok := range bands {
				if ok && d.inBands(b) {
					ix.note(strconv.Itoa(adif), b, "", true)
				}
			}
		}
	}
	return ix
}

func (ix *Index) note(key, band, class string, confirmed bool) {
	k := slotKey{key: key}
	if ix.def.perBand() {
		k.band = band
	}
	if ix.def.PerMode {
		k.mode = class
	}
	ix.everWorked[key] = true
	st := WorkedUnconfirmed
	if confirmed {
		st = Confirmed
		ix.everConfirm[key] = true
	}
	if ix.slots[k] != Confirmed {
		ix.slots[k] = st
	}
}

func (d Definition) inBands(b string) bool { return len(d.Bands) == 0 || slices.Contains(d.Bands, b) }

// entryKey is the slot key a log entry counts for.
func (d Definition) entryKey(e Entry) (string, bool) {
	switch d.Kind {
	case KindDXCC:
		return strconv.Itoa(e.DXCC), e.DXCC > 0
	case KindField:
		// an entry of unknown entity is let through: the log carried the state itself
		if e.State == "" || (e.DXCC != 0 && !d.allowsEntity(e.DXCC)) || !d.allowsItem(e.State) {
			return "", false
		}
		return e.State, true
	}
	return "", false
}

// allowsEntity: a field award applies to the listed entities only.
func (d Definition) allowsEntity(adif int) bool {
	return len(d.DXCC) == 0 || slices.Contains(d.DXCC, adif)
}

func (d Definition) allowsItem(k string) bool {
	return len(d.Items) == 0 || slices.Contains(d.Items, k)
}

// Definition is the award the index was built for.
func (ix *Index) Definition() Definition { return ix.def }

// StateOf is the state the log holds for a call (normalized), "" when none.
func (ix *Index) StateOf(call string) string { return ix.callState[call] }

// Input is a candidate station.
type Input struct {
	Call string // normalized
	Band string
	Mode string // canonical (cw, usb, ..., data), "" = unknown
	// ADIF is the DXCC entity number; 0 = not resolved (cty missing).
	ADIF int
	// Entity is the entity's label for the wire (its primary prefix).
	Entity string
	// State is the station's state from the callbook; "" = unknown.
	State string
}

// Hit is the state of the one slot a candidate would fill in an award.
type Hit struct {
	Award string
	// Key is the slot's key: the entity's prefix, a state, a multiplier. ""
	// when Status is Unknown (for a DXCC award: the entity is not resolved).
	Key    string
	Band   string // "" when the award counts over all bands
	Mode   string // mode class when the award slices or filters by it
	Status Status
	// Needed: the slot is not yet satisfied under the award's rule.
	Needed bool
	// Fresh: the key is confirmed in no slice at all (a first for the award).
	Fresh bool
	// ATNO: the key is worked in no slice at all, not even unconfirmed (an
	// all-time new one for the award, not just a new band or mode).
	ATNO bool
}

// satisfied reports whether the status meets the rule.
func satisfied(r Rule, s Status) bool {
	if r == RuleWorked {
		return s == WorkedUnconfirmed || s == Confirmed
	}
	return s == Confirmed
}

// Evaluate values the candidate for the award: ok false when the award does
// not apply to it (a band or mode outside the award, an entity or key it does
// not cover). A slot that cannot be told gives a Hit with Status Unknown.
func (ix *Index) Evaluate(in Input) (Hit, bool) {
	d := ix.def
	if !d.inBands(in.Band) {
		return Hit{}, false
	}
	unknown := Hit{Award: d.Code, Status: Unknown}
	if d.perBand() {
		unknown.Band = in.Band
	}
	class := ClassOf(in.Mode)
	sliced := len(d.Modes) > 0 || d.PerMode
	if sliced {
		if class != "" && len(d.Modes) > 0 && !slices.Contains(d.Modes, class) {
			return Hit{}, false
		}
		unknown.Mode = class
	}
	var key, label string
	switch d.Kind {
	case KindDXCC:
		if !ix.loaded || in.ADIF == 0 {
			return unknown, true
		}
		key, label = strconv.Itoa(in.ADIF), in.Entity
		if label == "" {
			label = key
		}
	case KindField:
		if !ix.loaded || in.ADIF == 0 {
			return unknown, true
		}
		if !d.allowsEntity(in.ADIF) {
			return Hit{}, false
		}
		st := strings.ToUpper(strings.TrimSpace(in.State))
		if st == "" {
			st = ix.callState[in.Call]
		}
		if st == "" {
			return unknown, true
		}
		if !d.allowsItem(st) {
			return Hit{}, false
		}
		key, label = st, st
	default:
		return Hit{}, false
	}
	if sliced && class == "" {
		unknown.Key = label
		return unknown, true
	}
	sk := slotKey{key: key}
	if d.perBand() {
		sk.band = in.Band
	}
	if d.PerMode {
		sk.mode = class
	}
	st, ok := ix.slots[sk]
	if !ok {
		st = Unworked
	}
	h := Hit{Award: d.Code, Key: label, Status: st}
	if d.perBand() {
		h.Band = in.Band
	}
	if sliced {
		h.Mode = class
	}
	h.Needed = !satisfied(d.Rule, st)
	h.Fresh = h.Needed && !ix.everConfirm[key]
	h.ATNO = h.Needed && !ix.everWorked[key]
	return h, true
}

// NeedsState reports whether evaluating the award wants the candidate's
// state (so the caller looks it up only when the log does not hold it).
func (d Definition) NeedsState() bool { return d.Kind == KindField }

// AppliesToEntity reports whether a field award covers the entity.
func (d Definition) AppliesToEntity(adif int) bool { return d.allowsEntity(adif) }
