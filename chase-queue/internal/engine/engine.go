// Package engine ranks the stations bmrec hears calling CQ for the active
// activity: in a contest by multiplier value and the contest log, in DX
// chasing by DXCC entities not yet confirmed on the band. It hides stations
// spotted on the cluster (same station, same band, within 30 minutes),
// already worked, handled or skipped, and keeps the Skip, handled and Undo
// state every window shares. It does no I/O of its own beyond the Persister
// it is handed; one mutex guards it and is never held across I/O.
package engine

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"chase-queue/internal/bus"
	"chase-queue/internal/calls"
	"chase-queue/internal/contest"
	"chase-queue/internal/cty"
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
	Now  func() time.Time
}

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
	activity string
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

	// contest
	contest store.Contest
	model   *contest.Model
	held    *contest.Held
	worked  *contest.Worked
	qsos    int
	handled map[string]string // call|band -> how
	skipped map[string]skipMark

	// DX chasing
	dxWorked  map[store.WorkedKey]bool
	confirmed map[int]map[string]bool // ADIF -> bands
	confLoad  bool                    // confirmations have been loaded at least once
	dxSkips   map[string]time.Time    // call|band -> until
}

// New returns an engine in DX mode with no history.
func New(cfg Config) *Engine {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Engine{cfg: cfg, activity: bus.ActivityDX, skipRows: map[int64]bool{}, listed: map[int64]bus.Ranked{},
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
	first, last time.Time
}

// ClusterSpot is one spot from chase-queue's own DX-cluster login.
type ClusterSpot struct {
	Call, Band, Spotter, Comment string
	FreqHz                       float64
	At                           time.Time
}

// AddClusterSpot records a DX-cluster spot. The station is then spotted (a
// heard row of it leaves the list) and, while needed, listed as a cluster
// row for SpotHorizon after its latest spot.
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
	cs.freqHz, cs.spotter, cs.comment, cs.last = s.FreqHz, s.Spotter, s.Comment, s.At
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
	e.activity, e.contest, e.model = bus.ActivityContest, c, m
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
	e.activity, e.model, e.contest = bus.ActivityDX, nil, store.Contest{}
	e.skipRows, e.last = map[int64]bool{}, nil
}

// Contest reports the running contest instance, if any.
func (e *Engine) Contest() (store.Contest, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.contest, e.activity == bus.ActivityContest
}

// SetContestQSOs replaces the contest log (after the writer stored a batch).
func (e *Engine) SetContestQSOs(contestID int64, qsos []store.ContestQSO) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.activity == bus.ActivityContest && e.contest.ID == contestID {
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
// mode, from Log4OM and Club Log.
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
	e.confLoad = true
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

// spottedLocked says whether a human spot names this station on the band
// within the horizon: the same call, or a portable form of it that resolves
// to the same entity.
func (e *Engine) spottedLocked(call, band string, now time.Time) bool {
	spots := e.spots
	for _, cs := range e.clSpots {
		spots = append(spots[:len(spots):len(spots)], bus.Spot{Call: cs.call, Band: cs.band, At: cs.last})
	}
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

// dxValue values a callsign for DX chasing: new when its entity is not
// confirmed on the band, with two new slots when it is confirmed on no band
// at all. Unknown until Club Log's confirmations are loaded, or when the
// entity has no ADIF number (cty.csv missing).
func (e *Engine) dxValueLocked(call, band string) contest.Valuation {
	if e.cfg.CTY == nil || !e.confLoad {
		return contest.Valuation{Class: contest.ClassUnknown}
	}
	ent, ok := e.cfg.CTY.DXCC(call)
	if !ok || ent.ADIF == 0 {
		return contest.Valuation{Class: contest.ClassUnknown}
	}
	bands := e.confirmed[ent.ADIF]
	if bands[band] {
		return contest.Valuation{Class: contest.ClassNone}
	}
	v := contest.Valuation{Class: contest.ClassNew, NewSlots: 1, NewDXCC: &ent}
	if len(bands) == 0 {
		v.NewSlots = 2
	}
	return v
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
	contestOn := e.activity == bus.ActivityContest && e.model != nil
	value := func(c *contest.Candidate) {
		if contestOn {
			c.Val = e.model.Value(c.Call, c.Band, e.held)
		} else {
			c.Val = e.dxValueLocked(c.Call, c.Band)
		}
		if c.Val.NewDXCC != nil && e.cfg.Rank != nil {
			c.Rank, _ = e.cfg.Rank(c.Val.NewDXCC.ADIF)
		}
	}
	cands := make([]contest.Candidate, 0, len(e.cands)+len(e.clSpots))
	for _, r := range e.cands {
		c := contest.Candidate{RowID: r.RowID, Band: r.Band, FreqHz: r.FreqHz, Call: strings.ToUpper(strings.TrimSpace(r.Call)),
			Trusted: r.Trusted, FirstSeen: r.FirstUTC, LastHeard: r.LastUTC, Likelihood: 0, Skipped: e.skipRows[r.RowID]}
		if c.Call != "" {
			// a heard station on the cluster leaves: its cluster row stands for it
			if e.spottedLocked(c.Call, c.Band, now) {
				continue
			}
			value(&c)
		}
		cands = append(cands, c)
	}
	cluster := map[int64]*clusterSpot{}
	for _, cs := range e.clSpots {
		c := contest.Candidate{RowID: cs.rowID, Band: cs.band, FreqHz: cs.freqHz, Call: cs.call, Trusted: true,
			FirstSeen: cs.first, LastHeard: cs.last}
		value(&c)
		cands = append(cands, c)
		cluster[cs.rowID] = cs
	}
	v := contest.Verdicts{}
	tag := bus.TagNeed
	if contestOn {
		tag = bus.TagMult
		v.Worked = e.worked.Has
		v.Handled = func(call, band string) bool { _, ok := e.handled[key(call, band)]; return ok }
		v.Skipped = func(c contest.Candidate) (bool, bool) {
			m, ok := e.skipped[key(c.Call, c.Band)]
			return ok, ok && c.Trusted && !m.trusted
		}
	} else {
		v.Worked = func(call, band string) bool {
			return e.dxWorked[store.WorkedKey{Call: contest.NormalizeCall(call), Band: band}]
		}
		v.Skipped = func(c contest.Candidate) (bool, bool) {
			until, ok := e.dxSkips[key(c.Call, c.Band)]
			return ok && now.Before(until), false
		}
	}
	res := contest.Order(cands, v)
	st := bus.ChaseState{Seq: e.seq, Activity: e.activity, Ranked: make([]bus.Ranked, 0, len(res.Queue))}
	if contestOn {
		st.Contest = &bus.ContestInfo{ID: e.contest.ID, Name: e.contest.Name, StartDate: e.contest.StartDate, QSOs: e.qsos,
			Unsupported: e.model.Def.UnsupportedSlots()}
	}
	e.listed = map[int64]bus.Ranked{}
	nCluster := 0
	for _, it := range res.Queue {
		r := bus.Ranked{RowID: it.RowID, Call: it.Call, Band: it.Band, Needed: it.Tier == contest.TierNewMult}
		if cs := cluster[it.RowID]; cs != nil {
			// only needed cluster stations are listed: the cluster is full of the rest
			if !r.Needed || nCluster >= MaxClusterRows {
				continue
			}
			nCluster++
			r.Source = bus.SourceCluster
			r.Spot = &bus.ClusterSpot{FreqHz: int64(cs.freqHz), Spotter: cs.spotter, Comment: cs.comment, At: cs.last}
		}
		if r.Needed {
			r.Tag = tag
		}
		st.Ranked = append(st.Ranked, r)
		e.listed[r.RowID] = r
	}
	if n := len(e.last); n > 0 {
		st.Undo = e.last[n-1].call
		if st.Undo == "" {
			st.Undo = "skip"
		}
	}
	return st
}

// ConfirmationsLoaded reports whether Club Log's confirmations are known.
func (e *Engine) ConfirmationsLoaded() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.confLoad
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
	u := undo{act: e.activity, op: "skip", call: call, band: t.Band, rowID: t.RowID, contestID: e.contest.ID}
	var persist func() error
	switch {
	case call == "":
		e.skipRows[t.RowID] = true
	case e.activity == bus.ActivityContest:
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
	if e.activity != bus.ActivityContest {
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
		e.pushLocked(undo{act: e.activity, op: "skip", band: t.Band, rowID: t.RowID})
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
	e.pushLocked(undo{act: e.activity, op: "handled", call: call, band: t.Band, contestID: e.contest.ID})
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
