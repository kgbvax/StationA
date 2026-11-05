// Package engine ranks the stations bmrec hears calling CQ and the cluster
// spots for the active goals: each enabled award (DXCC per band, a field
// award such as WAS, the running contest's multipliers) says which slots a
// station would fill (package award). It hides stations
// spotted on the cluster (same station, same band, within 30 minutes),
// handled or skipped; one already worked on the band stays, below the
// unworked ones and not needed. It keeps the Skip, handled and Undo
// state every window shares. It does no I/O of its own beyond the Persister
// it is handed; one mutex guards it and is never held across I/O.
package engine

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"chase-queue/internal/award"
	"chase-queue/internal/bus"
	"chase-queue/internal/calls"
	"chase-queue/internal/contest"
	"chase-queue/internal/cty"
	"chase-queue/internal/mode"
	"chase-queue/internal/store"
)

// SpotHorizon is how long a human spot keeps its station off the list.
const SpotHorizon = 30 * time.Minute

const undoKeep = 20

var (
	// ErrNotListed: the action names a station that is no longer on the list
	// (another window acted first, or it was spotted or worked meanwhile).
	ErrNotListed = errors.New("the station is no longer on the list")
	// ErrNothingToUndo: the undo stack is empty.
	ErrNothingToUndo = errors.New("nothing to undo")
	// ErrNeedCall: marking a station handled needs a callsign.
	ErrNeedCall = errors.New("a callsign is needed")
	errBadCall  = errors.New("not a callsign")
)

var validCall = regexp.MustCompile(`^[A-Z0-9]{1,6}(/[A-Z0-9]{1,6}){0,2}$`)

// Persister is what the engine stores.
type Persister interface {
	MarkContestHandled(contestID int64, call, band, how string, at time.Time) error
	ClearContestHandled(contestID int64, call, band string) error
	SetDXSkip(store.DXSkip) error
	ClearDXSkip(call, band string) error
}

// Config configures an Engine.
type Config struct {
	Store Persister
	// CTY resolves DXCC entities (with ADIF numbers from cty.csv); nil
	// leaves every DX station unknown.
	CTY *cty.DB
	// Rank is the Club Log Most Wanted rank of an entity (1 = rarest).
	Rank func(adif int) (int, bool)
	// State gives a station's state from the callbook ("" = not known); it
	// is asked only for a call a field award applies to and the log has no
	// state for.
	State func(call string) string
	Now   func() time.Time
}

// Goal switches an award on or off and ranks it against the others. The
// zero Priority takes the award's default.
type Goal struct {
	Award    string
	Priority int
	Enabled  bool
}

// Default priorities (higher ranks first): the running contest above DXCC,
// DXCC above any other award a goal enables.
const (
	prioContest = 100
	prioDXCC    = 10
	prioOther   = 50
)

type skipMark struct{ trusted bool }

type undo struct {
	act       string
	op        string // skip | handled
	call      string
	band      string
	rowID     int64
	contestID int64
}

// Engine holds the queue state.
type Engine struct {
	cfg Config

	mu       sync.Mutex
	seq      int64
	candTS   time.Time
	cands    []bus.Candidate
	spots    []bus.Spot
	skipRows map[int64]bool // callsign-less candidates skipped, by row
	last     []undo
	listed   map[int64]bus.Ranked // the last computed list, by row

	// DX-cluster spots of the last SpotHorizon, by call|band; each keeps the
	// negative row id it was listed under while it lasts
	clSpots map[string]*clusterSpot
	clNext  int64

	// bmrec gate and buddies
	gate        string                 // "" or "off", "trusted", "qualified"
	qualSeen    bool                   // some candidate row carries the qualified flag
	unqualified int                    // bmrec rows the gate kept off the list, at the last Compute
	trace       []RowTrace             // why each candidate row is or is not listed, at the last Compute
	seenListed  map[string]*ListedSeen // rows listed in the last 24 h, by call|band|source
	buddies     []Buddy

	// contest: model is set while an instance runs (the contest goal)
	contest store.Contest
	model   *contest.Model
	held    *contest.Held
	worked  *contest.Worked
	qsos    int
	handled map[string]string // call|band -> how
	skipped map[string]skipMark

	// history
	dxWorked  map[store.WorkedKey]bool
	confirmed map[int]map[string]bool // Club Log: ADIF -> bands
	confLoad  bool                    // confirmations have been loaded at least once
	logEnt    []award.Entry           // the station-log view
	logLoad   bool                    // the station-log view has been loaded at least once
	dxSkips   map[string]time.Time    // call|band -> until

	// awards and goals
	awards  *award.Catalog
	goalCfg map[string]Goal // configured goals by lower-case award code
	dirty   bool            // the award indexes need a rebuild
	static  []goalIndex     // the enabled catalog awards, by priority
}

// goalIndex is an enabled catalog award with its slots indexed.
type goalIndex struct {
	code string
	prio int
	ix   *award.Index
}

// New returns an engine with the built-in DXCC award as its only goal and no
// history.
func New(cfg Config) *Engine {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Engine{cfg: cfg, awards: award.NewCatalog(), goalCfg: map[string]Goal{}, dirty: true, skipRows: map[int64]bool{}, listed: map[int64]bus.Ranked{},
		handled: map[string]string{}, skipped: map[string]skipMark{}, dxWorked: map[store.WorkedKey]bool{},
		confirmed: map[int]map[string]bool{}, dxSkips: map[string]time.Time{}, clSpots: map[string]*clusterSpot{}}
}

// MaxClusterRows caps the cluster rows on the list.
const MaxClusterRows = 40

type clusterSpot struct {
	rowID       int64
	call, band  string
	freqHz      float64
	spotter     string
	comment     string
	cluster     string
	rbn         bool
	first, last time.Time
	human       time.Time // latest human spot; its freq, comment and spotter stand while it is within SpotHorizon
}

// ClusterSpot is one spot from one of chase-queue's own DX-cluster logins.
type ClusterSpot struct {
	Call, Band, Spotter, Comment string
	FreqHz                       float64
	At                           time.Time
	// Cluster is the [[cluster]] name the spot came from; RBN marks a
	// skimmer spot.
	Cluster string
	RBN     bool
}

// Buddy is a station the operator wants listed.
type Buddy struct {
	Call  string
	Note  string
	Boost int
}

// Bmrec gate values (config [bmrec] gate); the zero value is off.
const (
	GateOff       = "off"
	GateTrusted   = "trusted"
	GateQualified = "qualified"
)

// SetBmrecGate chooses which of bmrec's rows count as available stations:
// "off" (all), "trusted" (a known callsign that is read back or typed by the
// operator) or "qualified" (trusted and the row says qualified:true).
func (e *Engine) SetBmrecGate(g string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gate = g
}

// SetBuddies replaces the buddy list.
func (e *Engine) SetBuddies(bs []Buddy) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.buddies = e.buddies[:0]
	for _, b := range bs {
		if b.Call = contest.NormalizeCall(b.Call); b.Call != "" {
			e.buddies = append(e.buddies, b)
		}
	}
}

// buddyOfLocked finds the buddy a callsign belongs to: the same call, else the
// same station under a portable form (calls.SameStation).
func (e *Engine) buddyOfLocked(call string) (Buddy, bool) {
	n := contest.NormalizeCall(call)
	if n == "" {
		return Buddy{}, false
	}
	for _, b := range e.buddies {
		if b.Call == n {
			return b, true
		}
	}
	for _, b := range e.buddies {
		if calls.SameStation(b.Call, n) {
			return b, true
		}
	}
	return Buddy{}, false
}

// GateProblem is what is wrong with the bmrec gate ("" = nothing): the
// qualified gate is on and bmrec's rows carry no qualified flag.
func (e *Engine) GateProblem() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gate == GateQualified && len(e.cands) > 0 && !e.qualSeen {
		return "bmrec sends no qualified flag: no bmrec row passes the qualified gate"
	}
	return ""
}

// passesGate reports whether a bmrec row counts as an available station.
func (e *Engine) passesGate(r bus.Candidate) bool { return e.gateReason(r) == "" }

// gateReason is why the [bmrec] gate keeps a row off the list ("" = it passes).
func (e *Engine) gateReason(r bus.Candidate) string {
	switch e.gate {
	case GateTrusted, GateQualified:
	default:
		return ""
	}
	if strings.TrimSpace(r.Call) == "" {
		return "no callsign yet"
	}
	if !(r.Trusted || r.CallSource == "confirmed" || r.CallSource == "owner") {
		return "callsign not trusted (not read back by the station, not typed by the operator)"
	}
	if e.gate == GateQualified {
		switch {
		case r.Qualified == nil:
			return "bmrec sent no qualified flag for this row"
		case !*r.Qualified:
			return "not qualified in bmrec"
		}
	}
	return ""
}

// AddClusterSpot records a DX-cluster spot. The station is then spotted (a
// heard row of it leaves the list) and, while needed or a buddy, listed as a
// cluster row for SpotHorizon after its latest spot. Several clusters
// spotting the same call on a band make one row: the freshest spot wins.
func (e *Engine) AddClusterSpot(s ClusterSpot) {
	call := contest.NormalizeCall(s.Call)
	if call == "" || s.Band == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	k := call + "|" + s.Band
	cs := e.clSpots[k]
	if cs == nil {
		e.clNext--
		cs = &clusterSpot{rowID: e.clNext, call: call, band: s.Band, first: s.At}
		e.clSpots[k] = cs
	}
	if s.RBN {
		// a skimmer measures the TX frequency and carries no operating
		// instructions: it only keeps a human spot's row alive
		if !cs.human.IsZero() && s.At.Sub(cs.human) <= SpotHorizon {
			if s.At.After(cs.last) {
				cs.last = s.At
			}
			return
		}
		if s.At.Before(cs.last) {
			return // an older spot of the same station on another cluster
		}
	} else {
		if s.At.Before(cs.human) {
			return // an older human spot of the same station on another cluster
		}
		cs.human = s.At
	}
	cs.freqHz, cs.spotter, cs.comment = s.FreqHz, s.Spotter, s.Comment
	if s.At.After(cs.last) {
		cs.last = s.At
	}
	cs.cluster, cs.rbn = s.Cluster, s.RBN
}

// tuneMode is the mode a heard station is tuned in: the band plan's, else the
// sideband convention.
func tuneMode(hz float64) string {
	if m := mode.FromFreq(hz); m != "" {
		return m
	}
	return mode.Sideband(hz)
}

func key(call, band string) string { return contest.NormalizeCall(call) + "|" + band }

// SetCandidates takes bmrec's current rows and spots. A state older than the
// one already held (a lower seq) is ignored; it reports whether it was taken.
func (e *Engine) SetCandidates(st bus.CQFinderState) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	// a lower seq is stale unless bmrec's clock says the state is newer: bmrec
	// restarting restarts its counter
	if st.Seq != 0 && st.Seq < e.seq && !st.TS.After(e.candTS) {
		return false
	}
	e.seq, e.candTS, e.cands, e.spots = st.Seq, st.TS, st.Rows, st.Spotted
	e.qualSeen = false
	for _, c := range st.Rows {
		if c.Qualified != nil {
			e.qualSeen = true
			break
		}
	}
	// row skips of rows that are gone are no longer needed
	open := map[int64]bool{}
	for _, c := range st.Rows {
		open[c.RowID] = true
	}
	for id := range e.skipRows {
		if !open[id] {
			delete(e.skipRows, id)
		}
	}
	return true
}

// ActivateContest switches to a contest instance with its model, handled
// stations and logged QSOs. Undo does not cross activities.
func (e *Engine) ActivateContest(c store.Contest, m *contest.Model, handled []store.ContestHandled, qsos []store.ContestQSO) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.contest, e.model = c, m
	e.handled = map[string]string{}
	for _, h := range handled {
		e.handled[key(h.Call, h.Band)] = h.How
	}
	e.skipped, e.skipRows, e.last = map[string]skipMark{}, map[int64]bool{}, nil
	e.setQSOsLocked(qsos)
}

// ActivateDX switches to DX chasing.
func (e *Engine) ActivateDX() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.model, e.contest = nil, store.Contest{}
	e.skipRows, e.last = map[int64]bool{}, nil
}

// Contest reports the running contest instance, if any.
func (e *Engine) Contest() (store.Contest, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.contest, e.model != nil
}

// activityLocked is the wire's activity: "contest" while a contest instance
// runs, else "dx". It is derived; the goals decide what is needed.
func (e *Engine) activityLocked() string {
	if e.model != nil {
		return bus.ActivityContest
	}
	return bus.ActivityDX
}

// SetAwards installs the award catalog and the configured goals. A goal that
// names an unknown award is an error and changes nothing.
func (e *Engine) SetAwards(cat *award.Catalog, goals []Goal) error {
	cfg := map[string]Goal{}
	for _, g := range goals {
		k := strings.ToLower(strings.TrimSpace(g.Award))
		if _, ok := cat.Get(k); !ok && k != award.CodeContest {
			return errors.New("goal names an unknown award: " + g.Award)
		}
		cfg[k] = g
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.awards, e.goalCfg, e.dirty = cat, cfg, true
	return nil
}

// ReplaceAwards swaps the catalog and goals of a running engine. Unlike
// SetAwards it tolerates a goal whose award is gone from the new catalog: the
// goal stays configured but matches nothing (rebuildLocked only walks the
// catalog), and its award code is returned so the caller can report it.
func (e *Engine) ReplaceAwards(cat *award.Catalog, goals []Goal) (missing []string) {
	cfg := map[string]Goal{}
	for _, g := range goals {
		k := strings.ToLower(strings.TrimSpace(g.Award))
		if _, ok := cat.Get(k); !ok && k != award.CodeContest {
			missing = append(missing, g.Award)
		}
		cfg[k] = g
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.awards, e.goalCfg, e.dirty = cat, cfg, true
	return missing
}

// SetContestQSOs replaces the contest log (after the writer stored a batch).
func (e *Engine) SetContestQSOs(contestID int64, qsos []store.ContestQSO) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.model != nil && e.contest.ID == contestID {
		e.setQSOsLocked(qsos)
	}
}

func (e *Engine) setQSOsLocked(qs []store.ContestQSO) {
	in := make([]contest.QSOIn, 0, len(qs))
	for _, q := range qs {
		in = append(in, contest.QSOIn{Call: q.Call, Band: q.Band, Mode: q.Mode, Exch: q.Exch, Invalid: q.Invalid})
	}
	e.held, e.worked, e.qsos = e.model.HeldFrom(in), e.model.NewWorked(in), len(qs)
}

// SetDXWorked replaces the station log: every callsign worked on a band, any
// mode, from station-log's view, or without it from Log4OM and Club Log.
func (e *Engine) SetDXWorked(w map[store.WorkedKey]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dxWorked = map[store.WorkedKey]bool{}
	for k := range w {
		e.dxWorked[store.WorkedKey{Call: contest.NormalizeCall(k.Call), Band: k.Band}] = true
	}
}

// AddDXWorked records one QSO as it is logged.
func (e *Engine) AddDXWorked(call, band string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dxWorked[store.WorkedKey{Call: contest.NormalizeCall(call), Band: band}] = true
}

// SetConfirmations replaces Club Log's confirmations; until it is called
// once, every DX station's need is unknown.
func (e *Engine) SetConfirmations(cs []store.Confirmation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.confirmed = map[int]map[string]bool{}
	for _, c := range cs {
		if e.confirmed[c.DXCC] == nil {
			e.confirmed[c.DXCC] = map[string]bool{}
		}
		e.confirmed[c.DXCC][c.Band] = true
	}
	e.confLoad, e.dirty = true, true
}

// SetLogConfirmations replaces the station-log view by the entities it has
// confirmed on a band (no calls, states or modes). SetLogEntries is the full
// form.
func (e *Engine) SetLogConfirmations(cs []store.Confirmation) {
	es := make([]award.Entry, 0, len(cs))
	for _, c := range cs {
		es = append(es, award.Entry{DXCC: c.DXCC, Band: c.Band, Confirmed: true})
	}
	e.SetLogEntries(es)
}

// SetLogEntries replaces the station-log view the awards read (its worked
// rows). Club Log's matrix stays separate; either source loaded is enough to
// value a DXCC slot, and a band confirmed in either counts.
func (e *Engine) SetLogEntries(es []award.Entry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logEnt, e.logLoad, e.dirty = es, true, true
}

// SetDXSkips restores the DX skips still in force (at start).
func (e *Engine) SetDXSkips(ks []store.DXSkip) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, k := range ks {
		e.dxSkips[key(k.Call, k.Band)] = k.Until
	}
}

// NextUTCMidnight is when a DX skip made at t ends.
func NextUTCMidnight(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// spottedLocked says whether one of spots names this station on the band
// within the horizon: the same call, or a portable form of it that resolves
// to the same entity.
func (e *Engine) spottedLocked(spots []bus.Spot, call, band string, now time.Time) bool {
	for _, s := range spots {
		if now.Sub(s.At) > SpotHorizon || s.Band != band || !calls.SameStation(s.Call, call) {
			continue
		}
		if contest.NormalizeCall(s.Call) == contest.NormalizeCall(call) {
			return true
		}
		if e.cfg.CTY != nil {
			a, ok1 := e.cfg.CTY.DXCC(s.Call)
			b, ok2 := e.cfg.CTY.DXCC(call)
			if ok1 && ok2 && a.Primary == b.Primary {
				return true
			}
		}
	}
	return false
}

// valued is what the goals say about one station.
type valued struct {
	val    contest.Valuation
	prio   int
	awards int
	needs  []award.Hit // the slots that are needed, by goal priority
}

// valueLocked evaluates the station for every active goal. A station is
// needed when any enabled goal's slot is not yet satisfied; it is unknown,
// never "not needed", while no goal can tell and none needs it. NewSlots and
// the Most Wanted entity come from the first needed goal that has them.
func (e *Engine) valueLocked(call, band, mode string) valued {
	in := award.Input{Call: contest.NormalizeCall(call), Band: band, Mode: mode}
	var ent cty.Entity
	entOK := false
	if e.cfg.CTY != nil {
		if got, ok := e.cfg.CTY.DXCC(call); ok && got.ADIF != 0 {
			ent, entOK = got, true
			in.ADIF, in.Entity = got.ADIF, got.Primary
		}
	}
	var out valued
	unknown, first := false, true
	for _, g := range e.goalsLocked() {
		var hits []award.Hit
		var cv contest.Valuation
		if g.contest != nil {
			hits, cv = g.contest.Evaluate(call, band)
		} else {
			d := g.ix.Definition()
			st := in
			if d.NeedsState() && entOK && d.AppliesToEntity(in.ADIF) && g.ix.StateOf(in.Call) == "" && e.cfg.State != nil {
				st.State = e.cfg.State(in.Call)
			}
			if h, ok := g.ix.Evaluate(st); ok {
				hits = []award.Hit{h}
			}
		}
		needed, fresh := false, false
		for _, h := range hits {
			switch {
			case h.Needed:
				needed = true
				fresh = fresh || h.Fresh
				out.needs = append(out.needs, h)
			case h.Status == award.Unknown:
				unknown = true
			}
		}
		if !needed {
			continue
		}
		out.awards++
		slots := 1
		switch {
		case g.contest != nil:
			slots = cv.NewSlots
		case fresh:
			slots = 2
		}
		if first {
			first, out.prio, out.val.NewSlots = false, g.prio, slots
		}
		if out.val.NewDXCC == nil {
			switch {
			case g.contest != nil && cv.NewDXCC != nil:
				out.val.NewDXCC = cv.NewDXCC
			case g.contest == nil && g.ix.Definition().Kind == award.KindDXCC && entOK:
				out.val.NewDXCC = &ent
			}
		}
	}
	switch {
	case out.awards > 0:
		out.val.Class = contest.ClassNew
	case unknown:
		out.val.Class = contest.ClassUnknown
	}
	return out
}

// activeGoal is an enabled award in the order of its priority; exactly one of
// ix and contest is set.
type activeGoal struct {
	prio    int
	ix      *award.Index
	contest *award.Contest
}

// goalsLocked lists the enabled goals, highest priority first: the catalog
// awards a goal switches on (DXCC is on unless a goal turns it off) and the
// running contest.
func (e *Engine) goalsLocked() []activeGoal {
	if e.dirty {
		e.rebuildLocked()
	}
	out := make([]activeGoal, 0, len(e.static)+1)
	for _, g := range e.static {
		out = append(out, activeGoal{prio: g.prio, ix: g.ix})
	}
	if e.model != nil {
		g := e.goalCfg[award.CodeContest]
		if g.Award == "" || g.Enabled {
			prio := g.Priority
			if prio == 0 {
				prio = prioContest
			}
			out = append(out, activeGoal{prio: prio, contest: &award.Contest{Model: e.model, Held: e.held}})
		}
	}
	slices.SortStableFunc(out, func(a, b activeGoal) int { return b.prio - a.prio })
	return out
}

// rebuildLocked indexes the log for each enabled catalog award.
func (e *Engine) rebuildLocked() {
	l := &award.Log{Loaded: e.logLoad, Entries: e.logEnt, ClubLoaded: e.confLoad, Club: e.confirmed}
	e.static = e.static[:0]
	for _, code := range e.awards.Codes() {
		def, _ := e.awards.Get(code)
		g, set := e.goalCfg[strings.ToLower(code)]
		on, prio := set && g.Enabled, g.Priority
		if !set && strings.EqualFold(code, award.CodeDXCC) {
			on = true
		}
		if !on {
			continue
		}
		if prio == 0 {
			prio = prioOther
			if strings.EqualFold(code, award.CodeDXCC) {
				prio = prioDXCC
			}
		}
		e.static = append(e.static, goalIndex{code: code, prio: prio, ix: def.Build(l)})
	}
	e.dirty = false
}

// Compute ranks the current candidates and returns the state to publish
// (without TS, problems, instances or files: the caller adds those).
func (e *Engine) Compute() bus.ChaseState {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.cfg.Now()
	for k, until := range e.dxSkips {
		if !now.Before(until) {
			delete(e.dxSkips, k)
		}
	}
	for k, cs := range e.clSpots {
		if now.Sub(cs.last) > SpotHorizon {
			delete(e.clSpots, k)
		}
	}
	contestOn := e.model != nil
	evals := map[int64][]award.Hit{}
	value := func(c *contest.Candidate, mode string) {
		v := e.valueLocked(c.Call, c.Band, mode)
		c.Val, c.Priority, c.Awards = v.val, v.prio, v.awards
		if len(v.needs) > 0 {
			evals[c.RowID] = v.needs
		}
		if c.Val.NewDXCC != nil && e.cfg.Rank != nil {
			c.Rank, _ = e.cfg.Rank(c.Val.NewDXCC.ADIF)
		}
	}
	own := make([]bus.Spot, 0, len(e.clSpots))
	for _, cs := range e.clSpots {
		own = append(own, bus.Spot{Call: cs.call, Band: cs.band, At: cs.last})
	}
	workedOn := func(call, band string) bool {
		if contestOn {
			return e.worked.Has(call, band)
		}
		return e.dxWorked[store.WorkedKey{Call: contest.NormalizeCall(call), Band: band}]
	}
	cands := make([]contest.Candidate, 0, len(e.cands)+len(e.clSpots))
	e.unqualified = 0
	var trace []RowTrace
	bmrecRow := map[int64]bus.Candidate{}
	buddyOf := map[int64]Buddy{}
	twinOf := map[int64]string{} // heard row -> call|band of the own-cluster spot that may stand for it
	for _, r := range e.cands {
		bmrecRow[r.RowID] = r
		if why := e.gateReason(r); why != "" {
			e.unqualified++
			trace = append(trace, bmrecTrace(r, "gate", why))
			continue
		}
		c := contest.Candidate{RowID: r.RowID, Band: r.Band, FreqHz: r.FreqHz, Call: strings.ToUpper(strings.TrimSpace(r.Call)),
			Trusted: r.Trusted, FirstSeen: r.FirstUTC, LastHeard: r.LastUTC, Likelihood: 0, Skipped: e.skipRows[r.RowID]}
		if c.Call != "" {
			if b, ok := e.buddyOfLocked(c.Call); ok {
				c.Buddy, c.BuddyBoost = true, b.Boost
				buddyOf[r.RowID] = b
			}
			// a heard station on our own cluster login leaves when its cluster row is
			// listed and stands for it (a station worked on the band has no cluster
			// row, so it stays); that is only known after ranking, see twinOf
			onBand := workedOn(c.Call, c.Band)
			if !onBand && e.spottedLocked(own, c.Call, c.Band, now) {
				twinOf[r.RowID] = key(c.Call, c.Band)
			}
			// the band plan's mode, not a sideband guess: a mode-sliced award
			// values a heard CW or data station by its segment, or Unknown
			value(&c, mode.FromFreq(r.FreqHz))
			// a spot only bmrec has (its own post, which our login drops as the
			// operator's, or another node) has no cluster row here: it hides a
			// station that is not needed, never a needed one
			if c.Val.Class != contest.ClassNew && !onBand && e.spottedLocked(e.spots, c.Call, c.Band, now) {
				trace = append(trace, bmrecTrace(r, "hidden", "spotted by a spot only bmrec has, and not needed for any award"))
				continue
			}
		}
		cands = append(cands, c)
	}
	cluster := map[int64]*clusterSpot{}
	for _, cs := range e.clSpots {
		c := contest.Candidate{RowID: cs.rowID, Band: cs.band, FreqHz: cs.freqHz, Call: cs.call, Trusted: true,
			FirstSeen: cs.first, LastHeard: cs.last}
		if b, ok := e.buddyOfLocked(cs.call); ok {
			c.Buddy, c.BuddyBoost = true, b.Boost
			buddyOf[cs.rowID] = b
		}
		value(&c, mode.Derive(cs.freqHz, cs.comment))
		cands = append(cands, c)
		cluster[cs.rowID] = cs
	}
	v := contest.Verdicts{}
	if contestOn {
		v.Worked = workedOn
		v.Handled = func(call, band string) bool { _, ok := e.handled[key(call, band)]; return ok }
		v.Skipped = func(c contest.Candidate) (bool, bool) {
			m, ok := e.skipped[key(c.Call, c.Band)]
			return ok, ok && c.Trusted && !m.trusted
		}
	} else {
		v.Worked = workedOn
		v.Skipped = func(c contest.Candidate) (bool, bool) {
			until, ok := e.dxSkips[key(c.Call, c.Band)]
			return ok && now.Before(until), false
		}
	}
	res := contest.Order(cands, v)
	st := bus.ChaseState{Seq: e.seq, Activity: e.activityLocked(), BmrecUnqualified: e.unqualified, Ranked: make([]bus.Ranked, 0, len(res.Queue))}
	if contestOn {
		st.Contest = &bus.ContestInfo{ID: e.contest.ID, Name: e.contest.Name, StartDate: e.contest.StartDate, QSOs: e.qsos,
			Unsupported: e.model.Def.UnsupportedSlots()}
	}
	e.listed = map[int64]bus.Ranked{}
	nCluster := 0
	listedCluster := map[string]bool{}
	for _, it := range res.Queue {
		r := bus.Ranked{RowID: it.RowID, Call: it.Call, Band: it.Band, Needed: it.Tier == contest.TierNewMult, OnBand: it.Tier == contest.TierWorked,
			FreqHz: int64(math.Round(it.FreqHz)), Source: bus.SourceBmrec, Mode: tuneMode(it.FreqHz)}
		b, isBuddy := buddyOf[it.RowID]
		if cs := cluster[it.RowID]; cs != nil {
			// only needed cluster stations and buddies are listed: the cluster
			// is full of the rest; buddies do not count against the cap
			if !isBuddy {
				if !r.Needed || nCluster >= MaxClusterRows {
					continue
				}
				nCluster++
			}
			listedCluster[key(cs.call, cs.band)] = true
			r.Source = bus.SourceCluster
			r.ClusterName = cs.cluster
			r.Mode = mode.Derive(cs.freqHz, cs.comment)
			r.Spot = &bus.ClusterSpot{FreqHz: int64(cs.freqHz), Spotter: cs.spotter, Comment: cs.comment, At: cs.last, RBN: cs.rbn}
		}
		r.Buddy, r.BuddyNote = isBuddy, b.Note
		if r.Needed {
			r.Tag = bus.TagNeed
			for _, h := range evals[it.RowID] {
				r.Needs = append(r.Needs, bus.Need{Award: h.Award, Key: h.Key, Band: h.Band, Mode: h.Mode, Status: string(h.Status), ATNO: h.ATNO})
				if h.Award == award.CodeContest {
					r.Tag = bus.TagMult
				}
			}
		}
		st.Ranked = append(st.Ranked, r)
	}
	// a heard row leaves only when the cluster row that stands for it is listed
	st.Ranked = slices.DeleteFunc(st.Ranked, func(r bus.Ranked) bool {
		k, twin := twinOf[r.RowID]
		return twin && listedCluster[k]
	})
	for _, r := range st.Ranked {
		e.listed[r.RowID] = r
	}
	// the trace: what happened to every candidate that got as far as the ranking
	inRanked := map[int64]bus.Ranked{}
	for _, r := range st.Ranked {
		inRanked[r.RowID] = r
	}
	for _, h := range res.Hidden {
		if r, ok := bmrecRow[h.RowID]; ok {
			trace = append(trace, bmrecTrace(r, "hidden", string(h.Reason)))
		}
	}
	hidden := map[int64]bool{}
	for _, h := range res.Hidden {
		hidden[h.RowID] = true
	}
	for _, it := range res.Queue {
		if hidden[it.RowID] {
			continue
		}
		cs := cluster[it.RowID]
		var t RowTrace
		if cs != nil {
			t = RowTrace{RowID: it.RowID, Source: bus.SourceCluster, Call: cs.call, Band: cs.band, FreqHz: float64(cs.freqHz)}
		} else if r, ok := bmrecRow[it.RowID]; ok {
			t = bmrecTrace(r, "", "")
		} else {
			continue
		}
		if lr, ok := inRanked[it.RowID]; ok {
			t.Outcome = "listed"
			t.Detail = fmt.Sprintf("needed=%t on_band=%t mode=%q", lr.Needed, lr.OnBand, lr.Mode)
		} else if cs != nil {
			t.Outcome, t.Detail = "not listed", "cluster rows are listed only when needed (or a buddy) and within the cap"
		} else {
			t.Outcome, t.Detail = "hidden", "replaced by the listed cluster row of the same call and band"
		}
		trace = append(trace, t)
	}
	e.trace = trace
	if e.seenListed == nil {
		e.seenListed = map[string]*ListedSeen{}
	}
	for k, v := range e.seenListed {
		if now.Sub(v.Last) > 24*time.Hour {
			delete(e.seenListed, k)
		}
	}
	for _, r := range st.Ranked {
		k := r.Call + "|" + r.Band + "|" + r.Source
		if v := e.seenListed[k]; v != nil {
			v.Last, v.Needed = now, r.Needed
		} else {
			e.seenListed[k] = &ListedSeen{Call: r.Call, Band: r.Band, Source: r.Source, First: now, Last: now, Needed: r.Needed}
		}
	}
	if n := len(e.last); n > 0 {
		st.Undo = e.last[n-1].call
		if st.Undo == "" {
			st.Undo = "skip"
		}
	}
	return st
}

// RowTrace says why one candidate row is, or is not, on the list.
type RowTrace struct {
	RowID      int64   `json:"row_id"`
	Source     string  `json:"source"`
	Call       string  `json:"call"`
	Band       string  `json:"band"`
	FreqHz     float64 `json:"freq_hz"`
	Trusted    bool    `json:"call_trusted,omitempty"`
	CallSource string  `json:"call_source,omitempty"`
	Qualified  *bool   `json:"qualified,omitempty"`
	Outcome    string  `json:"outcome"` // listed | gate | hidden | not listed
	Detail     string  `json:"detail,omitempty"`
}

func bmrecTrace(r bus.Candidate, outcome, detail string) RowTrace {
	return RowTrace{RowID: r.RowID, Source: bus.SourceBmrec, Call: r.Call, Band: r.Band, FreqHz: r.FreqHz,
		Trusted: r.Trusted, CallSource: r.CallSource, Qualified: r.Qualified, Outcome: outcome, Detail: detail}
}

// ListedSeen is a row the list carried at some point in the last 24 hours.
type ListedSeen struct {
	Call   string    `json:"call"`
	Band   string    `json:"band"`
	Source string    `json:"source"`
	First  time.Time `json:"first_listed"`
	Last   time.Time `json:"last_listed"`
	Needed bool      `json:"needed"`
}

// Trace is the engine's answer to "why is this station not on the list".
type Trace struct {
	Gate           string       `json:"gate"`
	Candidates     int          `json:"bmrec_candidates"`
	QualifiedFlag  bool         `json:"qualified_flag_seen"`
	GateKept       int          `json:"gate_kept_off"`
	ClusterSpots   int          `json:"cluster_spots"`
	Rows           []RowTrace   `json:"rows"`
	RecentlyListed []ListedSeen `json:"recently_listed"` // rows listed at any point in the last 24 h, newest first
}

// Trace recomputes the list and returns what happened to every candidate
// row (bmrec rows and cluster spots) at that computation.
func (e *Engine) Trace() Trace {
	e.Compute()
	e.mu.Lock()
	defer e.mu.Unlock()
	return Trace{Gate: e.gate, Candidates: len(e.cands), QualifiedFlag: e.qualSeen, GateKept: e.unqualified,
		ClusterSpots: len(e.clSpots), Rows: append([]RowTrace(nil), e.trace...), RecentlyListed: e.recentLocked()}
}

func (e *Engine) recentLocked() []ListedSeen {
	out := make([]ListedSeen, 0, len(e.seenListed))
	for _, v := range e.seenListed {
		out = append(out, *v)
	}
	slices.SortFunc(out, func(a, b ListedSeen) int { return b.Last.Compare(a.Last) })
	return out
}

// SetCTY replaces the country files (after a refresh).
func (e *Engine) SetCTY(db *cty.DB) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg.CTY = db
}

// ConfirmationsLoaded reports whether any confirmations are known: Club
// Log's or the station-log view's.
func (e *Engine) ConfirmationsLoaded() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.confLoad || e.logLoad
}

// AwardProblems lists what leaves an enabled award unable to value stations
// (every candidate would be Unknown): a field award without a loaded
// station-log view, and, with dxcc true, DXCC without any confirmations.
func (e *Engine) AwardProblems(dxcc bool) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dirty {
		e.rebuildLocked()
	}
	var out []string
	for _, g := range e.static {
		switch g.ix.Definition().Kind {
		case award.KindField:
			if !e.logLoad {
				out = append(out, "Award "+g.code+" needs station-log: not configured or not loaded, every station stays unknown for it")
			}
		case award.KindDXCC:
			if dxcc && !e.confLoad && !e.logLoad {
				out = append(out, "DXCC confirmations not loaded yet")
			}
		}
	}
	return out
}

// Skip hides the station: in a contest until something new is known about
// it (its callsign is confirmed), in DX chasing until 00:00 UTC. A row
// without a callsign is skipped by row.
func (e *Engine) Skip(t bus.Target) error {
	persist, err := e.skip(t)
	if persist != nil {
		if perr := persist(); err == nil {
			err = perr
		}
	}
	return err
}

// actCall is the callsign an action applies to: the listed row's own, else
// the one the operator typed (validated), never a payload call that
// contradicts the row.
func actCall(r bus.Ranked, typed string) (string, error) {
	if c := contest.NormalizeCall(r.Call); c != "" {
		return c, nil
	}
	c := contest.NormalizeCall(typed)
	if c != "" && !validCall.MatchString(c) {
		return "", errBadCall
	}
	return c, nil
}

func (e *Engine) skip(t bus.Target) (func() error, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.listed[t.RowID]
	if !ok || r.Band != t.Band {
		return nil, ErrNotListed
	}
	call, err := actCall(r, t.Call)
	if err != nil {
		return nil, err
	}
	u := undo{act: e.activityLocked(), op: "skip", call: call, band: t.Band, rowID: t.RowID, contestID: e.contest.ID}
	var persist func() error
	switch {
	case call == "":
		e.skipRows[t.RowID] = true
	case e.model != nil:
		e.skipped[key(call, t.Band)] = skipMark{trusted: e.trustedLocked(t.RowID)}
	default:
		until := NextUTCMidnight(e.cfg.Now())
		e.dxSkips[key(call, t.Band)] = until
		if e.cfg.Store != nil {
			persist = func() error { return e.cfg.Store.SetDXSkip(store.DXSkip{Call: call, Band: t.Band, Until: until}) }
		}
	}
	e.pushLocked(u)
	delete(e.listed, t.RowID)
	return persist, nil
}

// Handled marks a contest station dealt with (spotted or tuned to). In DX
// chasing a QSY only tunes the radio: the station stays until it is logged.
func (e *Engine) Handled(t bus.Target) error {
	persist, err := e.handled1(t)
	if persist != nil {
		if perr := persist(); err == nil {
			err = perr
		}
	}
	return err
}

func (e *Engine) handled1(t bus.Target) (func() error, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.model == nil {
		return nil, nil
	}
	r, ok := e.listed[t.RowID]
	if !ok || r.Band != t.Band {
		return nil, ErrNotListed
	}
	call, err := actCall(r, t.Call)
	if err != nil {
		return nil, err
	}
	if call == "" {
		if t.How != "qsy" {
			return nil, ErrNeedCall
		}
		// a candidate without a callsign that was tuned to: out of the list by row, like a skip
		e.skipRows[t.RowID] = true
		e.pushLocked(undo{act: e.activityLocked(), op: "skip", band: t.Band, rowID: t.RowID})
		delete(e.listed, t.RowID)
		return nil, nil
	}
	how := t.How
	if how == "" {
		how = "spot"
	}
	e.handled[key(call, t.Band)] = how
	var persist func() error
	if e.cfg.Store != nil {
		cid, now := e.contest.ID, e.cfg.Now()
		persist = func() error { return e.cfg.Store.MarkContestHandled(cid, call, t.Band, how, now) }
	}
	e.pushLocked(undo{act: e.activityLocked(), op: "handled", call: call, band: t.Band, contestID: e.contest.ID})
	delete(e.listed, t.RowID)
	return persist, nil
}

// Undo reverses the last Skip or handled mark of the current activity.
func (e *Engine) Undo() error {
	persist, err := e.undo1()
	if persist != nil {
		if perr := persist(); err == nil {
			err = perr
		}
	}
	return err
}

func (e *Engine) undo1() (func() error, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.last) == 0 {
		return nil, ErrNothingToUndo
	}
	u := e.last[len(e.last)-1]
	e.last = e.last[:len(e.last)-1]
	var persist func() error
	switch {
	case u.op == "skip" && u.call == "":
		delete(e.skipRows, u.rowID)
	case u.op == "skip" && u.act == bus.ActivityContest:
		delete(e.skipped, key(u.call, u.band))
	case u.op == "skip":
		delete(e.dxSkips, key(u.call, u.band))
		if e.cfg.Store != nil {
			persist = func() error { return e.cfg.Store.ClearDXSkip(u.call, u.band) }
		}
	case u.op == "handled":
		delete(e.handled, key(u.call, u.band))
		if e.cfg.Store != nil {
			persist = func() error { return e.cfg.Store.ClearContestHandled(u.contestID, u.call, u.band) }
		}
	}
	return persist, nil
}

func (e *Engine) pushLocked(u undo) {
	e.last = append(e.last, u)
	if len(e.last) > undoKeep {
		e.last = slices.Clone(e.last[len(e.last)-undoKeep:])
	}
}

func (e *Engine) trustedLocked(rowID int64) bool {
	for _, c := range e.cands {
		if c.RowID == rowID {
			return c.Trusted
		}
	}
	return false
}
