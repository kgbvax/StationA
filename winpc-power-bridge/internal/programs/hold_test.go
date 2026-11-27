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
	a.PowerSeqState([]byte(`{"phase":"running","chain":{"state":"running","step":"dax"}}`), now)
	h.Update(now)
	if !r.held {
		t.Fatal("no power request while the chain runs")
	}
	a.PowerSeqState([]byte(`{"phase":"running","chain":{"state":"failed"}}`), now)
	h.Update(now)
	if r.held {
		t.Fatal("power request kept after the chain failed")
	}
}

func TestHoldWhileSessionHasFreshBeats(t *testing.T) {
	a, h, r := newHold()
	now := time.Unix(1_800_000_000, 0)
	a.AutoOpState([]byte(`{"session":{"state":"running","source":"live"}}`), false, now)
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
	a.AutoOpState([]byte(`{"session":{"state":"connecting","source":"live"}}`), false, now)
	h.Update(now)
	a.AutoOpState([]byte(`{"session":{"state":"ended","source":"live"}}`), false, now.Add(time.Second))
	h.Update(now.Add(time.Second))
	if r.held {
		t.Fatal("power request kept after the session ended")
	}
}

func TestRetainedSessionStateIsNotFresh(t *testing.T) {
	a, h, r := newHold()
	now := time.Unix(1_800_000_000, 0)
	a.AutoOpState([]byte(`{"session":{"state":"running","source":"live"}}`), true, now)
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
	a.PowerSeqState([]byte(`{"chain":{"state":"running"}}`), now)
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
	a.PowerSeqState([]byte(`{"chain":{"state":"running"}}`), now)
	a.PowerSeqState([]byte(`not json`), now)
	h.Update(now)
	if !r.held {
		t.Fatal("malformed payload changed the chain state")
	}
}

func TestHoldWhileReadyChainIsYoung(t *testing.T) {
	a, h, r := newHold()
	now := time.Unix(1_800_000_000, 0)
	ts := now.UTC().Format(time.RFC3339)
	a.PowerSeqState([]byte(`{"chain":{"state":"ready","ts":"`+ts+`"}}`), now.Add(time.Minute))
	h.Update(now.Add(29 * time.Minute))
	if !r.held {
		t.Fatal("no power request for a ready chain younger than 30 min")
	}
	h.Update(now.Add(31 * time.Minute))
	if r.held {
		t.Fatal("power request kept for a ready chain older than 30 min")
	}
}

func TestReadyChainWithoutTSUsesReceiveTime(t *testing.T) {
	a, h, r := newHold()
	now := time.Unix(1_800_000_000, 0)
	a.PowerSeqState([]byte(`{"chain":{"state":"ready","ts":"garbage"}}`), now)
	h.Update(now.Add(29 * time.Minute))
	if !r.held {
		t.Fatal("no power request for a ready chain received less than 30 min ago")
	}
	h.Update(now.Add(30 * time.Minute))
	if r.held {
		t.Fatal("power request kept 30 min after the ready chain arrived")
	}
}

func TestReplaySessionDoesNotHold(t *testing.T) {
	a, h, r := newHold()
	now := time.Unix(1_800_000_000, 0)
	a.AutoOpState([]byte(`{"session":{"state":"running","source":"replay"}}`), false, now)
	a.AutoOpBeat(now)
	h.Update(now)
	if r.held || a.SessionActive(now) {
		t.Fatal("a replay session counted as active")
	}
	a.AutoOpState([]byte(`{"session":{"state":"running"}}`), false, now)
	if a.SessionActive(now) {
		t.Fatal("a session without source counted as live")
	}
}

func TestOfflinePowerSeqIgnoresRunningChain(t *testing.T) {
	a, h, r := newHold()
	now := time.Unix(1_800_000_000, 0)
	a.PowerSeqState([]byte(`{"chain":{"state":"running"}}`), now)
	a.PowerSeqStatus([]byte("offline"))
	h.Update(now)
	if r.held {
		t.Fatal("power request held for the retained chain of an offline powerseq")
	}
	a.PowerSeqStatus([]byte("online"))
	h.Update(now)
	if !r.held {
		t.Fatal("no power request after powerseq came back online")
	}
}
