package gs232

import (
	"bufio"
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
// mutex-guarded: the server's connection goroutine writes while the test
// goroutine reads, and the TCP ack is no happens-before edge for the race
// detector.
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
	return append([]float64(nil), f.sets...)
}
func (f *fakeController) Stops() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

func startTestServer(t *testing.T, ctrl Controller) (addr string, cancel func()) {
	t.Helper()
	// Bind to a random free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1", ln.Addr().(*net.TCPAddr).Port, ctrl, nil)
	// Reuse our listener: replace srv.Run's own Listen by accepting on ln here.
	ctx, stop := context.WithCancel(context.Background())
	go func() {
		// Accept loop mirroring Server.Run but on the provided listener.
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.handle(ctx, conn)
		}
	}()
	return ln.Addr().String(), func() { stop(); _ = ln.Close() }
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// readLine reads until \r or EOF (GS-232 responses are \r-terminated).
func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	b, err := r.ReadBytes('\r')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return string(b)
}

func TestQueryC(t *testing.T) {
	ctrl := &fakeController{az: 180}
	addr, cancel := startTestServer(t, ctrl)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()
	r := bufio.NewReader(conn)

	if _, err := conn.Write([]byte("C\r")); err != nil {
		t.Fatal(err)
	}
	got := readLine(t, r)
	if got != "+0180+0000\r" {
		t.Errorf("C response = %q, want +0180+0000\\r", got)
	}
}

func TestQueryC2(t *testing.T) {
	ctrl := &fakeController{az: 45}
	addr, cancel := startTestServer(t, ctrl)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()
	r := bufio.NewReader(conn)

	if _, err := conn.Write([]byte("C2\r")); err != nil {
		t.Fatal(err)
	}
	got := readLine(t, r)
	// 45 -> "+0045+0000\r" (+0aaa with aaa zero-padded to 3)
	if got != "+0045+0000\r" {
		t.Errorf("C2 response = %q, want +0045+0000\\r", got)
	}
}

func TestMoveM(t *testing.T) {
	ctrl := &fakeController{}
	addr, cancel := startTestServer(t, ctrl)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()
	r := bufio.NewReader(conn)

	if _, err := conn.Write([]byte("M090\r")); err != nil {
		t.Fatal(err)
	}
	_ = readLine(t, r) // ack "\r"

	if sets := ctrl.Sets(); len(sets) != 1 || sets[0] != 90 {
		t.Errorf("Move M090 -> sets = %v, want [90]", sets)
	}
}

func TestSetW(t *testing.T) {
	ctrl := &fakeController{}
	addr, cancel := startTestServer(t, ctrl)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()
	r := bufio.NewReader(conn)

	// W aaa eee: elevation is ignored for an azimuth-only rotator.
	if _, err := conn.Write([]byte("W180 000\r")); err != nil {
		t.Fatal(err)
	}
	_ = readLine(t, r)

	if sets := ctrl.Sets(); len(sets) != 1 || sets[0] != 180 {
		t.Errorf("Set W180 000 -> sets = %v, want [180]", sets)
	}
}

func TestStopS(t *testing.T) {
	ctrl := &fakeController{}
	addr, cancel := startTestServer(t, ctrl)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()
	r := bufio.NewReader(conn)

	if _, err := conn.Write([]byte("S\r")); err != nil {
		t.Fatal(err)
	}
	_ = readLine(t, r)

	if n := ctrl.Stops(); n != 1 {
		t.Errorf("S -> stops = %d, want 1", n)
	}
}

func TestUnknown(t *testing.T) {
	ctrl := &fakeController{}
	addr, cancel := startTestServer(t, ctrl)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()
	r := bufio.NewReader(conn)

	if _, err := conn.Write([]byte("Z\r")); err != nil {
		t.Fatal(err)
	}
	got := readLine(t, r)
	if got != "?>\r" {
		t.Errorf("unknown response = %q, want ?>\\r", got)
	}
}

func TestNewBindsAndRuns(t *testing.T) {
	// Exercises Server.Run end-to-end on a real ephemeral port so the listen
	// path (net.JoinHostPort + net.Listen) is covered, not just handle().
	ctrl := &fakeController{az: 270}
	// Discover a free port, then have the server bind to it. (Closing and
	// rebinding has a tiny TOCTOU race, acceptable in a test.)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	srv := New("127.0.0.1", port, ctrl, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()
	// Give the goroutine a moment to bind, then connect.
	if err := waitForDial("127.0.0.1:" + strconv.Itoa(port)); err != nil {
		t.Fatalf("server did not come up: %v", err)
	}

	conn := dial(t, "127.0.0.1:"+strconv.Itoa(port))
	defer conn.Close()
	r := bufio.NewReader(conn)
	if _, err := conn.Write([]byte("C\r")); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, r); got != "+0270+0000\r" {
		t.Errorf("C response = %q, want +0270+0000\\r", got)
	}

	cancel()
	select {
	case <-errCh:
		// Run returned after listener closed.
	case <-time.After(2 * time.Second):
		t.Error("Server.Run did not return after ctx cancel")
	}
}

// waitForDial returns nil once the address accepts a TCP connection.
func waitForDial(addr string) error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return net.UnknownNetworkError("server not up: " + addr)
}

// TestLockedRefusesMovePassesStopAndQuery drives the server through the
// antenna-mode lock gate (as main wires it): in storm, M and W never reach the
// device (the client still gets its ack), while S and C still work.
func TestLockedRefusesMovePassesStopAndQuery(t *testing.T) {
	dev := &fakeController{az: 175}
	gate := lock.New(dev, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, 0)
	gate.Apply([]byte(`{"ant_mode":"storm","lock":{"seq":1,"ground":true,"retract":true}}`))
	addr, cancel := startTestServer(t, gate)
	defer cancel()

	conn := dial(t, addr)
	defer conn.Close()
	r := bufio.NewReader(conn)
	send := func(cmd string) string {
		t.Helper()
		if _, err := conn.Write([]byte(cmd + "\r")); err != nil {
			t.Fatal(err)
		}
		return readLine(t, r)
	}

	for _, cmd := range []string{"M090", "W180 000"} {
		if got := send(cmd); got != "\r" {
			t.Errorf("%s ack = %q, want \\r", cmd, got)
		}
	}
	if sets := dev.Sets(); len(sets) != 0 {
		t.Errorf("locked M/W reached the device: %v", sets)
	}
	_ = send("S")
	if n := dev.Stops(); n != 1 {
		t.Errorf("S while locked -> stops = %d, want 1 (stop is never refused)", n)
	}
	if got := send("C"); got != "+0175+0000\r" {
		t.Errorf("C while locked = %q, want +0175+0000\\r", got)
	}

	// Unlocked again: the same command drives the rotator.
	gate.Apply([]byte(`{"ant_mode":"normal"}`))
	_ = send("M090")
	if sets := dev.Sets(); len(sets) != 1 || sets[0] != 90 {
		t.Errorf("M090 after unlock -> sets = %v, want [90]", sets)
	}
}
