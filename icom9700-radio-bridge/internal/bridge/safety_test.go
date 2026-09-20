package bridge

import (
	"errors"
	"testing"
	"time"
)

// --- U6 safety core (plan R4/R10/R11/R12/R13) ---------------------------------
//
// Every loss path ends the same way: PTT-off attempted, permit dropped, the
// safety fact published — asserted against the fake radio's actual PTT
// state, never the bridge's own bookkeeping alone.

func armedInState(h *bharness) bool {
	m := h.cli.lastState(h.t)
	armed, _ := m["armed"].(bool)
	return armed
}

// The watchdog forces the unkey, drops the permit, and publishes the
// safety-class fact that persists until the operator's ack (KTD-5/R12).
func TestWatchdogForcesPttOffAndDisarm(t *testing.T) {
	h := newBHWithTimings(t, 300*time.Millisecond, 10*time.Second)
	h.armAndLive()

	h.cli.sendCmd([]byte(`{"action":"ptt","value":"on"}`))
	waitTrue(t, "fake PTT keyed", 3*time.Second, func() bool { return h.f.PTT() })

	// The bound elapses: the radio MUST unkey without operator action.
	waitTrue(t, "watchdog forced the unkey", 5*time.Second, func() bool { return !h.f.PTT() })
	waitTrue(t, "armed dropped", 3*time.Second, func() bool { return !armedInState(h) })
	waitCmdErr(t, h.cli, errTxWatchdog)

	// The safety fact persists — a poll/heartbeat must not decay it.
	h.pollNow()
	h.b.publishState(false)
	if e := h.cli.stateErr(t); e != errTxWatchdog {
		t.Fatalf("watchdog error decayed after a poll: %q", e)
	}

	// Operator ack (arm) clears the safety fact.
	h.cli.sendCmd([]byte(`{"action":"arm"}`))
	waitCmdErr(t, h.cli, "")
}

// The watchdog trip WITHOUT a live session: immediate disarm + the
// undeliverable fact, and the OnLive hook delivers the owed PTT-off on the
// safety-driven reconnect (the session-scoped watchdog contract).
func TestWatchdogWithoutSessionRedeliversOnReconnect(t *testing.T) {
	h := newBHWithTimings(t, 250*time.Millisecond, 400*time.Millisecond)
	h.armAndLive()

	h.cli.sendCmd([]byte(`{"action":"ptt","value":"on"}`))
	waitTrue(t, "fake PTT keyed", 3*time.Second, func() bool { return h.f.PTT() })

	// Kill the session path while keyed: the loss fires first (fail-disarm),
	// then the watchdog trip cannot deliver — the fact records undeliverable.
	h.f.SetSilent(true)
	waitTrue(t, "session loss disarmed", 6*time.Second, func() bool { return !armedInState(h) })

	// Heal the radio and let the operator back in: the reconnect must carry
	// the pending PTT-off BEFORE any pending demand (fireOnLive ordering).
	// (The failed redelivery burned the full handshake budget twice against
	// the silent fake, so the window is generous.)
	h.f.SetSilent(false)
	h.cli.sendCmd([]byte(`{"action":"arm"}`))
	waitTrue(t, "pending ptt-off delivered on reconnect", 10*time.Second,
		func() bool { return !h.f.PTT() })
	// The permit is back and the fact is on the bus (fresh, not a stale
	// snapshot: the redelivery flipped tx, so the poll republishes).
	h.pollNow()
	h.b.publishState(false)
	waitTrue(t, "re-armed on the bus", 3*time.Second, func() bool { return armedInState(h) })
}

// MQTT-plane loss: the permit drops and the unkey is attempted DIRECTLY
// against the still-live radio session (the CI-V path does not depend on
// the broker).
func TestMqttLossDisarmsAndForcesPttOff(t *testing.T) {
	h := newBH(t)
	h.armAndLive()

	h.cli.sendCmd([]byte(`{"action":"ptt","value":"on"}`))
	waitTrue(t, "fake PTT keyed", 3*time.Second, func() bool { return h.f.PTT() })

	// The paho ConnectionLostHandler delegates here (Start wires it); the
	// test calls the seam directly.
	h.b.onMqttLoss(errors.New("test: broker gone"))

	waitTrue(t, "mqtt loss forced the unkey", 3*time.Second, func() bool { return !h.f.PTT() })
	waitTrue(t, "mqtt loss disarmed", 3*time.Second, func() bool { return !armedInState(h) })

	// The permit is gone: PTT is refused with the taxonomy rejection.
	h.cli.sendCmd([]byte(`{"action":"ptt","value":"on"}`))
	waitCmdErr(t, h.cli, errPttNotArmed)
}

// Session loss mid-PTT: immediate disarm (R11) and the owed PTT-off rides
// the safety-driven reconnect.
func TestSessionLossMidPttDisarmsAndRedelivers(t *testing.T) {
	h := newBHWithTimings(t, 0, 400*time.Millisecond)
	h.armAndLive()

	h.cli.sendCmd([]byte(`{"action":"ptt","value":"on"}`))
	waitTrue(t, "fake PTT keyed", 3*time.Second, func() bool { return h.f.PTT() })

	// The radio goes silent: the session's loss watchdog fires OnLoss —
	// fail-disarm, pendingOff queued; the redelivery demand fails against
	// the silent radio (recorded undeliverable) and stays pending.
	h.f.SetSilent(true)
	waitTrue(t, "session loss disarmed", 6*time.Second, func() bool { return !armedInState(h) })

	// Heal: the operator's arm demand reconnects; OnLive delivers the owed
	// PTT-off before anything else runs.
	h.f.SetSilent(false)
	h.cli.sendCmd([]byte(`{"action":"arm"}`))
	waitTrue(t, "pending ptt-off delivered on reconnect", 10*time.Second,
		func() bool { return !h.f.PTT() })
	h.pollNow()
	h.b.publishState(false)
	waitTrue(t, "re-armed on the bus", 3*time.Second, func() bool { return armedInState(h) })
}

// A session drop drops the permit even without PTT (R11: armed drops on
// session loss; mqtt-api.md's contract is now true).
func TestSessionLossDisarmsWithoutPtt(t *testing.T) {
	h := newBHWithTimings(t, 0, 400*time.Millisecond)
	h.armAndLive()

	h.f.SetSilent(true)
	waitTrue(t, "session loss disarmed", 6*time.Second, func() bool { return !armedInState(h) })
}

// A fresh bridge process boots disarmed (R11: no outstanding PTT-off to
// deliver, nothing retained to honor).
func TestBootFailDisarmed(t *testing.T) {
	h := newBH(t)
	if armedInState(h) {
		t.Fatal("fresh bridge booted armed")
	}
	h.cli.sendCmd([]byte(`{"action":"ptt","value":"on"}`))
	waitCmdErr(t, h.cli, errPttNotArmed)
	if h.f.PTT() {
		t.Error("PTT frame reached the radio on a fresh boot")
	}
}

// The unkey is never gated: a ptt-off proceeds without the permit (the one
// direction that must always work), and ends with the radio released and
// the permit still absent.
func TestPttOffAllowedWhileDisarmed(t *testing.T) {
	h := newBH(t)
	h.armAndLive()

	h.cli.sendCmd([]byte(`{"action":"ptt","value":"on"}`))
	waitTrue(t, "fake PTT keyed", 3*time.Second, func() bool { return h.f.PTT() })

	h.cli.sendCmd([]byte(`{"action":"disarm"}`))
	waitTrue(t, "disarmed while keyed", 3*time.Second, func() bool { return !armedInState(h) })

	h.cli.sendCmd([]byte(`{"action":"ptt","value":"off"}`))
	waitTrue(t, "unkeyed while disarmed", 3*time.Second, func() bool { return !h.f.PTT() })
	waitTrue(t, "permit still absent", time.Second, func() bool { return !armedInState(h) })
}
