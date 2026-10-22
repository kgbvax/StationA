package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"ultrabridge/internal/ub/protocol"
)

// scriptDev is a scripted RCU-06 for the lock tests: it counts commands by
// code, can be taken offline, keeps the motors "moving" after a retract until
// the test stops them, and can fail the next retracts.
type scriptDev struct {
	mu          sync.Mutex
	freqKHz     uint16
	moving      bool
	offline     bool
	failRetract int // number of upcoming retracts to fail
	counts      map[byte]int
}

func newScriptDev() *scriptDev {
	return &scriptDev{freqKHz: 14175, counts: map[byte]int{}}
}

func (d *scriptDev) Close() error { return nil }

func (d *scriptDev) Exchange(_ context.Context, com byte, data []byte, _ time.Duration) (protocol.Packet, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.offline {
		return protocol.Packet{}, errors.New("read timeout")
	}
	d.counts[com]++
	switch com {
	case protocol.CmdStatusQuery:
		var motor byte
		if d.moving {
			motor = 1
		}
		payload := []byte{1, 0, 0, byte(d.freqKHz), byte(d.freqKHz >> 8), 4, protocol.ModeNormal, 0, 0, motor, 7, 30}
		return protocol.Packet{Com: protocol.ReplyOK, Data: payload}, nil
	case protocol.CmdMovingStatus:
		var total uint16
		if d.moving {
			total = 500
		}
		return protocol.Packet{Com: protocol.ReplyOK, Data: []byte{byte(total), byte(total >> 8), 0, 0}}, nil
	case protocol.CmdRetract:
		if d.failRetract > 0 {
			d.failRetract--
			return protocol.Packet{Com: protocol.ReplyInvalidCommand}, nil
		}
		d.moving = true
		return protocol.Packet{Com: protocol.ReplyOK}, nil
	case protocol.CmdChangeFrequency:
		d.freqKHz = uint16(data[0]) | uint16(data[1])<<8
		return protocol.Packet{Com: protocol.ReplyOK}, nil
	}
	return protocol.Packet{Com: protocol.ReplyInvalidCommand}, nil
}

func (d *scriptDev) count(com byte) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.counts[com]
}

func (d *scriptDev) set(fn func(d *scriptDev)) {
	d.mu.Lock()
	fn(d)
	d.mu.Unlock()
}

const (
	stormRetract   = `{"ant_mode":"storm","lock":{"seq":7,"ground":true,"retract":true}}`
	stormGrounding = `{"ant_mode":"storm","lock":{"seq":7,"ground":true,"retract":false}}`
	normalState    = `{"ant_mode":"normal"}`
)

// poll is one main-loop iteration: refresh, motor poll, ensure-retract.
func poll(ctx context.Context, c *Controller) {
	_ = c.Refresh(ctx)
	_ = c.PollMotorStatus(ctx)
	c.EnsureRetract(ctx)
}

// TestLockRefusesExtendAllowsRetract: while locked, frequency/band/direction
// (SetFrequency/SetMode) return ErrLocked and never reach the RCU-06, retract
// still works; unlock lets extends through again.
func TestLockRefusesExtendAllowsRetract(t *testing.T) {
	ctx := context.Background()
	dev := newScriptDev()
	c := NewController(dev)
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if changed := c.ApplyAntMode([]byte(stormGrounding)); !changed {
		t.Fatal("normal → storm must report a label change")
	}
	if got := c.State().Lock; got != "storm" {
		t.Fatalf("State().Lock = %q, want storm", got)
	}
	if err := c.SetFrequency(ctx, 21225, "forward"); !errors.Is(err, ErrLocked) {
		t.Fatalf("SetFrequency while locked: err = %v, want ErrLocked", err)
	}
	if err := c.SetMode(ctx, "reverse"); !errors.Is(err, ErrLocked) {
		t.Fatalf("SetMode while locked: err = %v, want ErrLocked", err)
	}
	if n := dev.count(protocol.CmdChangeFrequency); n != 0 {
		t.Fatalf("%d change-frequency commands reached the device while locked", n)
	}
	if err := c.Retract(ctx); err != nil {
		t.Fatalf("Retract while locked: %v", err)
	}

	if changed := c.ApplyAntMode([]byte(normalState)); !changed {
		t.Fatal("storm → normal must report a label change")
	}
	if got := c.State().Lock; got != "" {
		t.Fatalf("State().Lock = %q after unlock, want empty", got)
	}
	if err := c.SetFrequency(ctx, 21225, "forward"); err != nil {
		t.Fatalf("SetFrequency after unlock: %v", err)
	}
	if n := dev.count(protocol.CmdChangeFrequency); n != 1 {
		t.Fatalf("change-frequency count = %d after unlock, want 1", n)
	}
}

// TestNoLockStateIsNormal: an empty payload (no retained ant-mode state, or
// the retained topic cleared) is normal — rollout-safe.
func TestNoLockStateIsNormal(t *testing.T) {
	c := NewController(newScriptDev())
	c.ApplyAntMode([]byte(stormRetract))
	c.ApplyAntMode(nil)
	if got := c.LockLabel(); got != "" {
		t.Fatalf("empty payload left lock %q, want unlocked", got)
	}
	if err := c.SetFrequency(context.Background(), 21225, "forward"); err != nil {
		t.Fatalf("SetFrequency with no ant-mode state: %v", err)
	}
}

// TestMalformedLockKeepsLastKnown: a malformed ant-mode payload never unlocks.
func TestMalformedLockKeepsLastKnown(t *testing.T) {
	c := NewController(newScriptDev())
	c.ApplyAntMode([]byte(stormGrounding))
	for _, bad := range []string{`{not json`, `{"ant_mode":"hurricane"}`} {
		if changed := c.ApplyAntMode([]byte(bad)); changed {
			t.Errorf("malformed %q reported a label change", bad)
		}
		if got := c.LockLabel(); got != "storm" {
			t.Fatalf("after malformed %q lock = %q, want storm (last known)", bad, got)
		}
	}
}

// TestEnsureRetractOnNewSeq: a lock with retract drives one retract, waits
// (no re-send) while the motors move, settles once retracted && !moving, and
// re-retracts only on a new seq.
func TestEnsureRetractOnNewSeq(t *testing.T) {
	ctx := context.Background()
	dev := newScriptDev()
	c := NewController(dev)
	poll(ctx, c)
	if n := dev.count(protocol.CmdRetract); n != 0 {
		t.Fatalf("retract sent while unlocked (%d)", n)
	}

	c.ApplyAntMode([]byte(stormRetract)) // also covers "bridge start once the lock is known"
	poll(ctx, c)
	if n := dev.count(protocol.CmdRetract); n != 1 {
		t.Fatalf("retract count = %d after lock, want 1", n)
	}
	if s := c.State(); !s.Retracted || !s.MotorsMoving {
		t.Fatalf("after retract: retracted=%v moving=%v, want true/true", s.Retracted, s.MotorsMoving)
	}
	poll(ctx, c) // still moving: wait, do not re-send
	if n := dev.count(protocol.CmdRetract); n != 1 {
		t.Fatalf("retract re-sent while the motors were moving (count %d)", n)
	}
	dev.set(func(d *scriptDev) { d.moving = false })
	poll(ctx, c)
	poll(ctx, c)
	if n := dev.count(protocol.CmdRetract); n != 1 {
		t.Fatalf("retract re-sent after it completed (count %d)", n)
	}

	c.ApplyAntMode([]byte(stormRetract)) // same seq republished: no new retract
	poll(ctx, c)
	if n := dev.count(protocol.CmdRetract); n != 1 {
		t.Fatalf("same seq re-retracted (count %d)", n)
	}

	c.ApplyAntMode([]byte(`{"ant_mode":"storm","lock":{"seq":8,"ground":true,"retract":true,"park_az":175}}`))
	poll(ctx, c)
	if n := dev.count(protocol.CmdRetract); n != 2 {
		t.Fatalf("retract count = %d after a new seq, want 2", n)
	}
}

// TestNoRetractWhileRetractFalse: antmode grounds first; a lock with
// retract=false refuses extends but must not retract. Retract appearing on
// the same seq then triggers it.
func TestNoRetractWhileRetractFalse(t *testing.T) {
	ctx := context.Background()
	dev := newScriptDev()
	c := NewController(dev)
	c.ApplyAntMode([]byte(stormGrounding))
	poll(ctx, c)
	poll(ctx, c)
	if n := dev.count(protocol.CmdRetract); n != 0 {
		t.Fatalf("retracted %d time(s) with lock.retract=false", n)
	}
	c.ApplyAntMode([]byte(stormRetract)) // same seq, retract now set
	poll(ctx, c)
	if n := dev.count(protocol.CmdRetract); n != 1 {
		t.Fatalf("retract count = %d once lock.retract appeared, want 1", n)
	}
}

// TestEnsureRetractDeviceBackOnline: after an RCU-06 outage (power cycle) the
// elements are retracted again, without hammering the dead link meanwhile.
func TestEnsureRetractDeviceBackOnline(t *testing.T) {
	ctx := context.Background()
	dev := newScriptDev()
	c := NewController(dev)
	c.ApplyAntMode([]byte(stormRetract))
	poll(ctx, c)
	dev.set(func(d *scriptDev) { d.moving = false })
	poll(ctx, c)
	if n := dev.count(protocol.CmdRetract); n != 1 {
		t.Fatalf("precondition: retract count = %d, want 1", n)
	}

	dev.set(func(d *scriptDev) { d.offline = true })
	poll(ctx, c)
	poll(ctx, c)
	if !c.State().Offline {
		t.Fatal("precondition: controller should be offline")
	}
	dev.set(func(d *scriptDev) { d.offline = false })
	poll(ctx, c)
	if n := dev.count(protocol.CmdRetract); n != 2 {
		t.Fatalf("retract count = %d after the device came back, want 2", n)
	}
}

// TestEnsureRetractBackOnlineDuringWait: an RCU-06 power cycle while the
// bridge waits for the motors after an accepted retract (sent, still moving)
// re-issues the retract — the device may have re-extended — and the latch
// does not claim retracted across the outage until the retract is re-accepted.
func TestEnsureRetractBackOnlineDuringWait(t *testing.T) {
	ctx := context.Background()
	dev := newScriptDev()
	c := NewController(dev)
	c.ApplyAntMode([]byte(stormRetract))
	poll(ctx, c) // retract accepted, motors moving: waiting
	if n := dev.count(protocol.CmdRetract); n != 1 {
		t.Fatalf("precondition: retract count = %d, want 1", n)
	}

	dev.set(func(d *scriptDev) { d.offline = true })
	poll(ctx, c)
	if !c.State().Offline {
		t.Fatal("precondition: controller should be offline")
	}
	// Back with the motors running (the controller re-extends after power-up).
	dev.set(func(d *scriptDev) { d.offline, d.moving, d.failRetract = false, true, 1 })
	poll(ctx, c) // first re-retract fails
	if c.State().Retracted {
		t.Fatal("latch claims retracted after the power cycle although no retract was accepted since")
	}
	poll(ctx, c)
	if n := dev.count(protocol.CmdRetract); n != 3 {
		t.Fatalf("retract count = %d after the device came back mid-wait, want 3 (1 + failed + accepted)", n)
	}
	dev.set(func(d *scriptDev) { d.moving = false })
	poll(ctx, c)
	poll(ctx, c)
	if n := dev.count(protocol.CmdRetract); n != 3 {
		t.Fatalf("retract re-sent after it completed (count %d)", n)
	}
	if !c.State().Retracted {
		t.Fatal("latch not set after the re-retract completed")
	}
}

// TestEnsureRetractRetriesFailures: a failed retract is retried on the next
// poll until it is accepted.
func TestEnsureRetractRetriesFailures(t *testing.T) {
	ctx := context.Background()
	dev := newScriptDev()
	dev.failRetract = 2
	c := NewController(dev)
	c.ApplyAntMode([]byte(stormRetract))
	poll(ctx, c)
	poll(ctx, c)
	if c.State().Retracted {
		t.Fatal("latch set although every retract failed")
	}
	poll(ctx, c)
	if n := dev.count(protocol.CmdRetract); n != 3 {
		t.Fatalf("retract attempts = %d, want 3 (2 failed + 1 accepted)", n)
	}
	if !c.State().Retracted {
		t.Fatal("latch not set after the retried retract was accepted")
	}
}

// TestUnlockStopsEnsureRetract: leaving the mode never retracts (and never
// auto-extends — that is antennaselect's band-follow).
func TestUnlockStopsEnsureRetract(t *testing.T) {
	ctx := context.Background()
	dev := newScriptDev()
	dev.offline = true
	c := NewController(dev)
	c.ApplyAntMode([]byte(stormRetract))
	poll(ctx, c) // offline: nothing sent
	c.ApplyAntMode([]byte(normalState))
	dev.set(func(d *scriptDev) { d.offline = false })
	poll(ctx, c)
	if n := dev.count(protocol.CmdRetract); n != 0 {
		t.Fatalf("retract sent after unlock (%d)", n)
	}
	if n := dev.count(protocol.CmdChangeFrequency); n != 0 {
		t.Fatalf("unlock auto-extended (%d change-frequency)", n)
	}
}

// TestLockStartupHold: with the hold armed, extends are refused until the
// first ant-mode state arrives or the hold window passes.
func TestLockStartupHold(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC)
	c := NewController(newScriptDev())
	c.now = func() time.Time { return now }
	c.SetLockHold(3 * time.Second)

	if err := c.SetFrequency(ctx, 21225, "forward"); !errors.Is(err, ErrLocked) {
		t.Fatalf("SetFrequency during the startup hold: err = %v, want ErrLocked", err)
	}
	if err := c.Retract(ctx); err != nil {
		t.Fatalf("Retract during the startup hold: %v", err)
	}
	now = now.Add(4 * time.Second)
	if err := c.SetFrequency(ctx, 21225, "forward"); err != nil {
		t.Fatalf("SetFrequency after the hold expired: %v", err)
	}

	c2 := NewController(newScriptDev())
	c2.now = func() time.Time { return now }
	c2.SetLockHold(3 * time.Second)
	c2.ApplyAntMode([]byte(normalState))
	if err := c2.SetFrequency(ctx, 21225, "forward"); err != nil {
		t.Fatalf("SetFrequency once the (normal) state arrived: %v", err)
	}
}

// TestApplyAntModeKicks: a lock change or a new retract request wakes the
// poll loop; a republished identical snapshot does not.
func TestApplyAntModeKicks(t *testing.T) {
	c := NewController(newScriptDev())
	kicked := func() bool {
		select {
		case <-c.LockKick():
			return true
		default:
			return false
		}
	}
	c.ApplyAntMode([]byte(stormGrounding))
	c.ApplyAntMode([]byte(stormRetract)) // buffered kick: must not block
	if !kicked() {
		t.Fatal("lock did not kick the poll loop")
	}
	c.ApplyAntMode([]byte(stormRetract))
	if kicked() {
		t.Fatal("an unchanged snapshot kicked the poll loop")
	}
	c.ApplyAntMode([]byte(normalState))
	if !kicked() {
		t.Fatal("unlock did not kick the poll loop")
	}
}

// TestLockRefusalWarnsOncePerStreak: repeated refusals log one Warn, the rest
// at Info; an unlock resets the streak so the next lock warns again.
func TestLockRefusalWarnsOncePerStreak(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	c := NewController(newScriptDev())
	c.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	warns := func() int { return strings.Count(buf.String(), "level=WARN msg=\"ultrabeam extend refused") }

	c.ApplyAntMode([]byte(stormGrounding))
	_ = c.SetFrequency(ctx, 21225, "forward")
	_ = c.SetMode(ctx, "reverse")
	_ = c.SetFrequency(ctx, 14175, "forward")
	if n := warns(); n != 1 {
		t.Fatalf("refusal Warns = %d in one streak, want 1:\n%s", n, buf.String())
	}
	c.ApplyAntMode([]byte(normalState))
	c.ApplyAntMode([]byte(stormGrounding))
	_ = c.SetFrequency(ctx, 21225, "forward")
	if n := warns(); n != 2 {
		t.Fatalf("refusal Warns = %d after unlock + relock, want 2:\n%s", n, buf.String())
	}
}
