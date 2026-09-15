package bridge

// Cmd-path unit tests: the gates and pure helpers at unit level (the
// end-to-end behaviors are covered in bridge_test.go over the fake radio).

import (
	"context"
	"strings"
	"testing"
	"time"

	"icom9700-radio-bridge/internal/civ"
)

// TestDefaultArmGateStrings pins the v1 admission rule and the exact R10
// rejection strings, armed checked before live.
func TestDefaultArmGateStrings(t *testing.T) {
	if err := defaultArmGate(true, true); err != nil {
		t.Errorf("armed+live must pass, got %v", err)
	}
	if err := defaultArmGate(false, false); err == nil || err.Error() != ErrPTTNotArmed {
		t.Errorf("unarmed = %v, want exactly %q", err, ErrPTTNotArmed)
	}
	if err := defaultArmGate(false, true); err == nil || err.Error() != ErrPTTNotArmed {
		t.Errorf("unarmed-live = %v, want exactly %q (armed checked first)", err, ErrPTTNotArmed)
	}
	if err := defaultArmGate(true, false); err == nil || err.Error() != ErrPTTNotLive {
		t.Errorf("armed-not-live = %v, want exactly %q", err, ErrPTTNotLive)
	}
}

// TestPTTWhileArmedButNotLive covers the second R10 string end-to-end: an
// arm whose connect demand is abandoned (ctx dies first) leaves the permit
// in place while no session is live — ptt then reports the session, not the
// permit.
func TestPTTWhileArmedButNotLive(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	// Radio silent: the arm-driven connect series runs; the cmd ctx dies
	// before it concludes, so the permit stays but no session comes up.
	fr.StopAnswering()
	dead, cancel := contextWithDeadline(time.Now().Add(20 * time.Millisecond))
	defer cancel()
	runOnWorker(b, func() { _ = b.Execute(dead, []byte(`{"action":"arm"}`)) })
	waitFor(t, 2*time.Second, "arm permit set without a session", func() bool {
		m, ok := tryState(fake, st)
		return ok && m["armed"] == true
	})

	deliverCmd(b, `{"action":"ptt","value":"on"}`)
	if got := lastError(t, fake, st); got != ErrPTTNotLive {
		t.Fatalf("error = %q, want exactly %q", got, ErrPTTNotLive)
	}
	for _, f := range fr.CivFramesReceived() {
		if len(f) >= 5 && f[4] == civ.CmdStatus {
			t.Fatalf("PTT frame reached the radio without a live session: % x", f)
		}
	}
}

// TestCmdValueValidation walks the local (pre-radio) rejection paths.
func TestCmdValueValidation(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newTestBridge(t, fr, newRadioModel())
	st := topicState(b.opts)

	cases := []struct {
		payload string
		want    string
	}{
		{`{"action":"set_freq","value":"abc"}`, "freq rejected: invalid value"},
		{`{"action":"set_freq","vfo":"side","value":"433400000"}`, "freq rejected: vfo must be"},
		{`{"action":"set_freq","value":"999999999"}`, "freq rejected: out of band for main"},
		{`{"action":"set_mode","value":"rtty"}`, "mode rejected: unsupported"},
		{`{"action":"set_data","value":"maybe"}`, "data rejected: invalid value"},
		{`{"action":"set_preamp","value":"9"}`, "preamp rejected: invalid value"},
		{`{"action":"set_attenuator","value":"10dB"}`, "attenuator rejected: invalid value"},
		{`{"action":"set_power","value":"300"}`, "power rejected: invalid value"},
		{`{"action":"ptt","value":"key"}`, "ptt rejected: invalid value"},
	}
	for _, tc := range cases {
		deliverCmd(b, tc.payload)
		if got := lastError(t, fake, st); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s → error %q, want prefix %q", tc.payload, got, tc.want)
		}
		if len(fr.LoginAttempts()) != 0 {
			t.Fatalf("%s drove login attempts — local validation must never touch the radio", tc.payload)
		}
	}
}

// TestUnparseableTsRejectedAndFutureTsTolerated exercises the ts gate
// tolerance window (stale/far-future dropped, slight skew tolerated).
func TestUnparseableTsRejectedAndFutureTsTolerated(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newTestBridge(t, fr, newRadioModel())
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"set_freq","value":"433400000","ts":"not-a-time"}`)
	if got := lastError(t, fake, st); !strings.HasPrefix(got, "cmd ts unparseable") {
		t.Errorf("error = %q, want an unparseable-ts rejection", got)
	}

	// 5 s in the future: within the ±30 s tolerance, so the cmd dispatches
	// (against the dead radio the connect fails — but NOT with a stale-cmd
	// rejection).
	future := time.Now().Add(5 * time.Second).UTC().Format(time.RFC3339)
	deliverCmd(b, `{"action":"set_freq","value":"433400000","ts":"`+future+`"}`)
	got := lastError(t, fake, st)
	if strings.HasPrefix(got, "stale cmd") || strings.HasPrefix(got, "cmd ts unparseable") {
		t.Errorf("future ts within tolerance rejected anyway: %q", got)
	}
}

// TestClipCmdErr bounds rejection strings at cmdErrMax runes.
func TestClipCmdErr(t *testing.T) {
	long := strings.Repeat("x", cmdErrMax+50)
	got := clipCmdErr(long)
	if len([]rune(got)) != cmdErrMax+1 { // +1: the ellipsis
		t.Fatalf("clipped length = %d, want %d (+ellipsis)", len([]rune(got)), cmdErrMax)
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("clip must append the ellipsis")
	}
	if clipCmdErr("short") != "short" {
		t.Error("short messages must pass through untouched")
	}
}

// TestVfoScopedFrames pins the bracket sequence: no flip → the bare apply;
// a flip → select, 07 D2 read (the reply-side scoping gate), apply, select
// back, 07 D2 read.
func TestVfoScopedFrames(t *testing.T) {
	c := civ.NewCodec()
	apply := [][]byte{{0xfe, 0xfe, 0xa2, 0xe0, 0xAA, 0x00, 0xfd}} // a fake 7-byte command frame

	same := vfoScopedFrames(c, civ.VfoMain, civ.VfoMain, apply)
	if len(same) != 1 {
		t.Fatalf("no-flip sequence = %d frames, want just the apply", len(same))
	}

	flipped := vfoScopedFrames(c, civ.VfoSub, civ.VfoMain, apply)
	want := []struct {
		cmd byte
		sub byte
		has bool
	}{
		{civ.CmdSelectBand, civ.BandSelSub, true},
		{civ.CmdSelectBand, civ.BandSelRead, true},
		{0xAA, 0, false},
		{civ.CmdSelectBand, civ.BandSelMain, true},
		{civ.CmdSelectBand, civ.BandSelRead, true},
	}
	if len(flipped) != len(want) {
		t.Fatalf("flip sequence = %d frames, want %d", len(flipped), len(want))
	}
	for i, w := range want {
		f := flipped[i]
		if f[4] != w.cmd {
			t.Errorf("frame %d cmd = %02x, want %02x", i, f[4], w.cmd)
		}
		if w.has && f[5] != w.sub {
			t.Errorf("frame %d sub = %02x, want %02x", i, f[5], w.sub)
		}
	}
}

// contextWithDeadline is a tiny helper so the tests do not import context
// twice under different spellings.
func contextWithDeadline(d time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(context.Background(), d)
}
