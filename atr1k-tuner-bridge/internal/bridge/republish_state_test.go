package bridge

import (
	"testing"

	"atr1k-tuner-bridge/internal/tuner"
)

// RepublishState re-lands the retained /state after an MQTT reconnect despite
// the change-only dedup (review S2) — the gap: a broker flush left /state
// missing indefinitely while the tuner sat steady. It must no-op before the
// first telemetry (an early OnConnect must not publish a zero snapshot).
func TestRepublishState(t *testing.T) {
	b, pub, _ := newTestBridge(t)
	statePublishes := func() int {
		n := 0
		for _, m := range pub.Messages() {
			if m.Topic == "muehle/hf/tuner/state" {
				n++
			}
		}
		return n
	}

	// Before first telemetry: deliberate no-op.
	b.RepublishState()
	if got := statePublishes(); got != 0 {
		t.Fatalf("RepublishState before first telemetry published %d states, want 0", got)
	}

	// First telemetry publishes once and arms the dedup latch.
	b.HandleTelemetry(tuner.State{Inline: true, SWR: 1.2})
	if got := statePublishes(); got != 1 {
		t.Fatalf("first telemetry published %d states, want 1", got)
	}

	// An unchanged snapshot alone must NOT publish (dedup holds)...
	b.HandleTelemetry(tuner.State{Inline: true, SWR: 1.2})
	if got := statePublishes(); got != 1 {
		t.Fatalf("unchanged telemetry republished (%d states), want dedup", got)
	}

	// ...but the reconnect restore re-lands it.
	b.RepublishState()
	if got := statePublishes(); got != 2 {
		t.Fatalf("RepublishState produced %d states, want 2 (the reconnect re-land)", got)
	}
}
