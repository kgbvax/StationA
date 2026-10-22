package pstrotator

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"wrc-rotator-bridge/internal/lock"
)

// fakeController records commands and reports a fixed azimuth. It is
// mutex-guarded so the UDP server's goroutine can write while the test
// goroutine reads under the race detector.
type fakeController struct {
	mu    sync.Mutex
	az    float64
	sets  []float64
	stops int
}

func (f *fakeController) SetAz(az float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, az)
	return nil
}
func (f *fakeController) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return nil
}
func (f *fakeController) Jog(string) error { return nil }
func (f *fakeController) CurrentAz() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.az
}

func (f *fakeController) Sets() []float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]float64, len(f.sets))
	copy(out, f.sets)
	return out
}
func (f *fakeController) Stops() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

// startTestServer binds the UDP socket itself, then serves it, so a datagram
// sent right after this returns can never arrive before the socket exists.
func startTestServer(t *testing.T, ctrl Controller) (string, func()) {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().(*net.UDPAddr)

	srv := New("127.0.0.1", addr.Port, ctrl, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx, pc) }()

	return addr.String(), func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
			t.Error("Server.Serve did not return after ctx cancel")
		}
	}
}

// waitFor polls cond until it holds or the deadline passes. It replaces fixed
// sleeps, which flaked under load (-race, busy CI).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fence sends a STOP after msg and waits until it is handled. The server
// handles datagrams one at a time in arrival order, so once the fence stop
// is counted every earlier datagram has been handled — this is how a test
// proves that something did NOT happen without a fixed sleep.
func fence(t *testing.T, addr string, stops func() int) {
	t.Helper()
	want := stops() + 1
	sendUDP(t, addr, "<PST><STOP>1</STOP></PST>")
	waitFor(t, "fence stop", func() bool { return stops() >= want })
}

func sendUDP(t *testing.T, addr string, msg string) {
	t.Helper()
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
}

func TestSetAzimuth(t *testing.T) {
	ctrl := &fakeController{}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

	sendUDP(t, addr, "<PST><AZIMUTH>180</AZIMUTH></PST>")
	waitFor(t, "set_az", func() bool { return len(ctrl.Sets()) > 0 })

	if len(ctrl.Sets()) != 1 || ctrl.Sets()[0] != 180 {
		t.Errorf("sets = %v, want [180]", ctrl.Sets())
	}
}

func TestSetAzimuthWithElevationIgnored(t *testing.T) {
	ctrl := &fakeController{}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

	sendUDP(t, addr, "<PST><AZIMUTH>090</AZIMUTH><ELEVATION>45</ELEVATION></PST>")
	waitFor(t, "set_az", func() bool { return len(ctrl.Sets()) > 0 })

	if len(ctrl.Sets()) != 1 || ctrl.Sets()[0] != 90 {
		t.Errorf("sets = %v, want [90]", ctrl.Sets())
	}
}

func TestSetAzimuthDecimal(t *testing.T) {
	ctrl := &fakeController{}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

	sendUDP(t, addr, "<PST><AZIMUTH>123.5</AZIMUTH></PST>")
	waitFor(t, "set_az", func() bool { return len(ctrl.Sets()) > 0 })

	if len(ctrl.Sets()) != 1 || ctrl.Sets()[0] != 123.5 {
		t.Errorf("sets = %v, want [123.5]", ctrl.Sets())
	}
}

func TestStop(t *testing.T) {
	ctrl := &fakeController{}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

	sendUDP(t, addr, "<PST><STOP>1</STOP></PST>")
	waitFor(t, "stop", func() bool { return ctrl.Stops() > 0 })

	if ctrl.Stops() != 1 {
		t.Errorf("stops = %d, want 1", ctrl.Stops())
	}
}

func TestParkIgnored(t *testing.T) {
	ctrl := &fakeController{}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

	sendUDP(t, addr, "<PST><PARK>1</PARK></PST>")
	fence(t, addr, ctrl.Stops)

	if len(ctrl.Sets()) != 0 || ctrl.Stops() != 1 {
		t.Errorf("park should not drive; sets=%v stops=%d (want only the fence stop)", ctrl.Sets(), ctrl.Stops())
	}
}

func TestUnknownPacket(t *testing.T) {
	ctrl := &fakeController{}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

	sendUDP(t, addr, "<PST><FOO>1</FOO></PST>")
	fence(t, addr, ctrl.Stops)

	if len(ctrl.Sets()) != 0 || ctrl.Stops() != 1 {
		t.Errorf("unknown should not drive; sets=%v stops=%d (want only the fence stop)", ctrl.Sets(), ctrl.Stops())
	}
}

// query sends msg (an AZ? query) and returns the reply received on port+1.
func query(t *testing.T, addr, msg string) string {
	t.Helper()
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatal(err)
	}

	// Listen on port+1 where the reply is expected.
	replyPort := udpAddr.Port + 1
	pc, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(replyPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}

	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no reply on port+1: %v", err)
	}
	return string(buf[:n])
}

func TestQueryReply(t *testing.T) {
	ctrl := &fakeController{az: 247}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

	if got, want := query(t, addr, "<PST>AZ?</PST>"), "<PST><AZIMUTH>247</AZIMUTH></PST>"; got != want {
		t.Errorf("query reply = %q, want %q", got, want)
	}
}

func TestQueryReplyCaseInsensitive(t *testing.T) {
	ctrl := &fakeController{az: 33}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

	if got, want := query(t, addr, "<pst>az?</pst>"), "<PST><AZIMUTH>33</AZIMUTH></PST>"; got != want {
		t.Errorf("query reply = %q, want %q", got, want)
	}
}

// TestLockedRefusesAzimuthPassesStopAndQuery drives the server through the
// antenna-mode lock gate (as main wires it): in storm, <AZIMUTH> never
// reaches the device, while <STOP> and the AZ? query still work.
func TestLockedRefusesAzimuthPassesStopAndQuery(t *testing.T) {
	dev := &fakeController{az: 175}
	gate := lock.New(dev, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, 0)
	gate.Apply([]byte(`{"ant_mode":"storm","lock":{"seq":1,"ground":true,"retract":true}}`))
	addr, cleanup := startTestServer(t, gate)
	defer cleanup()

	sendUDP(t, addr, "<PST><AZIMUTH>90</AZIMUTH></PST>")
	sendUDP(t, addr, "<PST><AZIMUTH>120</AZIMUTH><ELEVATION>10</ELEVATION></PST>")
	fence(t, addr, dev.Stops) // the fence is itself a STOP: it must reach the device

	if sets := dev.Sets(); len(sets) != 0 {
		t.Errorf("locked <AZIMUTH> reached the device: %v", sets)
	}
	if dev.Stops() != 1 {
		t.Errorf("stops = %d, want 1 (stop is never refused)", dev.Stops())
	}
	if got, want := query(t, addr, "<PST>AZ?</PST>"), "<PST><AZIMUTH>175</AZIMUTH></PST>"; got != want {
		t.Errorf("locked query reply = %q, want %q", got, want)
	}

	// Unlocked again: the same datagram drives the rotator.
	gate.Apply([]byte(`{"ant_mode":"normal"}`))
	sendUDP(t, addr, "<PST><AZIMUTH>90</AZIMUTH></PST>")
	waitFor(t, "set_az after unlock", func() bool { return len(dev.Sets()) > 0 })
	if sets := dev.Sets(); len(sets) != 1 || sets[0] != 90 {
		t.Errorf("sets after unlock = %v, want [90]", sets)
	}
}
