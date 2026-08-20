package bridge

import (
	"testing"
	"time"

	"acom1200s-pa-bridge/internal/acom"
)

// TestHandleTelemetryDeduplicatesIdleRx verifies that many identical OPR/RX
// telemetry frames only produce one /state publish, then a heartbeat after the
// interval elapses.
func TestHandleTelemetryDeduplicatesIdleRx(t *testing.T) {
	b, pub := newTestBridge(t, &fakeCommander{}, false)

	base := acom.Observation{
		ForwardPower:   0,
		ReflectedPower: 0,
		SWR:            0,
		Temperature:    21.9,
		BandIndex:      3,
		BandName:       "80m",
		ModeRaw:        "OPR/RX",
		ErrByte:        0xFF,
		ErrMsg:         "NONE",
	}

	// First frame publishes.
	b.HandleTelemetry(base)
	if got := len(pub.Messages()); got != 1 {
		t.Fatalf("first frame: got %d publishes, want 1", got)
	}

	// Many identical RX frames should be suppressed.
	for i := 0; i < 20; i++ {
		b.HandleTelemetry(base)
	}
	if got := len(pub.Messages()); got != 1 {
		t.Fatalf("after 20 identical RX frames: got %d publishes, want 1", got)
	}

	// After the heartbeat interval, one more identical frame publishes.
	b.mu.Lock()
	b.lastPubAt = time.Now().Add(-heartbeatInterval - time.Millisecond)
	b.mu.Unlock()
	b.HandleTelemetry(base)
	if got := len(pub.Messages()); got != 2 {
		t.Fatalf("after heartbeat: got %d publishes, want 2", got)
	}
}

// TestHandleTelemetryAlwaysPublishesWhileKeyed verifies that every telemetry
// frame publishes while keyed=tx (live meters during transmission).
func TestHandleTelemetryAlwaysPublishesWhileKeyed(t *testing.T) {
	b, pub := newTestBridge(t, &fakeCommander{}, false)

	base := acom.Observation{
		ForwardPower:   100,
		ReflectedPower: 2,
		SWR:            1.3,
		Temperature:    35.0,
		BandIndex:      5,
		BandName:       "20m",
		ModeRaw:        "OPR/TX",
		ErrByte:        0xFF,
		ErrMsg:         "NONE",
	}

	for i := 0; i < 5; i++ {
		b.HandleTelemetry(base)
	}
	if got := len(pub.Messages()); got != 5 {
		t.Fatalf("while keyed=tx: got %d publishes, want 5", got)
	}
}

// TestHandleTelemetryPublishesOnSignificantChange verifies that a change in a
// non-noise field (mode, band, fault, power, keyed state) triggers a publish
// even in RX and before the heartbeat.
func TestHandleTelemetryPublishesOnSignificantChange(t *testing.T) {
	b, pub := newTestBridge(t, &fakeCommander{}, false)

	rx1 := acom.Observation{
		ForwardPower:   0,
		ReflectedPower: 0,
		SWR:            0,
		Temperature:    21.9,
		BandIndex:      3,
		BandName:       "80m",
		ModeRaw:        "OPR/RX",
		ErrByte:        0xFF,
		ErrMsg:         "NONE",
	}
	b.HandleTelemetry(rx1) // publish #1

	// Identical temp-only change should not publish.
	rx2 := rx1
	rx2.Temperature = 22.0
	b.HandleTelemetry(rx2)
	if got := len(pub.Messages()); got != 1 {
		t.Fatalf("temp-only change: got %d publishes, want 1", got)
	}

	// Band change should publish immediately.
	rx3 := rx1
	rx3.BandIndex = 5
	rx3.BandName = "20m"
	b.HandleTelemetry(rx3)
	if got := len(pub.Messages()); got != 2 {
		t.Fatalf("band change: got %d publishes, want 2", got)
	}
}
