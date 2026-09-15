package bridge

// Safety-core tests (plan U6, R4/R10-R13, KTD-4/KTD-5): the arm-permit
// lifecycle, the TX watchdog, and the loss-of-plane rules, driven end-to-end
// over the fake IC-9700 with the recording paho fake (the bridge_test.go
// fixture shape — same package, its helpers are reused). Every bound is
// shrunk for test speed: the watchdog runs at 150 ms, the session cadences at
// the testOpts figures.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"icom9700-radio-bridge/internal/civ"
	"icom9700-radio-bridge/internal/radio"
)

// watchdogBound is the shrunk R12 bound for these tests. The production
// figure (180 s) comes from config at wiring time.
const watchdogBound = 150 * time.Millisecond

// newSafetyBridge wires the bridge over the fake radio with a 150 ms TX
// watchdog, an optional capturing logger and the recording paho fake. The
// poll ticker runs exactly as production's telemetryLoop does — its 1C 00
// read is what carries the radio's key state into the bridge cache, which
// the trip decision consults.
func newSafetyBridge(t *testing.T, fr *civ.FakeRadio, model *radioModel, log *slog.Logger) (*Bridge, *recordingPaho) {
	return newSafetyBridgeBound(t, fr, model, log, watchdogBound)
}

// newSafetyBridgeBound is the general form: the watchdog bound is a parameter
// so the KTD-5 cancel tests can set it ABOVE the fake's session-loss
// detection window (SessionTimeout, 400 ms shrunk) — the loss must be
// observed before the bound for the cancel path to be the one under test.
func newSafetyBridgeBound(t *testing.T, fr *civ.FakeRadio, model *radioModel, log *slog.Logger, bound time.Duration) (*Bridge, *recordingPaho) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o := testOpts(fr)
	o.TXWatchdog = bound
	if log != nil {
		o.Log = log
	} else {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	b := New(ctx, o)
	t.Cleanup(b.Close)
	go func() {
		tick := time.NewTicker(o.PollInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				b.Poll(ctx)
			}
		}
	}()
	fake := &recordingPaho{}
	b.OnMQTTConnect(fake)
	if model != nil {
		model.attach(fr)
	}
	return b, fake
}

// captureLog returns a text-handler logger over buf (for the Warn assertions:
// journalctl -p warning is the station's only error filter).
func captureLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

// pttOnIndex returns the index of the last PTT-on frame the radio received,
// or -1.
func pttOnIndex(fr *civ.FakeRadio, on []byte) int {
	idx := -1
	for i, f := range fr.CivFramesReceived() {
		if string(f) == string(on) {
			idx = i
		}
	}
	return idx
}

// pttOffAfter asserts a PTT-off frame arrived at a LATER position than the
// PTT-on frame — the unkey is the CI-V frame after the key-up, never before.
func pttOffAfter(fr *civ.FakeRadio, on, off []byte) bool {
	idxOn := pttOnIndex(fr, on)
	if idxOn < 0 {
		return false
	}
	for i, f := range fr.CivFramesReceived() {
		if i > idxOn && string(f) == string(off) {
			return true
		}
	}
	return false
}

// countFrames counts exact-byte frames the radio received.
func countFrames(fr *civ.FakeRadio, want []byte) int {
	n := 0
	for _, f := range fr.CivFramesReceived() {
		if string(f) == string(want) {
			n++
		}
	}
	return n
}

// keyUp drives the bridge to armed+live+keyed and waits for the radio truth
// (tx:"tx") to surface — the state the loss rules and the watchdog operate
// on. Returns the exact PTT-on frame.
func keyUp(t *testing.T, b *Bridge, fake *recordingPaho, fr *civ.FakeRadio) []byte {
	t.Helper()
	st := topicState(b.opts)
	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	waitFor(t, 2*time.Second, "armed published", func() bool {
		return lastState(t, fake, st)["armed"] == true
	})
	deliverCmd(b, `{"action":"ptt","value":"on"}`)
	on := b.codec.BuildPTT(true)
	waitFor(t, 2*time.Second, "PTT-on frame at the radio", func() bool {
		return frameSent(fr, on)
	})
	waitFor(t, 2*time.Second, "tx = tx published (the poll readback)", func() bool {
		return lastState(t, fake, st)["tx"] == "tx"
	})
	return on
}

// ---------------------------------------------------------------------------
// the seams are filled (U6 installs them in New)
// ---------------------------------------------------------------------------

func TestSafetyCoreInstallsSeams(t *testing.T) {
	fr := mustFakeRadio(t)
	b, _ := newSafetyBridge(t, fr, newRadioModel(), nil)

	if b.opts.ArmGate == nil {
		t.Fatal("Options.ArmGate not installed — the safety core must own the PTT admission seam")
	}
	if b.opts.OnPTTOn == nil {
		t.Fatal("Options.OnPTTOn not installed — the watchdog must re-arm per PTT-on")
	}
	// The admission rule stays the R10 v1 rule with the exact strings.
	if err := b.opts.ArmGate(true, true); err != nil {
		t.Errorf("armed+live must pass, got %v", err)
	}
	if err := b.opts.ArmGate(false, false); err == nil || err.Error() != ErrPTTNotArmed {
		t.Errorf("unarmed = %v, want exactly %q", err, ErrPTTNotArmed)
	}
	if err := b.opts.ArmGate(false, true); err == nil || err.Error() != ErrPTTNotArmed {
		t.Errorf("unarmed-live = %v, want exactly %q (armed first)", err, ErrPTTNotArmed)
	}
	if err := b.opts.ArmGate(true, false); err == nil || err.Error() != ErrPTTNotLive {
		t.Errorf("armed-not-live = %v, want exactly %q", err, ErrPTTNotLive)
	}
}

// ---------------------------------------------------------------------------
// the TX watchdog trip (R12): unkey at the bound, disarm, safety fact, Warn
// ---------------------------------------------------------------------------

func TestWatchdogTripUnkeysDisarmsErrorsAndLogsWarn(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	var buf bytes.Buffer
	b, fake := newSafetyBridge(t, fr, model, captureLog(&buf))
	st := topicState(b.opts)

	on := keyUp(t, b, fake, fr)
	off := b.codec.BuildPTT(false)

	// The trip: armed:false published, the safety fact in /state.error, and
	// the PTT-off frame on the wire — AFTER the key-up (frame order).
	waitFor(t, 3*time.Second, "watchdog trip disarmed the permit", func() bool {
		return lastState(t, fake, st)["armed"] == false
	})
	wantFact := FactWatchdogTrip + " " + watchdogBound.String()
	if got := lastError(t, fake, st); got != wantFact {
		t.Fatalf("error = %q, want exactly %q", got, wantFact)
	}
	if !pttOffAfter(fr, on, off) {
		t.Error("no PTT-off frame arrived after the PTT-on frame")
	}
	if n := countFrames(fr, off); n != 1 {
		t.Errorf("radio received %d PTT-off frames, want exactly 1 (the trip's)", n)
	}
	// The unkeyed radio truth surfaces on the next poll read.
	waitFor(t, 2*time.Second, "tx = rx after the trip's unkey", func() bool {
		return lastState(t, fake, st)["tx"] == "rx"
	})

	// All three legs logged at WARN with the slot attr — journalctl -p
	// warning is the station's only error filter (logging convention). The
	// text handler quotes multi-word msg values, so the fragments match the
	// quoted form.
	logs := buf.String()
	for _, want := range []string{
		`msg="tx watchdog tripped`,
		`msg="safety PTT-off dispatched`,
		`msg="arm permit dropped`,
		"slot=tsite/tstation/tradio",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log output missing %q — the trip legs must log at Warn with the slot attr\ngot:\n%s", want, logs)
		}
	}
	if n := strings.Count(logs, "level=WARN"); n < 3 {
		t.Errorf("only %d WARN lines for the three trip legs\ngot:\n%s", n, logs)
	}

	// The trip leaves the session up but disarmed: a further PTT is rejected
	// by the gate with the exact string, and no frame reaches the radio.
	before := len(fr.CivFramesReceived())
	deliverCmd(b, `{"action":"ptt","value":"on"}`)
	if got := lastError(t, fake, st); got != ErrPTTNotArmed {
		t.Fatalf("post-trip ptt error = %q, want exactly %q", got, ErrPTTNotArmed)
	}
	if len(fr.CivFramesReceived()) != before {
		t.Error("a rejected ptt produced radio traffic")
	}
}

// TestWatchdogTripErrorPersistsUntilOperatorAck: the safety fact survives the
// session_state error→idle decay; only the next arm/cmd acks it (R17).
func TestWatchdogTripErrorPersistsUntilOperatorAck(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newSafetyBridge(t, fr, newRadioModel(), nil)
	st := topicState(b.opts)

	keyUp(t, b, fake, fr)
	wantFact := FactWatchdogTrip + " " + watchdogBound.String()
	waitFor(t, 3*time.Second, "watchdog trip fact", func() bool {
		return lastError(t, fake, st) == wantFact
	})

	// Now the radio vanishes: the session errors ("session lost" — the
	// session fact wins while present) and decays error→idle. The TRIP fact
	// must still be there afterwards.
	fr.StopAnswering()
	waitFor(t, 3*time.Second, "session_state decayed to idle", func() bool {
		return lastState(t, fake, st)["session_state"] == "idle"
	})
	if got := lastError(t, fake, st); got != wantFact {
		t.Fatalf("error = %q after the decay — the trip fact must persist (only session_state decays)", got)
	}

	// The operator acks by arming again: the fact clears.
	fr.Reboot(0)
	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	waitFor(t, 2*time.Second, "trip fact acked by the successful arm", func() bool {
		return lastError(t, fake, st) == ""
	})
}

// ---------------------------------------------------------------------------
// the MQTT-plane loss rule (R4): unkey over the radio plane, disarm, record
// ---------------------------------------------------------------------------

func TestMQTTLossForcesPTTOffDisarmsAndAllowsRearm(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newSafetyBridge(t, fr, newRadioModel(), nil)
	st := topicState(b.opts)

	on := keyUp(t, b, fake, fr)
	off := b.codec.BuildPTT(false)

	// The broker vanishes while keyed: the loss rule unkeys over the RADIO
	// plane (UDP — independent of the lost MQTT link) and disarms, well
	// within the watchdog bound.
	b.OnMQTTConnectionLost(errors.New("test: broker gone"))
	waitFor(t, watchdogBound, "disarmed within the watchdog bound", func() bool {
		return lastState(t, fake, st)["armed"] == false
	})
	if !pttOffAfter(fr, on, off) {
		t.Error("no PTT-off frame after the PTT-on frame on the MQTT loss")
	}
	if got := lastError(t, fake, st); got != FactMQTTLost {
		t.Errorf("error = %q, want exactly %q", got, FactMQTTLost)
	}
	// The watchdog was cancelled with the key-up — no trip fact may ever
	// appear alongside the loss fact.
	waitFor(t, 3*watchdogBound, "no watchdog trip fact", func() bool {
		return lastError(t, fake, st) == FactMQTTLost
	})

	// After the reconnect the operator re-arms and keys again — the gate
	// accepts a fresh permit.
	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	waitFor(t, 2*time.Second, "rearmed", func() bool {
		return lastState(t, fake, st)["armed"] == true
	})
	deliverCmd(b, `{"action":"ptt","value":"on"}`)
	waitFor(t, 2*time.Second, "re-keyed after the rearm", func() bool {
		return lastState(t, fake, st)["tx"] == "tx"
	})
}

// ---------------------------------------------------------------------------
// session loss mid-PTT (KTD-5): the watchdog cancels, the safety demand
// re-drives the PTT-off, the permit stays dropped
// ---------------------------------------------------------------------------

func TestSessionDropMidPTTCancelsWatchdogAndRedrivesPTTOff(t *testing.T) {
	fr := mustFakeRadio(t)
	// The bound must sit ABOVE the fake's session-loss detection window
	// (SessionTimeout, 400 ms shrunk): the loss has to be observed first, or
	// the trip would legitimately fire before the KTD-5 cancel path runs.
	b, fake := newSafetyBridgeBound(t, fr, newRadioModel(), nil, 1500*time.Millisecond)
	st := topicState(b.opts)

	on := keyUp(t, b, fake, fr)
	off := b.codec.BuildPTT(false)

	// The radio reboots under the keyed carrier: involuntary session loss.
	// The session disarms itself; the safety core cancels the watchdog and
	// marks the PTT-off outstanding — the safety-driven reconnect (a full
	// re-login against the fresh radio ID) delivers it.
	fr.Reboot(400 * time.Millisecond)

	waitFor(t, 3*time.Second, "disarmed by the session loss", func() bool {
		return lastState(t, fake, st)["armed"] == false
	})
	// The PTT-off was reissued: PTTOffPending flips true the moment the
	// safety demand owns it.
	waitFor(t, 2*time.Second, "PTTOffPending set by the safety core", func() bool {
		return b.sess.PTTOffPending()
	})
	// The re-login delivers the frame; the outstanding flag drains.
	waitFor(t, 4*time.Second, "PTT-off delivered over the re-login", func() bool {
		return !b.sess.PTTOffPending()
	})
	if !pttOffAfter(fr, on, off) {
		t.Error("no PTT-off frame after the PTT-on frame across the session loss")
	}
	// No spurious watchdog error: the trip never happened, the loss resolved
	// the TX state. Give the cancelled bound a chance to be wrong — were the
	// watchdog still armed it would fire within the settle window — and then
	// assert the published fact is clean.
	time.Sleep(1500 * time.Millisecond)
	if got := lastError(t, fake, st); strings.Contains(got, FactWatchdogTrip) {
		t.Fatalf("spurious watchdog trip after the session loss: %q", got)
	}
	waitFor(t, 2*time.Second, "session live again over the fresh radio ID", func() bool {
		return b.sess.SessionState() == radio.StateLive
	})
}

// TestWatchdogExpiryWithoutLiveSession drives the down-at-expiry branch
// directly (white-box): the timer fires with the session already gone — the
// branch the cancel race can produce (a dropped OnArmDrop enqueue). Expected:
// disarm + error immediately, PTTOffPending true — the safety demand owns
// delivery, and if the demand exhausts, the terminal R2 fact lands.
func TestWatchdogExpiryWithoutLiveSession(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newSafetyBridge(t, fr, newRadioModel(), nil)
	st := topicState(b.opts)

	// Radio unreachable and the bridge cache simulating a keyed carrier (no
	// session ever went live in this test — the state is built by hand).
	fr.StopAnswering()
	b.mu.Lock()
	b.radio.tx = true
	b.mu.Unlock()
	runOnWorker(b, func() { b.safety.pttOn() }) // an unresolved key-up, no session

	waitFor(t, 3*time.Second, "trip fact published while down", func() bool {
		return lastError(t, fake, st) == FactWatchdogTrip+" "+watchdogBound.String()
	})
	waitFor(t, time.Second, "disarmed", func() bool {
		return lastState(t, fake, st)["armed"] == false
	})
	if !b.sess.PTTOffPending() {
		t.Fatal("PTTOffPending false — the safety demand must own delivery while down")
	}
	// The safety demand runs against the dead radio and exhausts its bound
	// (MaxAttempts x spacing): the terminal R2 fact is the session's own
	// safety-class slot and takes over /state.error.
	waitFor(t, 4*time.Second, "terminal undeliverable fact", func() bool {
		return lastError(t, fake, st) == radio.FactPTTOffUndeliverable
	})
	if b.sess.PTTOffPending() {
		t.Error("PTTOffPending still true after the demand concluded")
	}
}

// ---------------------------------------------------------------------------
// the permit-drop rules: operator disarm during PTT, session loss while armed
// ---------------------------------------------------------------------------

func TestDisarmDuringLivePTTSendsPTTOffAndCancelsWatchdog(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newSafetyBridge(t, fr, newRadioModel(), nil)
	st := topicState(b.opts)

	on := keyUp(t, b, fake, fr)
	off := b.codec.BuildPTT(false)

	// The operator disarms while the carrier is up: the permit drop forces
	// the PTT-off and cancels the watchdog — a deliberate operator action,
	// not a fault (no error fact).
	deliverCmd(b, `{"action":"disarm"}`)
	waitFor(t, 2*time.Second, "disarmed", func() bool {
		return lastState(t, fake, st)["armed"] == false
	})
	waitFor(t, 2*time.Second, "unkeyed by the permit drop", func() bool {
		return lastState(t, fake, st)["tx"] == "rx"
	})
	if !pttOffAfter(fr, on, off) {
		t.Error("no PTT-off frame after the PTT-on frame on the disarm")
	}
	if n := countFrames(fr, off); n != 1 {
		t.Errorf("radio received %d PTT-off frames, want exactly 1", n)
	}

	// The cancelled watchdog must not fire later: past the bound with no
	// trip, and no error fact (an operator disarm is not a fault).
	time.Sleep(3 * watchdogBound)
	if got := lastError(t, fake, st); got != "" {
		t.Fatalf("error = %q after a clean disarm — must be empty (no fault, no spurious trip)", got)
	}
}

func TestArmThenSessionDropRepublishesDisarmed(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newSafetyBridge(t, fr, newRadioModel(), nil)
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	waitFor(t, 2*time.Second, "armed:true published while live", func() bool {
		return lastState(t, fake, st)["armed"] == true
	})

	fr.StopAnswering()
	waitFor(t, 3*time.Second, "armed:false republished on the session loss", func() bool {
		return lastState(t, fake, st)["armed"] == false
	})
	if lastState(t, fake, st)["device_online"] != false {
		t.Error("device_online must read false after the loss (R16)")
	}
}

// ---------------------------------------------------------------------------
// restart (R11 fail-disarmed): a fresh process has no permit and no state
// ---------------------------------------------------------------------------

func TestRestartFailsDisarmedAndRejectsPTT(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b1, fake1 := newSafetyBridge(t, fr, model, nil)

	on := keyUp(t, b1, fake1, fr)
	// Unkey before the restart so the fake radio is left clean (a real
	// process death while keyed is the recorded R17 exposure — a stopped
	// bridge sends no PTT-off).
	deliverCmd(b1, `{"action":"ptt","value":"off"}`)
	off := b1.codec.BuildPTT(false)
	waitFor(t, 2*time.Second, "b1 unkeyed", func() bool {
		return frameSent(fr, off)
	})
	b1.Close()

	// A NEW bridge process over the SAME radio: fail-disarmed, and a ptt cmd
	// is rejected with the exact string — no permit survives the restart.
	b2, fake2 := newSafetyBridge(t, fr, model, nil)
	st2 := topicState(b2.opts)
	waitFor(t, 2*time.Second, "fresh process publishes armed:false", func() bool {
		m, ok := tryState(fake2, st2)
		return ok && m["armed"] == false
	})

	deliverCmd(b2, `{"action":"ptt","value":"on"}`)
	if got := lastError(t, fake2, st2); got != ErrPTTNotArmed {
		t.Fatalf("error = %q, want exactly %q", got, ErrPTTNotArmed)
	}
	if n := countFrames(fr, on); n != 1 {
		t.Errorf("radio received %d PTT-on frames, want only the pre-restart one", n)
	}
}
