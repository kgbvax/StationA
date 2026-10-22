// SPDX-License-Identifier: AGPL-3.0-or-later

// Package policy is antmode's mode state machine: which antenna mode the
// station is in, who put it there, and when automation may change it. Pure:
// no I/O, the clock is passed in. The rules (decision record "HF antenna
// mode" in docs/known-issues.md):
//
//   - A manual set_ant_mode always wins and is never undone by automation,
//     except that an auto-entered storm may later auto-exit.
//   - Presence: manned iff powerseq is starting, running or waking (a wake is
//     triggered by operator activity); anything else — including powerseq
//     offline or silent — is unmanned. (The engine refines this: a 60 s dwell
//     on manned → unmanned, and powerseq's lost phase after a restart.)
//   - A qualifying warning active while the mode is normal: manned, or
//     auto_enter off → storm_suggested (unless dismissed for those ids);
//     unmanned with auto_enter on → storm, source auto.
//   - An auto-entered storm exits to normal after exit_holdoff of quiet time
//     with no qualifying warning, counted only across SUCCESSFUL polls: an
//     outage neither counts nor resets the quiet time.
//   - A manual storm and maintenance exit only manually.
//   - A manual normal while warnings are active acknowledges those ids: they
//     never auto-enter again (a new id — DWD issues one on escalation — does).
//   - dismiss_suggestion hides the suggestion for the current ids only.
//   - repark bumps seq; set_ant_mode to the current mode is a no-op.
package policy

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"codeberg.org/kgbvax/stationa/shared/antmode"

	"antmode/internal/nina"
)

// Source is who entered the current mode.
type Source string

const (
	Manual Source = "manual"
	Auto   Source = "auto"
)

// Presence is whether an operator is at the station.
type Presence string

const (
	Manned   Presence = "manned"
	Unmanned Presence = "unmanned"
)

// PresenceOf derives presence from powerseq's two inputs.
func PresenceOf(statusOnline bool, phase string) Presence {
	if !statusOnline {
		return Unmanned
	}
	switch phase {
	case "starting", "running", "waking":
		return Manned
	}
	return Unmanned
}

// Config is the policy configuration.
type Config struct {
	AutoEnter   bool
	ExitHoldoff time.Duration
}

// State is the persisted policy state.
type State struct {
	Mode   antmode.Mode `json:"ant_mode"`
	Source Source       `json:"source"`
	Since  time.Time    `json:"since"`
	// Seq is the lock sequence: bumped on every locked-mode entry and repark.
	Seq uint64 `json:"seq"`
	// Acked: warning ids acknowledged by a manual normal (no auto entry).
	Acked []string `json:"acked,omitempty"`
	// Dismissed: warning ids whose suggestion the operator dismissed.
	Dismissed []string `json:"dismissed,omitempty"`
	// QuietFor is the quiet time accumulated by an auto-entered storm.
	QuietFor time.Duration `json:"quiet_for_ns"`
	// Reason explains the last mode decision.
	Reason string `json:"reason,omitempty"`
}

// Result reports what a call changed.
type Result struct {
	ModeChanged bool
	From        antmode.Mode
	Reparked    bool
	// Changed: anything in State changed (persist).
	Changed bool
}

// Policy wraps State with the in-memory quiet-time bookkeeping.
type Policy struct {
	cfg Config
	S   State
	// lastClean is the previous successful poll that found no qualifying
	// warning while in auto storm. Zero after a warning, an outage, a mode
	// change or a restart: the next interval is not counted.
	lastClean time.Time
}

// New returns a policy over s (a loaded or zero state; a zero mode is normal).
func New(cfg Config, s State) *Policy {
	if s.Mode == "" {
		s.Mode = antmode.Normal
	}
	if s.Source == "" {
		s.Source = Manual
	}
	sort.Strings(s.Acked)
	sort.Strings(s.Dismissed)
	return &Policy{cfg: cfg, S: s}
}

// SetMode applies an operator set_ant_mode. active is the currently active
// qualifying warnings (acknowledged by a manual normal).
func (p *Policy) SetMode(m antmode.Mode, now time.Time, active []nina.Warning) (Result, error) {
	if !m.Valid() {
		return Result{}, fmt.Errorf("unknown ant_mode %q", m)
	}
	if m == p.S.Mode {
		return Result{}, nil // no-op; use repark to park again
	}
	from := p.S.Mode
	reason := "operator: " + string(m)
	if m == antmode.Normal && len(active) > 0 {
		n := 0
		for _, w := range active {
			if !contains(p.S.Acked, w.ID) {
				p.S.Acked = insert(p.S.Acked, w.ID)
				n++
			}
		}
		if n > 0 {
			reason += fmt.Sprintf(" (acknowledged %d active warning(s))", n)
		}
	}
	p.enter(m, Manual, now, reason)
	return Result{ModeChanged: true, From: from, Changed: true}, nil
}

func (p *Policy) enter(m antmode.Mode, src Source, now time.Time, reason string) {
	p.S.Mode, p.S.Source, p.S.Since, p.S.Reason = m, src, now, reason
	if m != antmode.Normal {
		p.S.Seq++
	}
	p.S.QuietFor = 0
	p.lastClean = time.Time{}
}

// Repark bumps seq in a locked mode; ignored in normal.
func (p *Policy) Repark() Result {
	if p.S.Mode == antmode.Normal {
		return Result{}
	}
	p.S.Seq++
	p.S.Reason = "operator: repark"
	return Result{Reparked: true, Changed: true}
}

// Dismiss hides the suggestion for the active ids.
func (p *Policy) Dismiss(active []nina.Warning) Result {
	changed := false
	for _, w := range active {
		if !contains(p.S.Dismissed, w.ID) {
			p.S.Dismissed = insert(p.S.Dismissed, w.ID)
			changed = true
		}
	}
	return Result{Changed: changed}
}

// Poll records one warning poll. ok=false is an outage: nothing counts. On a
// successful poll the acknowledged/dismissed sets are pruned to the active
// ids, and an auto storm accumulates quiet time and exits after the holdoff.
func (p *Policy) Poll(now time.Time, ok bool, active []nina.Warning) Result {
	if !ok {
		p.lastClean = time.Time{}
		return Result{}
	}
	var r Result
	ids := map[string]bool{}
	for _, w := range active {
		ids[w.ID] = true
	}
	if a := prune(p.S.Acked, ids); len(a) != len(p.S.Acked) {
		p.S.Acked, r.Changed = a, true
	}
	if d := prune(p.S.Dismissed, ids); len(d) != len(p.S.Dismissed) {
		p.S.Dismissed, r.Changed = d, true
	}
	if p.S.Mode != antmode.Storm || p.S.Source != Auto {
		return r
	}
	if len(active) > 0 {
		if p.S.QuietFor != 0 {
			r.Changed = true
		}
		p.S.QuietFor, p.lastClean = 0, time.Time{}
		return r
	}
	if !p.lastClean.IsZero() && now.After(p.lastClean) {
		p.S.QuietFor += now.Sub(p.lastClean)
		r.Changed = true
	}
	p.lastClean = now
	if p.S.QuietFor >= p.cfg.ExitHoldoff {
		p.enter(antmode.Normal, Auto, now,
			fmt.Sprintf("auto: no qualifying warning for %s", p.cfg.ExitHoldoff))
		return Result{ModeChanged: true, From: antmode.Storm, Changed: true}
	}
	return r
}

// Evaluate decides automatic storm entry. hold suppresses it (startup, before
// presence is known).
func (p *Policy) Evaluate(now time.Time, active []nina.Warning, presence Presence, hold bool) Result {
	if hold || p.S.Mode != antmode.Normal || presence != Unmanned || !p.cfg.AutoEnter {
		return Result{}
	}
	fresh := p.unacked(active)
	if len(fresh) == 0 {
		return Result{}
	}
	p.enter(antmode.Storm, Auto, now, "auto: DWD "+events(fresh)+" active, station unmanned")
	return Result{ModeChanged: true, From: antmode.Normal, Changed: true}
}

// Suggested: normal, a qualifying warning neither acknowledged nor dismissed,
// and the station manned or auto entry off.
func (p *Policy) Suggested(active []nina.Warning, presence Presence) bool {
	if p.S.Mode != antmode.Normal {
		return false
	}
	if presence == Unmanned && p.cfg.AutoEnter {
		return false // Evaluate enters instead
	}
	for _, w := range p.unacked(active) {
		if !contains(p.S.Dismissed, w.ID) {
			return true
		}
	}
	return false
}

// SuggestReason is the /state reason while a suggestion is shown.
func SuggestReason(active []nina.Warning, presence Presence) string {
	return "DWD " + events(active) + " active, station " + string(presence) + ": storm suggested"
}

// AutoExitAt is when an auto storm will exit if the polls stay quiet (the
// exit itself happens at the first successful poll at or after it); zero
// when not in auto storm or no quiet time is running (warning active, or no
// successful quiet poll yet). Counted from the last clean poll, so it only
// changes at a poll and the /state dedupe holds between polls.
func (p *Policy) AutoExitAt() time.Time {
	if p.S.Mode != antmode.Storm || p.S.Source != Auto || p.lastClean.IsZero() {
		return time.Time{}
	}
	return p.lastClean.Add(p.cfg.ExitHoldoff - p.S.QuietFor)
}

func (p *Policy) unacked(active []nina.Warning) []nina.Warning {
	var out []nina.Warning
	for _, w := range active {
		if !contains(p.S.Acked, w.ID) {
			out = append(out, w)
		}
	}
	return out
}

func events(ws []nina.Warning) string {
	seen := map[string]bool{}
	var ev []string
	for _, w := range ws {
		if !seen[w.Event] {
			seen[w.Event] = true
			ev = append(ev, w.Event)
		}
	}
	return strings.Join(ev, ", ")
}

func contains(s []string, v string) bool {
	i := sort.SearchStrings(s, v)
	return i < len(s) && s[i] == v
}

func insert(s []string, v string) []string {
	i := sort.SearchStrings(s, v)
	s = append(s, "")
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

func prune(s []string, keep map[string]bool) []string {
	var out []string
	for _, v := range s {
		if keep[v] {
			out = append(out, v)
		}
	}
	return out
}
