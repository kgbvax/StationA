package programs

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeRequest struct {
	held       bool
	sets, clrs int
	err        error
}

func (r *fakeRequest) Set() error {
	if r.err != nil {
		return r.err
	}
	r.sets++
	r.held = true
	return nil
}

func (r *fakeRequest) Clear() error {
	r.clrs++
	r.held = false
	return nil
}

func newHold() (*Activity, *PowerHold, *fakeRequest) {
	a := &Activity{}
	r := &fakeRequest{}
	return a, NewPowerHold(a, r, slog.New(slog.NewTextHandler(io.Discard, nil))), r
}

func TestHoldWhileChainRuns(t *testing.T) {
	a, h, r := newHold()
	now := time.Unix(1_800_000_000, 0)
	a.PowerSeqState([]byte(`{"phase":"running","chain":{"state":"running","step":"dax"}}`))
	h.Update(now)
	if !r.held {
		t.Fatal("no power request while the chain runs")
	}
	a.PowerSeqState([]byte(`{"phase":"running","chain":{"state":"ready"}}`))
	h.Update(now)
	if r.held {
		t.Fatal("power request kept after the chain ended")
	}
}

func TestHoldWhileSessionHasFreshBeats(t *testing.T) {
	a, h, r := newHold()
	now := time.Unix(1_800_000_000, 0)
	a.AutoOpState([]byte(`{"session":{"state":"running"}}`), false, now)
	h.Update(now)
	if !r.held || !a.SessionActive(now) {
		t.Fatal("no power request during a fresh session")
	}
	a.AutoOpBeat(now.Add(20 * time.Second))
	h.Update(now.Add(40 * time.Second))
	if !r.held {
		t.Fatal("released while beats are fresh")
	}
	// No beat for more than 30 s: 75auto froze or crashed.
	h.Update(now.Add(51 * time.Second))
	if r.held || a.SessionActive(now.Add(51*time.Second)) {
		t.Fatal("power request kept without fresh beats")
	}
	if r.sets != 1 || r.clrs != 1 {
		t.Errorf("sets = %d clears = %d, want one each (edge-triggered)", r.sets, r.clrs)
	}
}

func TestSessionEndedReleases(t *testing.T) {
	a, h, r := newHold()
	now := time.Unix(1_800_000_000, 0)
	a.AutoOpState([]byte(`{"session":{"state":"connecting"}}`), false, now)
	h.Update(now)
	a.AutoOpState([]byte(`{"session":{"state":"ended"}}`), false, now.Add(time.Second))
	h.Update(now.Add(time.Second))
	if r.held {
		t.Fatal("power request kept after the session ended")
	}
}

func TestRetainedSessionStateIsNotFresh(t *testing.T) {
	a, h, r := newHold()
	now := time.Unix(1_800_000_000, 0)
	a.AutoOpState([]byte(`{"session":{"state":"running"}}`), true, now)
	h.Update(now)
	if r.held || a.SessionActive(now) {
		t.Fatal("a retained replay counted as a fresh session")
	}
	a.AutoOpBeat(now.Add(time.Second))
	if !a.SessionActive(now.Add(time.Second)) {
		t.Fatal("beat after a retained running state did not make the session active")
	}
}

func TestSetFailureRetried(t *testing.T) {
	a, h, r := newHold()
	now := time.Unix(1_800_000_000, 0)
	a.PowerSeqState([]byte(`{"chain":{"state":"running"}}`))
	r.err = errors.New("denied")
	h.Update(now)
	r.err = nil
	h.Update(now)
	if !r.held {
		t.Fatal("failed Set not retried")
	}
}

func TestMalformedPayloadsIgnored(t *testing.T) {
	a, h, r := newHold()
	now := time.Unix(1_800_000_000, 0)
	a.PowerSeqState([]byte(`{"chain":{"state":"running"}}`))
	a.PowerSeqState([]byte(`not json`))
	h.Update(now)
	if !r.held {
		t.Fatal("malformed payload changed the chain state")
	}
}
