package lock

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"wrc-rotator-bridge/internal/rotor"
)

type fakeDev struct {
	mu    sync.Mutex
	sets  []float64
	stops int
	jogs  []string
	az    float64
	err   error
}

func (f *fakeDev) SetAz(az float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, az)
	return f.err
}
func (f *fakeDev) Stop() error { f.mu.Lock(); defer f.mu.Unlock(); f.stops++; return nil }
func (f *fakeDev) Jog(dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jogs = append(f.jogs, dir)
	return nil
}
func (f *fakeDev) CurrentAz() float64 { return f.az }

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func quiet() *slog.Logger            { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func newGate(hold time.Duration) (*Gate, *fakeDev, *clock) {
	c := &clock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	d := &fakeDev{}
	return New(d, quiet(), c.now, hold), d, c
}

const storm = `{"ant_mode":"storm","lock":{"seq":1,"ground":true,"retract":true,"park_az":180}}`

func TestUnlockedPassesThrough(t *testing.T) {
	g, d, _ := newGate(0)
	if err := g.SetAz(90); err != nil {
		t.Fatal(err)
	}
	if err := g.Jog("fwd"); err != nil {
		t.Fatal(err)
	}
	if len(d.sets) != 1 || len(d.jogs) != 1 {
		t.Fatalf("sets=%v jogs=%v", d.sets, d.jogs)
	}
}

func TestStartupHold(t *testing.T) {
	g, d, c := newGate(3 * time.Second)
	if err := g.SetAz(90); !errors.Is(err, rotor.ErrLocked) {
		t.Fatalf("set_az during startup hold: %v, want ErrLocked", err)
	}
	if err := g.Stop(); err != nil || d.stops != 1 {
		t.Fatal("stop must pass during the startup hold")
	}
	c.add(3 * time.Second)
	if err := g.SetAz(90); err != nil {
		t.Fatalf("after hold with no ant-mode state (rollout): %v", err)
	}
	g2, _, _ := newGate(3 * time.Second)
	g2.Apply(nil) // empty retained = normal
	if err := g2.SetAz(90); err != nil {
		t.Fatalf("normal state seen ends the hold early: %v", err)
	}
}

func TestLockedRefusesMotionAllowsStop(t *testing.T) {
	g, d, _ := newGate(0)
	if !g.Apply([]byte(storm)) {
		t.Fatal("label change not reported")
	}
	if g.Label() != "storm" {
		t.Fatalf("label %q", g.Label())
	}
	for _, err := range []error{g.SetAz(10), g.Jog("rev"), g.SetAzOverride(10, "maintenance")} {
		if !errors.Is(err, rotor.ErrLocked) {
			t.Fatalf("motion while storm-locked: %v", err)
		}
	}
	if err := g.Stop(); err != nil || d.stops != 1 {
		t.Fatal("stop refused")
	}
	if len(d.sets) != 0 || len(d.jogs) != 0 {
		t.Fatalf("motion leaked: %v %v", d.sets, d.jogs)
	}
}

func TestMalformedKeepsLastKnown(t *testing.T) {
	g, _, _ := newGate(0)
	g.Apply([]byte(storm))
	g.Apply([]byte(`{"ant_mode":`))
	if g.Label() != "storm" {
		t.Fatal("malformed payload unlocked the rotator")
	}
	g.Apply([]byte(`{"ant_mode":"normal"}`))
	if err := g.SetAz(5); err != nil {
		t.Fatalf("normal must unlock: %v", err)
	}
}

func TestMaintenanceNudge(t *testing.T) {
	g, d, _ := newGate(0)
	g.Apply([]byte(`{"ant_mode":"maintenance","lock":{"seq":2,"nudge":true}}`))
	if err := g.SetAz(100); !errors.Is(err, rotor.ErrLocked) {
		t.Fatal("untagged set_az accepted in maintenance")
	}
	if err := g.SetAzOverride(100, "something"); !errors.Is(err, rotor.ErrLocked) {
		t.Fatal("wrong override tag accepted")
	}
	if err := g.SetAzOverride(100, "maintenance"); err != nil {
		t.Fatal(err)
	}
	if err := g.Jog("fwd"); !errors.Is(err, rotor.ErrLocked) {
		t.Fatal("jog accepted in maintenance")
	}
	g.Apply([]byte(`{"ant_mode":"maintenance","lock":{"seq":2,"nudge":false}}`))
	if err := g.SetAzOverride(100, "maintenance"); !errors.Is(err, rotor.ErrLocked) {
		t.Fatal("nudge accepted without lock.nudge")
	}
	if len(d.sets) != 1 {
		t.Fatalf("sets %v", d.sets)
	}
}

func online(az float64, moving bool) rotor.State {
	return rotor.State{Az: az, Moving: moving, DeviceOnline: true}
}

func TestParkOncePerSeq(t *testing.T) {
	g, d, c := newGate(0)
	g.Apply([]byte(storm))
	g.Step()
	if len(d.sets) != 0 {
		t.Fatal("park sent while device offline")
	}
	g.Observe(online(10, false))
	c.add(StableFor - time.Second)
	g.Step()
	if len(d.sets) != 0 {
		t.Fatal("park sent before the readback was stable")
	}
	c.add(time.Second)
	g.Step()
	if len(d.sets) != 1 || d.sets[0] != 180 {
		t.Fatalf("park not sent: %v", d.sets)
	}
	g.Step()
	if len(d.sets) != 1 {
		t.Fatal("park sent twice")
	}
	g.Observe(online(100, true))
	g.Observe(online(179, false))
	if !g.park.arrived {
		t.Fatal("arrival not detected")
	}
	// Republish of the same lock: no re-park. Drift: no re-park.
	g.Apply([]byte(storm))
	g.Observe(online(150, false))
	c.add(time.Minute)
	g.Step()
	if len(d.sets) != 1 {
		t.Fatalf("re-parked on republish/drift: %v", d.sets)
	}
	// New seq (repark): parks again.
	g.Apply([]byte(`{"ant_mode":"storm","lock":{"seq":2,"park_az":200}}`))
	c.add(StableFor)
	g.Step()
	if len(d.sets) != 2 || d.sets[1] != 200 {
		t.Fatalf("repark: %v", d.sets)
	}
}

func TestParkResentAfterReconnectOnlyIfNotArrived(t *testing.T) {
	g, d, c := newGate(0)
	g.Apply([]byte(storm))
	g.Observe(online(10, false))
	c.add(StableFor)
	g.Step()
	g.Observe(online(50, true))
	g.Observe(rotor.State{}) // WRC lost mid-park
	g.Observe(online(60, false))
	c.add(StableFor)
	g.Step()
	if len(d.sets) != 2 {
		t.Fatalf("park not re-sent after reconnect: %v", d.sets)
	}
	g.Observe(online(180, false))
	g.Observe(rotor.State{})
	g.Observe(online(180, false))
	c.add(StableFor)
	g.Step()
	if len(d.sets) != 2 {
		t.Fatalf("arrived park re-sent: %v", d.sets)
	}
}

func TestStopCancelsPark(t *testing.T) {
	g, d, c := newGate(0)
	g.Apply([]byte(storm))
	g.Observe(online(10, false))
	c.add(StableFor)
	g.Step()
	_ = g.Stop()
	g.Observe(online(60, false))
	g.Observe(rotor.State{})
	g.Observe(online(60, false))
	c.add(time.Minute)
	g.Step()
	if len(d.sets) != 1 {
		t.Fatalf("cancelled park resumed: %v", d.sets)
	}
}

func TestAlreadyAtPark(t *testing.T) {
	g, d, c := newGate(0)
	g.Apply([]byte(storm))
	g.Observe(online(181, false))
	c.add(StableFor)
	g.Step()
	if len(d.sets) != 0 || !g.park.arrived {
		t.Fatalf("sets %v arrived %v", d.sets, g.park.arrived)
	}
}

func TestParkWriteFailureRetries(t *testing.T) {
	g, d, c := newGate(0)
	d.err = errors.New("wrc: websocket not connected")
	g.Apply([]byte(storm))
	g.Observe(online(10, false))
	c.add(StableFor)
	g.Step()
	d.err = nil
	g.Step()
	if len(d.sets) != 2 {
		t.Fatalf("failed park write not retried: %v", d.sets)
	}
}

func TestUnlockClearsPark(t *testing.T) {
	g, d, c := newGate(0)
	g.Apply([]byte(`{"ant_mode":"storm","lock":{"seq":1,"ground":true}}`))
	g.Observe(online(10, false))
	c.add(StableFor)
	g.Step()
	if len(d.sets) != 0 {
		t.Fatal("parked without park_az")
	}
	g.Apply([]byte(`{"ant_mode":"normal"}`))
	g.Step()
	if g.Label() != "" || len(d.sets) != 0 {
		t.Fatal("normal did not clear")
	}
}
