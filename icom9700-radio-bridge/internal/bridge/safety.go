// safety.go — the U6 safety core (plan R4/R10/R11/R12/R13, KTD-4/KTD-5):
// the armed-permit lifecycle, the max-TX watchdog, and the loss-of-plane
// rules. The permit is bridge-local (fail-disarmed on boot/restart), drops
// on session loss and MQTT-plane loss, and the watchdog bounds a keyed PTT
// even while everything else is healthy. PTT-off delivery is attempted
// directly against the radio session — independent of the MQTT plane — and
// re-issued with priority on the safety-driven reconnect (the OnLive hook
// fires before any pending demand).
//
// Watchdog-trip and undeliverable facts are safety-class /state.error:
// they persist until the operator's next arm/cmd ack instead of decaying
// with the session state (R17 posture — observed facts only).
package bridge

import (
	"context"
	"errors"
	"time"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"

	"icom9700-radio-bridge/internal/civ"
)

// errArmedDropped is the in-demand rejection when the permit vanished
// between /cmd acceptance and the send-time re-check (R10: the gate holds
// at dispatch, not just at acceptance).
var errArmedDropped = errors.New("armed permit dropped before send")

// errTxWatchdog is the safety-class fact the watchdog trip publishes; it
// persists until the operator's next arm/cmd (setCmdErr ack semantics).
const errTxWatchdog = "tx watchdog expired: ptt forced off"

// pttOffRedeliveryBudget bounds ONE safety-driven redelivery series: the
// session's own retry policy (max_attempts x attempt_spacing, default
// 3x30 s) plus handshake headroom. A failure leaves pendingOff set — the
// OnLive hook retries on the next handshake instead of re-dialing here.
const pttOffRedeliveryBudget = 150 * time.Second

// installSafety registers the loss-of-plane hooks with the radio manager.
// Called once from New; the hooks run on the session's goroutines and stay
// cheap (the work is Enqueued or handed to a bounded goroutine).
func (b *Bridge) installSafety() {
	b.opts.Manager.OnLoss(b.safetyOnLoss)
	b.opts.Manager.OnLive(b.safetyOnLive)
}

// safetyOnLoss is the session-loss hook (R11): the permit drops IMMEDIATELY
// (fail-disarmed) and, if the radio was keyed, the PTT-off redelivery is
// queued — the safety-driven reconnect. It runs on the session goroutine.
func (b *Bridge) safetyOnLoss(err error) {
	b.mu.Lock()
	b.armed = false
	keyed := b.radio.tx == "tx"
	b.stopWatchdogLocked()
	if keyed {
		b.pendingOff = true
	}
	b.mu.Unlock()
	b.log.Warn("session lost — fail-disarmed (R11)", "err", err, "keyed", keyed)
	if keyed {
		go b.deliverPttOff("session-loss")
	}
	sharedmqtt.Enqueue(b.jobs, func() { b.publishState(false) })
}

// safetyOnLive is the reconnect hook: a PTT-off the radio never saw is
// re-issued with priority, BEFORE any pending demand runs (the session
// fires the hook from the handshake path). Runs on the session goroutine.
func (b *Bridge) safetyOnLive(ctx context.Context, c *civ.Client) error {
	b.mu.Lock()
	pending := b.pendingOff
	b.mu.Unlock()
	if !pending {
		return nil
	}
	if _, err := b.mgr.Session().RoundTrip(ctx, c, civ.CmdPTT(false), 5*time.Second); err != nil {
		return err
	}
	b.mu.Lock()
	b.pendingOff = false
	b.mu.Unlock()
	b.log.Warn("pending ptt-off delivered on reconnect (safety redelivery)")
	sharedmqtt.Enqueue(b.jobs, func() { b.publishState(false) })
	return nil
}

// onMqttLoss is the MQTT-plane-loss rule: disarm, and attempt the unkey
// DIRECTLY against the radio session (the CI-V path does not depend on the
// broker). Runs on a paho goroutine — the round-trip moves to a bounded
// goroutine, nothing here blocks. The disarmed facts surface on reconnect
// (the OnConnect ritual force-republishes /state).
func (b *Bridge) onMqttLoss(err error) {
	b.mu.Lock()
	wasPermit := b.armed
	b.armed = false
	keyed := b.radio.tx == "tx"
	b.stopWatchdogLocked()
	if keyed {
		b.pendingOff = true
	}
	b.mu.Unlock()
	if wasPermit || keyed {
		b.log.Warn("mqtt lost while armed or keyed — disarmed, ptt-off attempted out-of-band (U6)", "err", err)
	}
	if keyed {
		go b.deliverPttOff("mqtt-loss")
	}
}

// deliverPttOff forces the unkey through one bounded demand series. On
// failure the pending flag stays set and the OnLive hook owns the retry —
// this goroutine never dials twice (R2: no login storm).
func (b *Bridge) deliverPttOff(why string) {
	ctx, cancel := context.WithTimeout(context.Background(), pttOffRedeliveryBudget)
	defer cancel()
	err := b.mgr.Session().Demand(ctx, func(c *civ.Client) error {
		_, err := b.mgr.Session().RoundTrip(ctx, c, civ.CmdPTT(false), 5*time.Second)
		return err
	})
	if err != nil {
		b.log.Warn("ptt-off undeliverable — redelivery pending on reconnect",
			"why", why, "err", err)
		b.setCmdErr(errPttOffUndeliv)
	} else {
		b.mu.Lock()
		b.pendingOff = false
		b.mu.Unlock()
		b.log.Warn("ptt-off delivered", "why", why)
	}
	sharedmqtt.Enqueue(b.jobs, func() { b.publishState(false) })
}

// armWatchdog (re)arms the max-TX bound; called after a successful PTT-on.
// The timer fires into the jobs worker so the trip serializes with the
// command path.
func (b *Bridge) armWatchdog() {
	b.mu.Lock()
	defer b.mu.Unlock()
	bound := b.opts.TXWatchdog
	if bound <= 0 {
		bound = defaultTXWatchdog
	}
	if b.txWatchdog == nil {
		b.txWatchdog = time.AfterFunc(bound, func() {
			sharedmqtt.Enqueue(b.jobs, b.txWatchdogTrip)
		})
		return
	}
	b.txWatchdog.Reset(bound)
}

// stopWatchdogLocked disarms the bound; every disarm path (operator,
// session loss, MQTT loss, watchdog trip) runs through it.
func (b *Bridge) stopWatchdogLocked() {
	if b.txWatchdog != nil {
		b.txWatchdog.Stop()
	}
}

// txWatchdogTrip is the watchdog expiry (R12): force the unkey, drop the
// permit, and publish the safety-class fact — logged Warn with the slot
// attr so journalctl -p warning surfaces it (the station error filter).
func (b *Bridge) txWatchdogTrip() {
	b.mu.Lock()
	wasKeyed := b.radio.tx == "tx" || b.armed
	b.armed = false
	b.stopWatchdogLocked()
	b.mu.Unlock()
	if !wasKeyed {
		return // stale firing after an explicit ptt-off won the race
	}
	b.log.Warn("tx watchdog expired — forcing ptt-off and disarm (KTD-5)",
		"bound", b.opts.TXWatchdog)
	// The bound has already elapsed; give the unkey one command window, not
	// the full redelivery budget — on failure the OnLive hook owns the rest.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := b.mgr.Session().Demand(ctx, func(c *civ.Client) error {
		_, err := b.mgr.Session().RoundTrip(ctx, c, civ.CmdPTT(false), 5*time.Second)
		return err
	})
	b.setCmdErr(errTxWatchdog)
	if err != nil {
		b.log.Warn("watchdog ptt-off failed — redelivery pending on reconnect", "err", err)
		b.setCmdErr(errPttOffUndeliv)
		b.mu.Lock()
		b.pendingOff = true
		b.mu.Unlock()
	}
	b.publishState(false)
}

// safetyClose stops the watchdog on shutdown (a trip firing into a dead
// jobs worker is harmless but pointless).
func (b *Bridge) safetyClose() {
	b.mu.Lock()
	b.stopWatchdogLocked()
	b.mu.Unlock()
}

// defaultTXWatchdog is the config default (session.tx_watchdog); kept as a
// named constant so the doc prose and the code cannot drift apart.
const defaultTXWatchdog = 180 * time.Second
