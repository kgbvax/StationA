package bridge

import (
	"testing"
	"time"

	"icom9700-radio-bridge/internal/radio"
)

// device_online is CAPTURE liveness (R16, redefined 2026-09): it follows
// session_state==live. The healthy idle reads false — never a station
// fault — and an audio demand flips it true.
func TestDeviceOnlineFollowsCapture(t *testing.T) {
	h := newBH(t)
	// Healthy idle: device_online false, and the slot must NOT read as an
	// offline fault anywhere.
	waitTrue(t, "session reached idle", 2*time.Second, func() bool {
		return h.b.mgr.Snapshot().SessionState == radio.StateIdle
	})
	m := h.cli.lastState(t)
	if on, _ := m["device_online"].(bool); on {
		t.Error("device_online true at healthy idle — R16 violated")
	}

	h.captureLive()
	waitTrue(t, "device_online true while capturing", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		on, _ := m["device_online"].(bool)
		return on
	})
}

// The telemetry fields are monitor-gated: absent entirely while the
// monitor is off (never zeroed, never frozen — R6), present while on and
// the radio responds.
func TestStateShapeMonitorGate(t *testing.T) {
	h := newBHMon(t)

	// Monitor off: no telemetry keys at all.
	m := h.cli.lastState(t)
	for _, key := range []string{"freq_hz", "mode", "s_meter", "swr", "alc", "radio_responding"} {
		if _, has := m[key]; has {
			t.Errorf("%q present while the monitor is off", key)
		}
	}
	if on, _ := m["monitor"].(bool); on {
		t.Error("monitor flag true while off")
	}

	h.cli.sendCmd([]byte(`{"action":"monitor_on"}`))
	waitTrue(t, "monitor on in /state", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		on, _ := m["monitor"].(bool)
		return on
	})

	// Responding radio: the measured fields appear.
	h.mon.push(RadioState{
		Responding: true,
		FreqHz:     145800000,
		Band:       "2m",
		Mode:       "fm",
		Satellite:  true,
		SMeter:     intPtr(88),
	})
	waitTrue(t, "telemetry while monitor on", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		fh, _ := m["freq_hz"].(float64)
		return fh == 145800000
	})
	m = h.cli.lastState(t)
	if sat, _ := m["satellite"].(bool); !sat {
		t.Errorf("satellite = %v", m["satellite"])
	}

	// monitor_off: telemetry gone again (the monitor flag flips and the
	// fields are omitted in the same publish).
	h.cli.sendCmd([]byte(`{"action":"monitor_off"}`))
	waitTrue(t, "monitor off in /state", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		on, _ := m["monitor"].(bool)
		return !on
	})
	if _, has := h.cli.lastState(t)["freq_hz"]; has {
		t.Error("telemetry survived monitor_off")
	}
}

// A live-but-deaf radio (monitor on, no CI-V answer): radio_responding
// false and the measured fields omitted — never frozen values.
func TestStateDeafRadioOmitsTelemetry(t *testing.T) {
	h := newBHMon(t)
	h.cli.sendCmd([]byte(`{"action":"monitor_on"}`))
	waitTrue(t, "monitor on in /state", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		on, _ := m["monitor"].(bool)
		return on
	})

	// First an answering radio (values in /state), then deafness.
	h.mon.push(RadioState{Responding: true, FreqHz: 432650000, Band: "70cm", Mode: "fm"})
	waitTrue(t, "answering telemetry", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		fh, _ := m["freq_hz"].(float64)
		return fh == 432650000
	})
	h.mon.push(RadioState{Responding: false})
	waitTrue(t, "deaf gate", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		rr, ok := m["radio_responding"].(bool)
		_, hasFreq := m["freq_hz"]
		return ok && !rr && !hasFreq
	})
}

// The heartbeat keeps the retained ts fresh while the snapshot is
// unchanged: a forced publish after the heartbeat window goes out with a
// new ts even though nothing changed (KTD14).
func TestHeartbeatFreshTs(t *testing.T) {
	h := newBH(t)
	first := h.cli.lastState(t)

	// An immediate republish of the unchanged snapshot dedups away.
	h.cli.mu.Lock()
	before := len(h.cli.pubs)
	h.cli.mu.Unlock()
	h.b.publishState(false)
	h.cli.mu.Lock()
	after := len(h.cli.pubs)
	h.cli.mu.Unlock()
	if after != before {
		t.Fatalf("unchanged snapshot republished (%d -> %d)", before, after)
	}

	// Past the heartbeat window the unchanged snapshot republishes (with a
	// fresh ts — RFC3339's second granularity makes an ts-string comparison
	// flaky when both publishes land in the same second, so assert on the
	// publish itself).
	h.b.mu.Lock()
	h.b.lastPub = h.b.lastPub.Add(-(stateHeartbeat + time.Second))
	h.b.mu.Unlock()
	h.cli.mu.Lock()
	before = len(h.cli.pubs)
	h.cli.mu.Unlock()
	h.b.publishState(false)
	h.cli.mu.Lock()
	after = len(h.cli.pubs)
	h.cli.mu.Unlock()
	if after != before+1 {
		t.Error("heartbeat-window publish deduped away — the retained ts would go stale (KTD14 violated)")
	}
	_ = first
}
