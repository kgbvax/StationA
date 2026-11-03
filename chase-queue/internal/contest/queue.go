package contest

import (
	"cmp"
	"slices"
	"time"
)

// Tier is a candidate's place in the work order (R13).
type Tier int

const (
	TierNewMult   Tier = iota // would give a new multiplier
	TierBuddy                 // a buddy nothing new is known to be needed for
	TierUnknown               // multiplier state cannot be told
	TierUnworked              // has a callsign, nothing new
	TierWorked                // already worked on this band: listed, not needed
	TierCandidate             // no callsign yet
)

// Why a station is not in the queue (R21).
const (
	HiddenSpotted = "spotted"
	HiddenHandled = "handled"
	HiddenSkipped = "skipped"
)

// Candidate is one open board row as the queue sees it.
type Candidate struct {
	RowID     int64
	Band      string
	FreqHz    float64
	Call      string // "" = no callsign yet
	Trusted   bool   // the callsign source is confirmed or the owner's
	FirstSeen time.Time
	LastHeard time.Time
	// Likelihood orders callsign-less candidates (the row's CQ score).
	Likelihood float64
	Val        Valuation // zero when Call is ""
	// Rank is the Club Log rank of Val.NewDXCC; 0 = not ranked.
	Rank int
	// Skipped marks a candidate without a callsign the operator skipped.
	Skipped bool
	// Priority is the goal priority of the most important award the station
	// is needed for (higher first) and Awards how many awards need it; both
	// order the new-multiplier tier ahead of NewSlots. Zero when only one
	// goal (or none) is active.
	Priority int
	Awards   int
	// Buddy marks a station on the buddy list; BuddyBoost orders buddies
	// among themselves (higher first).
	Buddy      bool
	BuddyBoost int
}

// Verdicts answers per-station questions for the order. Nil functions answer
// false.
type Verdicts struct {
	Worked  func(call, band string) bool
	Spotted func(call, band string) bool
	Handled func(call, band string) bool
	// Returned says whether something new is known about a skipped
	// station (R16): its callsign changed or was confirmed.
	Skipped func(c Candidate) (skipped, returned bool)
}

// Item is one queue entry.
type Item struct {
	Candidate
	Tier Tier
}

// HiddenItem is a station kept out of the queue and why.
type HiddenItem struct {
	Candidate
	Reason string
}

// Result is the queue in work order and what was left out.
type Result struct {
	Queue  []Item
	Hidden []HiddenItem
}

func has(f func(string, string) bool, call, band string) bool { return f != nil && f(call, band) }

func tierOf(c Candidate, worked bool) Tier {
	switch {
	case c.Call == "":
		return TierCandidate
	case worked:
		return TierWorked
	case c.Val.Class == ClassNew:
		return TierNewMult
	case c.Buddy:
		return TierBuddy
	case c.Val.Class == ClassUnknown:
		return TierUnknown
	}
	return TierUnworked
}

// Order applies R19's eligibility (done by the caller), R11/R16's hiding
// and R13's tiers. A station already worked on its band (a dupe in a contest)
// is not hidden: it sits in TierWorked, after every unworked one. A buddy
// nothing new is needed for sits in TierBuddy, just below the new-multiplier
// tier. Within the new-multiplier tier: the higher goal priority first, then a
// buddy (higher boost first), then the station needed by more awards, then more new slots first,
// then ranked DXCC entities by Club Log rank, then stations new only in a
// district or zone slot, each by age (oldest first).
func Order(cands []Candidate, v Verdicts) Result {
	var res Result
	for _, c := range cands {
		if c.Call != "" {
			reason := ""
			switch {
			case has(v.Handled, c.Call, c.Band):
				reason = HiddenHandled
			case has(v.Spotted, c.Call, c.Band):
				reason = HiddenSpotted
			}
			if reason != "" {
				res.Hidden = append(res.Hidden, HiddenItem{c, reason})
				continue
			}
			if v.Skipped != nil {
				if skipped, back := v.Skipped(c); skipped && !back {
					res.Hidden = append(res.Hidden, HiddenItem{c, HiddenSkipped})
					continue
				}
			}
		} else if c.Skipped {
			res.Hidden = append(res.Hidden, HiddenItem{c, HiddenSkipped})
			continue
		}
		res.Queue = append(res.Queue, Item{c, tierOf(c, c.Call != "" && has(v.Worked, c.Call, c.Band))})
	}
	slices.SortStableFunc(res.Queue, func(a, b Item) int {
		if a.Tier != b.Tier {
			return cmp.Compare(a.Tier, b.Tier)
		}
		if a.Tier == TierCandidate {
			if d := cmp.Compare(b.Likelihood, a.Likelihood); d != 0 {
				return d
			}
			return a.FirstSeen.Compare(b.FirstSeen)
		}
		if a.Tier == TierNewMult {
			if a.Priority != b.Priority {
				return cmp.Compare(b.Priority, a.Priority)
			}
			if a.Buddy != b.Buddy {
				return buddyFirst(a.Buddy)
			}
			if d := cmp.Compare(b.BuddyBoost, a.BuddyBoost); d != 0 && a.Buddy {
				return d
			}
			if a.Awards != b.Awards {
				return cmp.Compare(b.Awards, a.Awards)
			}
			if a.Val.NewSlots != b.Val.NewSlots {
				return cmp.Compare(b.Val.NewSlots, a.Val.NewSlots)
			}
			ar, br := rankKey(a), rankKey(b)
			if ar != br {
				return cmp.Compare(ar, br)
			}
		}
		if a.Tier == TierBuddy {
			if d := cmp.Compare(b.BuddyBoost, a.BuddyBoost); d != 0 {
				return d
			}
		}
		return a.FirstSeen.Compare(b.FirstSeen)
	})
	return res
}

// buddyFirst orders a pair that differs in Buddy: the buddy first.
func buddyFirst(aBuddy bool) int {
	if aBuddy {
		return -1
	}
	return 1
}

// rankKey sorts a new DXCC entity by Club Log rank (smaller = rarer = first),
// an unranked one after every ranked one, and a station new only in a
// district or zone slot after those.
func rankKey(i Item) int {
	switch {
	case i.Val.NewDXCC == nil:
		return 1 << 30
	case i.Rank <= 0:
		return 1<<30 - 1
	}
	return i.Rank
}
