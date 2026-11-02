// Package model turns the latest bus inputs, the view filters and the
// operator's keystrokes into the list chaseq-ui shows. It does no I/O apart
// from the filters file (filters.go), so it is tested without a broker.
package model

import (
	"fmt"
	"math"
	"sync"
	"time"

	"chaseq-ui/internal/bus"
)

// StaleAfter is how long the chase state may stay silent: chase-queue
// republishes every 30 s, so 75 s is two missed heartbeats plus margin.
const StaleAfter = 75 * time.Second

// Row is one displayed station.
type Row struct {
	RowID       int64
	Call        string
	Band        string
	FreqHz      int64 // 0 = unknown
	Needed      bool
	Tag         string
	Cluster     bool
	Highlighted bool
}

// Tunable reports whether the radio can be tuned to this row.
func (r Row) Tunable() bool { return r.FreqHz > 0 }

// View is everything the page renders.
type View struct {
	Rows           []Row
	Highlight      int64 // row_id; 0 with no rows
	Problems       []string
	Offline        string // non-empty = the one status line; actions are disabled
	ActionsEnabled bool
	Activity       string
	ContestName    string
	Undo           string
	Hidden         int // ranked rows removed by the filters
	Filters        Filters
}

// Model holds the inputs. Safe for concurrent use.
type Model struct {
	mu        sync.Mutex
	now       func() time.Time
	chase     bus.ChaseState
	haveChase bool
	lastChase time.Time
	status    *bool // nil = unknown
	freq      map[int64]int64
	filters   Filters
	highlight int64
	version   uint64
}

// New returns a Model; now may be nil for time.Now.
func New(now func() time.Time) *Model {
	if now == nil {
		now = time.Now
	}
	return &Model{now: now, freq: map[int64]int64{}}
}

// SetChase takes a chase state.
func (m *Model) SetChase(s bus.ChaseState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chase, m.haveChase, m.lastChase = s, true, m.now()
	m.version++
}

// Version counts the chase states received.
func (m *Model) Version() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.version
}

// Signature identifies the ranking and the undo label: it changes when an
// action visibly changed chase-queue's list.
func (m *Model) Signature() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	sig := m.chase.Undo + "|"
	for _, r := range m.chase.Ranked {
		sig += fmt.Sprintf("%d,", r.RowID)
	}
	return sig
}

// SetStatus takes chase-queue's status (its last-will is "offline").
func (m *Model) SetStatus(online bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = &online
}

// SetCQFinder takes bmrec's candidates: the frequency of heard stations.
func (m *Model) SetCQFinder(s bus.CQFinderState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := make(map[int64]int64, len(s.Rows))
	for _, r := range s.Rows {
		if r.FreqHz > 0 {
			f[r.RowID] = int64(math.Round(r.FreqHz))
		}
	}
	m.freq = f
}

// SetFilters replaces the filters.
func (m *Model) SetFilters(f Filters) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.filters = f
}

// Filters returns the current filters.
func (m *Model) Filters() Filters {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filters
}

// Move moves the highlight by delta within the shown rows, clamped.
func (m *Model) Move(delta int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, _ := m.shown()
	if len(rows) == 0 {
		return
	}
	i := m.indexOf(rows, m.highlight)
	if i < 0 {
		i = 0
	}
	i += delta
	if i < 0 {
		i = 0
	}
	if i > len(rows)-1 {
		i = len(rows) - 1
	}
	m.highlight = rows[i].RowID
}

// Select sets the highlight to rowID when it is shown; it reports success.
func (m *Model) Select(rowID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, _ := m.shown()
	if m.indexOf(rows, rowID) < 0 {
		return false
	}
	m.highlight = rowID
	return true
}

// Snapshot returns the current view and keeps the highlight valid.
func (m *Model) Snapshot() View {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, hidden := m.shown()
	if m.indexOf(rows, m.highlight) < 0 {
		m.highlight = 0
		if len(rows) > 0 {
			m.highlight = rows[0].RowID
		}
	}
	for i := range rows {
		rows[i].Highlighted = rows[i].RowID == m.highlight
	}
	v := View{
		Rows: rows, Highlight: m.highlight, Problems: m.chase.Problems,
		Activity: m.chase.Activity, Undo: m.chase.Undo, Hidden: hidden, Filters: m.filters,
	}
	if m.chase.Contest != nil {
		v.ContestName = m.chase.Contest.Name
	}
	v.Offline = m.offlineLine()
	v.ActionsEnabled = v.Offline == ""
	return v
}

// Find returns the shown row with rowID.
func (m *Model) Find(rowID int64) (Row, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, _ := m.shown()
	if i := m.indexOf(rows, rowID); i >= 0 {
		return rows[i], true
	}
	return Row{}, false
}

// Highlighted returns the highlighted row.
func (m *Model) Highlighted() (Row, bool) {
	v := m.Snapshot()
	for _, r := range v.Rows {
		if r.Highlighted {
			return r, true
		}
	}
	return Row{}, false
}

// Activity returns the current activity and whether actions are enabled.
func (m *Model) Activity() (string, bool) {
	v := m.Snapshot()
	return v.Activity, v.ActionsEnabled
}

// StillListed reports whether rowID is in the latest chase state (unfiltered).
func (m *Model) StillListed(rowID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.chase.Ranked {
		if r.RowID == rowID {
			return true
		}
	}
	return false
}

func (m *Model) offlineLine() string {
	switch {
	case m.status != nil && !*m.status:
		return "chase-queue is offline"
	case !m.haveChase:
		return "waiting for chase-queue"
	case m.now().Sub(m.lastChase) > StaleAfter:
		return fmt.Sprintf("chase-queue has been silent for %d s", int(m.now().Sub(m.lastChase).Seconds()))
	}
	return ""
}

// shown applies the filters; callers hold mu.
func (m *Model) shown() ([]Row, int) {
	var rows []Row
	hidden := 0
	for _, r := range m.chase.Ranked {
		if !m.filters.hasBand(r.Band) || (m.filters.NeededOnly && !r.Needed) {
			hidden++
			continue
		}
		row := Row{RowID: r.RowID, Call: r.Call, Band: r.Band, Needed: r.Needed, Tag: r.Tag,
			Cluster: r.Source == bus.SourceCluster}
		if row.Cluster && r.Spot != nil {
			row.FreqHz = r.Spot.FreqHz
		} else {
			row.FreqHz = m.freq[r.RowID]
		}
		rows = append(rows, row)
	}
	return rows, hidden
}

func (m *Model) indexOf(rows []Row, id int64) int {
	for i, r := range rows {
		if r.RowID == id {
			return i
		}
	}
	return -1
}
