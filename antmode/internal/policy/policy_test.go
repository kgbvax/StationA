// SPDX-License-Identifier: AGPL-3.0-or-later

package policy

import (
	"encoding/json"
	"testing"
	"time"

	"codeberg.org/kgbvax/stationa/shared/antmode"

	"antmode/internal/nina"
)

var t0 = time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC)

func w(id string) nina.Warning { return nina.Warning{ID: id, Event: "STURMBÖEN", II: "52"} }

func newP(autoEnter bool) *Policy {
	return New(Config{AutoEnter: autoEnter, ExitHoldoff: 60 * time.Minute}, State{})
}

func TestPresenceOf(t *testing.T) {
	cases := []struct {
		online bool
		phase  string
		want   Presence
	}{
		{true, "starting", Manned}, {true, "running", Manned}, {true, "waking", Manned},
		{true, "idle", Unmanned}, {true, "standby", Unmanned}, {true, "entering_standby", Unmanned},
		{true, "stopping", Unmanned}, {true, "", Unmanned},
		{false, "running", Unmanned}, // powerseq offline → unmanned, even with a stale running
	}
	for _, c := range cases {
		if got := PresenceOf(c.online, c.phase); got != c.want {
			t.Errorf("PresenceOf(%v, %q) = %s, want %s", c.online, c.phase, got, c.want)
		}
	}
}

func TestZeroStateIsNormal(t *testing.T) {
	p := newP(true)
	if p.S.Mode != antmode.Normal || p.S.Source != Manual {
		t.Fatalf("zero state = %+v", p.S)
	}
}

func TestManualSetMode(t *testing.T) {
	p := newP(false)
	r, err := p.SetMode(antmode.Storm, t0, nil)
	if err != nil || !r.ModeChanged || r.From != antmode.Normal {
		t.Fatalf("enter storm: %+v %v", r, err)
	}
	if p.S.Mode != antmode.Storm || p.S.Source != Manual || p.S.Seq != 1 || !p.S.Since.Equal(t0) {
		t.Fatalf("state = %+v", p.S)
	}
	// Same mode: no-op, no seq bump.
	if r, _ := p.SetMode(antmode.Storm, t0.Add(time.Minute), nil); r.ModeChanged || r.Changed || p.S.Seq != 1 {
		t.Fatalf("same-mode set changed something: %+v seq=%d", r, p.S.Seq)
	}
	// storm → maintenance is a new entry (seq bump).
	if r, _ := p.SetMode(antmode.Maintenance, t0.Add(2*time.Minute), nil); !r.ModeChanged || p.S.Seq != 2 {
		t.Fatalf("maintenance: %+v seq=%d", r, p.S.Seq)
	}
	// → normal: no seq bump.
	if r, _ := p.SetMode(antmode.Normal, t0.Add(3*time.Minute), nil); !r.ModeChanged || p.S.Seq != 2 {
		t.Fatalf("normal: %+v seq=%d", r, p.S.Seq)
	}
	if _, err := p.SetMode("hurricane", t0, nil); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

func TestRepark(t *testing.T) {
	p := newP(false)
	if r := p.Repark(); r.Reparked || p.S.Seq != 0 {
		t.Fatal("repark in normal must be ignored")
	}
	_, _ = p.SetMode(antmode.Maintenance, t0, nil)
	if r := p.Repark(); !r.Reparked || p.S.Seq != 2 {
		t.Fatalf("repark: %+v seq=%d", r, p.S.Seq)
	}
}

func TestAutoEnterUnmannedOnly(t *testing.T) {
	act := []nina.Warning{w("dwd.1")}
	p := newP(true)
	// Manned → suggestion, no entry.
	if r := p.Evaluate(t0, act, Manned, false); r.ModeChanged {
		t.Fatal("entered storm while manned")
	}
	if !p.Suggested(act, Manned) {
		t.Fatal("manned: no suggestion")
	}
	// Startup hold → nothing.
	if r := p.Evaluate(t0, act, Unmanned, true); r.ModeChanged {
		t.Fatal("entered during hold")
	}
	// Unmanned → auto storm.
	r := p.Evaluate(t0, act, Unmanned, false)
	if !r.ModeChanged || p.S.Mode != antmode.Storm || p.S.Source != Auto || p.S.Seq != 1 {
		t.Fatalf("auto entry: %+v %+v", r, p.S)
	}
	if p.S.Reason == "" || p.Suggested(act, Unmanned) {
		t.Fatalf("reason %q / suggestion while in storm", p.S.Reason)
	}
}

func TestAutoEnterOffOnlySuggests(t *testing.T) {
	act := []nina.Warning{w("dwd.1")}
	p := newP(false)
	if r := p.Evaluate(t0, act, Unmanned, false); r.ModeChanged {
		t.Fatal("auto_enter=false entered storm")
	}
	if !p.Suggested(act, Unmanned) || !p.Suggested(act, Manned) {
		t.Fatal("auto_enter=false: no suggestion")
	}
	if p.Suggested(nil, Manned) {
		t.Fatal("suggestion without warning")
	}
}

func TestDismissCurrentIdsOnly(t *testing.T) {
	p := newP(false)
	a := []nina.Warning{w("dwd.1")}
	if r := p.Dismiss(a); !r.Changed {
		t.Fatal("dismiss did not change state")
	}
	if p.Suggested(a, Manned) {
		t.Fatal("dismissed id still suggested")
	}
	if !p.Suggested(append(a, w("dwd.2")), Manned) {
		t.Fatal("a new id must suggest again")
	}
	if r := p.Dismiss(a); r.Changed {
		t.Fatal("dismissing twice changed state")
	}
	// A dismissed (not acknowledged) warning still auto-enters when unmanned.
	p2 := newP(true)
	p2.Dismiss(a)
	if r := p2.Evaluate(t0, a, Unmanned, false); !r.ModeChanged {
		t.Fatal("dismissal must not block auto entry")
	}
}

func TestManualNormalAcknowledges(t *testing.T) {
	p := newP(true)
	a := []nina.Warning{w("dwd.1")}
	p.Evaluate(t0, a, Unmanned, false)
	if p.S.Mode != antmode.Storm {
		t.Fatal("setup: not in storm")
	}
	r, _ := p.SetMode(antmode.Normal, t0.Add(time.Minute), a)
	if !r.ModeChanged || p.S.Source != Manual || len(p.S.Acked) != 1 {
		t.Fatalf("manual normal: %+v %+v", r, p.S)
	}
	// Same id, unmanned: no re-entry, no suggestion.
	if r := p.Evaluate(t0.Add(2*time.Minute), a, Unmanned, false); r.ModeChanged {
		t.Fatal("acknowledged warning re-entered storm")
	}
	if p.Suggested(a, Manned) {
		t.Fatal("acknowledged warning suggested")
	}
	// A new (escalated) id enters again.
	esc := append(a, nina.Warning{ID: "dwd.2", Event: "ORKANBÖEN", II: "55"})
	if r := p.Evaluate(t0.Add(3*time.Minute), esc, Unmanned, false); !r.ModeChanged {
		t.Fatal("new warning id did not enter storm")
	}
}

func TestManualNormalWithoutWarningsAcksNothing(t *testing.T) {
	p := newP(true)
	_, _ = p.SetMode(antmode.Storm, t0, nil)
	_, _ = p.SetMode(antmode.Normal, t0, nil)
	if len(p.S.Acked) != 0 {
		t.Fatalf("acked = %v", p.S.Acked)
	}
}

func TestAutoExitCountsOnlySuccessfulPolls(t *testing.T) {
	p := newP(true)
	a := []nina.Warning{w("dwd.1")}
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	p.Evaluate(at(0), a, Unmanned, false)
	p.Poll(at(5), true, a) // still warned
	// Warning gone: the first clean poll starts the clock (counts nothing).
	p.Poll(at(10), true, nil)
	if p.S.QuietFor != 0 {
		t.Fatalf("first clean poll counted %s", p.S.QuietFor)
	}
	if got := p.AutoExitAt(); !got.Equal(at(70)) {
		t.Fatalf("auto_exit_at = %v", got)
	}
	p.Poll(at(40), true, nil) // +30
	// NINA outage 40..120: never counts, never resets.
	p.Poll(at(45), false, nil)
	p.Poll(at(100), false, nil)
	if !p.AutoExitAt().IsZero() {
		t.Fatal("auto_exit_at shown during an outage")
	}
	p.Poll(at(120), true, nil) // first clean after outage: restarts the clock
	if p.S.QuietFor != 30*time.Minute {
		t.Fatalf("quiet after outage = %s, want 30m", p.S.QuietFor)
	}
	if r := p.Poll(at(149), true, nil); r.ModeChanged { // 59 min
		t.Fatal("exited before the holdoff")
	}
	r := p.Poll(at(150), true, nil) // 60 min
	if !r.ModeChanged || p.S.Mode != antmode.Normal || p.S.Source != Auto {
		t.Fatalf("auto exit: %+v %+v", r, p.S)
	}
}

func TestAutoExitWarningResetsQuiet(t *testing.T) {
	p := newP(true)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	p.Evaluate(at(0), []nina.Warning{w("dwd.1")}, Unmanned, false)
	p.Poll(at(5), true, nil)
	p.Poll(at(50), true, nil) // 45 min quiet
	p.Poll(at(55), true, []nina.Warning{w("dwd.2")})
	if p.S.QuietFor != 0 {
		t.Fatal("a new warning did not reset the quiet time")
	}
	p.Poll(at(60), true, nil)
	if r := p.Poll(at(115), true, nil); r.ModeChanged {
		t.Fatal("exited 55 min after the reset")
	}
	if r := p.Poll(at(120), true, nil); !r.ModeChanged {
		t.Fatal("did not exit 60 min after the reset")
	}
}

func TestManualStormAndMaintenanceNeverAutoExit(t *testing.T) {
	for _, m := range []antmode.Mode{antmode.Storm, antmode.Maintenance} {
		p := newP(true)
		_, _ = p.SetMode(m, t0, nil)
		for i := 1; i <= 50; i++ {
			if r := p.Poll(t0.Add(time.Duration(i*5)*time.Minute), true, nil); r.ModeChanged {
				t.Fatalf("%s auto-exited", m)
			}
		}
	}
	// Maintenance is never auto-entered over, nor storm over maintenance.
	p := newP(true)
	_, _ = p.SetMode(antmode.Maintenance, t0, nil)
	if r := p.Evaluate(t0, []nina.Warning{w("dwd.1")}, Unmanned, false); r.ModeChanged {
		t.Fatal("auto storm replaced maintenance")
	}
}

func TestAutoStormThenManualStormKeeps(t *testing.T) {
	p := newP(true)
	p.Evaluate(t0, []nina.Warning{w("dwd.1")}, Unmanned, false)
	// Operator returns and confirms storm: same mode → no-op, source stays
	// auto (set to the current mode is a no-op by contract).
	if r, _ := p.SetMode(antmode.Storm, t0, nil); r.ModeChanged {
		t.Fatal("same-mode set changed the mode")
	}
}

func TestPrune(t *testing.T) {
	p := newP(false)
	p.Dismiss([]nina.Warning{w("dwd.1"), w("dwd.2")})
	_, _ = p.SetMode(antmode.Storm, t0, nil)
	_, _ = p.SetMode(antmode.Normal, t0, []nina.Warning{w("dwd.1"), w("dwd.2")})
	// A failed poll prunes nothing.
	p.Poll(t0, false, nil)
	if len(p.S.Acked) != 2 || len(p.S.Dismissed) != 2 {
		t.Fatal("outage pruned")
	}
	r := p.Poll(t0, true, []nina.Warning{w("dwd.2")})
	if !r.Changed || len(p.S.Acked) != 1 || p.S.Acked[0] != "dwd.2" || len(p.S.Dismissed) != 1 {
		t.Fatalf("prune: %+v %+v", r, p.S)
	}
}

func TestStateJSONRoundTrip(t *testing.T) {
	p := newP(true)
	p.Evaluate(t0, []nina.Warning{w("dwd.1")}, Unmanned, false)
	p.Poll(t0.Add(time.Minute), true, nil)
	p.Poll(t0.Add(11*time.Minute), true, nil)
	p.S.Acked = []string{"b", "a"}
	b, err := json.Marshal(p.S)
	if err != nil {
		t.Fatal(err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	q := New(Config{AutoEnter: true, ExitHoldoff: time.Hour}, s)
	if q.S.Mode != antmode.Storm || q.S.Source != Auto || q.S.QuietFor != 10*time.Minute || q.S.Acked[0] != "a" {
		t.Fatalf("round trip: %+v", q.S)
	}
	// After a restart the gap since the last poll does not count.
	q.Poll(t0.Add(80*time.Minute), true, nil)
	if q.S.QuietFor != 10*time.Minute {
		t.Fatalf("restart gap counted: %s", q.S.QuietFor)
	}
}
