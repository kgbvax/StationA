package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ultrabridge/internal/ub/protocol"
)

// retractToRest runs a lock retract on c until the elements are in and the
// motors have stopped (the scriptDev keeps them moving until told otherwise).
func retractToRest(ctx context.Context, t *testing.T, c *Controller, dev *scriptDev) {
	t.Helper()
	poll(ctx, c)
	dev.set(func(d *scriptDev) { d.moving = false })
	poll(ctx, c)
	if s := c.State(); !s.Retracted || s.MotorsMoving {
		t.Fatalf("precondition: retracted=%v moving=%v, want true/false", s.Retracted, s.MotorsMoving)
	}
}

// TestRetractLatchSurvivesRestartAfterUnlock: a storm retracts, the mode
// returns to normal while the radio is off (nothing extends), then the bridge
// restarts. The restarted bridge must still report retracted and must not let
// the 25 kHz deadband swallow the band-follow re-tune that brings the elements
// out (the RCU-06 still reports its pre-retract frequency).
func TestRetractLatchSurvivesRestartAfterUnlock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "retract.json")
	dev := newScriptDev()

	c1 := NewController(dev)
	c1.PersistTo(path)
	c1.ApplyAntMode([]byte(stormRetract))
	retractToRest(ctx, t, c1, dev)
	c1.ApplyAntMode([]byte(normalState))
	poll(ctx, c1)

	c2 := NewController(dev) // bridge restart
	c2.PersistTo(path)
	c2.ApplyAntMode([]byte(normalState))
	poll(ctx, c2)
	if !c2.State().Retracted {
		t.Fatal("restarted bridge lost the retract latch: /state would read retracted:false with the elements in")
	}
	if err := c2.SetFrequency(ctx, 14175+10, "forward"); err != nil {
		t.Fatalf("SetFrequency after restart: %v", err)
	}
	if n := dev.count(protocol.CmdChangeFrequency); n != 1 {
		t.Fatalf("change-frequency count = %d, want 1 (deadband swallowed the re-extend)", n)
	}
	if c2.State().Retracted {
		t.Fatal("latch still set after the re-extend was accepted")
	}

	c3 := NewController(dev) // another restart: the cleared latch persists too
	c3.PersistTo(path)
	poll(ctx, c3)
	if c3.State().Retracted {
		t.Fatal("a cleared latch came back as retracted after a restart")
	}
	if err := c3.SetFrequency(ctx, 14185+10, "forward"); err != nil {
		t.Fatal(err)
	}
	if n := dev.count(protocol.CmdChangeFrequency); n != 1 {
		t.Fatalf("deadband not applied once extended (change-frequency count %d, want 1)", n)
	}
}

// TestRetractLatchRestartWhileUnpoweredUnderLock: storm with a power borrow —
// elements in, antenna power off (RCU-06 offline), then the bridge restarts,
// the mode unlocks while the RCU-06 is still dark (the follow frequency is
// lost), and power returns. /state must come back retracted (so antennaselect
// re-sends) and the re-sent same-band frequency must reach the RCU-06.
func TestRetractLatchRestartWhileUnpoweredUnderLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "retract.json")
	dev := newScriptDev()

	c1 := NewController(dev)
	c1.PersistTo(path)
	c1.ApplyAntMode([]byte(stormRetract))
	retractToRest(ctx, t, c1, dev)
	dev.set(func(d *scriptDev) { d.offline = true })
	poll(ctx, c1)

	c2 := NewController(dev)
	c2.PersistTo(path)
	c2.ApplyAntMode([]byte(stormRetract))
	poll(ctx, c2) // offline: no retract possible
	c2.ApplyAntMode([]byte(normalState))
	if err := c2.SetFrequency(ctx, 14175, "forward"); err == nil {
		t.Fatal("precondition: the follow frequency should fail while the RCU-06 is offline")
	}
	dev.set(func(d *scriptDev) { d.offline = false })
	poll(ctx, c2)
	if s := c2.State(); s.Offline || !s.Retracted {
		t.Fatalf("back online after unlock: offline=%v retracted=%v, want false/true", s.Offline, s.Retracted)
	}
	if err := c2.SetFrequency(ctx, 14175, "forward"); err != nil {
		t.Fatal(err)
	}
	if n := dev.count(protocol.CmdChangeFrequency); n != 1 {
		t.Fatalf("change-frequency count = %d, want 1 (same-band re-extend swallowed)", n)
	}
}

// TestDeadbandBypassSurvivesBackOnlineClear: under the lock an RCU-06 power
// cycle clears the published latch until the re-issued retract is accepted.
// If that retract never lands before the unlock, the elements may still be in
// from the first retract, so the next same-band frequency must still reach
// the RCU-06.
func TestDeadbandBypassSurvivesBackOnlineClear(t *testing.T) {
	ctx := context.Background()
	dev := newScriptDev()
	c := NewController(dev)
	c.ApplyAntMode([]byte(stormRetract))
	retractToRest(ctx, t, c, dev)
	dev.set(func(d *scriptDev) { d.offline = true })
	poll(ctx, c)
	dev.set(func(d *scriptDev) { d.offline, d.failRetract = false, 100 })
	poll(ctx, c)
	if c.State().Retracted {
		t.Fatal("precondition: back-online edge under the lock should clear the published latch")
	}
	c.ApplyAntMode([]byte(normalState))
	if err := c.SetFrequency(ctx, 14175, "forward"); err != nil {
		t.Fatal(err)
	}
	if n := dev.count(protocol.CmdChangeFrequency); n != 1 {
		t.Fatalf("change-frequency count = %d, want 1 (deadband swallowed the re-extend)", n)
	}
}

// TestPersistUnreadableIgnored: a corrupt latch file is ignored (fresh start).
func TestPersistUnreadableIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retract.json")
	if err := os.WriteFile(path, []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewController(newScriptDev())
	c.PersistTo(path)
	if c.State().Retracted {
		t.Fatal("corrupt latch file loaded as retracted")
	}
}

// TestRetractSeqConfirmsOnlyFreshSettledRetract: /state.retract_seq names the
// lock seq whose ensure-retract finished. A latch left over from an earlier
// lock, or the ACK-time snapshot (retracted, motors not yet reported moving),
// must not confirm the current seq; a settled poll after the retract does.
func TestRetractSeqConfirmsOnlyFreshSettledRetract(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC)
	dev := newScriptDev()
	c := NewController(dev)
	c.now = func() time.Time { return now }

	c.ApplyAntMode([]byte(stormRetract)) // seq 7
	retractToRest(ctx, t, c, dev)
	now = now.Add(3 * time.Second)
	poll(ctx, c)
	if s := c.State(); s.RetractSeq == nil || *s.RetractSeq != 7 {
		t.Fatalf("retract_seq = %v after a settled retract on seq 7, want 7", s.RetractSeq)
	}

	c.ApplyAntMode([]byte(normalState))
	if s := c.State(); s.RetractSeq != nil {
		t.Fatalf("retract_seq = %d after unlock, want absent", *s.RetractSeq)
	}
	poll(ctx, c)

	// Second entry: ground first (latch still true from seq 7), then retract.
	c.ApplyAntMode([]byte(`{"ant_mode":"storm","lock":{"seq":9,"ground":true,"retract":false}}`))
	poll(ctx, c)
	c.ApplyAntMode([]byte(`{"ant_mode":"storm","lock":{"seq":9,"ground":true,"retract":true}}`))
	if s := c.State(); s.RetractSeq != nil {
		t.Fatalf("stale latch confirmed seq 9 before any retract (retract_seq %d)", *s.RetractSeq)
	}
	// The RCU-06 accepts the retract but the motors do not read moving yet.
	dev.set(func(d *scriptDev) { d.noMotion = true })
	poll(ctx, c)
	if n := dev.count(protocol.CmdRetract); n != 2 {
		t.Fatalf("retract count = %d, want 2", n)
	}
	if s := c.State(); s.RetractSeq != nil {
		t.Fatalf("ACK-time snapshot confirmed seq 9 (retract_seq %d)", *s.RetractSeq)
	}
	poll(ctx, c) // same instant: still within the settle window
	if s := c.State(); s.RetractSeq != nil {
		t.Fatalf("seq 9 confirmed inside the settle window (retract_seq %d)", *s.RetractSeq)
	}
	now = now.Add(3 * time.Second)
	poll(ctx, c)
	if s := c.State(); s.RetractSeq == nil || *s.RetractSeq != 9 {
		t.Fatalf("retract_seq = %v after the settled retract on seq 9, want 9", s.RetractSeq)
	}

	// RCU-06 power cycle under the lock: confirmation withdrawn until redone.
	dev.set(func(d *scriptDev) { d.offline = true })
	poll(ctx, c)
	dev.set(func(d *scriptDev) { d.offline = false })
	poll(ctx, c)
	if s := c.State(); s.RetractSeq != nil {
		t.Fatalf("retract_seq = %d right after the RCU-06 came back, want absent", *s.RetractSeq)
	}
	now = now.Add(3 * time.Second)
	poll(ctx, c)
	if s := c.State(); s.RetractSeq == nil || *s.RetractSeq != 9 {
		t.Fatalf("retract_seq = %v after the re-retract settled, want 9", s.RetractSeq)
	}
}
