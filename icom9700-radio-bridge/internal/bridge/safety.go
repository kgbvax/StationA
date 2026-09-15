package bridge

// The safety core (plan U6, R4/R10-R13, KTD-4/KTD-5): the TX safety machinery
// as its own unit — the arm-permit admission seam, the max-TX watchdog, and
// the loss-of-plane rules. It fills the two U5 seams (Options.ArmGate,
// Options.OnPTTOn) and the main.go OnConnectionLost seam without touching the
// dispatch path.
//
// The armed permit is BRIDGE-LOCAL and fails disarmed (R11): a fresh process
// has no permit. The radio.Session owns the permit's storage and drops it on
// involuntary session loss and on a failed arm-driven connect (OnArmDrop);
// the safety core consumes those edges for watchdog bookkeeping.
//
// The TX watchdog (R12) bounds ONLY the live-session stuck-keyed case: its
// force-release is a PTT-off frame that needs a session. While the session is
// down, the watchdog disarms and errors immediately and reissues the PTT-off
// through the safety-driven reconnect (R2); until the unkey-on-session-loss
// bench pin lands (deploy Go/No-Go gate 5), radio self-unkey is the only
// bound in that state — a keyed carrier with a dead session and a blocked
// reconnect is a recorded exposure vector (R17), NOT a watchdog-bounded one.
//
// Concurrency: every decision runs on the bridge's single jobs worker (the
// stationa serialization rule); only the watchdog timer callback lands on its
// own goroutine, and it does nothing but enqueue the trip. The mu-guarded
// fields (timer, keyed) are touched from the timer goroutine, the paho
// OnConnectionLost goroutine and the worker.

import (
	"context"
	"sync"
	"time"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"

	"icom9700-radio-bridge/internal/radio"
)

// The safety-class /state.error facts this core records (R12; the plan's
// taxonomy wording). They ride the bridge error override (setCmdErr — the U5
// errorFact merge: the session fact wins while present, the bridge override
// persists until the next admitted arm/cmd ack), because the frozen
// radio.Session API exposes no setter for its own safety-fact slot. See the
// U5 errorFact doc: the override survives the session_state error→idle decay
// — exactly the R17 never-dismiss posture for safety facts.
const (
	// FactWatchdogTrip is the R12 force-release fact. The bound is appended
	// at trip time: "watchdog trip: tx exceeded 180s".
	FactWatchdogTrip = "watchdog trip: tx exceeded"
	// FactMQTTLost is the R4 loss-of-plane fact, recorded on every MQTT
	// connection loss (main.go OnConnectionLost seam).
	FactMQTTLost = "mqtt connection lost"
)

// safetyCore is the TX safety machinery of one bridge. Built by New; the
// bridge wires its methods onto Options.ArmGate, Options.OnPTTOn, the
// session's OnArmDrop hook and the /state snapshot edge.
type safetyCore struct {
	b     *Bridge
	bound time.Duration // the R12 max-TX bound (config session.tx_watchdog)

	mu    sync.Mutex  // guards timer + keyed (timer goroutine, paho goroutine, worker)
	timer *time.Timer // non-nil while the watchdog is armed
	keyed bool        // a bridge-dispatched PTT-on is unresolved (no trip/permit-drop/observed-unkey has consumed it)
}

// newSafetyCore builds the core. bound <= 0 falls back to the config default
// (180 s, KTD-5) so a hand-built Options in a bench cannot disable the bound.
func newSafetyCore(b *Bridge, bound time.Duration) *safetyCore {
	if bound <= 0 {
		bound = 180 * time.Second
	}
	return &safetyCore{b: b, bound: bound}
}

// --- PTT admission (the Options.ArmGate seam) ------------------------------

// gate is the safety core's PTT admission rule. The v1 rule is unchanged from
// the U5 default (R10): armed AND live, armed checked first, exact strings —
// the core owns the SEAM because the permit lifecycle around it (watchdog
// trip, loss-of-plane disarm) is what keeps `armed` honest, not because the
// static check grew conditions. After any safety drop the permit is false, so
// this gate alone rejects further PTT until the operator re-arms.
func (s *safetyCore) gate(armed, live bool) error {
	return defaultArmGate(armed, live)
}

// --- the TX watchdog (R12, KTD-5) ------------------------------------------

// pttOn arms (or re-arms) the watchdog — the Options.OnPTTOn seam, fired on
// the jobs worker after a PTT-on dispatch left for the radio. One bound per
// key-up: a re-key restarts the window. The fired callback captures its own
// timer so a stale fire can never consume a newer key-up's window (review:
// generation check).
func (s *safetyCore) pttOn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.keyed = true
	var t *time.Timer
	t = time.AfterFunc(s.bound, func() {
		sharedmqtt.Enqueue(s.b.jobs, func() { s.trip(t) })
	})
	s.timer = t
}

// pttOff resolves the key-up state on a successful bridge PTT-off dispatch —
// keyed means "the bridge keyed up and nothing resolved it"; a completed
// key-down IS that resolution. Without it, a later permit drop force-unkeys
// an unrelated carrier and, while the session is down, dials the radio for a
// PTT-off nobody owes (review finding: the safety lifecycle must track its
// cause, not just its onset).
func (s *safetyCore) pttOff() {
	s.resolveKeyed()
}

// trip is the watchdog expiry, on the jobs worker. The timer argument is the
// generation check: a fire whose timer was replaced by a re-key (or consumed
// by a permit drop / PTT-off) is stale and a no-op, so a queued fire can
// never trip a fresh key-up at ~0 elapsed. When it does stand, the force is
// UNCONDITIONAL: keyed means the bridge dispatched a PTT-on and nothing
// resolved it — a stale rx cache must not skip the unkey (a redundant
// PTT-off is harmless; a missed one leaves the carrier up with no permit).
func (s *safetyCore) trip(t *time.Timer) {
	s.mu.Lock()
	if s.timer == nil || s.timer != t {
		s.mu.Unlock()
		return
	}
	s.timer.Stop()
	s.timer = nil
	keyed := s.keyed
	s.keyed = false
	s.mu.Unlock()
	if !keyed {
		return
	}

	fact := FactWatchdogTrip + " " + s.bound.String()
	s.b.log.Warn("tx watchdog tripped — forcing PTT-off",
		"bound", s.bound.String(), "error", fact)

	// 1. Unkey the radio first (live: the frame goes out now; down: the
	// safety-driven reconnect owns delivery).
	s.forcePTTOff()
	// 2. Record the safety-class fact BEFORE the disarm: on the down branch
	// the safety demand occupies the session manager for its whole connect
	// series, and SetArmed's confirmation waits behind it — the error must
	// not. The fact persists in /state.error across the session_state
	// decay until the next arm/cmd acks it (R17 posture).
	s.b.setCmdErr(fact)
	s.b.publishState(false)
	// 3. Drop the permit — the watchdog trip is not operator activity.
	s.disarm("tx watchdog trip")
}

// forcePTTOff delivers a PTT-off through the session's safety channel:
// Session.RequestPTTOff. While live it delivers the frame inline
// (deliverPTTOff — logged Warn by the session); while down the frame becomes
// outstanding and the safety-driven connect demand re-delivers it (R2), with
// the terminal fact `ptt-off undeliverable: session unavailable` landing in
// the session's own safety-fact slot if the demand exhausts its bound.
//
// Deliberately NOT Session.Do+BuildPTT despite the same immediacy while
// live: the Live()->Do race can promote the queued closure into a CMD-driven
// connect demand (enqueueWork sees a dead session and dials) — a watchdog
// unkey that dials the radio would steal the session wfview holds (the R2
// never-steal rule). RequestPTTOff cannot misclass, and the R12 scoping
// lives HERE: the down branch is the recorded exposure — with the session
// dead and the reconnect blocked, radio self-unkey (deploy Go/No-Go gate 5)
// is the only remaining bound.
func (s *safetyCore) forcePTTOff() {
	s.b.log.Warn("safety PTT-off dispatched")
	s.b.sess.RequestPTTOff()
}

// disarm drops the permit through the session (SetArmed false never fails;
// the bound only guards a wedged manager). The drop republishes /state with
// armed:false via the session's own snapshot edge.
func (s *safetyCore) disarm(why string) {
	ctx, cancel := context.WithTimeout(context.Background(), doTimeout)
	defer cancel()
	if err := s.b.sess.SetArmed(ctx, false); err != nil {
		s.b.log.Warn("arm permit drop did not confirm", "why", why, "err", err)
		return
	}
	s.b.log.Warn("arm permit dropped", "why", why)
}

// --- loss-of-plane rules (KTD-5) -------------------------------------------

// onArmDrop consumes the session's OnArmDrop hook (the permit dropped on its
// own: involuntary session loss, failed arm connect): stop the watchdog —
// the KTD-5 rule that a session loss mid-PTT cancels the bound (the radio-
// side unkey plus the PTT-off re-delivery below take over; no watchdog
// expiry error once the loss already resolved the TX state). `keyed` is NOT
// consumed here: onSnapshot owns it (the published armed:false follows this
// hook and forces the PTT-off exactly once).
func (s *safetyCore) onArmDrop(reason string) {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()
	s.b.log.Warn("watchdog cancelled: arm permit dropped", "reason", reason)
}

// onSnapshot runs for every session snapshot (the OnStateChange edge, already
// on the jobs worker). The unified permit-drop rule: whenever the permit
// reads false while the safety core still has an unresolved key-up, force the
// PTT-off. This one rule covers every plane loss that ends in a published
// armed:false — operator disarm during live PTT, involuntary session loss
// mid-PTT (the session dropped the permit; the re-delivery demand is what
// re-drives the frame), the watchdog trip's own disarm (keyed already
// consumed there — no double frame), and the MQTT-loss disarm.
func (s *safetyCore) onSnapshot(snap radio.Snapshot) {
	if snap.Armed {
		return
	}
	if !s.resolveKeyed() {
		return
	}
	// Unconditional on the TX cache on purpose: `keyed` means the bridge
	// dispatched a PTT-on and nothing resolved it; a stale rx cache (the
	// key-up transceive not read back yet) must not skip the unkey. A
	// redundant PTT-off to an already-unkeyed radio is harmless; a missed one
	// leaves the carrier up with no permit. Warn: safety-relevant, this is
	// the journalctl -p warning line (logging convention).
	s.b.log.Warn("arm permit dropped while keyed — forcing PTT-off")
	s.forcePTTOff()
}

// --- the MQTT-plane loss rule (R4) ------------------------------------------
//
// Control-plane loss degrades the bus to inaction, so the rule must not
// depend on the bus: OnConnectionLost (paho goroutine) only enqueues; the
// worker runs onMQTTLost, which unkeys over the RADIO plane (UDP — an
// independent path from the lost broker connection), disarms, and records
// the fact. On reconnect paho's OnConnect re-subscribes /cmd and the bridge
// ritual republishes /state carrying armed:false + the fact; with the permit
// gone, the arm gate rejects every cmd until the operator re-arms.

// OnMQTTConnectionLost is the main.go OnConnectionLost seam. Safe from the
// paho goroutine: it only enqueues (never blocks, never publishes inline).
func (b *Bridge) OnMQTTConnectionLost(err error) {
	sharedmqtt.Enqueue(b.jobs, func() { b.onMQTTLost(err) })
}

// onMQTTLost applies the R4 rule, on the jobs worker. Unkey first and
// WITHOUT blocking on the session manager: RequestPTTOff posts — while live
// it delivers inline, while down it becomes the safety-driven connect
// demand. Keyed is consumed here (not left for onSnapshot) so the snapshot
// edge cannot re-issue the same force-off; if PermitDrop finds nothing left
// it is a no-op.
func (b *Bridge) onMQTTLost(err error) {
	b.log.Warn("MQTT plane lost — applying the loss-of-plane rule (R4): ptt-off + disarm", "err", err)
	keyed := b.safety.resolveKeyed()
	if keyed || b.radioTXOn() {
		b.sess.RequestPTTOff()
	}
	b.safety.disarm("mqtt plane lost")
	b.setCmdErr(FactMQTTLost)
	b.publishState(false)
}

// resolveKeyed consumes the keyed flag and stops a running watchdog. Returns
// whether a key-up was unresolved.
func (s *safetyCore) resolveKeyed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	keyed := s.keyed
	s.keyed = false
	return keyed
}

// radioTXOn reads the bridge's cached radio TX truth (the 1C 00 readbacks —
// transceive broadcasts and the per-tick poll read, publish.go). It reflects
// the RADIO's key state regardless of who keyed it (bus PTT or front panel).
func (b *Bridge) radioTXOn() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.radio.tx
}
