package lock

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	onSet func() // runs inside SetAz, before it records (simulates a slow write)
}

func (f *fakeDev) SetAz(az float64) error {
	if f.onSet != nil {
		f.onSet()
	}
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
	g.StartHold()
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

// A slow broker must not use the hold up: it runs from StartHold (MQTT
// connected), and motion is refused before that.
func TestStartupHoldRunsFromConnect(t *testing.T) {
	g, d, c := newGate(3 * time.Second)
	c.add(30 * time.Second) // ConnectRetry waiting for the broker
	if err := g.SetAz(90); !errors.Is(err, rotor.ErrLocked) {
		t.Fatalf("set_az before MQTT connected: %v, want ErrLocked", err)
	}
	g.StartHold()
	c.add(2 * time.Second)
	if err := g.SetAz(90); !errors.Is(err, rotor.ErrLocked) {
		t.Fatalf("set_az inside the hold from connect: %v, want ErrLocked", err)
	}
	c.add(time.Second)
	if err := g.SetAz(90); err != nil {
		t.Fatalf("after the hold from connect: %v", err)
	}
	if len(d.sets) != 1 {
		t.Fatalf("sets %v", d.sets)
	}
	// A disabled lock (hold 0) never holds, connected or not.
	g0, _, _ := newGate(0)
	if err := g0.SetAz(1); err != nil {
		t.Fatalf("hold 0: %v", err)
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

const maint = `{"ant_mode":"maintenance","lock":{"seq":5,"park_az":0,"nudge":true}}`

// A nudge during an underway maintenance park ends the park: a later WRC
// reconnect must not turn the boom back to park_az.
func TestNudgeEndsUnderwayPark(t *testing.T) {
	g, d, c := newGate(0)
	g.Apply([]byte(maint))
	g.Observe(online(180, false))
	c.add(StableFor)
	g.Step()
	g.Observe(online(120, true))
	if err := g.SetAzOverride(45, "maintenance"); err != nil {
		t.Fatal(err)
	}
	g.Observe(online(45, false))
	g.Observe(rotor.State{}) // WRC lost
	g.Observe(online(45, false))
	c.add(time.Minute)
	g.Step()
	if len(d.sets) != 2 || d.sets[0] != 0 || d.sets[1] != 45 {
		t.Fatalf("sets %v, want [0 45] (no re-park after the nudge)", d.sets)
	}
}

// A nudge before the park was sent ends it too (the operator has the azimuth).
func TestNudgeBeforeParkSentEndsPark(t *testing.T) {
	g, d, c := newGate(0)
	g.Apply([]byte(maint))
	g.Observe(online(180, false))
	if err := g.SetAzOverride(170, "maintenance"); err != nil {
		t.Fatal(err)
	}
	c.add(time.Minute)
	g.Observe(online(170, false))
	c.add(StableFor)
	g.Step()
	if len(d.sets) != 1 || d.sets[0] != 170 {
		t.Fatalf("sets %v, want only the nudge", d.sets)
	}
}

// The park write in flight and a nudge are ordered: the nudge lands last.
func TestNudgeWaitsForParkWrite(t *testing.T) {
	g, d, c := newGate(0)
	g.Apply([]byte(maint))
	g.Observe(online(180, false))
	c.add(StableFor)
	entered, release := make(chan struct{}), make(chan struct{})
	first := true
	d.onSet = func() {
		if first {
			first = false
			close(entered)
			<-release
		}
	}
	stepped := make(chan struct{})
	go func() { g.Step(); close(stepped) }()
	<-entered
	nudged := make(chan error)
	go func() { nudged <- g.SetAzOverride(45, "maintenance") }()
	select {
	case err := <-nudged:
		t.Fatalf("nudge did not wait for the park write in flight: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-stepped
	if err := <-nudged; err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.sets) != 2 || d.sets[1] != 45 {
		t.Fatalf("sets %v, want the park then the nudge", d.sets)
	}
}

func newPersistGate(t *testing.T, path string) (*Gate, *fakeDev, *clock) {
	t.Helper()
	g, d, c := newGate(0)
	g.PersistTo(path)
	return g, d, c
}

// A finished park survives a bridge restart: the same seq is not parked
// again (no re-aim after drift, a stop or a nudge stays respected).
func TestRestartKeepsFinishedPark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "park.json")
	g, d, c := newPersistGate(t, path)
	g.Apply([]byte(storm))
	g.Observe(online(10, false))
	c.add(StableFor)
	g.Step()
	g.Observe(online(179, false)) // arrived
	if len(d.sets) != 1 {
		t.Fatalf("sets %v", d.sets)
	}

	// Restart after drift to 190.
	g2, d2, c2 := newPersistGate(t, path)
	g2.Apply([]byte(storm))
	g2.Observe(online(190, false))
	c2.add(time.Minute)
	g2.Step()
	if len(d2.sets) != 0 {
		t.Fatalf("restart re-parked a finished seq: %v", d2.sets)
	}
	// A new seq parks.
	g2.Apply([]byte(`{"ant_mode":"storm","lock":{"seq":2,"park_az":200}}`))
	c2.add(StableFor)
	g2.Step()
	if len(d2.sets) != 1 || d2.sets[0] != 200 {
		t.Fatalf("new seq after restart: %v", d2.sets)
	}
}

func TestRestartKeepsCancelledAndNudgedPark(t *testing.T) {
	for _, tc := range []struct {
		name, lock string
		end        func(g *Gate)
	}{
		{"stop", storm, func(g *Gate) { _ = g.Stop() }},
		{"nudge", maint, func(g *Gate) { _ = g.SetAzOverride(45, "maintenance") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "park.json")
			g, _, c := newPersistGate(t, path)
			g.Apply([]byte(tc.lock))
			g.Observe(online(100, false))
			c.add(StableFor)
			g.Step()
			tc.end(g)

			g2, d2, c2 := newPersistGate(t, path)
			g2.Apply([]byte(tc.lock))
			g2.Observe(online(60, false))
			c2.add(time.Minute)
			g2.Step()
			if len(d2.sets) != 0 {
				t.Fatalf("restart re-parked after %s: %v", tc.name, d2.sets)
			}
		})
	}
}

// A park underway at the restart (sent, never arrived) is sent again, and a
// record for another mode/park_az does not match.
func TestRestartResendsUnfinishedPark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "park.json")
	g, _, c := newPersistGate(t, path)
	g.Apply([]byte(storm))
	g.Observe(online(10, false))
	c.add(StableFor)
	g.Step() // sent, restart before arrival

	g2, d2, c2 := newPersistGate(t, path)
	g2.Apply([]byte(storm))
	g2.Observe(online(90, false))
	c2.add(StableFor)
	g2.Step()
	if len(d2.sets) != 1 || d2.sets[0] != 180 {
		t.Fatalf("unfinished park not re-sent after restart: %v", d2.sets)
	}
	g2.Observe(online(180, false)) // arrived, recorded

	g3, d3, c3 := newPersistGate(t, path)
	g3.Apply([]byte(`{"ant_mode":"maintenance","lock":{"seq":1,"park_az":180}}`))
	g3.Observe(online(90, false))
	c3.add(StableFor)
	g3.Step()
	if len(d3.sets) != 1 {
		t.Fatalf("record for another mode matched: %v", d3.sets)
	}
}

func TestUnreadableParkRecordIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "park.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, d, c := newPersistGate(t, path)
	g.Apply([]byte(storm))
	g.Observe(online(10, false))
	c.add(StableFor)
	g.Step()
	if len(d.sets) != 1 {
		t.Fatalf("sets %v", d.sets)
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

func TestCreepIsNotStable(t *testing.T) {
	g, d, c := newGate(0)
	g.Apply([]byte(storm))
	// Wind creep with moving=false: 0.5° per second never exceeds the
	// per-frame band, but 2.5° from where the window started is not still.
	az := 10.0
	g.Observe(online(az, false))
	for i := 0; i < 5; i++ {
		c.add(time.Second)
		az += 0.5
		g.Observe(online(az, false))
		g.Step()
	}
	if len(d.sets) != 0 {
		t.Fatalf("park sent while the readback crept: %v", d.sets)
	}
	c.add(StableFor)
	g.Step()
	if len(d.sets) != 1 {
		t.Fatalf("park not sent once the readback held: %v", d.sets)
	}
}

func TestStopRacingParkWriteStillWins(t *testing.T) {
	g, d, c := newGate(0)
	g.Apply([]byte(storm))
	g.Observe(online(10, false))
	c.add(StableFor)
	// The operator stop lands while the park write is in flight (it may
	// reach the WRC first); the gate must stop again after the write.
	d.onSet = func() { _ = g.Stop() }
	g.Step()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.sets) != 1 || d.stops != 2 {
		t.Fatalf("sets=%v stops=%d, want the park then a second stop", d.sets, d.stops)
	}
}

func TestStopAfterParkWriteNoExtraStop(t *testing.T) {
	g, d, c := newGate(0)
	g.Apply([]byte(storm))
	g.Observe(online(10, false))
	c.add(StableFor)
	g.Step()
	_ = g.Stop()
	if d.stops != 1 {
		t.Fatalf("stops=%d, want 1", d.stops)
	}
}

func TestRefusalStreakWarnsAgainOnNewReason(t *testing.T) {
	var out bytes.Buffer
	c := &clock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	g := New(&fakeDev{}, slog.New(slog.NewTextHandler(&out, nil)), c.now, 3*time.Second)
	_ = g.SetAz(1) // startup hold: Warn
	_ = g.SetAz(2) // same streak: Info
	g.Apply([]byte(storm))
	_ = g.SetAz(3) // lock entered: Warn again
	_ = g.Jog("fwd")
	g.Apply([]byte(`{"ant_mode":"maintenance","lock":{"seq":2}}`))
	_ = g.SetAz(4) // lock label changed: Warn again
	if n := strings.Count(out.String(), "level=WARN"); n != 3 {
		t.Fatalf("%d Warn lines, want 3:\n%s", n, out.String())
	}
}
