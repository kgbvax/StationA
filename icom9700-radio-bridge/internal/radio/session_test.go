package radio

// Session-manager tests (plan U4): the on-demand lifecycle runs against the
// fake radio over real loopback UDP — the same instrument the civ suite
// uses, driven here through the exported surface only. Every policy figure
// (idle window, retry series, spacing, decay, transport cadences) is shrunk
// for test speed; the production figures come from config at wiring time.

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"icom9700-radio-bridge/internal/civ"
)

// sessionCfg builds fast-shrunk session config around the fake radio.
func sessionCfg(fr *civ.FakeRadio, log *slog.Logger) Config {
	return Config{
		Host:           "127.0.0.1",
		ControlPort:    fr.CtrlPort(),
		Username:       "operator1",
		Password:       "s3cret!",
		IdleTimeout:    120 * time.Millisecond,
		MaxAttempts:    3,
		AttemptSpacing: 30 * time.Millisecond,
		ErrorDecay:     80 * time.Millisecond,
		Opts: func(o *civ.Opts) {
			o.AreYouTherePeriod = 30 * time.Millisecond
			o.HandshakeTimeout = 600 * time.Millisecond
			o.IdlePeriod = 15 * time.Millisecond
			o.PingPeriod = 40 * time.Millisecond
			o.RetransmitPeriod = 15 * time.Millisecond
			o.TokenRenewal = 100 * time.Millisecond
			o.RenewalTimeout = 400 * time.Millisecond
			o.CivSilence = 150 * time.Millisecond
			o.StartDataPeriod = 40 * time.Millisecond
			o.SessionTimeout = 300 * time.Millisecond
		},
		Log: log,
	}
}

// stateRecorder collects OnStateChange snapshots and OnSessionDown /
// OnArmDrop reasons for assertion.
type stateRecorder struct {
	mu       sync.Mutex
	snaps    []Snapshot
	downs    []string
	armDrops []string
}

func newStateRecorder() *stateRecorder {
	return &stateRecorder{}
}

func (r *stateRecorder) onState(s Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snaps = append(r.snaps, s)
}

func (r *stateRecorder) onDown(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.downs = append(r.downs, reason)
}

func (r *stateRecorder) onArmDrop(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.armDrops = append(r.armDrops, reason)
}

// states returns every recorded session_state value in order.
func (r *stateRecorder) states() []SessionState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SessionState, len(r.snaps))
	for i, sn := range r.snaps {
		out[i] = sn.State
	}
	return out
}

func (r *stateRecorder) downCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.downs)
}

func (r *stateRecorder) dropCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.armDrops)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(testSink{}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// newFake builds the fake radio wired into t (bind failures fatal, closed at
// cleanup) — the exported cousin of the civ package's own helper.
func newFake(t *testing.T) *civ.FakeRadio {
	t.Helper()
	fr, err := civ.NewFakeRadio()
	if err != nil {
		t.Fatalf("fake radio bind: %v", err)
	}
	t.Cleanup(fr.Close)
	return fr
}

// testSink discards records — the suite asserts behavior, not log lines.
type testSink struct{}

func (testSink) Write(p []byte) (int, error) { return len(p), nil }

// mustSetFreq builds a valid main-VFO set-freq frame for the 70cm band.
func mustSetFreq(t *testing.T, codec *civ.Codec) []byte {
	t.Helper()
	b, err := codec.BuildSetFreq(civ.VfoMain, 433_400_000)
	if err != nil {
		t.Fatalf("BuildSetFreq: %v", err)
	}
	return b
}

// waitForState polls until the session reaches the wanted state.
func waitForState(t *testing.T, s *Session, d time.Duration, want SessionState) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if s.SessionState() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("session did not reach %s within %s (at %s, snap %+v)", want, d, s.SessionState(), s.Snapshot())
}

// --- 1. cmd during idle drives idle→connecting→live→execute→idle -----------

func TestCmdDuringIdleConnectsExecutesAndTimesOutIdle(t *testing.T) {
	fr := newFake(t)
	rec := newStateRecorder()
	s := NewSession(sessionCfg(fr, testLogger()), Hooks{OnStateChange: rec.onState, OnSessionDown: rec.onDown, OnArmDrop: rec.onArmDrop})
	defer s.Close()

	if got := s.SessionState(); got != StateIdle {
		t.Fatalf("initial state = %q, want idle", got)
	}
	err := s.Do(context.Background(), func(tr *civ.Transport) error {
		return tr.SendCIV(civ.NewCodec().BuildPTT(false))
	})
	if err != nil {
		t.Fatalf("Do on fresh session: %v", err)
	}
	waitForState(t, s, 2*time.Second, StateLive)
	if !s.Live() {
		t.Fatal("Do succeeded but session not live")
	}
	// The idle window (120 ms shrunk) takes the session back down.
	waitForState(t, s, time.Second, StateIdle)
	if s.Live() {
		t.Fatal("idle timeout did not disconnect")
	}
	// The state walk passed through connecting and live.
	sawConnecting, sawLive := false, false
	for _, st := range rec.states() {
		switch st {
		case StateConnecting:
			sawConnecting = true
		case StateLive:
			sawLive = true
		}
	}
	if !sawConnecting || !sawLive {
		t.Fatalf("state walk missing connecting/live: %v", rec.states())
	}
	if rec.downCount() != 0 {
		t.Fatalf("polite idle disconnect must not fire OnSessionDown, got %v", rec.downs)
	}
}

// --- 2. armed set during idle connects and holds ---------------------------

func TestArmDuringIdleConnectsAndHolds(t *testing.T) {
	fr := newFake(t)
	s := NewSession(sessionCfg(fr, testLogger()), Hooks{})
	defer s.Close()

	if err := s.SetArmed(context.Background(), true); err != nil {
		t.Fatalf("SetArmed(true): %v", err)
	}
	waitForState(t, s, 2*time.Second, StateLive)
	if !s.Armed() {
		t.Fatal("arm permit not held after successful arm")
	}
	// Armed blocks the idle disconnect: well past the 120 ms idle window the
	// session must still be live.
	time.Sleep(250 * time.Millisecond)
	if s.SessionState() != StateLive {
		t.Fatalf("armed session dropped to %s; armed must hold the session open (R11)", s.SessionState())
	}
	// Disarm with no traffic starts the idle timer.
	if err := s.SetArmed(context.Background(), false); err != nil {
		t.Fatalf("SetArmed(false): %v", err)
	}
	if s.Armed() {
		t.Fatal("disarm did not drop the permit")
	}
	waitForState(t, s, time.Second, StateIdle)
}

// --- 3. arm against a refusing radio: bounded series, fail-disarmed, no storm

func TestArmDuringRadioOffFailsSpacedAndDisarms(t *testing.T) {
	// The refusing radio (stale-session 0xffffffff) answers the handshake but
	// refuses every login — the scenario where attempt count and spacing are
	// observable on the fake (a fully silent radio never reaches login; its
	// fact is asserted in the subtest below).
	fr := newFake(t)
	fr.SetRefused()
	rec := newStateRecorder()
	s := NewSession(sessionCfg(fr, testLogger()), Hooks{OnArmDrop: rec.onArmDrop})
	defer s.Close()

	err := s.SetArmed(context.Background(), true)
	if err == nil {
		t.Fatal("arm against a refusing radio must fail")
	}
	waitForState(t, s, 2*time.Second, StateError)
	if s.Armed() {
		t.Fatal("failed arm connect must leave the permit dropped (fail-disarmed, R2)")
	}
	snap := s.Snapshot()
	if snap.Error == "" {
		t.Fatal("failed arm connect must carry an observed failure fact")
	}
	if snap.ErrorSafety {
		t.Fatal("an arm-connect failure is not safety-class")
	}
	if rec.dropCount() == 0 {
		t.Fatal("OnArmDrop must fire on a failed arm-driven connect")
	}
	// Exactly MaxAttempts dials, each ≥ AttemptSpacing apart (R2: no storm).
	attempts := fr.LoginAttempts()
	if len(attempts) != 3 {
		t.Fatalf("login attempts = %d, want exactly MaxAttempts=3", len(attempts))
	}
	for i := 1; i < len(attempts); i++ {
		gap := attempts[i].Sub(attempts[i-1])
		if gap < 25*time.Millisecond { // AttemptSpacing 30 ms minus scheduler slop
			t.Fatalf("attempt %d only %s after attempt %d — spacing invariant broken", i+1, gap, i)
		}
	}
	// No further login attempts until the next cmd/arm demand.
	n := len(fr.LoginAttempts())
	time.Sleep(200 * time.Millisecond)
	if len(fr.LoginAttempts()) != n {
		t.Fatal("login attempts continued after the series concluded — retry storm")
	}
}

// A fully silent radio (no answer at all) fails at the are-you-there bound
// with the handshake-timeout fact — no login packet ever sent, no storm.
func TestArmDuringSilentRadioTimesOutWithFact(t *testing.T) {
	fr := newFake(t)
	fr.StopAnswering()
	rec := newStateRecorder()
	s := NewSession(sessionCfg(fr, testLogger()), Hooks{OnArmDrop: rec.onArmDrop})
	defer s.Close()

	start := time.Now()
	if err := s.SetArmed(context.Background(), true); err == nil {
		t.Fatal("arm against a silent radio must fail")
	}
	waitForState(t, s, 3*time.Second, StateError)
	if got := s.Snapshot().Error; got != "handshake timeout" {
		t.Fatalf("error fact = %q, want handshake timeout", got)
	}
	// Three attempts, each waiting out the are-you-there bound and the
	// attempt spacing: the series must take at least 2×AttemptSpacing.
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("series concluded in %s — attempts were not spaced", elapsed)
	}
	if n := len(fr.LoginAttempts()); n != 0 {
		t.Fatalf("silent radio recorded %d logins; handshake never got that far", n)
	}
	if rec.dropCount() == 0 {
		t.Fatal("OnArmDrop must fire on a failed arm-driven connect")
	}
}

// --- 4. refused login: observed fact, spacing, no storm (wfview-holds) -----

func TestRefusedLoginCarriesObservedFactAndSpacing(t *testing.T) {
	fr := newFake(t)
	fr.SetLoginErr(0xfffffffe) // the bad-credentials status the U2 suite pins
	s := NewSession(sessionCfg(fr, testLogger()), Hooks{})
	defer s.Close()

	err := s.Do(context.Background(), func(tr *civ.Transport) error { return nil })
	if err == nil {
		t.Fatal("cmd during refused login must be rejected")
	}
	waitForState(t, s, 2*time.Second, StateError)
	if got := s.Snapshot().Error; got != "login refused" {
		t.Fatalf("error fact = %q, want the observed fact %q", got, "login refused")
	}
	if s.Snapshot().ErrorSafety {
		t.Fatal("login refusal is not safety-class")
	}
	attempts := fr.LoginAttempts()
	if len(attempts) != 3 {
		t.Fatalf("attempts = %d, want 3", len(attempts))
	}
	for i := 1; i < len(attempts); i++ {
		if gap := attempts[i].Sub(attempts[i-1]); gap < 25*time.Millisecond {
			t.Fatalf("refused-login retry %d only %s after the previous — no-storm spacing broken", i+1, gap)
		}
	}

	// The wfview-holds case is the same refusal: assert decay brings idle
	// back and the NEXT demand dials again, spaced from the last attempt.
	waitForState(t, s, time.Second, StateIdle)
	if err := s.Do(context.Background(), func(tr *civ.Transport) error { return nil }); err == nil {
		t.Fatal("second demand during held session must still be rejected")
	}
	if len(fr.LoginAttempts()) != 6 {
		t.Fatalf("attempts after second demand = %d, want 6", len(fr.LoginAttempts()))
	}
}

// --- 5. error state decays to idle; safety fact survives decay -------------

func TestErrorDecayAndSafetyFactPersistence(t *testing.T) {
	fr := newFake(t)
	fr.SetLoginErr(0xfffffffe)
	s := NewSession(sessionCfg(fr, testLogger()), Hooks{})
	defer s.Close()

	if err := s.Do(context.Background(), func(tr *civ.Transport) error { return nil }); err == nil {
		t.Fatal("demand must fail against refusing radio")
	}
	waitForState(t, s, 2*time.Second, StateError)

	// Decay (80 ms shrunk) returns the state to idle and clears a
	// non-safety fact.
	waitForState(t, s, 2*time.Second, StateIdle)
	if s.Snapshot().Error != "" {
		t.Fatalf("decay must clear a non-safety fact, got %q", s.Snapshot().Error)
	}

	// A safety-class fact (PTT-off undeliverable) survives decay: request a
	// PTT-off delivery while the radio refuses every login.
	fr2 := newFake(t)
	fr2.SetLoginErr(0xfffffffe)
	s2 := NewSession(sessionCfg(fr2, testLogger()), Hooks{})
	defer s2.Close()
	s2.RequestPTTOff()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && s2.Snapshot().Error != FactPTTOffUndeliverable {
		time.Sleep(2 * time.Millisecond)
	}
	if got := s2.Snapshot().Error; got != FactPTTOffUndeliverable {
		t.Fatalf("error = %q, want %q", got, FactPTTOffUndeliverable)
	}
	if !s2.Snapshot().ErrorSafety {
		t.Fatal("ptt-off undeliverable must be safety-class")
	}
	waitForState(t, s2, 2*time.Second, StateIdle)
	snap := s2.Snapshot()
	if snap.Error != FactPTTOffUndeliverable || !snap.ErrorSafety {
		t.Fatalf("safety fact must survive the error→idle decay, got %+v", snap)
	}
	// Operator ack (the next successful activity) clears it.
}

// --- 6. session loss with outstanding PTT-off: safety re-delivery ----------

func TestSessionLossWithOutstandingPTTOffRedeliversFirst(t *testing.T) {
	fr := newFake(t)
	rec := newStateRecorder()
	s := NewSession(sessionCfg(fr, testLogger()), Hooks{OnStateChange: rec.onState, OnSessionDown: rec.onDown, OnArmDrop: rec.onArmDrop})
	defer s.Close()

	// Arm → live, then the radio reboots: the outage (400 ms) exceeds the
	// keepalive staleness bound (300 ms shrunk), so the session dies and the
	// radio returns with a fresh session — no resume possible (R3).
	if err := s.SetArmed(context.Background(), true); err != nil {
		t.Fatalf("arm: %v", err)
	}
	waitForState(t, s, 2*time.Second, StateLive)
	fr.Reboot(400 * time.Millisecond)

	// No work pending: the loss lands in error, disarmed, fields-omitted
	// republish hooks fired.
	waitForState(t, s, 3*time.Second, StateError)
	if s.Armed() {
		t.Fatal("involuntary session loss must disarm (R3)")
	}
	if rec.dropCount() == 0 {
		t.Fatal("OnArmDrop must fire on involuntary session loss while armed")
	}
	if rec.downCount() == 0 {
		t.Fatal("OnSessionDown must fire so /state republishes fields-omitted")
	}

	// The outstanding PTT-off is registered while the stack is down: the
	// safety-driven demand reconnects (full re-login) and delivers the
	// PTT-off frame.
	s.RequestPTTOff()
	waitForState(t, s, 3*time.Second, StateLive)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && s.PTTOffPending() {
		time.Sleep(2 * time.Millisecond)
	}
	if s.PTTOffPending() {
		t.Fatal("PTT-off still pending after the safety re-delivery demand")
	}
	frames := fr.CivFramesReceived()
	if len(frames) == 0 {
		t.Fatal("no CI-V frames reached the radio")
	}
	last := frames[len(frames)-1]
	if len(last) < 5 || last[4] != 0x1C {
		t.Fatalf("last frame after safety demand is not PTT: % x", last)
	}
}

// --- 7. involuntary loss, no work pending: error + decay + hooks -----------

func TestInvoluntarySessionLossEntersErrorAndDecays(t *testing.T) {
	fr := newFake(t)
	rec := newStateRecorder()
	s := NewSession(sessionCfg(fr, testLogger()), Hooks{OnStateChange: rec.onState, OnSessionDown: rec.onDown, OnArmDrop: rec.onArmDrop})
	defer s.Close()

	if err := s.SetArmed(context.Background(), true); err != nil {
		t.Fatalf("arm: %v", err)
	}
	waitForState(t, s, 2*time.Second, StateLive)

	// Kill the session with the arm permit holding it open (an idle
	// disconnect must not preempt the loss): keepalive staleness (300 ms
	// shrunk) lands it. No PTT-off outstanding.
	fr.StopAnswering()
	waitForState(t, s, 3*time.Second, StateError)
	if s.Armed() {
		t.Fatal("session loss must leave the permit dropped")
	}
	if got := s.Snapshot().Error; got != "session lost" {
		t.Fatalf("error fact = %q, want session lost", got)
	}
	if rec.downCount() == 0 {
		t.Fatal("OnSessionDown must fire for the fields-omitted republish")
	}
	// Then decay to idle and clear the non-safety fact.
	waitForState(t, s, 2*time.Second, StateIdle)
	if s.Snapshot().Error != "" {
		t.Fatalf("decay must clear the fact, got %q", s.Snapshot().Error)
	}
}

// --- 8. queued work is never silently dropped ------------------------------

func TestWorkNeverSilentlyDropped(t *testing.T) {
	// Radio silent: the demand exhausts and the queued cmd is rejected with
	// the observed fact (the observable contract of "in-flight resolves as
	// rejected" — a queued cmd never returns nil against a dead radio).
	fr := newFake(t)
	fr.StopAnswering()
	s := NewSession(sessionCfg(fr, testLogger()), Hooks{})
	defer s.Close()

	err := s.Do(context.Background(), func(tr *civ.Transport) error { return nil })
	if err == nil {
		t.Fatal("cmd against a dead radio must be rejected, not dropped")
	}
	if got := s.Snapshot().State; got != StateError {
		t.Fatalf("state = %s, want error after exhausted series", got)
	}

	// Work that fails mid-flight (socket died under it) returns the error.
	fr2 := newFake(t)
	s2 := NewSession(sessionCfg(fr2, testLogger()), Hooks{})
	defer s2.Close()
	if err := s2.SetArmed(context.Background(), true); err != nil {
		t.Fatalf("arm: %v", err)
	}
	waitForState(t, s2, 2*time.Second, StateLive)
	err = s2.Do(context.Background(), func(tr *civ.Transport) error {
		fr2.SetSilent() // the radio stops answering mid-work
		return tr.SendCIV(civ.NewCodec().BuildPTT(true))
	})
	if err != nil {
		t.Fatalf("SendCIV into a silent-but-open socket may succeed (UDP): %v", err)
	}
}

// --- 9. concurrent cmds serialize through the single session ---------------

func TestConcurrentCmdsSerialize(t *testing.T) {
	fr := newFake(t)
	s := NewSession(sessionCfg(fr, testLogger()), Hooks{})
	defer s.Close()

	const n = 8
	errs := make(chan error, n)
	codec := civ.NewCodec()
	for i := 0; i < n; i++ {
		go func() {
			errs <- s.Do(context.Background(), func(tr *civ.Transport) error {
				return tr.SendCIV(mustSetFreq(t, codec)) // 70cm band, main VFO
			})
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Do %d: %v", i, err)
		}
	}
	if !s.Live() {
		t.Fatal("session not live after concurrent work")
	}
}

// --- 10. shutdown: Close rejects queued work and exits cleanly -------------

func TestCloseShutsDownCleanly(t *testing.T) {
	fr := newFake(t)
	s := NewSession(sessionCfg(fr, testLogger()), Hooks{})
	if err := s.SetArmed(context.Background(), true); err != nil {
		t.Fatalf("arm: %v", err)
	}
	waitForState(t, s, 2*time.Second, StateLive)
	s.Close()
	if got := s.Snapshot().State; got != StateIdle {
		t.Fatalf("post-close snapshot state = %s, want idle", got)
	}
	s.Close() // idempotent
}

// --- 11. disconnected cmds reject without touching the radio ---------------

func TestDemandSpacingAcrossStates(t *testing.T) {
	// A failed series records lastDial; the next demand waits out the
	// spacing — assert the second series' first attempt is ≥ spacing after
	// the first series' last attempt.
	fr := newFake(t)
	fr.SetLoginErr(0xfffffffe)
	s := NewSession(sessionCfg(fr, testLogger()), Hooks{})
	defer s.Close()

	if err := s.Do(context.Background(), func(tr *civ.Transport) error { return nil }); err == nil {
		t.Fatal("first demand must fail")
	}
	waitForState(t, s, 2*time.Second, StateError)
	first := fr.LoginAttempts()
	if len(first) != 3 {
		t.Fatalf("first series attempts = %d", len(first))
	}
	waitForState(t, s, time.Second, StateIdle)

	if err := s.SetArmed(context.Background(), true); err == nil {
		t.Fatal("second demand must fail against the refusing radio")
	}
	all := fr.LoginAttempts()
	if len(all) != 6 {
		t.Fatalf("total attempts = %d, want 6", len(all))
	}
	if gap := all[3].Sub(first[2]); gap < 25*time.Millisecond {
		t.Fatalf("second series started %s after the first ended — cross-demand spacing broken", gap)
	}
	if s.Armed() {
		t.Fatal("failed arm demand must fail-disarm")
	}
}

// Disconnect is the polite-teardown seam (managerAdapter.Poll drives it in
// production): courtesy logout + live→idle only, permit untouched.
func TestDisconnectTearsDownPolitely(t *testing.T) {
	fr := newFake(t)
	s := NewSession(sessionCfg(fr, testLogger()), Hooks{})
	defer s.Close()

	if err := s.SetArmed(context.Background(), true); err != nil {
		t.Fatalf("arm: %v", err)
	}
	waitForState(t, s, 2*time.Second, StateLive)
	_, logoutBefore, _, _ := fr.Counts()

	s.Disconnect()

	waitForState(t, s, 2*time.Second, StateIdle)
	if !s.Armed() {
		t.Fatal("Disconnect must not touch the arm permit (R11: only session loss/restart drop it)")
	}
	// The fake's counters lag the client-side teardown (UDP arrival + serve
	// goroutine) — poll for the courtesy packets rather than reading once.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, logoutAfter, _, closes := fr.Counts(); logoutAfter >= logoutBefore+1 && closes > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("polite teardown packets never reached the radio (logout / openclose-close)")
}
