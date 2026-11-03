package action

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"chaseq-ui/internal/bus"
	"chaseq-ui/internal/loggers"
	"chaseq-ui/internal/model"
)

type pub struct {
	mu   sync.Mutex
	msgs []string // topic + " " + payload
	err  error
	onP  func(topic, payload string)
}

func (p *pub) Publish(topic string, payload []byte) error {
	p.mu.Lock()
	p.msgs = append(p.msgs, topic+" "+string(payload))
	f := p.onP
	p.mu.Unlock()
	if f != nil {
		f(topic, string(payload))
	}
	return p.err
}

func (p *pub) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.msgs...)
}

type fakeLogger struct {
	name  string
	res   loggers.Result
	calls []string
	at    func() // called on send (ordering checks)
}

func (f *fakeLogger) Name() string { return f.name }
func (f *fakeLogger) SendCall(_ context.Context, call string) loggers.Result {
	f.calls = append(f.calls, call)
	if f.at != nil {
		f.at()
	}
	return f.res
}

var fast = Timing{TuneWait: 120 * time.Millisecond, ConfirmWait: 120 * time.Millisecond, Poll: 5 * time.Millisecond}

type rig struct {
	r   *Runner
	m   *model.Model
	pub *pub
	rad *Radio
	l4  *fakeLogger
	dx  *fakeLogger
}

func newRig(activity string, rows ...bus.Ranked) *rig {
	m := model.New(nil)
	m.SetChase(bus.ChaseState{Activity: activity, Ranked: rows})
	p := &pub{}
	rad := &Radio{}
	l4 := &fakeLogger{name: "Log4OM", res: loggers.Result{Status: loggers.OK}}
	dx := &fakeLogger{name: "DXLog", res: loggers.Result{Status: loggers.OK}}
	r := &Runner{Model: m, Radio: rad, Pub: p, ChaseCmd: "t/chase/cmd", RadioCmd: "t/radio/cmd",
		Loggers: []loggers.Sender{l4, dx}, Timing: fast}
	return &rig{r, m, p, rad, l4, dx}
}

func rk(id int64, call, band string) bus.Ranked {
	r := bus.Ranked{RowID: id, Call: call, Band: band, Needed: true}
	switch id {
	case 7:
		r.FreqHz, r.Mode = 14231500, "usb"
	case 8:
		r.FreqHz, r.Mode = 7074000, "lsb"
	case 9:
		r.FreqHz, r.Mode = 5357000, "usb"
	}
	return r
}

func status(rep Report, name string) loggers.Status {
	for _, p := range rep.Parts {
		if p.Name == name {
			return p.Status
		}
	}
	return "missing"
}

// A radio that follows set_freq immediately.
func followRadio(g *rig, hz int64) {
	g.pub.onP = func(topic, payload string) {
		if strings.Contains(payload, `"set_freq"`) {
			g.rad.Set(bus.RadioState{FreqHz: hz})
		}
	}
}

func TestActContestTunesFillsAndMarksHandled(t *testing.T) { // AE4
	g := newRig(bus.ActivityContest, rk(7, "DL1ABC", "20m"))
	followRadio(g, 14231500)
	// chase-queue removes the row once it gets "handled".
	prev := g.pub.onP
	g.pub.onP = func(topic, payload string) {
		prev(topic, payload)
		if strings.Contains(payload, `"handled"`) {
			g.m.SetChase(bus.ChaseState{Activity: bus.ActivityContest, Undo: "DL1ABC"})
		}
	}
	rep := g.r.Act(context.Background(), 7)
	msgs := g.pub.all()
	if len(msgs) != 3 || !strings.Contains(msgs[0], `"set_freq","value":"14231500"`) ||
		!strings.Contains(msgs[1], `"set_mode","value":"usb"`) || !strings.Contains(msgs[2], `"handled"`) ||
		!strings.Contains(msgs[2], `\"call\":\"DL1ABC\"`) {
		t.Fatalf("published = %q", msgs)
	}
	for _, n := range []string{"Radio", "Log4OM", "DXLog", "chase-queue"} {
		if status(rep, n) != loggers.OK {
			t.Errorf("%s = %v (%+v)", n, status(rep, n), rep.Parts)
		}
	}
}

func TestActDXPublishesNoHandled(t *testing.T) {
	g := newRig(bus.ActivityDX, rk(7, "EA8XYZ", "20m"))
	followRadio(g, 14231500)
	rep := g.r.Act(context.Background(), 7)
	for _, m := range g.pub.all() {
		if strings.Contains(m, "handled") {
			t.Errorf("DX activity published %s", m)
		}
	}
	if status(rep, "chase-queue") != "missing" {
		t.Errorf("DX report has a chase-queue part: %+v", rep.Parts)
	}
}

func TestActRowWithoutCallTunesOnly(t *testing.T) { // AE3
	g := newRig(bus.ActivityDX, rk(7, "", "20m"))
	followRadio(g, 14231500)
	rep := g.r.Act(context.Background(), 7)
	if len(g.l4.calls)+len(g.dx.calls) != 0 {
		t.Errorf("loggers were sent calls: %v %v", g.l4.calls, g.dx.calls)
	}
	if status(rep, "Radio") != loggers.OK || status(rep, "Log4OM") != loggers.Skipped {
		t.Errorf("parts = %+v", rep.Parts)
	}
}

func TestActOneLoggerFails(t *testing.T) { // AE2
	g := newRig(bus.ActivityDX, rk(7, "EA8XYZ", "20m"))
	followRadio(g, 14231500)
	g.dx.res = loggers.Result{Status: loggers.Failed, Detail: "not reached"}
	rep := g.r.Act(context.Background(), 7)
	if status(rep, "Log4OM") != loggers.OK || status(rep, "DXLog") != loggers.Failed || status(rep, "Radio") != loggers.OK {
		t.Errorf("parts = %+v", rep.Parts)
	}
}

func TestModeFromRow(t *testing.T) {
	for _, c := range []struct{ mode, want string }{{"lsb", "lsb"}, {"cw", "cw"}, {"usb", "usb"}, {"data", ""}, {"am", ""}, {"fm", ""}, {"", ""}} {
		row := rk(8, "K1A", "40m")
		row.Mode = c.mode
		g := newRig(bus.ActivityDX, row)
		followRadio(g, 7074000)
		g.r.Act(context.Background(), 8)
		got := ""
		for _, m := range g.pub.all() {
			if strings.Contains(m, `"set_mode"`) {
				got = m
			}
		}
		if (c.want == "") != (got == "") || (c.want != "" && !strings.Contains(got, `"value":"`+c.want+`"`)) {
			t.Errorf("mode %q published %q, want set_mode %q", c.mode, got, c.want)
		}
	}
}

func TestSplitSendsNoSetMode(t *testing.T) {
	g := newRig(bus.ActivityDX, rk(7, "EA8XYZ", "20m"))
	g.rad.Set(bus.RadioState{Split: true, FreqHz: 14105000, RxFreqHz: 14100000})
	g.pub.onP = func(_, payload string) {
		if strings.Contains(payload, `"set_freq"`) {
			g.rad.Set(bus.RadioState{Split: true, FreqHz: 14105000, RxFreqHz: 14231500})
		}
	}
	rep := g.r.Act(context.Background(), 7)
	for _, m := range g.pub.all() {
		if strings.Contains(m, "set_mode") {
			t.Errorf("split published %s", m)
		}
	}
	if status(rep, "Radio") != loggers.OK || !strings.Contains(rep.Parts[0].Detail, "split: mode unchanged") {
		t.Errorf("radio part = %+v", rep.Parts[0])
	}
}

func TestLoggersWaitForTheTune(t *testing.T) {
	g := newRig(bus.ActivityDX, rk(7, "EA8XYZ", "20m"))
	var tunedWhenSent bool
	g.l4.at = func() { tunedWhenSent = g.rad.Get().Tuned() == 14231500 }
	g.pub.onP = func(_, payload string) {
		if strings.Contains(payload, `"set_freq"`) {
			go func() { time.Sleep(60 * time.Millisecond); g.rad.Set(bus.RadioState{FreqHz: 14231500}) }()
		}
	}
	g.r.Act(context.Background(), 7)
	if !tunedWhenSent {
		t.Error("Log4OM was sent the call before the radio reached the target")
	}
}

func TestRadioNeverArrivesStillSendsCalls(t *testing.T) {
	g := newRig(bus.ActivityDX, rk(7, "EA8XYZ", "20m"))
	rep := g.r.Act(context.Background(), 7)
	if status(rep, "Radio") != loggers.NotConfirmed || len(g.l4.calls) != 1 || len(g.dx.calls) != 1 {
		t.Errorf("parts = %+v calls=%v %v", rep.Parts, g.l4.calls, g.dx.calls)
	}
}

func TestHandledStillListedIsNotConfirmed(t *testing.T) {
	g := newRig(bus.ActivityContest, rk(7, "DL1ABC", "20m"))
	followRadio(g, 14231500)
	rep := g.r.Act(context.Background(), 7)
	if status(rep, "chase-queue") != loggers.NotConfirmed {
		t.Errorf("parts = %+v", rep.Parts)
	}
}

func TestNoFrequencySkipsRadio(t *testing.T) {
	g := newRig(bus.ActivityDX, rk(99, "NOFREQ", "20m"))
	rep := g.r.Act(context.Background(), 99)
	if status(rep, "Radio") != loggers.Skipped || len(g.l4.calls) != 1 {
		t.Errorf("parts = %+v", rep.Parts)
	}
}

func TestDisabledPublishesNothing(t *testing.T) { // AE6
	g := newRig(bus.ActivityDX, rk(7, "EA8XYZ", "20m"))
	g.m.SetStatus(false)
	for _, rep := range []Report{g.r.Act(context.Background(), 7), g.r.Skip(context.Background(), 7), g.r.Undo(context.Background())} {
		if status(rep, "List") != loggers.Failed {
			t.Errorf("report = %+v", rep)
		}
	}
	if len(g.pub.all()) != 0 || len(g.l4.calls) != 0 {
		t.Errorf("published %q, calls %v", g.pub.all(), g.l4.calls)
	}
}

func TestSkipAndUndo(t *testing.T) {
	g := newRig(bus.ActivityDX, rk(7, "EA8XYZ", "20m"), rk(2, "B2", "20m"))
	g.pub.onP = func(_, payload string) {
		if strings.Contains(payload, `"skip"`) {
			g.m.SetChase(bus.ChaseState{Activity: "dx", Undo: "EA8XYZ", Ranked: []bus.Ranked{rk(2, "B2", "20m")}})
		}
	}
	rep := g.r.Skip(context.Background(), 7)
	m := g.pub.all()
	if len(m) != 1 || !strings.Contains(m[0], `"skip"`) || !strings.Contains(m[0], `\"row_id\":7`) || status(rep, "chase-queue") != loggers.OK {
		t.Fatalf("skip: %q %+v", m, rep.Parts)
	}
	g.pub.onP = func(_, payload string) {
		if strings.Contains(payload, `"undo"`) {
			g.m.SetChase(bus.ChaseState{Activity: "dx", Ranked: []bus.Ranked{rk(7, "EA8XYZ", "20m"), rk(2, "B2", "20m")}})
		}
	}
	rep = g.r.Undo(context.Background())
	m = g.pub.all()
	if m[1] != `t/chase/cmd {"action":"undo","value":""}` || status(rep, "chase-queue") != loggers.OK {
		t.Errorf("undo: %q %+v", m, rep.Parts)
	}
}

func TestPublishError(t *testing.T) {
	g := newRig(bus.ActivityDX, rk(7, "EA8XYZ", "20m"))
	g.pub.err = errors.New("broker down")
	rep := g.r.Act(context.Background(), 7)
	if status(rep, "Radio") != loggers.Failed || len(g.l4.calls) != 1 {
		t.Errorf("parts = %+v", rep.Parts)
	}
}

func TestRowLeftTheList(t *testing.T) {
	g := newRig(bus.ActivityDX, rk(7, "EA8XYZ", "20m"))
	rep := g.r.Act(context.Background(), 12345)
	if status(rep, "List") != loggers.Failed || len(g.pub.all()) != 0 {
		t.Errorf("parts = %+v", rep.Parts)
	}
}

func TestUndoWithNothingToUndoPublishesNothing(t *testing.T) {
	g := newRig(bus.ActivityDX, rk(7, "EA8XYZ", "20m"))
	rep := g.r.Undo(context.Background())
	if len(g.pub.all()) != 0 || status(rep, "chase-queue") != loggers.NotConfirmed {
		t.Errorf("published %q, parts %+v", g.pub.all(), rep.Parts)
	}
}

func TestSkipNeedsTheUndoMarkNotJustTheRowGone(t *testing.T) {
	g := newRig(bus.ActivityDX, rk(7, "EA8XYZ", "20m"), rk(2, "B2", "20m"))
	// The row expires on its own but chase-queue never took the skip.
	g.pub.onP = func(_, payload string) {
		if strings.Contains(payload, `"skip"`) {
			g.m.SetChase(bus.ChaseState{Activity: "dx", Ranked: []bus.Ranked{rk(2, "B2", "20m")}})
		}
	}
	rep := g.r.Skip(context.Background(), 7)
	if status(rep, "chase-queue") != loggers.NotConfirmed {
		t.Errorf("parts = %+v", rep.Parts)
	}
}

func TestUndoNotConfirmedByUnrelatedListChange(t *testing.T) {
	g := newRig(bus.ActivityDX, rk(7, "EA8XYZ", "20m"))
	g.m.SetChase(bus.ChaseState{Activity: "dx", Undo: "EA8XYZ", Ranked: []bus.Ranked{rk(7, "EA8XYZ", "20m")}})
	// Same undo label, one row leaves: not an undo.
	g.pub.onP = func(_, payload string) {
		if strings.Contains(payload, `"undo"`) {
			g.m.SetChase(bus.ChaseState{Activity: "dx", Undo: "EA8XYZ"})
		}
	}
	if rep := g.r.Undo(context.Background()); status(rep, "chase-queue") != loggers.NotConfirmed {
		t.Errorf("parts = %+v", rep.Parts)
	}
}

func TestSplitWithoutSliceAFrequencyIsNotAFalseTimeout(t *testing.T) {
	g := newRig(bus.ActivityDX, rk(7, "EA8XYZ", "20m"))
	g.rad.Set(bus.RadioState{Split: true, FreqHz: 14105000}) // no rx_freq_hz
	rep := g.r.Act(context.Background(), 7)
	if status(rep, "Radio") != loggers.OK || !strings.Contains(rep.Parts[0].Detail, "slice A frequency not reported") {
		t.Errorf("radio part = %+v", rep.Parts[0])
	}
}

// The same call on a second band repeats the undo label; the skip is still confirmed.
func TestSkipConfirmedWhenUndoLabelRepeats(t *testing.T) {
	g := newRig(bus.ActivityDX, rk(7, "K1ABC", "40m"))
	g.m.SetChase(bus.ChaseState{Activity: "dx", Undo: "K1ABC", Ranked: []bus.Ranked{rk(7, "K1ABC", "40m")}})
	g.pub.onP = func(_, payload string) {
		if strings.Contains(payload, `"skip"`) {
			g.m.SetChase(bus.ChaseState{Activity: "dx", Undo: "K1ABC"})
		}
	}
	if rep := g.r.Skip(context.Background(), 7); status(rep, "chase-queue") != loggers.OK {
		t.Errorf("parts = %+v", rep.Parts)
	}
}

func TestSkipOfRowWithoutCallExpectsSkipLabel(t *testing.T) {
	g := newRig(bus.ActivityDX, rk(9, "", "20m"))
	g.pub.onP = func(_, payload string) {
		if strings.Contains(payload, `"skip"`) {
			g.m.SetChase(bus.ChaseState{Activity: "dx", Undo: "skip"})
		}
	}
	if rep := g.r.Skip(context.Background(), 9); status(rep, "chase-queue") != loggers.OK {
		t.Errorf("parts = %+v", rep.Parts)
	}
}
