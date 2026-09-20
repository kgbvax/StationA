package radio

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"icom9700-radio-bridge/internal/civ"
)

// harness wires the shared fake radio (civ.FakeRadio drives the same wire
// grammar the transport tests pin) with a fast-timed session whose state
// machine is running.
type harness struct {
	t   *testing.T
	f   *civ.FakeRadio
	s   *Session
	ctx context.Context
}

func newHarness(t *testing.T, script func(o *SessionOptions)) *harness {
	t.Helper()
	f := civ.NewFakeRadio(t)
	o := fastSessionOpts(f)
	if script != nil {
		script(&o)
	}
	s := NewSession(o)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Run(ctx) }()
	return &harness{t: t, f: f, s: s, ctx: ctx}
}

// fastSessionOpts: the plan's timers shrunk ~1000x against the fake radio.
// PingInterval stays well under LossWatchdog — the production invariant
// (keepalives ≪ loss bound); a harness that lets the control stream go
// silent past the watchdog races the idle close and eats the teardown
// packets (the loss path closes the sockets mid-teardown).
func fastSessionOpts(f *civ.FakeRadio) SessionOptions {
	return SessionOptions{
		Host:            "127.0.0.1",
		ControlPort:     f.Addr().Port,
		CIVPort:         f.CIVPort(),
		Username:        "bridge",
		Password:        "hunter2",
		IdleTimeout:     150 * time.Millisecond,
		MaxAttempts:     3,
		AttemptSpacing:  40 * time.Millisecond,
		DecayTimeout:    250 * time.Millisecond,
		HandshakeTO:     200 * time.Millisecond,
		AreYouThere:     25 * time.Millisecond,
		HandshakeBudget: 600 * time.Millisecond,
		LossWatchdog:    150 * time.Millisecond,
		PingInterval:    25 * time.Millisecond,
		Logger:          slog.Default(),
	}
}

func waitState(t *testing.T, s *Session, want string, d time.Duration) Snapshot {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if snap := s.Snapshot(); snap.SessionState == want {
			return snap
		}
		time.Sleep(5 * time.Millisecond)
	}
	snap := s.Snapshot()
	t.Fatalf("state %q not reached in %s (at %q err=%q)", want, d, snap.SessionState, snap.Err)
	return snap
}

func waitTrue(t *testing.T, what string, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s not observed within %s", what, d)
}

// Plan U4 scenario 1: a cmd during idle drives idle -> connecting -> live
// -> execute, and after the idle timeout with no further work the session
// disconnects (the radio's single session goes back to wfview).
func TestCmdDemandLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	waitState(t, h.s, StateIdle, time.Second)

	executed := make(chan struct{}, 1)
	err := h.s.Demand(h.ctx, func(c *civ.Client) error {
		close(executed)
		return nil
	})
	if err != nil {
		t.Fatalf("Demand: %v", err)
	}
	waitTrue(t, "demand executed", time.Second, func() bool {
		select {
		case <-executed:
			return true
		default:
			return false
		}
	})
	snap := waitState(t, h.s, StateLive, time.Second)
	if snap.RadioName != "IC-9700" {
		t.Errorf("RadioName = %q", snap.RadioName)
	}

	// Idle timeout disconnects: back to idle AND the fake saw the close
	// (the datagram is in flight — poll for it; the window is generous
	// because the suite runs parallel).
	waitState(t, h.s, StateIdle, 2*time.Second)
	waitTrue(t, "close packet at the fake", 3*time.Second, func() bool {
		_, _, _, closes, _ := h.f.Counts()
		return closes >= 1
	})
}

// Plan U4 scenario 2: armed set during idle connects and HOLDS the session
// open (R11) — no idle disconnect while held; disarm starts the idle clock
// (scenario 4).
func TestHoldBlocksIdleDisconnect(t *testing.T) {
	h := newHarness(t, nil)
	waitState(t, h.s, StateIdle, time.Second)

	if err := h.s.SetHold(h.ctx, true); err != nil {
		t.Fatalf("SetHold(true): %v", err)
	}
	waitState(t, h.s, StateLive, time.Second)
	if snap := h.s.Snapshot(); !snap.Hold {
		t.Error("Snapshot.Hold = false while held")
	}
	time.Sleep(400 * time.Millisecond) // ~2.5x the idle timeout
	if snap := h.s.Snapshot(); snap.SessionState != StateLive {
		t.Fatalf("held session dropped to %q — armed must hold the session open", snap.SessionState)
	}

	// Disarm: the idle clock starts.
	if err := h.s.SetHold(h.ctx, false); err != nil {
		t.Fatalf("SetHold(false): %v", err)
	}
	if snap := h.s.Snapshot(); snap.Hold {
		t.Error("Snapshot.Hold still true after disarm")
	}
	waitState(t, h.s, StateIdle, 2*time.Second)
}

// Plan U4 scenario 3: arm during radio-off — three spaced attempts, error
// state, the arm rejected, hold stays false, and NO further login attempts
// until the next demand (R2).
func TestArmDuringRadioOff(t *testing.T) {
	h := newHarness(t, nil)
	h.f.SetSilent(true)

	err := h.s.SetHold(h.ctx, true)
	if err == nil {
		t.Fatal("SetHold against a silent radio must fail")
	}
	snap := h.s.Snapshot()
	if snap.SessionState != StateError {
		t.Errorf("state = %q, want error", snap.SessionState)
	}
	if snap.Hold {
		t.Error("hold set despite the failed connect — the arm was rejected")
	}
	// A radio-off connect never reaches the login — the attempts are
	// handshake series, one Dial each, counted by the session itself.
	attempts := h.s.Snapshot().ConnectAttempts
	if attempts != 3 {
		t.Fatalf("connect attempts = %d, want exactly 3", attempts)
	}
	// No further attempts while the session sits in error.
	time.Sleep(300 * time.Millisecond)
	if attempts2 := h.s.Snapshot().ConnectAttempts; attempts2 != attempts {
		t.Errorf("background connect storm: %d -> %d attempts", attempts, attempts2)
	}
}

// Plan U4 scenario 9 (wfview holds the session): refused logins produce
// exactly 3 spaced attempts, then the error state carries the observed
// fact — and a NEW demand retries the full series again (no storm, no
// permanent lockout).
func TestWFViewContention(t *testing.T) {
	h := newHarness(t, nil)
	h.f.SetRefuseLogin(true)

	err := h.s.Demand(h.ctx, func(*civ.Client) error { return nil })
	if err == nil {
		t.Fatal("Demand against refused logins must fail")
	}
	if !errors.Is(err, civ.ErrLoginRejected) {
		t.Fatalf("err = %v, want ErrLoginRejected (the observed fact)", err)
	}
	if snap := h.s.Snapshot(); !strings.Contains(snap.Err, "login refused") {
		t.Errorf("error state text = %q, want the observed fact", snap.Err)
	}
	logins, _, _, _, _ := h.f.Counts()
	if logins != 3 {
		t.Fatalf("login attempts = %d, want 3 spaced attempts", logins)
	}
	// The next demand retries the full series (error -> connecting).
	err = h.s.Demand(h.ctx, func(*civ.Client) error { return nil })
	if err == nil {
		t.Fatal("second Demand must fail too")
	}
	logins2, _, _, _, _ := h.f.Counts()
	if logins2 != 6 {
		t.Errorf("login attempts after second demand = %d, want 6 (3 spaced per demand)", logins2)
	}
}

// Bench 2026-09-20 (real radio busy reject): ONE attempt, then the error
// carries the observed fact — retrying into a held session is a storm.
func TestLoginBusySingleAttempt(t *testing.T) {
	h := newHarness(t, nil)
	h.f.SetBusyLogin(true)

	err := h.s.Demand(h.ctx, func(*civ.Client) error { return nil })
	if !errors.Is(err, civ.ErrLoginBusy) {
		t.Fatalf("err = %v, want ErrLoginBusy", err)
	}
	if snap := h.s.Snapshot(); !strings.Contains(snap.Err, "held") {
		t.Errorf("error state text = %q, want the busy fact", snap.Err)
	}
	if attempts := h.s.Snapshot().ConnectAttempts; attempts != 1 {
		t.Fatalf("connect attempts = %d, want exactly 1 (no retry storm into a held session)", attempts)
	}
}

// Plan U4 scenario 4 lives in TestHoldBlocksIdleDisconnect (disarm starts
// the idle timer).

// KTD-2 regression (production, 2026-09-20): the telemetry poll is a free
// RIDER. Repeated rides while live must NOT restart the idle clock — the
// session decays to idle on schedule even though polls continue every few
// milliseconds. A demanding poll kept the radio's single LAN session open
// forever, starving manual wfview.
func TestRideDoesNotExtendIdle(t *testing.T) {
	h := newHarness(t, nil)
	waitState(t, h.s, StateIdle, time.Second)

	if err := h.s.SetHold(h.ctx, true); err != nil {
		t.Fatalf("SetHold: %v", err)
	}
	waitState(t, h.s, StateLive, time.Second)
	if err := h.s.SetHold(h.ctx, false); err != nil {
		t.Fatalf("SetHold(false): %v", err)
	}

	// Poll-shaped rides, faster than the idle timeout, across the whole
	// decay window.
	deadline := time.Now().Add(2 * time.Second)
	rides := 0
	for time.Now().Before(deadline) {
		_ = h.s.Ride(h.ctx, func(*civ.Client) error { return nil })
		rides++
		if h.s.Snapshot().SessionState == StateIdle {
			if rides < 2 {
				t.Fatalf("session decayed after %d rides — rides never ran", rides)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session still live after %d rides across the decay window — a ride extends the idle clock", rides)
}

// Ride runs fn against a live session; from idle it is a quiet no-op that
// never dials (KTD-2/R2).
func TestRideNoOpWhenIdle(t *testing.T) {
	h := newHarness(t, nil)
	waitState(t, h.s, StateIdle, time.Second)

	executed := false
	if err := h.s.Ride(h.ctx, func(*civ.Client) error { executed = true; return nil }); err != nil {
		t.Fatalf("Ride from idle: %v", err)
	}
	if executed {
		t.Error("Ride executed against a non-live session")
	}
	if snap := h.s.Snapshot(); snap.SessionState != StateIdle {
		t.Errorf("state = %q after an idle Ride, want idle (a ride must not dial)", snap.SessionState)
	}
	// The radio heard nothing.
	logins, _, _, _, _ := h.f.Counts()
	if logins != 0 {
		t.Errorf("logins = %d after an idle Ride, want 0", logins)
	}
}

// Plan U4 scenario 5: refuseLogin 3x — covered by TestWFViewContention
// (spaced attempts, observed-fact error text).

// An involuntary session loss with no work pending: error state, hold
// cleared (fail-disarmed), the loss hook fires, and the error decays to
// idle (plan state machine).
func TestSessionLossDecays(t *testing.T) {
	h := newHarness(t, nil)
	waitState(t, h.s, StateIdle, time.Second)

	losses := make(chan error, 1)
	h.s.OnLoss(func(err error) { losses <- err })

	if err := h.s.SetHold(h.ctx, true); err != nil {
		t.Fatalf("SetHold: %v", err)
	}
	waitState(t, h.s, StateLive, time.Second)

	// The radio vanishes.
	h.f.SetSilent(true)

	select {
	case err := <-losses:
		if err == nil {
			t.Fatal("loss hook delivered nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session loss not detected")
	}
	waitState(t, h.s, StateError, time.Second)
	if snap := h.s.Snapshot(); snap.Hold {
		t.Error("hold survived a session loss — fail-disarm violated (R11)")
	}
	// Decay to idle.
	waitState(t, h.s, StateIdle, 2*time.Second)
	if snap := h.s.Snapshot(); snap.Err != "" {
		t.Errorf("stale error text after decay: %q", snap.Err)
	}
}

// Plan U4 scenario 8: a session loss resolves an in-flight cmd as
// REJECTED, not silently dropped.
func TestInFlightRejectedOnLoss(t *testing.T) {
	h := newHarness(t, nil)
	waitState(t, h.s, StateIdle, time.Second)

	demandErr := make(chan error, 1)
	go func() {
		demandErr <- h.s.Demand(h.ctx, func(c *civ.Client) error {
			// Hold the demand in flight until the session dies.
			select {
			case <-c.Lost:
				return errors.New("radio: session lost mid-command")
			case <-time.After(3 * time.Second):
				return errors.New("radio: test timeout")
			}
		})
	}()

	waitState(t, h.s, StateLive, time.Second)
	h.f.SetSilent(true)

	select {
	case err := <-demandErr:
		if err == nil {
			t.Fatal("in-flight demand succeeded across a session loss")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("in-flight demand never resolved")
	}
}

// Plan U4 scenario 6: the radio reboots mid-session — the loss is
// detected (loss watchdog), the hold drops, and the NEXT demand performs a
// FULL re-login (fresh handshake against the new radio identity), with the
// on-live hook fired before pending work (the safety core's PTT-off-first
// slot).
func TestRadioRebootFullRelogin(t *testing.T) {
	h := newHarness(t, nil)
	waitState(t, h.s, StateIdle, time.Second)

	liveCount := 0
	h.s.OnLive(func(ctx context.Context, c *civ.Client) error {
		liveCount++
		return nil
	})
	losses := make(chan error, 1)
	h.s.OnLoss(func(err error) { losses <- err })

	if err := h.s.SetHold(h.ctx, true); err != nil {
		t.Fatalf("SetHold: %v", err)
	}
	waitState(t, h.s, StateLive, time.Second)

	// Reboot: silence, then answer again with a NEW radio identity.
	h.f.SetSilent(true)
	h.f.SetRadioSID(0xcafebabe)
	time.Sleep(400 * time.Millisecond) // loss watchdog fires
	select {
	case <-losses:
	case <-time.After(2 * time.Second):
		t.Fatal("reboot loss not detected")
	}
	waitState(t, h.s, StateError, time.Second)
	if snap := h.s.Snapshot(); snap.Hold {
		t.Error("hold survived the reboot loss")
	}

	// The operator (or safety core) retries: full re-login succeeds against
	// the new identity.
	h.f.SetSilent(false)
	loginsBefore, _, _, _, _ := h.f.Counts()
	err := h.s.Demand(h.ctx, func(*civ.Client) error { return nil })
	if err != nil {
		t.Fatalf("Demand after reboot: %v", err)
	}
	loginsAfter, _, _, _, _ := h.f.Counts()
	if loginsAfter <= loginsBefore {
		t.Error("no fresh login after reboot (resume is not allowed — R3)")
	}
	if liveCount == 0 {
		t.Error("on-live hook never fired after the reconnect")
	}
	waitState(t, h.s, StateLive, time.Second)
}

// A reply-consuming round trip through the live session: the demand's CI-V
// command gets the radio's FB ack routed back (replies vs broadcasts).
func TestDemandRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	waitState(t, h.s, StateIdle, time.Second)

	err := h.s.Demand(h.ctx, func(c *civ.Client) error {
		_, err := h.s.RoundTrip(h.ctx, c, civ.CmdTransceiverID(), time.Second)
		return err
	})
	if err != nil {
		t.Fatalf("Demand round trip: %v", err)
	}

	// And a PTT-shaped frame gets the fake's ack too (the safety gating
	// around this lives in U6; here we prove the transport path).
	err = h.s.Demand(h.ctx, func(c *civ.Client) error {
		_, err := h.s.RoundTrip(h.ctx, c, civ.CmdPTT(true), time.Second)
		return err
	})
	if err != nil {
		t.Fatalf("PTT-shaped round trip: %v", err)
	}
}
