// Package action runs what the operator asks of a row: tune the radio, put
// the callsign into the loggers, mark the station handled (contest), Skip and
// Undo. Every part reports its own result and a failed part never stops the
// next one. The radio and chase-queue parts are fire-and-forget on the bus, so
// they are confirmed from the next state the bus delivers.
package action

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"chaseq-ui/internal/bus"
	"chaseq-ui/internal/loggers"
	"chaseq-ui/internal/model"
)

// Publisher sends a non-retained MQTT message.
type Publisher interface {
	Publish(topic string, payload []byte) error
}

// Part is one step's outcome.
type Part struct {
	Name   string
	Status loggers.Status
	Detail string
}

// Report is what an action did, part by part.
type Report struct {
	Action string // "act" | "skip" | "undo"
	Call   string
	Parts  []Part
}

// Timing are the confirmation waits.
type Timing struct {
	TuneWait    time.Duration // radio state must reach the target
	ConfirmWait time.Duration // chase state must show the effect
	Poll        time.Duration
}

// DefaultTiming is KTD7's: 2 s for the radio, 5 s for chase-queue.
var DefaultTiming = Timing{TuneWait: 2 * time.Second, ConfirmWait: 5 * time.Second, Poll: 20 * time.Millisecond}

// Radio holds the latest flexbridge state.
type Radio struct {
	mu sync.Mutex
	st bus.RadioState
}

// Set stores a radio state.
func (r *Radio) Set(s bus.RadioState) { r.mu.Lock(); r.st = s; r.mu.Unlock() }

// Get returns the latest radio state.
func (r *Radio) Get() bus.RadioState { r.mu.Lock(); defer r.mu.Unlock(); return r.st }

// Runner runs actions, one at a time.
type Runner struct {
	Model    *model.Model
	Radio    *Radio
	Pub      Publisher
	ChaseCmd string
	RadioCmd string
	Loggers  []loggers.Sender
	Timing   Timing

	mu sync.Mutex
}

func (r *Runner) timing() Timing {
	if r.Timing.Poll <= 0 {
		return DefaultTiming
	}
	return r.Timing
}

func part(name string, s loggers.Status, detail string) Part { return Part{name, s, detail} }

// Act is Enter or a click on rowID: tune, then the loggers once the radio is
// there, then (contest) handled.
func (r *Runner) Act(ctx context.Context, rowID int64) Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep := Report{Action: "act"}
	row, ok := r.Model.Find(rowID)
	if !ok {
		rep.Parts = append(rep.Parts, part("List", loggers.Failed, "the station left the list"))
		return rep
	}
	rep.Call = row.Call
	if refuse := r.refusal(); refuse != "" {
		rep.Parts = append(rep.Parts, part("List", loggers.Failed, refuse))
		return rep
	}
	rep.Parts = append(rep.Parts, r.tune(ctx, row))
	for _, l := range r.Loggers {
		if row.Call == "" {
			rep.Parts = append(rep.Parts, part(l.Name(), loggers.Skipped, "no callsign"))
			continue
		}
		res := l.SendCall(ctx, row.Call)
		rep.Parts = append(rep.Parts, part(l.Name(), res.Status, res.Detail))
	}
	if activity, _ := r.Model.Activity(); activity == bus.ActivityContest {
		rep.Parts = append(rep.Parts, r.chaseCmd("handled", row))
	}
	return rep
}

// Skip hides rowID in chase-queue.
func (r *Runner) Skip(ctx context.Context, rowID int64) Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep := Report{Action: "skip"}
	row, ok := r.Model.Find(rowID)
	if !ok {
		rep.Parts = append(rep.Parts, part("List", loggers.Failed, "the station left the list"))
		return rep
	}
	rep.Call = row.Call
	if refuse := r.refusal(); refuse != "" {
		rep.Parts = append(rep.Parts, part("List", loggers.Failed, refuse))
		return rep
	}
	rep.Parts = append(rep.Parts, r.chaseCmd("skip", row))
	return rep
}

// Undo reverses chase-queue's last Skip or handled mark.
func (r *Runner) Undo(ctx context.Context) Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep := Report{Action: "undo"}
	if refuse := r.refusal(); refuse != "" {
		rep.Parts = append(rep.Parts, part("List", loggers.Failed, refuse))
		return rep
	}
	beforeLabel, beforeIDs := r.Model.UndoState()
	if beforeLabel == "" {
		rep.Parts = append(rep.Parts, part("chase-queue", loggers.NotConfirmed, "nothing to undo"))
		return rep
	}
	if err := r.Pub.Publish(r.ChaseCmd, bus.CmdPayload("undo", "")); err != nil {
		rep.Parts = append(rep.Parts, part("chase-queue", loggers.Failed, err.Error()))
		return rep
	}
	// Undo shows as a changed label or a row that was not listed before.
	if r.waitFor(r.timing().ConfirmWait, func() bool {
		label, ids := r.Model.UndoState()
		if label != beforeLabel {
			return true
		}
		for id := range ids {
			if !beforeIDs[id] {
				return true
			}
		}
		return false
	}) {
		rep.Parts = append(rep.Parts, part("chase-queue", loggers.OK, ""))
	} else {
		rep.Parts = append(rep.Parts, part("chase-queue", loggers.NotConfirmed, "list unchanged after 5 s (nothing to undo?)"))
	}
	return rep
}

func (r *Runner) refusal() string {
	v := r.Model.Snapshot()
	if !v.ActionsEnabled {
		return v.Offline
	}
	return ""
}

// tune sends set_freq then set_mode, and waits for the radio state.
func (r *Runner) tune(ctx context.Context, row model.Row) Part {
	if !row.Tunable() {
		return part("Radio", loggers.Skipped, "no frequency")
	}
	if err := r.Pub.Publish(r.RadioCmd, bus.CmdPayload("set_freq", fmt.Sprint(row.FreqHz))); err != nil {
		return part("Radio", loggers.Failed, err.Error())
	}
	note := ""
	// set_mode targets the TX slice, so in split it would change the wrong one.
	// flexbridge accepts only cw/usb/lsb: data, am, fm and unknown leave the mode.
	switch {
	case !settableMode(row.Mode):
		note = "mode unchanged"
		if row.Mode != "" {
			note += " (" + row.Mode + ")"
		}
	case r.Radio.Get().Split:
		note = "split: mode unchanged"
	default:
		if err := r.Pub.Publish(r.RadioCmd, bus.CmdPayload("set_mode", row.Mode)); err != nil {
			return part("Radio", loggers.Failed, err.Error())
		}
	}
	if st := r.Radio.Get(); st.Split && st.RxFreqHz == 0 {
		// flexbridge reports no slice-A frequency: nothing to confirm against.
		if note != "" {
			note += "; "
		}
		return part("Radio", loggers.OK, note+"slice A frequency not reported")
	}
	if r.waitFor(r.timing().TuneWait, func() bool { return near(r.Radio.Get().Tuned(), row.FreqHz) }) {
		return part("Radio", loggers.OK, note)
	}
	if note != "" {
		note = "; " + note
	}
	return part("Radio", loggers.NotConfirmed, "radio did not report the new frequency in time"+note)
}

// chaseCmd sends skip/handled and confirms it: the row is gone from the next
// chase state AND the undo label shows chase-queue took a mark (a row that
// merely expired, or a dropped command, leaves the label alone).
func (r *Runner) chaseCmd(action string, row model.Row) Part {
	// chase-queue labels the undo entry with the call, or "skip" without one.
	// Repeated marks repeat the label (the same call on two bands), so the
	// confirmation is "row gone and the label is the one this mark sets".
	want := row.Call
	if want == "" {
		want = "skip"
	}
	effect := func() bool {
		label, _ := r.Model.UndoState()
		return !r.Model.StillListed(row.RowID) && label == want
	}
	t := bus.Target{RowID: row.RowID, Band: row.Band, Call: row.Call}
	if err := r.Pub.Publish(r.ChaseCmd, bus.CmdPayload(action, bus.TargetValue(t))); err != nil {
		return part("chase-queue", loggers.Failed, err.Error())
	}
	if r.waitFor(r.timing().ConfirmWait, effect) {
		return part("chase-queue", loggers.OK, "")
	}
	return part("chase-queue", loggers.NotConfirmed, "chase-queue did not show the mark after 5 s")
}

func (r *Runner) waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(r.timing().Poll)
	}
}

func near(a, b int64) bool { return math.Abs(float64(a-b)) <= 50 }

// settableMode reports whether flexbridge's set_mode takes the mode.
func settableMode(m string) bool { return m == "cw" || m == "usb" || m == "lsb" }
