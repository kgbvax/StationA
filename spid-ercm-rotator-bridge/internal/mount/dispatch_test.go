// SPDX-License-Identifier: AGPL-3.0-or-later

package mount

// Unit tests for the mount dispatch core (plan U4). Pure in-package logic
// over fake controllers — no I/O. The two composition tests at the bottom
// drive U2's and U3's in-process mock devices through the façade to pin that
// their exported contracts satisfy Controller as-is.

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"spid-ercm-rotator-bridge/internal/config"
	"spid-ercm-rotator-bridge/internal/ercm"
	"spid-ercm-rotator-bridge/internal/spid"
)

// The U2/U3 drivers and their in-process mocks satisfy Controller without
// adapters (KTD12) — pinned at compile time.
var (
	_ Controller = spid.Axis(nil)
	_ Controller = (*spid.Mock)(nil)
	_ Controller = (*spid.Driver)(nil)
	_ Controller = (*ercm.Driver)(nil)
)

// --- test scaffolding -----------------------------------------------------------

// testControl returns a control envelope with a deadband large enough that
// boundary behavior is easy to aim at.
func testControl() config.ControlConfig {
	return config.ControlConfig{
		AZ: config.AxisControl{Min: 0, Max: 360, Deadband: 2, Park: 0},
		EL: config.AxisControl{Min: 0, Max: 90, Deadband: 2, Park: 0},
	}
}

// newTestMount builds the façade over two fakes and runs its workers until
// the test ends.
func newTestMount(t *testing.T, ctl config.ControlConfig, az, el *fakeCtrl) *Mount {
	t.Helper()
	m := New(az, el, ctl, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)
	return m
}

// waitFor polls cond until it holds or a generous deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(200 * time.Microsecond)
	}
}

func waitIdle(t *testing.T, m *Mount, ax Axis) {
	t.Helper()
	a := m.axes[ax]
	waitFor(t, fmt.Sprintf("%s axis idle", ax), func() bool {
		return !a.busyNow() && !a.haltingNow()
	})
}

// White-box pipeline-state probes.
func (a *axis) busyNow() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pending != nil || a.inFlight > 0
}

func (a *axis) haltingNow() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.halting
}

func (a *axis) pendingTarget() (float64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending == nil {
		return 0, false
	}
	return *a.pending, true
}

// fakeCtrl is the scriptable per-axis fake: canned readback + validity, a
// liveness switch, a SetTarget write log, a blocking gate for in-flight
// choreography, and concurrency instrumentation.
type fakeCtrl struct {
	mu          sync.Mutex
	online      bool
	rb          float64
	valid       bool
	targets     []float64
	stops       int
	inFlight    int
	maxInFlight int
	gate        chan struct{} // non-nil: every SetTarget parks on it (close to release)
}

// newFake: online by default (tests flip it for liveness refusals); the
// readback starts invalid, matching a driver pre-first-poll.
func newFake() *fakeCtrl { return &fakeCtrl{online: true} }

func (f *fakeCtrl) SetTarget(deg float64) error {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
	f.targets = append(f.targets, deg)
	return nil
}

func (f *fakeCtrl) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return nil
}

func (f *fakeCtrl) Readback() (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rb, f.valid
}

func (f *fakeCtrl) Online() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online
}

func (f *fakeCtrl) setReadback(deg float64, valid bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rb, f.valid = deg, valid
}

func (f *fakeCtrl) setOnline(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.online = on
}

func (f *fakeCtrl) setGate(ch chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = ch
}

func (f *fakeCtrl) written() []float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]float64, len(f.targets))
	copy(out, f.targets)
	return out
}

func (f *fakeCtrl) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

func (f *fakeCtrl) inFlightNow() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight
}

func (f *fakeCtrl) maxConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

// --- basic dispatch ----------------------------------------------------------------

// F1: both axes online, a two-axis intent dispatches once per axis.
func TestGotoDispatchesBothAxes(t *testing.T) {
	az, el := newFake(), newFake()
	az.setReadback(0, true)
	el.setReadback(0, true)
	m := newTestMount(t, testControl(), az, el)

	refs := m.Goto(Target{AZ: 180, EL: 45, HasAZ: true, HasEL: true})
	if len(refs) != 0 {
		t.Fatalf("unexpected refusals: %v", refs)
	}
	waitIdle(t, m, AZ)
	waitIdle(t, m, EL)
	if got := az.written(); len(got) != 1 || got[0] != 180 {
		t.Errorf("az writes = %v, want [180]", got)
	}
	if got := el.written(); len(got) != 1 || got[0] != 45 {
		t.Errorf("el writes = %v, want [45]", got)
	}

	// KTD14 moving inference: target far from readback ⇒ moving; within
	// deadband ⇒ not moving; unknown readback ⇒ never moving.
	if deg, ok := m.Target(AZ); !ok || deg != 180 {
		t.Errorf("Target(AZ) = %v, %v; want 180, true", deg, ok)
	}
	if !m.Moving(AZ) {
		t.Error("Moving(AZ) = false, want true (readback 0, target 180)")
	}
	az.setReadback(179, true)
	if m.Moving(AZ) {
		t.Error("Moving(AZ) = true after readback entered the deadband, want false")
	}
	az.setReadback(0, false)
	if m.Moving(AZ) {
		t.Error("Moving(AZ) = true with unknown readback validity, want false (KTD14)")
	}
}

// R8: an az-only intent never constructs an el intent.
func TestStructurallyOmittedAxisUntouched(t *testing.T) {
	az, el := newFake(), newFake()
	az.setReadback(0, true)
	m := newTestMount(t, testControl(), az, el)

	refs := m.Goto(Target{AZ: 200, HasAZ: true})
	if len(refs) != 0 {
		t.Fatalf("unexpected refusals: %v", refs)
	}
	waitIdle(t, m, AZ)
	if got := az.written(); len(got) != 1 || got[0] != 200 {
		t.Errorf("az writes = %v, want [200]", got)
	}
	if got := el.written(); len(got) != 0 {
		t.Errorf("el writes = %v, want none (structurally omitted)", got)
	}
	if el.stopCount() != 0 {
		t.Errorf("el stops = %d, want 0", el.stopCount())
	}

	// An intent carrying no axis at all is a no-op.
	refs = m.Goto(Target{})
	if len(refs) != 0 || len(az.written()) != 1 || len(el.written()) != 0 {
		t.Errorf("empty target: refs=%v az=%v el=%v", refs, az.written(), el.written())
	}
}

// --- refusals -----------------------------------------------------------------------

// R10: targets outside the travel envelope are refused before any serial write.
func TestLimitRefusalBeforeWrite(t *testing.T) {
	az, el := newFake(), newFake()
	az.setReadback(0, true)
	el.setReadback(0, true)
	m := newTestMount(t, testControl(), az, el)

	refs := m.Goto(Target{AZ: 400, EL: 95, HasAZ: true, HasEL: true})
	if len(refs) != 2 {
		t.Fatalf("refusals = %v, want 2", refs)
	}
	if refs[0].Axis != AZ || refs[0].Reason != RefusalLimit {
		t.Errorf("refusal[0] = %+v, want az/limit", refs[0])
	}
	if refs[1].Axis != EL || refs[1].Reason != RefusalLimit {
		t.Errorf("refusal[1] = %+v, want el/limit", refs[1])
	}
	waitIdle(t, m, AZ)
	waitIdle(t, m, EL)
	if len(az.written()) != 0 || len(el.written()) != 0 {
		t.Errorf("writes after refusal: az=%v el=%v", az.written(), el.written())
	}

	// Non-finite targets are limit refusals too.
	refs = m.Goto(Target{AZ: math.NaN(), HasAZ: true})
	if len(refs) != 1 || refs[0].Reason != RefusalLimit {
		t.Errorf("NaN target: refusals = %v, want one limit refusal", refs)
	}
	refs = m.Goto(Target{EL: math.Inf(1), HasEL: true})
	if len(refs) != 1 || refs[0].Reason != RefusalLimit {
		t.Errorf("Inf target: refusals = %v, want one limit refusal", refs)
	}

	// Refusal is per axis: a valid az target still dispatches alongside a
	// refused el target.
	refs = m.Goto(Target{AZ: 100, EL: 95, HasAZ: true, HasEL: true})
	if len(refs) != 1 || refs[0].Axis != EL || refs[0].Reason != RefusalLimit {
		t.Fatalf("mixed dispatch refusals = %v, want one el/limit", refs)
	}
	waitIdle(t, m, AZ)
	if got := az.written(); len(got) != 1 || got[0] != 100 {
		t.Errorf("az writes = %v, want [100] (per-axis refusal)", got)
	}
}

// AE3 logic side: a dead el axis refuses while az proceeds (R9).
func TestLivenessRefusalPerAxis(t *testing.T) {
	az, el := newFake(), newFake()
	az.setReadback(0, true)
	el.setReadback(0, true)
	// el offline: the device-link layer of the two-liveness rule.
	el.setOnline(false)
	m := newTestMount(t, testControl(), az, el)

	refs := m.Goto(Target{AZ: 90, EL: 30, HasAZ: true, HasEL: true})
	if len(refs) != 1 {
		t.Fatalf("refusals = %v, want 1", refs)
	}
	r := refs[0]
	if r.Axis != EL || r.Reason != RefusalLiveness {
		t.Errorf("refusal = %+v, want el/liveness", r)
	}
	if r.Target != 30 {
		t.Errorf("refusal target = %v, want 30", r.Target)
	}
	waitIdle(t, m, AZ)
	if got := az.written(); len(got) != 1 || got[0] != 90 {
		t.Errorf("az writes = %v, want [90] (az proceeds past a dead el)", got)
	}
	if got := el.written(); len(got) != 0 {
		t.Errorf("el writes = %v, want none (dead axis refuses motion)", got)
	}

	// The refusal must be distinguishable from a limit refusal (U6 maps them
	// to RPRT -9 vs -1; U7 logs them).
	if r.Reason == RefusalLimit {
		t.Error("liveness refusal carries the limit reason")
	}
	if r.Error() == "" || !strings.Contains(r.Error(), "liveness") {
		t.Errorf("refusal Error() = %q, want a liveness-labelled message", r.Error())
	}
}

// U6/U7 need limit and liveness refusals distinguishable in one command.
func TestRefusalReasonsDistinguishable(t *testing.T) {
	az, el := newFake(), newFake()
	el.setOnline(false)
	m := newTestMount(t, testControl(), az, el)

	refs := m.Goto(Target{AZ: 400, EL: 30, HasAZ: true, HasEL: true})
	if len(refs) != 2 {
		t.Fatalf("refusals = %v, want 2", refs)
	}
	var sawLimit, sawLiveness bool
	for _, r := range refs {
		switch r.Reason {
		case RefusalLimit:
			sawLimit = true
		case RefusalLiveness:
			sawLiveness = true
		}
	}
	if !sawLimit || !sawLiveness {
		t.Errorf("mixed refusals = %v, want one limit and one liveness", refs)
	}
	if refs[0].Error() == refs[1].Error() {
		t.Errorf("refusal messages not distinguishable: %q", refs[0].Error())
	}
}

// --- deadband ------------------------------------------------------------------------

// R12 + KTD8 pin: within deadband skips the write; at exactly the configured
// deadband distance it still counts as within; unknown readback never skips.
func TestDeadbandBoundaryExactlyConfigured(t *testing.T) {
	az, el := newFake(), newFake()
	az.setReadback(100, true) // deadband is 2 in testControl()
	m := newTestMount(t, testControl(), az, el)

	writes := 0
	for _, tc := range []struct {
		deg   float64
		write bool
	}{
		{102, false},  // exactly the deadband: within
		{98, false},   // exactly the deadband, other direction
		{102.5, true}, // beyond
		{97.4, true},  // beyond, other direction
		{100, false},  // dead on the readback
	} {
		m.Goto(Target{AZ: tc.deg, HasAZ: true})
		waitIdle(t, m, AZ)
		got := az.written()
		if tc.write {
			if len(got) != writes+1 || got[len(got)-1] != tc.deg {
				t.Errorf("target %v: writes = %v, want a write", tc.deg, got)
			}
			writes = len(got)
		} else if len(got) != writes {
			t.Errorf("target %v: writes = %v, want none (within deadband)", tc.deg, got)
		}
	}
	if len(el.written()) != 0 {
		t.Errorf("el writes = %v, want none", el.written())
	}
}

// KTD7/KTD8: a readback of unknown validity never skips a write.
func TestUnknownReadbackNeverSkips(t *testing.T) {
	az, el := newFake(), newFake()
	az.setReadback(100, false) // stale cache, validity unknown
	m := newTestMount(t, testControl(), az, el)

	m.Goto(Target{AZ: 100, HasAZ: true}) // dead on the cached value
	waitIdle(t, m, AZ)
	if got := az.written(); len(got) != 1 || got[0] != 100 {
		t.Errorf("az writes = %v, want [100] (unknown validity always writes)", got)
	}
	// KTD14: moving is false while validity is unknown, even far away.
	if m.Moving(AZ) {
		t.Error("Moving(AZ) = true with unknown readback validity, want false")
	}
}

// KTD7: a reopen clears readback validity, so a within-deadband target that
// skipped before the outage writes after it.
func TestReopenInvalidatesDeadbandSkip(t *testing.T) {
	az, el := newFake(), newFake()
	az.setReadback(100, true)
	m := newTestMount(t, testControl(), az, el)

	m.Goto(Target{AZ: 101, HasAZ: true}) // within deadband 2 of 100
	waitIdle(t, m, AZ)
	if got := az.written(); len(got) != 0 {
		t.Fatalf("pre-reopen writes = %v, want none", got)
	}

	// Reopen: the driver reports validity unknown until the first fresh reply.
	az.setReadback(100, false)
	m.Goto(Target{AZ: 101, HasAZ: true})
	waitIdle(t, m, AZ)
	if got := az.written(); len(got) != 1 || got[0] != 101 {
		t.Errorf("post-reopen writes = %v, want [101] (always-write after reopen)", got)
	}
}

// --- latest-wins coalescing (AE5, R11) ------------------------------------------------

// A burst of targets beyond slew rate: one in-flight serial motion per axis,
// the mount always moves toward the newest target, superseded targets never
// reach the wire.
func TestBurstLatestWinsOneInFlight(t *testing.T) {
	az, el := newFake(), newFake()
	az.setReadback(0, true)
	m := newTestMount(t, testControl(), az, el)

	gate := make(chan struct{})
	az.setGate(gate)

	m.Goto(Target{AZ: 10, HasAZ: true}) // in-flight, parked on the gate
	waitFor(t, "az in-flight", func() bool { return az.inFlightNow() == 1 })

	m.Goto(Target{AZ: 20, HasAZ: true}) // queued, superseded…
	m.Goto(Target{AZ: 30, HasAZ: true}) // …by this one
	waitFor(t, "newest target pending", func() bool {
		deg, ok := m.axes[AZ].pendingTarget()
		return ok && deg == 30
	})

	close(gate) // release the in-flight write; the pipeline drains to the newest
	waitIdle(t, m, AZ)

	got := az.written()
	if len(got) != 2 || got[0] != 10 || got[1] != 30 {
		t.Errorf("az writes = %v, want [10 30] (superseded 20 never written)", got)
	}
	if az.maxConcurrent() != 1 {
		t.Errorf("max concurrent serial writes = %d, want 1", az.maxConcurrent())
	}
	if len(el.written()) != 0 {
		t.Errorf("el writes = %v, want none (az-only burst)", el.written())
	}
}

// --- stop semantics (KTD8) --------------------------------------------------------------

// Stop clears every queued target on both axes, lets the in-flight write
// finish (the stop frame follows it on the wire), and a post-stop intent is
// admitted — the bounded epoch never jams the pipeline.
func TestStopClearsQueuedCancelsAndAdmitsPostStop(t *testing.T) {
	az, el := newFake(), newFake()
	az.setReadback(0, true)
	el.setReadback(0, true)
	m := newTestMount(t, testControl(), az, el)

	gate := make(chan struct{})
	az.setGate(gate)
	el.setGate(gate)

	m.Goto(Target{AZ: 80, EL: 45, HasAZ: true, HasEL: true}) // both in-flight
	waitFor(t, "both axes in-flight", func() bool {
		return az.inFlightNow() == 1 && el.inFlightNow() == 1
	})
	m.Goto(Target{AZ: 85, EL: 50, HasAZ: true, HasEL: true}) // both queued
	waitFor(t, "both targets queued", func() bool {
		azDeg, azOK := m.axes[AZ].pendingTarget()
		elDeg, elOK := m.axes[EL].pendingTarget()
		return azOK && azDeg == 85 && elOK && elDeg == 50
	})

	done := make(chan struct{})
	go func() {
		m.Stop()
		close(done)
	}()
	waitFor(t, "stop epoch reached both axes", func() bool {
		return m.axes[AZ].haltingNow() && m.axes[EL].haltingNow()
	})

	// Every queued target is cleared on both axes.
	if _, ok := m.axes[AZ].pendingTarget(); ok {
		t.Error("az queued target survived the stop")
	}
	if _, ok := m.axes[EL].pendingTarget(); ok {
		t.Error("el queued target survived the stop")
	}
	// KTD14: the recorded target is cleared, so moving is false after a stop.
	if _, ok := m.Target(AZ); ok {
		t.Error("Target(AZ) survived the stop, want cleared")
	}
	if m.Moving(AZ) || m.Moving(EL) {
		t.Error("Moving true after stop, want false (cleared on stop)")
	}

	close(gate) // the in-flight writes finish; the stop frames follow them
	<-done
	waitIdle(t, m, AZ)
	waitIdle(t, m, EL)

	if got := az.written(); len(got) != 1 || got[0] != 80 {
		t.Errorf("az writes = %v, want [80] (queued 85 canceled)", got)
	}
	if got := el.written(); len(got) != 1 || got[0] != 45 {
		t.Errorf("el writes = %v, want [45] (queued 50 canceled)", got)
	}
	if az.stopCount() != 1 || el.stopCount() != 1 {
		t.Errorf("stop frames: az=%d el=%d, want 1 each", az.stopCount(), el.stopCount())
	}

	// Bounded epoch (KTD8): an intent admitted after the stop dispatches.
	refs := m.Goto(Target{AZ: 90, EL: 60, HasAZ: true, HasEL: true})
	if len(refs) != 0 {
		t.Fatalf("post-stop intent refused: %v", refs)
	}
	waitIdle(t, m, AZ)
	waitIdle(t, m, EL)
	if got := az.written(); len(got) != 2 || got[1] != 90 {
		t.Errorf("az writes = %v, want [80 90] (post-stop intent admitted)", got)
	}
	if got := el.written(); len(got) != 2 || got[1] != 60 {
		t.Errorf("el writes = %v, want [45 60] (post-stop intent admitted)", got)
	}
}

// --- park (R7, KTD11) -------------------------------------------------------------------

// PARK's stop phase cancels the queue, then the park targets dispatch through
// the normal paths — the park slew is never suppressed by its own stop.
func TestParkStopThenDispatchNoSelfSuppression(t *testing.T) {
	az, el := newFake(), newFake()
	az.setReadback(5, true) // park is 0: |0−5| > deadband 2 ⇒ the park target must write
	el.setReadback(5, true)
	m := newTestMount(t, testControl(), az, el)

	gate := make(chan struct{})
	az.setGate(gate)
	m.Goto(Target{AZ: 40, HasAZ: true}) // in-flight, parked on the gate
	waitFor(t, "az in-flight", func() bool { return az.inFlightNow() == 1 })
	m.Goto(Target{AZ: 60, HasAZ: true}) // queued
	waitFor(t, "az target queued", func() bool {
		deg, ok := m.axes[AZ].pendingTarget()
		return ok && deg == 60
	})

	parked := make(chan struct{})
	go func() {
		m.Park()
		close(parked)
	}()
	waitFor(t, "park stop phase reached az", func() bool { return m.axes[AZ].haltingNow() })
	if _, ok := m.axes[AZ].pendingTarget(); ok {
		t.Error("az queued target survived PARK's stop phase")
	}

	close(gate)
	<-parked
	waitIdle(t, m, AZ)
	waitIdle(t, m, EL)

	if got := az.written(); len(got) != 2 || got[0] != 40 || got[1] != 0 {
		t.Errorf("az writes = %v, want [40 0] (in-flight completes, park target dispatches)", got)
	}
	if az.stopCount() != 1 {
		t.Errorf("az stop frames = %d, want 1", az.stopCount())
	}
	if got := el.written(); len(got) != 1 || got[0] != 0 {
		t.Errorf("el writes = %v, want [0] (el park target dispatched)", got)
	}
	if el.stopCount() != 1 {
		t.Errorf("el stop frames = %d, want 1", el.stopCount())
	}
	if deg, ok := m.Target(AZ); !ok || deg != 0 {
		t.Errorf("Target(AZ) after park = %v, %v; want 0, true", deg, ok)
	}
}

// An already-at-park axis is a deadband no-op: the stop phase still halts,
// but no park slew is written.
func TestParkAlreadyAtParkDeadbandNoop(t *testing.T) {
	az, el := newFake(), newFake()
	az.setReadback(0, true) // park is 0: dead on the readback
	el.setReadback(0, true)
	m := newTestMount(t, testControl(), az, el)

	m.Goto(Target{AZ: 10, EL: 10, HasAZ: true, HasEL: true})
	waitIdle(t, m, AZ)
	waitIdle(t, m, EL)

	if refs := m.Park(); len(refs) != 0 {
		t.Fatalf("park refusals = %v, want none", refs)
	}
	waitIdle(t, m, AZ)
	waitIdle(t, m, EL)

	if got := az.written(); len(got) != 1 || got[0] != 10 {
		t.Errorf("az writes = %v, want [10] only (park within deadband is a no-op)", got)
	}
	if got := el.written(); len(got) != 1 || got[0] != 10 {
		t.Errorf("el writes = %v, want [10] only (park within deadband is a no-op)", got)
	}
	if az.stopCount() != 1 || el.stopCount() != 1 {
		t.Errorf("stop frames: az=%d el=%d, want 1 each (stop phase still halts)", az.stopCount(), el.stopCount())
	}
	if m.Moving(AZ) || m.Moving(EL) {
		t.Error("Moving true while parked within deadband, want false")
	}
}

// A dead axis refuses its park target while the live axis still parks (R9
// applies to park dispatch like any other path).
func TestParkDeadAxisRefusedPerAxis(t *testing.T) {
	az, el := newFake(), newFake()
	az.setReadback(5, true)
	el.setOnline(false)
	m := newTestMount(t, testControl(), az, el)

	refs := m.Park()
	if len(refs) != 1 || refs[0].Axis != EL || refs[0].Reason != RefusalLiveness {
		t.Fatalf("park refusals = %v, want one el/liveness", refs)
	}
	waitIdle(t, m, AZ)
	if got := az.written(); len(got) != 1 || got[0] != 0 {
		t.Errorf("az writes = %v, want [0] (az parks past a dead el)", got)
	}
	if got := el.written(); len(got) != 0 {
		t.Errorf("el writes = %v, want none", got)
	}
}

// --- composition with the real U2/U3 mocks ---------------------------------------------

// The façade drives U2's in-process SPID mock and U3's driver-over-MockDevice
// directly — pinning that the driver contracts need no adapters (KTD12) and
// that the mocks are usable as fakes for the layers above.
func TestDriverMocksCompose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	azMock := spid.NewMock(spid.Opts{
		PollInterval:   2 * time.Millisecond,
		ReopenCooldown: 5 * time.Millisecond,
		WritePace:      time.Millisecond,
		ReadTimeout:    50 * time.Millisecond,
	})
	defer azMock.Close()
	go azMock.RunPoll(ctx)

	dev := ercm.NewMock()
	elDrv := ercm.New(ercm.Config{
		Opener:         func() (io.ReadWriteCloser, error) { return dev.Port(), nil },
		PollInterval:   2 * time.Millisecond,
		ReopenCooldown: 5 * time.Millisecond,
		ReplyTimeout:   100 * time.Millisecond,
	}, nil)
	go elDrv.Run(ctx)

	m := New(azMock, elDrv, testControl(), nil)
	go m.Run(ctx)

	waitFor(t, "az link online", azMock.Online)
	waitFor(t, "el link online", elDrv.Online)
	waitFor(t, "el readback valid", func() bool {
		_, valid := elDrv.Readback()
		return valid
	})

	refs := m.Goto(Target{AZ: 123, EL: 30, HasAZ: true, HasEL: true})
	if len(refs) != 0 {
		t.Fatalf("unexpected refusals: %v", refs)
	}
	waitFor(t, "SPID parked at 123", func() bool { return azMock.Target() == 123 })
	waitFor(t, "ERC-M W goto on the wire", func() bool {
		for _, w := range dev.Writes() {
			if w == "W000 030" {
				return true
			}
		}
		return false
	})

	// Stop reaches both drivers.
	m.Stop()
	waitFor(t, "SPID stop frame", func() bool {
		writes := azMock.Writes()
		return len(writes) > 0 && writes[len(writes)-1][11] == 0x0F
	})
	waitFor(t, "ERC-M stop frame", func() bool { return dev.StopCount() >= 1 })
}
