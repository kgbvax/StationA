package bridge

import (
	"testing"
	"time"

	"icom9700-radio-bridge/internal/radio"
)

// KTD-2/R2 regression: the telemetry poll is not itself a demand. From a
// healthy idle session it must return without dialing the radio — a
// demanding poll would grab the radio's single LAN session around the clock
// and starve manual wfview operating.
func TestPollIdleDoesNotDial(t *testing.T) {
	h := newBH(t)
	// The state machine reaches idle asynchronously — wait for it before
	// stimulating, or the snapshot below races the startup transition.
	deadline := time.Now().Add(2 * time.Second)
	for h.b.mgr.Snapshot().SessionState != radio.StateIdle {
		if time.Now().After(deadline) {
			t.Fatalf("session never reached idle (at %q)", h.b.mgr.Snapshot().SessionState)
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.pollNow()
	// A demanding poll would drive idle -> connecting -> (the fake radio
	// answers) live within milliseconds. The guard must leave the session
	// untouched and the radio silent.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if st := h.b.mgr.Snapshot().SessionState; st != radio.StateIdle {
			t.Fatalf("session_state = %q after idle poll, want idle (the poll dialed the radio)", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// While a session is live (opened by a real demand — arm here), the poll
// reads radio state as designed: /state gains the radio-measured fields.
func TestPollLiveReadsRadio(t *testing.T) {
	h := newBH(t)
	h.armAndLive()
	h.pollNow()
	h.b.publishState(false)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m := h.cli.lastState(t)
		if _, ok := m["freq_hz"].(float64); ok {
			if on, _ := m["device_online"].(bool); on {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("/state has no live radio truth after a live poll: %v", h.cli.lastState(t))
}
