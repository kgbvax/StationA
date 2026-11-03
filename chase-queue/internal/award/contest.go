package award

import "chase-queue/internal/contest"

// Contest wraps the contest model as an award of kind "contest": the
// multiplier slots of the definition are the award's slots and the contest
// log is its history. It is active while a contest instance runs.
type Contest struct {
	Model *contest.Model
	Held  *contest.Held
}

// Evaluate values the call on the band. The raw Valuation is returned too:
// the queue ranks by its NewSlots and entity.
func (c Contest) Evaluate(call, band string) ([]Hit, contest.Valuation) {
	v := c.Model.Value(call, band, c.Held)
	var hits []Hit
	for _, sv := range v.Slots {
		h := Hit{Award: CodeContest, Key: sv.Name}
		if sc := c.scope(sv.Slot); sc == contest.ScopeBand || sc == contest.ScopeBandM {
			h.Band = band
		}
		switch sv.State {
		case contest.StateNew:
			h.Status, h.Needed, h.Fresh = Unworked, true, true
		case contest.StateHeld:
			h.Status = WorkedUnconfirmed
		case contest.StateUnknown:
			h.Status, h.Key = Unknown, ""
		default: // the slot does not apply to this call
			continue
		}
		hits = append(hits, h)
	}
	return hits, v
}

func (c Contest) scope(index int) contest.Scope {
	for _, s := range c.Model.Def.Slots {
		if s.Index == index {
			return s.Scope
		}
	}
	return contest.ScopeBand
}
