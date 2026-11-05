// Package model turns the latest bus inputs, the view filters and the
// operator's keystrokes into the list chaseq-ui shows. It does no I/O apart
// from the filters file (filters.go), so it is tested without a broker.
package model

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"chaseq-ui/internal/bus"
)

// StaleAfter is how long the chase state may stay silent: chase-queue
// republishes every 30 s, so 75 s is two missed heartbeats plus margin.
const StaleAfter = 75 * time.Second

// Row is one displayed station.
type Row struct {
	RowID   int64
	Call    string
	Band    string
	FreqHz  int64  // 0 = unknown
	Mode    string // canonical; "" = unknown
	Needed  bool
	Tag     string
	Cluster bool
	// ClusterName names the DX cluster of a cluster row; RBN marks a
	// skimmer spot.
	ClusterName string
	RBN         bool
	Highlighted bool
	// Worked: the call is in the log at all. WorkedThisBand: on this band (the
	// row then comes with Needed false). WorkedOther: the other bands.
	Worked         bool
	WorkedThisBand bool
	WorkedOther    []string
	// Who is the compact QRZ line (name, place, grid); "" = nothing known.
	Who   string
	Needs []NeedTag
	Buddy bool
	// BuddyNote is the buddy-list note, the badge's hover text.
	BuddyNote string
	// AgeText is how long ago the station was last heard or spotted ("now",
	// "7 min", "2 h"); "" = unknown. ModeLabel is the mode as shown: empty for
	// usb and lsb, which say nothing the frequency does not.
	AgeText   string
	ModeLabel string
	// WorkedLast is "12 Oct 40m": when and on which band this CALLSIGN (not its
	// DXCC entity) was last logged; WorkedTitle spells it out. NewBand: worked
	// before, but never on this row's band.
	WorkedLast  string
	WorkedTitle string
	NewBand     bool
}

// NeedTag is one award need ready to show: Label is the tag text ("DXCC",
// "WAS TX"), Title the hover detail, Status the CSS class.
type NeedTag struct {
	Label, Title, Status string
	ATNO                 bool // an all-time new one: never worked in any slice
}

// MaxTuneHz is the FLEX-8400's top frequency; VHF/UHF rows (2 m, 70 cm) are
// shown and sent to the logger but never tune the radio.
const MaxTuneHz = 54_000_000

// Tunable reports whether the radio can be tuned to this row.
func (r Row) Tunable() bool { return r.FreqHz > 0 && r.FreqHz <= MaxTuneHz }

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
	// Awards are the award codes among the needs of the listed rows (the
	// unfiltered list, first seen = highest priority) plus any active filter
	// code, so a filter whose award left the list can still be switched off.
	Awards  []string
	Filters Filters
}

// Model holds the inputs. Safe for concurrent use.
type Model struct {
	mu        sync.Mutex
	now       func() time.Time
	chase     bus.ChaseState
	haveChase bool
	lastChase time.Time
	status    *bool // nil = unknown
	filters   Filters
	radioBand string // band the radio reports ("" = unknown)
	highlight int64
	version   uint64
}

// New returns a Model; now may be nil for time.Now.
func New(now func() time.Time) *Model {
	if now == nil {
		now = time.Now
	}
	return &Model{now: now}
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

// SetFilters replaces the filters.
func (m *Model) SetFilters(f Filters) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.filters = f
	m.follow()
}

// UpdateFilters changes the filters atomically and returns the result.
func (m *Model) UpdateFilters(f func(Filters) Filters) Filters {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.filters = f(m.filters)
	return m.filters
}

func sameBands(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// follow sets the band filter to the radio band while locked and the band is
// known; unknown keeps the last filter. Callers hold mu.
func (m *Model) follow() {
	if m.filters.LockBand && m.radioBand != "" {
		m.filters.Bands = []string{m.radioBand}
	}
}

// SetRadioBand takes the band the radio reports ("" = unknown or offline). It
// reports whether the effective filters changed, so callers re-render and
// save only then.
func (m *Model) SetRadioBand(band string) (Filters, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.radioBand = band
	before := m.filters.Bands
	m.follow()
	return m.filters, !sameBands(before, m.filters.Bands)
}

// SetLock switches the band lock. On: the filter becomes the radio band (kept
// as is while that is unknown). Off: the current band filter stays.
func (m *Model) SetLock(on bool) Filters {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.filters.LockBand = on
	m.follow()
	return m.filters
}

// ClearFilters resets every filter to the default; the band lock stays and
// keeps following the radio.
func (m *Model) ClearFilters() Filters {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.filters = Filters{LockBand: m.filters.LockBand}
	m.follow()
	return m.filters
}

// UndoState is the undo label and the set of listed row ids, for confirming
// an Undo from the list's visible effect.
func (m *Model) UndoState() (string, map[int64]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make(map[int64]bool, len(m.chase.Ranked))
	for _, r := range m.chase.Ranked {
		ids[r.RowID] = true
	}
	return m.chase.Undo, ids
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
		Awards: m.awardCodes(),
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
		if !m.filters.hasBand(r.Band) || (m.filters.NeededOnly && !r.Needed) ||
			!m.filters.hasAward(r.Needs) || !m.filters.hasMode(r.Mode) || !m.filters.hasSource(r.Source == bus.SourceCluster) {
			hidden++
			continue
		}
		row := Row{RowID: r.RowID, Call: r.Call, Band: r.Band, FreqHz: r.FreqHz, Mode: r.Mode,
			Needed: r.Needed, Tag: r.Tag, Cluster: r.Source == bus.SourceCluster}
		if row.Cluster {
			row.ClusterName = r.ClusterName
			if r.Spot != nil {
				row.RBN = r.Spot.RBN
				if row.FreqHz == 0 {
					row.FreqHz = r.Spot.FreqHz
				}
			}
		}
		row.BuddyNote = r.BuddyNote
		row.Who = whoLine(r.Info)
		row.Buddy = r.Buddy
		for _, n := range r.Needs {
			if n.Award != "" {
				row.Needs = append(row.Needs, needTag(n))
			}
		}
		row.AgeText = ageText(m.now(), r.HeardAt)
		if r.Mode != "usb" && r.Mode != "lsb" {
			row.ModeLabel = r.Mode
		}
		if w := r.WorkedBefore; w != nil {
			row.Worked, row.WorkedThisBand = true, w.ThisBand
			row.NewBand = !w.ThisBand
			row.WorkedLast, row.WorkedTitle = workedLast(m.now(), r.Call, w)
			for _, b := range w.Bands {
				if b != r.Band {
					row.WorkedOther = append(row.WorkedOther, b)
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, hidden
}

// ageText says how long ago t was: "now" under a minute, "7 min", then hours.
func ageText(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < 2*time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	default:
		return fmt.Sprintf("%d h", int(d/time.Hour))
	}
}

// workedLast is the short "12 Oct 40m" and the hover text of a callsign's last
// QSO; both "" when the log gave no time.
func workedLast(now time.Time, call string, w *bus.WorkedBefore) (string, string) {
	if w.Last.IsZero() {
		return "", ""
	}
	date := w.Last.UTC().Format("2 Jan")
	if w.Last.UTC().Year() != now.UTC().Year() {
		date = w.Last.UTC().Format("2 Jan 2006")
	}
	short, long := date, "was last logged on "+w.Last.UTC().Format("2 Jan 2006")
	if w.LastBand != "" {
		short += " " + w.LastBand
		long += " on " + w.LastBand
	}
	title := call + " (this callsign, not its DXCC entity) " + long
	if w.ThisBand {
		title += "; also worked on this row's band"
	} else {
		title += "; never worked on this row's band: a new band for it"
	}
	return short, title
}

// awardCodes lists the award codes of the unfiltered list in first-seen order,
// then the active filter codes not on it; callers hold mu.
func (m *Model) awardCodes() []string {
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, r := range m.chase.Ranked {
		for _, n := range r.Needs {
			add(n.Award)
		}
	}
	for _, a := range m.filters.Awards {
		add(a)
	}
	return out
}

func (m *Model) indexOf(rows []Row, id int64) int {
	for i, r := range rows {
		if r.RowID == id {
			return i
		}
	}
	return -1
}

// whoLine joins the QRZ fields worth a glance: name, then QTH (state),
// country, then grid. "" when info is absent or empty.
func whoLine(i *bus.Info) string {
	if i == nil {
		return ""
	}
	place := i.QTH
	if i.State != "" {
		if place != "" {
			place += " "
		}
		place += i.State
	}
	var parts []string
	for _, p := range []string{i.Name, place, i.Country, i.Grid} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " \u00b7 ")
}

func needTag(n bus.Need) NeedTag {
	label := n.Award
	if n.Key != "" {
		label += " " + n.Key
	}
	title := label
	for _, d := range []string{n.Band, n.Mode, n.Status} {
		if d != "" {
			title += ", " + d
		}
	}
	status := n.Status
	switch status {
	case "unworked", "worked-unconfirmed", "confirmed":
	default:
		status = "unknown"
	}
	if n.ATNO {
		title += ", ATNO (never worked)"
	}
	return NeedTag{Label: label, Title: title, Status: status, ATNO: n.ATNO}
}
