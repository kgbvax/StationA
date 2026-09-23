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

// The audio demand during idle drives idle -> connecting -> live, and
// audio_off releases the session: the idle timeout disconnects (the
// radio's single session goes back to wfview) and the fake saw the close.
func TestAudioOffReleasesSession(t *testing.T) {
	h := newHarness(t, nil)
	waitState(t, h.s, StateIdle, time.Second)

	if err := h.s.SetAudioDemand(h.ctx, true); err != nil {
		t.Fatalf("SetAudioDemand(true): %v", err)
	}
	snap := waitState(t, h.s, StateLive, time.Second)
	if snap.RadioName != "IC-9700" {
		t.Errorf("RadioName = %q", snap.RadioName)
	}

	if err := h.s.SetAudioDemand(h.ctx, false); err != nil {
		t.Fatalf("SetAudioDemand(false): %v", err)
	}
	// Idle timeout disconnects: back to idle (the polite teardown is the
	// transport's business, covered by the civ tests; with NoCIVData there
	// is no data-stream close packet to observe).
	waitState(t, h.s, StateIdle, 2*time.Second)
}

// A connect demand against a radio-off — three spaced attempts, error
// state, the demand rejected, the audio demand NOT set, and NO further
// login attempts until the next demand (R2).
func TestAudioConnectDuringRadioOff(t *testing.T) {
	h := newHarness(t, nil)
	h.f.SetSilent(true)

	err := h.s.SetAudioDemand(h.ctx, true)
	if err == nil {
		t.Fatal("SetAudioDemand against a silent radio must fail")
	}
	snap := h.s.Snapshot()
	if snap.SessionState != StateError {
		t.Errorf("state = %q, want error", snap.SessionState)
	}
	if snap.AudioDemand {
		t.Error("audio demand set despite the failed connect — the cmd was rejected")
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

	err := h.s.SetAudioDemand(h.ctx, true)
	if err == nil {
		t.Fatal("audio demand against refused logins must fail")
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
	err = h.s.SetAudioDemand(h.ctx, true)
	if err == nil {
		t.Fatal("second demand must fail too")
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

	err := h.s.SetAudioDemand(h.ctx, true)
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

// An involuntary session loss during an audio demand: error state, then —
// unlike the old armed hold — the audio demand SURVIVES (there is no fail-
// disarmed posture to keep; the demand is receive-only), and a refreshing
// audio_on after the radio returns reconnects with a FULL re-login (fresh
// handshake against the new radio identity, R3).
func TestSessionLossRecoversAudioDemand(t *testing.T) {
	h := newHarness(t, func(o *SessionOptions) {
		o.AudioDemandTTL = 5 * time.Second // outlive the test window
		o.AudioSink = func([]byte) {}
	})
	waitState(t, h.s, StateIdle, time.Second)

	if err := h.s.SetAudioDemand(h.ctx, true); err != nil {
		t.Fatalf("SetAudioDemand(true): %v", err)
	}
	waitState(t, h.s, StateLive, time.Second)

	// The radio vanishes.
	h.f.SetSilent(true)
	waitState(t, h.s, StateError, 3*time.Second)
	if snap := h.s.Snapshot(); !snap.AudioDemand {
		t.Error("audio demand dropped on session loss — it must survive (receive-only)")
	}

	// The radio returns with a NEW identity; the refreshing demand
	// reconnects with a full re-login.
	h.f.SetSilent(false)
	h.f.SetRadioSID(0xcafebabe)
	loginsBefore, _, _, _, _ := h.f.Counts()
	if err := h.s.SetAudioDemand(h.ctx, true); err != nil {
		t.Fatalf("refreshing demand after reboot: %v", err)
	}
	loginsAfter, _, _, _, _ := h.f.Counts()
	if loginsAfter <= loginsBefore {
		t.Error("no fresh login after reboot (resume is not allowed — R3)")
	}
	waitState(t, h.s, StateLive, time.Second)
}

// An error state with no retrying operator decays to idle so a stale fault
// never reads as current (plan state machine, R3).
func TestErrorDecaysToIdle(t *testing.T) {
	h := newHarness(t, nil)
	h.f.SetSilent(true)

	if err := h.s.SetAudioDemand(h.ctx, true); err == nil {
		t.Fatal("connect against a silent radio must fail")
	}
	waitState(t, h.s, StateError, time.Second)
	waitState(t, h.s, StateIdle, 2*time.Second)
	if snap := h.s.Snapshot(); snap.Err != "" {
		t.Errorf("stale error text after decay: %q", snap.Err)
	}
}

// Audio demand lifecycle (2026-09-21 preview sink): audio_on during idle
// connects and HOLDS the session open past the idle timeout; after the TTL
// expires without a refreshing audio_on the demand releases and the session
// idles out (a dead preview consumer must not pin the radio, KTD-2). The
// fake has no audio listener — OpenAudio failing is contained (logged, the
// demand still holds); PCM delivery is pinned by the civ package tests.
func TestAudioDemandLifecycle(t *testing.T) {
	h := newHarness(t, func(o *SessionOptions) {
		o.AudioDemandTTL = 400 * time.Millisecond
		o.AudioSink = func([]byte) {}
	})
	waitState(t, h.s, StateIdle, time.Second)

	if err := h.s.SetAudioDemand(h.ctx, true); err != nil {
		t.Fatalf("SetAudioDemand(true): %v", err)
	}
	waitState(t, h.s, StateLive, time.Second)
	if snap := h.s.Snapshot(); !snap.AudioDemand {
		t.Error("Snapshot.AudioDemand = false while demanded")
	}
	time.Sleep(400 * time.Millisecond) // ~2.5x the idle timeout
	if snap := h.s.Snapshot(); snap.SessionState != StateLive {
		t.Fatalf("audio demand did not hold the session open (at %q)", snap.SessionState)
	}

	// A refreshing audio_on before the TTL keeps the demand alive.
	if err := h.s.SetAudioDemand(h.ctx, true); err != nil {
		t.Fatalf("SetAudioDemand refresh: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if snap := h.s.Snapshot(); snap.SessionState != StateLive {
		t.Fatalf("refresh did not hold the session (at %q)", snap.SessionState)
	}

	// TTL expiry releases the session: it idles out again.
	waitState(t, h.s, StateIdle, 3*time.Second)
	if snap := h.s.Snapshot(); snap.AudioDemand {
		t.Error("AudioDemand still true after TTL expiry")
	}

	// audio_off during idle is a clean no-op.
	if err := h.s.SetAudioDemand(h.ctx, false); err != nil {
		t.Fatalf("SetAudioDemand(false): %v", err)
	}
}
