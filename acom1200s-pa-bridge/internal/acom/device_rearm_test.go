package acom

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.bug.st/serial"
)

// Tests for the frame-keyed re-arm and the keep-the-port-open outage
// handling. Live incident 2026-10-18: the amp was powered on 0.5 s after the
// bridge sent enable-telemetry to the still-dead amp; one re-send went out 4 s
// into its boot, then bytes kept arriving, the byte-keyed 5 s re-send never
// fired again and the amp was not armed for minutes.

// setRearm compresses the re-arm knobs (and silenceLimit) for a test.
func setRearm(t *testing.T, after, every, silence time.Duration) {
	t.Helper()
	oldAfter, oldEvery, oldSilence := rearmAfter, rearmEvery, silenceLimit
	rearmAfter, rearmEvery, silenceLimit = after, every, silence
	t.Cleanup(func() { rearmAfter, rearmEvery, silenceLimit = oldAfter, oldEvery, oldSilence })
}

// telemetryFrame builds a valid 72-byte 0x2F telemetry frame with the given
// raw mode byte and band index.
func telemetryFrame(mode, band byte) []byte {
	f := make([]byte, 72)
	f[0], f[1], f[2], f[3] = StartByte, MsgTelemetry, 72, mode
	f[66] = 0xFF // no fault
	f[69] = band
	f[71] = calculateChecksum(f[:71])
	return f
}

// countEnables counts enable-telemetry frames among the writes.
func countEnables(writes [][]byte) int {
	n := 0
	for _, w := range writes {
		if len(w) >= 2 && w[0] == StartByte && w[1] == MsgEnableAuto {
			n++
		}
	}
	return n
}

// readEvery returns a readFn that sleeps period, then delivers next().
func readEvery(period time.Duration, next func() []byte) func(p []byte) (int, error) {
	return func(p []byte) (int, error) {
		time.Sleep(period)
		b := next()
		return copy(p, b), nil
	}
}

// startRun opens d on port and runs it in the background. The returned stop
// cancels Run and fails the test if Run does not return promptly.
func startRun(t *testing.T, d *Device, port *mockPort, onObs func(Observation)) (done <-chan error, stop func()) {
	t.Helper()
	d.open = func() (serial.Port, error) { return port, nil }
	if err := d.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- d.Run(ctx, onObs) }()
	return ch, func() {
		cancel()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after ctx cancel")
		}
	}
}

func TestRunRearmsWhileGarbageFlows(t *testing.T) {
	setRearm(t, 20*time.Millisecond, 20*time.Millisecond, 150*time.Millisecond)
	port := &mockPort{readFn: readEvery(2*time.Millisecond, func() []byte { return []byte{0x00, 0x13, 0x37} })}
	d := New("/dev/x", 300, false, noopLog{})
	var downs atomic.Int32
	d.SetLinkDownHandler(func(string) { downs.Add(1) })
	done, stop := startRun(t, d, port, nil)

	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Run returned on a healthy port carrying garbage: %v", err)
	default:
	}
	stop() // the rx counters belong to the Run goroutine
	if n := countEnables(port.written()); n < 3 {
		t.Fatalf("enable re-sent %d times in total while only garbage arrived, want >= 3", n)
	}
	if n := downs.Load(); n != 1 {
		t.Fatalf("link-down reported %d times, want exactly 1", n)
	}
	if d.rxDiscarded == 0 {
		t.Fatal("discarded bytes not counted")
	}
}

func TestRunRearmsWhileOnlyNonTelemetryFrames(t *testing.T) {
	setRearm(t, 20*time.Millisecond, 20*time.Millisecond, 10*time.Second)
	ack := []byte{StartByte, MsgAck, 0x05, MsgTelemetry}
	ack = append(ack, calculateChecksum(ack))
	short := []byte{StartByte, MsgTelemetry, 0x06, 0x60, 0x00}
	short = append(short, calculateChecksum(short))
	var i atomic.Int32
	port := &mockPort{readFn: readEvery(2*time.Millisecond, func() []byte {
		if i.Add(1)%2 == 0 {
			return ack
		}
		return short
	})}
	d := New("/dev/x", 300, false, noopLog{})
	_, stop := startRun(t, d, port, nil)
	time.Sleep(200 * time.Millisecond)
	stop()

	writes := port.written()
	if n := countEnables(writes); n < 3 {
		t.Fatalf("enable re-sent %d times in total with only non-telemetry frames, want >= 3", n)
	}
	acks := 0
	for _, w := range writes {
		if w[1] == MsgAck {
			acks++
		}
	}
	if acks == 0 {
		t.Fatal("valid non-telemetry frames were not ACKed")
	}
	if d.rxIgnored == 0 {
		t.Fatal("non-telemetry frames not counted")
	}
}

func TestRunRearmsWhileModeOFF(t *testing.T) {
	setRearm(t, 50*time.Millisecond, 20*time.Millisecond, 10*time.Second)
	var mode atomic.Uint32
	mode.Store(0xA0) // OFF
	port := &mockPort{readFn: readEvery(2*time.Millisecond, func() []byte {
		return telemetryFrame(byte(mode.Load()), 5)
	})}
	d := New("/dev/x", 300, false, noopLog{})
	var offObs atomic.Int32
	_, stop := startRun(t, d, port, func(o Observation) {
		if o.ModeRaw == "OFF" {
			offObs.Add(1)
		}
	})
	defer stop()

	time.Sleep(150 * time.Millisecond)
	if offObs.Load() == 0 {
		t.Fatal("OFF frames did not reach onObs")
	}
	if n := countEnables(port.written()); n < 4 {
		t.Fatalf("enable sent %d times while the amp reports OFF, want >= 4 (frames flow, so only the mode rule re-arms)", n)
	}

	mode.Store(0x60) // OPR/RX: the amp is up and streaming
	time.Sleep(60 * time.Millisecond)
	settled := countEnables(port.written())
	time.Sleep(150 * time.Millisecond)
	if n := countEnables(port.written()); n != settled {
		t.Fatalf("enable still re-sent while streaming OPR/RX (%d -> %d)", settled, n)
	}
}

func TestRunNoRearmDuringHealthyStream(t *testing.T) {
	setRearm(t, 20*time.Millisecond, 20*time.Millisecond, 10*time.Second)
	port := &mockPort{readFn: readEvery(2*time.Millisecond, func() []byte { return telemetryFrame(0x60, 5) })}
	d := New("/dev/x", 300, false, noopLog{})
	var obs atomic.Int32
	_, stop := startRun(t, d, port, func(Observation) { obs.Add(1) })
	time.Sleep(400 * time.Millisecond)
	stop()

	if obs.Load() == 0 {
		t.Fatal("no observations from a healthy stream")
	}
	if n := countEnables(port.written()); n != 1 {
		t.Fatalf("enable sent %d times on a healthy stream, want exactly 1 (Open's)", n)
	}
}

// TestRunPowerUpScenario replays the incident: the amp is off (silent) when
// the port opens, so the Open enable is lost; it then boots and sends
// undecodable bytes; it streams telemetry only after an enable that arrives
// once it has booted. The port must stay open across the outage and the amp
// must be armed within a few re-arm periods of its boot.
func TestRunPowerUpScenario(t *testing.T) {
	setRearm(t, 20*time.Millisecond, 20*time.Millisecond, 60*time.Millisecond)
	const offFor, bootFor = 150 * time.Millisecond, 40 * time.Millisecond

	var opens atomic.Int32
	port := &mockPort{}
	start := time.Now()
	var bootedEnables int // enables written once the amp had booted
	port.readFn = func(p []byte) (int, error) {
		time.Sleep(2 * time.Millisecond)
		age := time.Since(start)
		switch {
		case age < offFor: // switched off: silent
			return 0, nil
		case age < offFor+bootFor: // booting: noise
			bootedEnables = countEnables(port.written())
			return copy(p, []byte{0x00, 0xFF}), nil
		default: // booted: streams only once armed after the boot
			if countEnables(port.written()) > bootedEnables {
				return copy(p, telemetryFrame(0x60, 5)), nil
			}
			return copy(p, []byte{0x00}), nil
		}
	}
	d := New("/dev/x", 300, false, noopLog{})
	d.open = func() (serial.Port, error) { opens.Add(1); return port, nil }
	var downs atomic.Int32
	d.SetLinkDownHandler(func(string) { downs.Add(1) })
	var firstObs atomic.Int64
	if err := d.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// Run must be gone before setRearm's cleanup restores the knobs.
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Run did not return after ctx cancel")
		}
	}()
	go func() {
		done <- d.Run(ctx, func(Observation) {
			firstObs.CompareAndSwap(0, int64(time.Since(start)))
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for firstObs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := time.Duration(firstObs.Load())
	if got == 0 {
		t.Fatal("amp was never armed after its boot (no telemetry observed)")
	}
	if limit := offFor + bootFor + 4*rearmEvery + 20*time.Millisecond; got > limit {
		t.Fatalf("first telemetry %v after start, want <= %v", got, limit)
	}
	if n := opens.Load(); n != 1 {
		t.Fatalf("port reopened %d times across the outage, want 1 open (port stays open)", n)
	}
	if n := downs.Load(); n != 1 {
		t.Fatalf("link-down reported %d times during the off phase, want 1", n)
	}
	if d.CurrentBand() != 5 {
		t.Fatalf("band after power-up = %d, want 5", d.CurrentBand())
	}
}

// TestRunKeepsPortOpenWhileAmpOff pins that a long-silent amp neither ends
// the run nor stops the re-arm, and that the outage is reported once.
func TestRunKeepsPortOpenWhileAmpOff(t *testing.T) {
	setRearm(t, 20*time.Millisecond, 20*time.Millisecond, 50*time.Millisecond)
	port := &mockPort{readFn: quietPortRead}
	d := New("/dev/x", 300, false, noopLog{})
	var downs atomic.Int32
	d.SetLinkDownHandler(func(string) { downs.Add(1) })
	done, stop := startRun(t, d, port, nil)
	defer stop()

	time.Sleep(300 * time.Millisecond) // 6x silenceLimit
	select {
	case err := <-done:
		t.Fatalf("Run returned while the amp is off: %v", err)
	default:
	}
	if port.isClosed() {
		t.Fatal("port closed while the amp is off")
	}
	if n := downs.Load(); n != 1 {
		t.Fatalf("link-down reported %d times, want 1", n)
	}
	if n := countEnables(port.written()); n < 5 {
		t.Fatalf("enable sent %d times over 300 ms of silence, want >= 5", n)
	}
}

func TestOpenResetsBandAndMode(t *testing.T) {
	d := New("/dev/x", 300, false, noopLog{})
	d.band, d.mode = 5, "OPR/RX"
	d.open = func() (serial.Port, error) { return &mockPort{}, nil }
	if err := d.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}
	if d.CurrentBand() != 0 || d.CurrentMode() != "" {
		t.Fatalf("after Open band=%d mode=%q, want 0 and empty", d.CurrentBand(), d.CurrentMode())
	}
	if err := d.SetBand("20m"); err == nil {
		t.Fatal("SetBand navigated from a stale band index after Open")
	}
}
