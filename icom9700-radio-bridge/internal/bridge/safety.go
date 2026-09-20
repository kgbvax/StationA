package bridge

import (
	"context"
	"time"

	"icom9700-radio-bridge/internal/civ"
)

// The U6 safety core: the max-TX watchdog (KTD5), the session-loss
// fail-disarm (R11) and the safety PTT-off reissue on reconnect.
//
// Invariants:
//   - a keyed PTT never outlives session.tx_watchdog — the timer fires a
//     PTT-off through the session whatever the operator does;
//   - a session loss always drops the armed permit (the permit is
//     bridge-held; the session's own hold drops independently) and, when
//     the radio was keyed at loss time, owes a PTT-off;
//   - the owed PTT-off is delivered FIRST on the next live session —
//     before any telemetry or operator work.
//
// Radio-truth note (bench 2026-09-20): while the session is down the
// radio keeps transmitting whatever state it was left in — the bound on
// that window is reconnect latency plus this reissue, which is exactly
// what the deployment Go/No-Go gate measures.

const (
	errTxWatchdog = "tx watchdog fired: radio unkeyed after "
	errPttOffFail = "safety ptt-off failed: "
)

// registerSafety wires the loss/reconnect hooks onto the session. Called
// from New.
func (b *Bridge) registerSafety() {
	b.opts.Manager.OnLoss(b.onSessionLoss)
	b.opts.Manager.OnLive(b.onReconnected)
}

// armTxWatchdog (re)starts the max-TX bound. Called after a PTT-on reached
// the radio; every path that unkeys stops it.
func (b *Bridge) armTxWatchdog() {
	if b.opts.TXWatchdog <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopTxWatchdogLocked()
	b.txWatch = time.AfterFunc(b.opts.TXWatchdog, b.fireTxWatchdog)
}

// stopTxWatchdog clears the bound — PTT-off reached the radio (operator or
// watchdog) or the session died (the reissue owns the radio now).
func (b *Bridge) stopTxWatchdog() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopTxWatchdogLocked()
}

func (b *Bridge) stopTxWatchdogLocked() {
	if b.txWatch != nil {
		b.txWatch.Stop()
		b.txWatch = nil
	}
}

// fireTxWatchdog unkeys the radio after a bounded keyed period. Runs on a
// timer goroutine; the Demand serializes with other session users.
func (b *Bridge) fireTxWatchdog() {
	d := b.opts.TXWatchdog
	b.log.Warn("tx watchdog fired", "keyed_for", d.String())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := b.mgr.Session().Demand(ctx, func(c *civ.Client) error {
		if _, err := b.mgr.Session().RoundTrip(ctx, c, civ.CmdPTT(false), 5*time.Second); err != nil {
			return err
		}
		// Readback-only truth, same as the PTT cmd path.
		if f, err := b.mgr.Session().RoundTrip(ctx, c, civ.BuildFrame(0x1C, []byte{0x00}), 5*time.Second); err == nil && len(f.Sub) >= 1 {
			sub := f.Sub
			if len(sub) > 1 {
				sub = sub[1:]
			}
			if len(sub) == 1 && sub[0] != 0x01 {
				b.mu.Lock()
				b.radio.tx = "rx"
				b.mu.Unlock()
			}
		}
		return nil
	})
	if err != nil {
		// The unkey itself failed — the session-loss path owns the radio
		// from here; surface the worst fact either way.
		b.setCmdErr(errPttOffFail + err.Error())
		b.log.Error("tx watchdog ptt-off undelivered", "err", err)
	} else {
		b.setCmdErr(errTxWatchdog + d.String())
	}
	b.stopTxWatchdog()
	b.publishState(true)
}

// onSessionLoss is the loss hook: fail-disarm (R11) and, when the radio
// was keyed at loss time, owe a safety PTT-off for the reconnect.
func (b *Bridge) onSessionLoss(err error) {
	b.mu.Lock()
	b.armed = false
	owed := b.radio.tx == "tx"
	if owed {
		b.pttOffOwed = true
	}
	b.stopTxWatchdogLocked()
	b.mu.Unlock()
	if owed {
		b.log.Warn("session lost while keyed — fail-disarmed, ptt-off owed", "err", err)
	} else {
		b.log.Warn("session lost — fail-disarmed", "err", err)
	}
	b.publishState(true)
}

// onReconnected is the live hook: deliver an owed safety PTT-off before
// any other work on the fresh session.
func (b *Bridge) onReconnected(ctx context.Context, c *civ.Client) error {
	b.mu.Lock()
	owed := b.pttOffOwed
	b.mu.Unlock()
	if !owed {
		return nil
	}
	b.log.Warn("safety: reissuing ptt-off on reconnect")
	if _, err := b.mgr.Session().RoundTrip(ctx, c, civ.CmdPTT(false), 5*time.Second); err != nil {
		return err // the session machinery logs it; the owe flag stays set
	}
	b.mu.Lock()
	b.pttOffOwed = false
	b.radio.tx = "rx"
	b.mu.Unlock()
	b.log.Warn("safety: owed ptt-off delivered")
	b.setCmdErr("")
	b.publishState(true)
	return nil
}
