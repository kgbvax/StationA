package pstrotator

import (
	"context"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
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

// startTestServer binds the UDP server to a random free port and returns the
// listen address plus a cancel function.
func startTestServer(t *testing.T, ctrl *fakeController) (string, func()) {
	t.Helper()

	// Discover a free UDP port.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().(*net.UDPAddr)
	_ = pc.Close()

	srv := New("127.0.0.1", addr.Port, ctrl, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	// Wait for the server to bind.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: addr.Port})
		if err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return addr.String(), func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
			t.Error("Server.Run did not return after ctx cancel")
		}
	}
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
	time.Sleep(50 * time.Millisecond)

	if len(ctrl.Sets()) != 1 || ctrl.Sets()[0] != 180 {
		t.Errorf("sets = %v, want [180]", ctrl.Sets())
	}
}

func TestSetAzimuthWithElevationIgnored(t *testing.T) {
	ctrl := &fakeController{}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

	sendUDP(t, addr, "<PST><AZIMUTH>090</AZIMUTH><ELEVATION>45</ELEVATION></PST>")
	time.Sleep(50 * time.Millisecond)

	if len(ctrl.Sets()) != 1 || ctrl.Sets()[0] != 90 {
		t.Errorf("sets = %v, want [90]", ctrl.Sets())
	}
}

func TestSetAzimuthDecimal(t *testing.T) {
	ctrl := &fakeController{}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

	sendUDP(t, addr, "<PST><AZIMUTH>123.5</AZIMUTH></PST>")
	time.Sleep(50 * time.Millisecond)

	if len(ctrl.Sets()) != 1 || ctrl.Sets()[0] != 123.5 {
		t.Errorf("sets = %v, want [123.5]", ctrl.Sets())
	}
}

func TestStop(t *testing.T) {
	ctrl := &fakeController{}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

	sendUDP(t, addr, "<PST><STOP>1</STOP></PST>")
	time.Sleep(50 * time.Millisecond)

	if ctrl.Stops() != 1 {
		t.Errorf("stops = %d, want 1", ctrl.Stops())
	}
}

func TestParkIgnored(t *testing.T) {
	ctrl := &fakeController{}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

	sendUDP(t, addr, "<PST><PARK>1</PARK></PST>")
	time.Sleep(50 * time.Millisecond)

	if len(ctrl.Sets()) != 0 || ctrl.Stops() != 0 {
		t.Errorf("park should not drive; sets=%v stops=%d", ctrl.Sets(), ctrl.Stops())
	}
}

func TestUnknownPacket(t *testing.T) {
	ctrl := &fakeController{}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

	sendUDP(t, addr, "<PST><FOO>1</FOO></PST>")
	time.Sleep(50 * time.Millisecond)

	if len(ctrl.Sets()) != 0 || ctrl.Stops() != 0 {
		t.Errorf("unknown should not drive; sets=%v stops=%d", ctrl.Sets(), ctrl.Stops())
	}
}

func TestQueryReply(t *testing.T) {
	ctrl := &fakeController{az: 247}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

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

	// Send query from a stable source port so we can identify the reply path if needed.
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("<PST>AZ?</PST>")); err != nil {
		t.Fatal(err)
	}

	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no reply on port+1: %v", err)
	}

	got := string(buf[:n])
	want := "<PST><AZIMUTH>247</AZIMUTH></PST>"
	if got != want {
		t.Errorf("query reply = %q, want %q", got, want)
	}
}

func TestQueryReplyCaseInsensitive(t *testing.T) {
	ctrl := &fakeController{az: 33}
	addr, cleanup := startTestServer(t, ctrl)
	defer cleanup()

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatal(err)
	}

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
	if _, err := conn.Write([]byte("<pst>az?</pst>")); err != nil {
		t.Fatal(err)
	}

	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no reply on port+1: %v", err)
	}

	got := string(buf[:n])
	want := "<PST><AZIMUTH>33</AZIMUTH></PST>"
	if got != want {
		t.Errorf("query reply = %q, want %q", got, want)
	}
}
