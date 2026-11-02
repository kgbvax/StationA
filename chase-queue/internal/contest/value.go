package contest

import (
	"fmt"
	"strconv"
	"strings"

	"chase-queue/internal/cty"
)

// State is what a slot says about a callsign.
type State int

const (
	StateNew     State = iota // the name is not held in the slot's scope yet
	StateHeld                 // already held
	StateUnknown              // bmrec cannot tell
	StateNA                   // the slot does not apply to this callsign
)

// SlotValue is the prediction for one slot.
type SlotValue struct {
	Slot  int
	Kind  SlotKind
	Name  string // the predicted multiplier name; "" when unknown or not applicable
	State State
}

// Class is the overall verdict on a callsign.
type Class int

const (
	ClassNone    Class = iota // every predicted name is held
	ClassNew                  // at least one slot would give a new multiplier
	ClassUnknown              // no new slot, and at least one slot cannot be told
)

// Valuation is the prediction for one callsign on one band.
type Valuation struct {
	Slots    []SlotValue
	Class    Class
	NewSlots int
	// NewDXCC is the entity of a new DXCC-kind slot; the queue ranks it by
	// Club Log rarity. Nil when no DXCC slot is new.
	NewDXCC *cty.Entity
}

// Model values callsigns for one contest. CTY and Prefill may be nil: every
// slot that needs the missing data then reads as unknown.
type Model struct {
	Def     *Definition
	CTY     *cty.DB
	Prefill *Prefill
}

// QSOIn is a logged QSO as the model needs it.
type QSOIn struct {
	Call    string
	Band    string
	Mode    string
	Exch    [4]string
	Invalid bool
}

func isSSB(mode string) bool {
	switch strings.ToUpper(strings.TrimSpace(mode)) {
	case "SSB", "USB", "LSB", "PH", "PHONE":
		return true
	}
	return false
}

func (s Slot) scopeKey(band string) string {
	switch s.Scope {
	case ScopeAll:
		return ""
	case ScopeMode:
		return "SSB"
	case ScopeBandM:
		return band + "|SSB"
	}
	return band
}

// Held is the set of multiplier names the log holds, per slot and scope.
type Held struct {
	sets map[string]map[string]bool
}

func heldKey(slot int, scope string) string { return strconv.Itoa(slot) + "|" + scope }

func (h *Held) has(slot int, scope, name string) bool {
	return h != nil && h.sets[heldKey(slot, scope)][name]
}

func (h *Held) add(slot int, scope, name string) {
	k := heldKey(slot, scope)
	if h.sets[k] == nil {
		h.sets[k] = map[string]bool{}
	}
	h.sets[k][name] = true
}

// Size is the number of names held across all slots.
func (h *Held) Size() int {
	n := 0
	if h != nil {
		for _, s := range h.sets {
			n += len(s)
		}
	}
	return n
}

// entity is the DXCC entity of a call by the contest's own list: WAE-only
// entities stay separate only in a contest that counts the WAE list.
func (m *Model) entity(call string) (cty.Entity, bool) {
	if m.CTY == nil {
		return cty.Entity{}, false
	}
	if m.Def.CountsWAE {
		return m.CTY.Lookup(call)
	}
	return m.CTY.DXCC(call)
}

type nameResult struct {
	kind  SlotKind
	name  string
	state State // StateUnknown, StateNA or StateNew (a name was found)
}

// name works out the multiplier name a slot gives a callsign. exch is the
// received exchange of a logged QSO; the zero value means a heard station,
// whose exchange is predicted from the prefill.
func (m *Model) name(s Slot, call string, exch [4]string, logged bool) nameResult {
	unknown := nameResult{kind: s.Kind, state: StateUnknown}
	if s.Unsupported != "" {
		return unknown
	}
	kind := s.Kind
	if len(s.Exceptions) > 0 {
		ent, ok := m.entity(call)
		if !ok {
			return unknown
		}
		for _, ex := range s.Exceptions {
			if ex.Dest.MatchString(ent.Primary) == ex.Negate {
				continue
			}
			switch ex.Def {
			case "NONE":
				return nameResult{kind: kind, state: StateNA}
			case "CUSTOM_MULT_LIST":
				kind = KindCustom
			case "WPX":
				kind = KindWPX
			}
			break
		}
	}
	res := nameResult{kind: kind, state: StateNew}
	switch kind {
	case KindDXCC:
		ent, ok := m.entity(call)
		if !ok {
			return unknown
		}
		res.name = ent.Primary
	case KindWPX:
		p, ok := cty.WPX(call)
		if !ok {
			return unknown
		}
		res.name = p
	case KindCQZone, KindITU:
		limit := 40
		if kind == KindITU {
			limit = 90
		}
		fromExchange := strings.HasPrefix(s.Field, "RCVD") || strings.HasPrefix(s.Field, "RECINFO")
		if fromExchange {
			if n, ok := m.zoneFromExchange(call, exch, logged, limit); ok {
				res.name = strconv.Itoa(n)
				return res
			}
		}
		if m.CTY == nil {
			return unknown
		}
		ent, ok := m.CTY.Lookup(call)
		if !ok {
			return unknown
		}
		if kind == KindITU {
			res.name = strconv.Itoa(ent.ITUZone)
		} else {
			res.name = strconv.Itoa(ent.CQZone)
		}
	case KindCustom:
		name, ok := m.customName(call, exch, logged)
		if !ok {
			return unknown
		}
		res.name = name
	default:
		return unknown
	}
	return res
}

func (m *Model) zoneFromExchange(call string, exch [4]string, logged bool, limit int) (int, bool) {
	if logged {
		for _, e := range exch {
			if n, err := strconv.Atoi(strings.TrimSpace(e)); err == nil && n >= 1 && n <= limit {
				return n, true
			}
		}
		return 0, false
	}
	if v, ok := m.Prefill.Lookup(call); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= limit {
			return n, true
		}
	}
	return 0, false
}

// customName finds the canonical list name for a call: a heard station's
// prefill value, or the first received-exchange element of a logged QSO that
// is on the list. Without a list, the prefill value is taken as it is.
func (m *Model) customName(call string, exch [4]string, logged bool) (string, bool) {
	if logged {
		for _, e := range exch {
			if head, ok := m.Def.List[strings.ToUpper(strings.TrimSpace(e))]; ok {
				return head, true
			}
		}
		return "", false
	}
	v, ok := m.Prefill.Lookup(call)
	if !ok {
		return "", false
	}
	if len(m.Def.List) == 0 {
		return v, true
	}
	head, ok := m.Def.List[v]
	return head, ok
}

// HeldFrom derives the multipliers the log holds by valuing each logged
// QSO with the model's own rules. Invalid QSOs and QSOs in other modes than
// SSB add nothing.
func (m *Model) HeldFrom(qsos []QSOIn) *Held {
	h := &Held{sets: map[string]map[string]bool{}}
	for _, q := range qsos {
		if q.Invalid || !isSSB(q.Mode) {
			continue
		}
		for _, s := range m.Def.Slots {
			r := m.name(s, q.Call, q.Exch, true)
			if r.state == StateNew && r.name != "" {
				h.add(s.Index, s.scopeKey(q.Band), r.name)
			}
		}
	}
	return h
}

// Value predicts what working call on band would give.
func (m *Model) Value(call, band string, h *Held) Valuation {
	v := Valuation{}
	anyUnknown := false
	for _, s := range m.Def.Slots {
		r := m.name(s, call, [4]string{}, false)
		sv := SlotValue{Slot: s.Index, Kind: r.kind, Name: r.name, State: r.state}
		if r.state == StateNew {
			if h.has(s.Index, s.scopeKey(band), r.name) {
				sv.State = StateHeld
			} else {
				v.NewSlots++
				if r.kind == KindDXCC && v.NewDXCC == nil {
					if ent, ok := m.entity(call); ok {
						e := ent
						v.NewDXCC = &e
					}
				}
			}
		}
		if sv.State == StateUnknown {
			anyUnknown = true
		}
		v.Slots = append(v.Slots, sv)
	}
	switch {
	case v.NewSlots > 0:
		v.Class = ClassNew
	case anyUnknown:
		v.Class = ClassUnknown
	}
	return v
}

// Worked answers whether a call is already in the log under the contest's
// dupe rule.
type Worked struct {
	perBand bool
	modeSSB bool // the rule counts the mode, so only SSB QSOs make a dupe
	set     map[string]bool
}

// NewWorked builds the worked set from the logged QSOs. DOUBLE_QSO decides
// the scope: the per-band variants count a call once per band, PER_MODE
// and PER_PERIOD once in the contest; a rule that includes the mode only
// counts SSB QSOs. An empty or unknown rule counts once per band.
func (m *Model) NewWorked(qsos []QSOIn) *Worked {
	rule := m.Def.DoubleQSO
	w := &Worked{perBand: true, set: map[string]bool{}}
	switch rule {
	case "PER_MODE", "PER_PERIOD", "PER_PERIOD_MODE":
		w.perBand = false
	}
	w.modeSSB = strings.Contains(rule, "MODE")
	for _, q := range qsos {
		if q.Invalid || (w.modeSSB && !isSSB(q.Mode)) {
			continue
		}
		w.set[w.key(q.Call, q.Band)] = true
	}
	return w
}

func (w *Worked) key(call, band string) string {
	if w.perBand {
		return NormalizeCall(call) + "|" + band
	}
	return NormalizeCall(call)
}

// Has reports whether the call is worked on the band.
func (w *Worked) Has(call, band string) bool {
	return w != nil && w.set[w.key(call, band)]
}

// Describe renders a valuation for the status line or a card: "NK (new)".
func (v Valuation) Describe() string {
	var parts []string
	for _, s := range v.Slots {
		st := map[State]string{StateNew: "new", StateHeld: "held", StateUnknown: "?", StateNA: "-"}[s.State]
		parts = append(parts, fmt.Sprintf("%s %s", s.Name, st))
	}
	return strings.Join(parts, ", ")
}
