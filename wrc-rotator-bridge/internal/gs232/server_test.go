package gs232

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

// fakeController records commands and reports a fixed azimuth.
type fakeController struct {
	az    float64
	sets  []float64
	stops int
}

func (f *fakeController) SetAz(az float64) error { f.sets = append(f.sets, az); return nil }
func (f *fakeController) Stop() error            { f.stops++; return nil }
func (f *fakeController) CurrentAz() float64     { return f.az }

func startTestServer(t *testing.T, ctrl *fakeController) (addr string, cancel func()) {
	t.Helper()
	// Bind to a random free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1", ln.Addr().(*net.TCPAddr).Port, ctrl, nil)
	// Reuse our listener: replace srv.Run's own Listen by accepting on ln here.
	ctx, cancel := context.WithCancel(context.Background())
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
	return ln.Addr().String(), cancel
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

	if len(ctrl.sets) != 1 || ctrl.sets[0] != 90 {
		t.Errorf("Move M090 -> sets = %v, want [90]", ctrl.sets)
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

	if len(ctrl.sets) != 1 || ctrl.sets[0] != 180 {
		t.Errorf("Set W180 000 -> sets = %v, want [180]", ctrl.sets)
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

	if ctrl.stops != 1 {
		t.Errorf("S -> stops = %d, want 1", ctrl.stops)
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
