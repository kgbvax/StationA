package radio

import (
	"context"
	"errors"
	"fmt"
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

	if err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, true); err != nil {
		t.Fatalf("SetAudioDemand(true): %v", err)
	}
	snap := waitState(t, h.s, StateLive, time.Second)
	if snap.RadioName != "IC-9700" {
		t.Errorf("RadioName = %q", snap.RadioName)
	}

	if err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, false); err != nil {
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

	err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, true)
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

	err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, true)
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
	err = h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, true)
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

	err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, true)
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

	if err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, true); err != nil {
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
	if err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, true); err != nil {
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

	if err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, true); err == nil {
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

	if err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, true); err != nil {
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
	if err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, true); err != nil {
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
	if err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, false); err != nil {
		t.Fatalf("SetAudioDemand(false): %v", err)
	}
}

// Regression (2026-10-07, live): audio_off then audio_on within one live
// session — the preview restarting — reopened the radio's audio stream but
// never restarted the PCM router (its "running" marker survived the router's
// exit), so the radio's audio reached the bridge and went nowhere. Each
// demand cycle must deliver PCM to the sink again.
func TestAudioReopenWithinLiveSessionStillForwards(t *testing.T) {
	audio := civ.NewFakeAudio(t)
	got := make(chan []byte, 256)
	h := newHarness(t, func(o *SessionOptions) {
		o.AudioPort = audio.Port()
		o.IdleTimeout = 2 * time.Second // stay live across the off/on gap
		o.AudioDemandTTL = 5 * time.Second
		o.AudioSink = func(pcm []byte) {
			select {
			case got <- pcm:
			default:
			}
		}
	})
	waitState(t, h.s, StateIdle, time.Second)

	expectPCM := func(round int) {
		t.Helper()
		pcm := make([]byte, 640) // ≥ the radio's minimum audio datagram (580 incl. header)
		pcm[0] = byte(round)
		deadline := time.After(2 * time.Second)
		for seq := uint16(1); ; seq++ {
			audio.Send(seq, pcm)
			select {
			case p := <-got:
				if len(p) == len(pcm) && p[0] == byte(round) {
					return
				}
			case <-time.After(30 * time.Millisecond):
			case <-deadline:
				t.Fatalf("round %d: no PCM reached the sink", round)
			}
		}
	}

	if err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, true); err != nil {
		t.Fatalf("audio_on: %v", err)
	}
	waitState(t, h.s, StateLive, time.Second)
	expectPCM(1)

	for round := 2; round <= 3; round++ {
		if err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, false); err != nil {
			t.Fatalf("audio_off: %v", err)
		}
		time.Sleep(50 * time.Millisecond) // the router sees its stream close
		if err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, true); err != nil {
			t.Fatalf("audio_on: %v", err)
		}
		if s := h.s.Snapshot().SessionState; s != StateLive {
			t.Fatalf("round %d: session left live (%q)", round, s)
		}
		expectPCM(round)
	}
}

// Per-holder demand (2026-10, vhfcam + bmrec): one holder's audio_off
// releases only that holder — the session stays live and the PCM keeps
// flowing to the sink for the other — and the last holder's audio_off
// releases the session to the idle clock.
func TestAudioOffReleasesOnlyItsHolder(t *testing.T) {
	audio := civ.NewFakeAudio(t)
	got := make(chan []byte, 256)
	h := newHarness(t, func(o *SessionOptions) {
		o.AudioPort = audio.Port()
		o.AudioDemandTTL = 5 * time.Second
		o.AudioSink = func(pcm []byte) {
			select {
			case got <- pcm:
			default:
			}
		}
	})
	waitState(t, h.s, StateIdle, time.Second)

	expectPCM := func(tag byte) {
		t.Helper()
		pcm := make([]byte, 640)
		pcm[0] = tag
		deadline := time.After(2 * time.Second)
		for seq := uint16(1); ; seq++ {
			audio.Send(seq, pcm)
			select {
			case p := <-got:
				if len(p) == len(pcm) && p[0] == tag {
					return
				}
			case <-time.After(30 * time.Millisecond):
			case <-deadline:
				t.Fatalf("tag %d: no PCM reached the sink", tag)
			}
		}
	}

	if err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, true); err != nil {
		t.Fatalf("audio_on default: %v", err)
	}
	if err := h.s.SetAudioDemand(h.ctx, "bmrec", true); err != nil {
		t.Fatalf("audio_on bmrec: %v", err)
	}
	waitState(t, h.s, StateLive, time.Second)
	if got := h.s.Snapshot().AudioHolders; strings.Join(got, ",") != "bmrec,default" {
		t.Fatalf("AudioHolders = %q, want [bmrec default]", got)
	}
	expectPCM(1)

	// vhfcam's audio_off (no holder = default) must not drop bmrec's hold.
	if err := h.s.SetAudioDemand(h.ctx, DefaultAudioHolder, false); err != nil {
		t.Fatalf("audio_off default: %v", err)
	}
	snap := h.s.Snapshot()
	if !snap.AudioDemand || strings.Join(snap.AudioHolders, ",") != "bmrec" {
		t.Fatalf("after default audio_off: demand=%v holders=%q, want true [bmrec]", snap.AudioDemand, snap.AudioHolders)
	}
	expectPCM(2)                       // the stream was not closed under bmrec
	time.Sleep(400 * time.Millisecond) // ~2.5x the idle timeout
	if s := h.s.Snapshot().SessionState; s != StateLive {
		t.Fatalf("bmrec's hold did not keep the session live (at %q)", s)
	}

	// An audio_off from a holder that holds nothing is a no-op.
	if err := h.s.SetAudioDemand(h.ctx, "stranger", false); err != nil {
		t.Fatalf("audio_off stranger: %v", err)
	}
	if !h.s.Snapshot().AudioDemand {
		t.Fatal("a non-holder's audio_off dropped the demand")
	}

	// The last holder's audio_off releases the session.
	if err := h.s.SetAudioDemand(h.ctx, "bmrec", false); err != nil {
		t.Fatalf("audio_off bmrec: %v", err)
	}
	if snap := h.s.Snapshot(); snap.AudioDemand || len(snap.AudioHolders) != 0 {
		t.Fatalf("after last audio_off: demand=%v holders=%q", snap.AudioDemand, snap.AudioHolders)
	}
	waitState(t, h.s, StateIdle, 2*time.Second)
}

// Each holder's TTL runs on its own: a holder that stops heartbeating
// expires while a refreshing holder keeps the session live; once the last
// holder stops refreshing, the session idles out.
func TestAudioHolderTTLsIndependent(t *testing.T) {
	h := newHarness(t, func(o *SessionOptions) {
		o.AudioDemandTTL = 300 * time.Millisecond
		o.AudioSink = func([]byte) {}
	})
	waitState(t, h.s, StateIdle, time.Second)

	for _, holder := range []string{DefaultAudioHolder, "bmrec"} {
		if err := h.s.SetAudioDemand(h.ctx, holder, true); err != nil {
			t.Fatalf("audio_on %s: %v", holder, err)
		}
	}
	waitState(t, h.s, StateLive, time.Second)

	// Only bmrec heartbeats, well inside its TTL, for ~3 TTLs.
	for i := 0; i < 9; i++ {
		time.Sleep(100 * time.Millisecond)
		if err := h.s.SetAudioDemand(h.ctx, "bmrec", true); err != nil {
			t.Fatalf("bmrec refresh: %v", err)
		}
	}
	snap := h.s.Snapshot()
	if snap.SessionState != StateLive {
		t.Fatalf("refreshing holder did not hold the session (at %q)", snap.SessionState)
	}
	if strings.Join(snap.AudioHolders, ",") != "bmrec" {
		t.Fatalf("AudioHolders = %q, want [bmrec] (default's TTL ran out)", snap.AudioHolders)
	}

	// bmrec stops too: its TTL expires and the session idles out.
	waitState(t, h.s, StateIdle, 3*time.Second)
	if snap := h.s.Snapshot(); snap.AudioDemand || len(snap.AudioHolders) != 0 {
		t.Errorf("demand survived every holder's TTL: demand=%v holders=%q", snap.AudioDemand, snap.AudioHolders)
	}
}

// The holder set is bounded: ids are 1..MaxAudioHolderLen chars of
// [A-Za-z0-9._-], and at most MaxAudioHolders holders live at once (a live
// holder may still refresh; a released slot is reusable).
func TestAudioHolderBounds(t *testing.T) {
	h := newHarness(t, func(o *SessionOptions) {
		o.AudioDemandTTL = 5 * time.Second
		o.AudioSink = func([]byte) {}
	})
	waitState(t, h.s, StateIdle, time.Second)

	for _, bad := range []string{"", strings.Repeat("x", MaxAudioHolderLen+1), "a b", "a,b", "bmrec\n", "ünï"} {
		if err := h.s.SetAudioDemand(h.ctx, bad, true); err == nil || !strings.Contains(err.Error(), "invalid audio holder") {
			t.Errorf("holder %q: err = %v, want the invalid-holder rejection", bad, err)
		}
	}
	if s := h.s.Snapshot(); s.SessionState != StateIdle || s.AudioDemand {
		t.Fatalf("an invalid holder touched the session (state %q demand %v)", s.SessionState, s.AudioDemand)
	}
	if err := ValidateAudioHolder(strings.Repeat("x", MaxAudioHolderLen)); err != nil {
		t.Errorf("max-length holder rejected: %v", err)
	}

	for i := 0; i < MaxAudioHolders; i++ {
		if err := h.s.SetAudioDemand(h.ctx, fmt.Sprintf("h%d", i), true); err != nil {
			t.Fatalf("holder %d: %v", i, err)
		}
	}
	if err := h.s.SetAudioDemand(h.ctx, "one-too-many", true); !errors.Is(err, ErrTooManyAudioHolders) {
		t.Fatalf("holder %d: err = %v, want ErrTooManyAudioHolders", MaxAudioHolders+1, err)
	}
	if n := len(h.s.Snapshot().AudioHolders); n != MaxAudioHolders {
		t.Fatalf("%d holders live, want %d", n, MaxAudioHolders)
	}
	if err := h.s.SetAudioDemand(h.ctx, "h0", true); err != nil {
		t.Errorf("a live holder's refresh at the bound was rejected: %v", err)
	}
	if err := h.s.SetAudioDemand(h.ctx, "h0", false); err != nil {
		t.Fatalf("audio_off h0: %v", err)
	}
	if err := h.s.SetAudioDemand(h.ctx, "one-too-many", true); err != nil {
		t.Errorf("released slot not reusable: %v", err)
	}
}
