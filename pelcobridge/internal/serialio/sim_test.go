package serialio

import (
	"testing"
	"time"

	"pelcots/internal/pelco"
)

// readFrames collects up to n decoded frames from p within timeout, ignoring
// raw-byte events (the reader also emits Raw on every read).
func readFrames(t *testing.T, p *Port, n int, timeout time.Duration) []pelco.Frame {
	t.Helper()
	var got []pelco.Frame
	deadline := time.Now().Add(timeout)
	for len(got) < n && time.Now().Before(deadline) {
		select {
		case ev, ok := <-p.Frames():
			if !ok {
				t.Fatal("frames channel closed")
			}
			if ev.Err != nil {
				t.Fatalf("read error: %v", ev.Err)
			}
			if ev.Frame != (pelco.Frame{}) {
				got = append(got, ev.Frame)
			}
		case <-time.After(time.Until(deadline)):
		}
	}
	return got
}

// TestSimSnapAndQuery verifies the emulator snaps to an absolute pan position
// and answers a position query with that same position — the loop the engine's
// poll depends on, without any hardware.
func TestSimSnapAndQuery(t *testing.T) {
	p := OpenSim(SimOptions{Addr: 1})
	defer p.Close()

	if err := p.Send(pelco.SetPan(1, 180)); err != nil {
		t.Fatalf("SetPan: %v", err)
	}
	if err := p.Send(pelco.QueryPan(1)); err != nil {
		t.Fatalf("QueryPan: %v", err)
	}

	frames := readFrames(t, p, 1, time.Second)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1: %+v", len(frames), frames)
	}
	if !frames[0].IsPanResponse() {
		t.Fatalf("not a pan response: %+v", frames[0])
	}
	if got, want := pelco.HundredthsToDeg(frames[0].Word()), 180.0; got != want {
		t.Fatalf("pan = %.2f, want %.2f", got, want)
	}
}

// TestSimTiltClamp verifies a SetTilt above the unit's travel clamps to 90° in
// the readback, matching the real device's 0–90° elevation range.
func TestSimTiltClamp(t *testing.T) {
	p := OpenSim(SimOptions{Addr: 1})
	defer p.Close()

	_ = p.Send(pelco.SetTilt(1, 999))
	_ = p.Send(pelco.QueryTilt(1))

	frames := readFrames(t, p, 1, time.Second)
	if len(frames) != 1 || !frames[0].IsTiltResponse() {
		t.Fatalf("want one tilt response: %+v", frames)
	}
	if got, want := pelco.HundredthsToDeg(frames[0].Word()), 90.0; got != want {
		t.Fatalf("tilt = %.2f, want %.2f (clamped)", got, want)
	}
}

// TestSimJogSteps verifies a jog command moves the emulated pan by JogStep per
// frame — the behavior the engine's closed-loop unwrap path relies on to
// accumulate observed travel.
func TestSimJogSteps(t *testing.T) {
	p := OpenSim(SimOptions{Addr: 1, StartPan: 0, JogStep: 5})
	defer p.Close()

	// Two right-jogs at normal speed → pan advances 10°.
	right := pelco.Jog(1, pelco.Direction{Pan: 1}.Cmd2(), 0x20, 0)
	_ = p.Send(right)
	_ = p.Send(right)
	_ = p.Send(pelco.QueryPan(1))

	frames := readFrames(t, p, 1, time.Second)
	if len(frames) != 1 || !frames[0].IsPanResponse() {
		t.Fatalf("want one pan response: %+v", frames)
	}
	if got, want := pelco.HundredthsToDeg(frames[0].Word()), 10.0; got != want {
		t.Fatalf("pan = %.2f, want %.2f after two 5° jogs", got, want)
	}
}

// TestSimAddrFilter verifies the emulator ignores commands addressed to a
// different Pelco-D camera address, so a query then yields no response.
func TestSimAddrFilter(t *testing.T) {
	p := OpenSim(SimOptions{Addr: 1})
	defer p.Close()

	_ = p.Send(pelco.SetPan(2, 90)) // addressed to addr 2, not us
	_ = p.Send(pelco.QueryPan(1))   // but this one is addressed to us

	// The SetPan to addr 2 is dropped; the pan query still reports the start
	// position (0°), proving the misaddressed set did not move us.
	frames := readFrames(t, p, 1, time.Second)
	if len(frames) != 1 || !frames[0].IsPanResponse() {
		t.Fatalf("want one pan response: %+v", frames)
	}
	if got := pelco.HundredthsToDeg(frames[0].Word()); got != 0 {
		t.Fatalf("misaddressed set moved us to %.2f; want 0", got)
	}
}

// TestSimIgnoresPresets verifies preset/self-check commands (CMD2 0x03/0x05/
// 0x07) are accepted but ignored by the simulator: they change neither the
// emulated position nor produce a response. This matters because the engine
// sends a DisableSelfCheck (set preset 105) on every connect, and those opcodes
// share low bits with the direction bits — a regression in IsJog would turn
// them into jogs that step the emulated position.
func TestSimIgnoresPresets(t *testing.T) {
	p := OpenSim(SimOptions{Addr: 1, StartPan: 0, JogStep: 5})
	defer p.Close()

	// Seed a known position.
	_ = p.Send(pelco.SetPan(1, 100))
	_ = p.Send(pelco.QueryPan(1))
	seed := readFrames(t, p, 1, time.Second)
	if len(seed) != 1 || pelco.HundredthsToDeg(seed[0].Word()) != 100 {
		t.Fatalf("seed pan = %.2f, want 100", pelco.HundredthsToDeg(seed[0].Word()))
	}

	// Send the disable-self-check frame and a couple of presets, then a query.
	_ = p.Send(pelco.DisableSelfCheck(1))
	_ = p.Send(pelco.GoToPreset(1, 105))
	_ = p.Send(pelco.ClearPreset(1, 8))
	_ = p.Send(pelco.QueryPan(1))

	// Only the query may produce a response, and the position must be unchanged.
	frames := readFrames(t, p, 1, time.Second)
	if len(frames) != 1 || !frames[0].IsPanResponse() {
		t.Fatalf("presets should not produce responses; got %+v", frames)
	}
	if got := pelco.HundredthsToDeg(frames[0].Word()); got != 100 {
		t.Fatalf("preset moved position to %.2f; want 100 (presets must be ignored)", got)
	}
}

// TestSimPelcoPAdaptive verifies the emulator speaks Pelco-P like the adaptive
// real head: a P-framed set + query (8-byte 0xA0/0xAF envelopes) moves the
// emulated position and the response comes back tagged — and framed — as
// Pelco-P, while the framing reader decodes it transparently.
func TestSimPelcoPAdaptive(t *testing.T) {
	p := OpenSim(SimOptions{Addr: 1})
	defer p.Close()

	set := pelco.SetPan(1, 180)
	set.Proto = pelco.ProtocolP
	query := pelco.QueryPan(1)
	query.Proto = pelco.ProtocolP
	if err := p.Send(set); err != nil {
		t.Fatalf("P SetPan: %v", err)
	}
	if err := p.Send(query); err != nil {
		t.Fatalf("P QueryPan: %v", err)
	}

	frames := readFrames(t, p, 1, time.Second)
	if len(frames) != 1 || !frames[0].IsPanResponse() {
		t.Fatalf("want one pan response: %+v", frames)
	}
	if frames[0].Proto != pelco.ProtocolP {
		t.Fatalf("response proto = %v, want p (sim must echo the query's protocol)", frames[0].Proto)
	}
	if got, want := pelco.HundredthsToDeg(frames[0].Word()), 180.0; got != want {
		t.Fatalf("pan = %.2f, want %.2f", got, want)
	}
	if wb := frames[0].Bytes(); len(wb) != pelco.PFrameLen || wb[0] != pelco.STX {
		t.Fatalf("wire frame % X is not a Pelco-P envelope", wb)
	}

	// A D-framed query after P traffic still works: the reader resyncs on 0xFF
	// and the sim answers in D.
	_ = p.Send(pelco.QueryPan(1))
	frames = readFrames(t, p, 1, time.Second)
	if len(frames) != 1 || frames[0].Proto != pelco.ProtocolD || pelco.HundredthsToDeg(frames[0].Word()) != 180.0 {
		t.Fatalf("D query after P traffic: %+v", frames)
	}
}
