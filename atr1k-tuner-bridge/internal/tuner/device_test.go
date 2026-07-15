package tuner

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type testLogger struct{ t *testing.T }

func (l *testLogger) Infof(format string, args ...any)  { l.t.Logf("INFO: "+format, args...) }
func (l *testLogger) Warnf(format string, args ...any)  { l.t.Logf("WARN: "+format, args...) }
func (l *testLogger) Debugf(format string, args ...any) { l.t.Logf("DEBUG: "+format, args...) }

// upgrader is a test WebSocket upgrader for the ATR-1000 side. It records outbound
// frames (commands the bridge sent) and can push inbound frames (meter/relay) to
// the device.
type upgrader struct {
	mu      sync.Mutex
	written [][]byte
	onReady chan *websocket.Conn
}

func (u *upgrader) serve(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Upgrade(w, r, nil, 1024, 1024)
	if err != nil {
		return
	}
	defer c.Close()
	u.onReady <- c
	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		_ = mt
		u.mu.Lock()
		u.written = append(u.written, append([]byte(nil), data...))
		u.mu.Unlock()
	}
}

func (u *upgrader) writtenFrames() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([][]byte, len(u.written))
	copy(out, u.written)
	return out
}

func newTestServer(t *testing.T) (*httptest.Server, *upgrader) {
	t.Helper()
	u := &upgrader{onReady: make(chan *websocket.Conn, 1)}
	srv := httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(srv.Close)
	return srv, u
}

// wsURL converts an http:// test URL to ws://.
func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestRunAndMeter(t *testing.T) {
	srv, up := newTestServer(t)
	dev := New(wsURL(srv), false, &testLogger{t})

	telemetry := make(chan State, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Wait for the device to connect, then push a meter frame from the "ATR".
	go func() {
		var conn *websocket.Conn
		select {
		case conn = <-up.onReady:
		case <-time.After(2 * time.Second):
			t.Error("ATR side never connected")
			return
		}
		m := make([]byte, 10)
		m[0], m[1] = 0xFF, scmdMeter
		binary.LittleEndian.PutUint16(m[4:6], 217) // SWR 2.17
		binary.LittleEndian.PutUint16(m[6:8], 750) // 750 W
		_ = conn.WriteMessage(websocket.BinaryMessage, m)
	}()

	go func() { _ = dev.Run(ctx, func(s State) { telemetry <- s }) }()

	// First telemetry is the online snapshot.
	select {
	case s := <-telemetry:
		if !s.DeviceOnline {
			t.Errorf("first snapshot device_online = false, want true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no online telemetry")
	}
	// Then the meter frame.
	var got State
	select {
	case got = <-telemetry:
	case <-time.After(2 * time.Second):
		t.Fatal("no meter telemetry")
	}
	if got.SWR != 2.17 {
		t.Errorf("swr = %v, want 2.17", got.SWR)
	}
	if got.Fwd != 750 {
		t.Errorf("fwd = %v, want 750", got.Fwd)
	}
}

func TestSetInlineWritesFrame(t *testing.T) {
	srv, up := newTestServer(t)
	dev := New(wsURL(srv), false, &testLogger{t})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-up.onReady:
		case <-time.After(2 * time.Second):
		}
	}()

	go func() { _ = dev.Run(ctx, func(State) {}) }()

	// Wait until connected, then put the tuner in line.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if dev.SetInline(true) == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The upgrader reads the inline frame asynchronously; poll for it.
	found := false
	pollDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(pollDeadline) {
		for _, f := range up.writtenFrames() {
			if len(f) >= 4 && f[0] == 0xFF && f[1] == scmdTuneStatus && f[3] == 1 {
				found = true
			}
		}
		if found {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Errorf("no inline frame in % x", up.writtenFrames())
	}
	if dev.Snapshot().Inline != true {
		t.Error("state.Inline should be true after SetInline(true)")
	}
}

func TestTuneTimeoutFault(t *testing.T) {
	// Test the timeout path directly: arm a settle state, fire onTuneTimeout,
	// confirm it clears Settling and sets Fault. No network needed — the timer
	// callback is deterministic logic.
	dev := New("ws://unused", false, &testLogger{t})

	dev.mu.Lock()
	dev.state.Settling = true
	dev.mu.Unlock()

	dev.onTuneTimeout()

	snap := dev.Snapshot()
	if snap.Settling {
		t.Error("settling should be false after timeout")
	}
	if snap.Fault != "tune timeout" {
		t.Errorf("fault = %q, want tune timeout", snap.Fault)
	}

	// A second timeout while already settled is a no-op (fault stays, no panic).
	dev.onTuneTimeout()
}

func TestTuneArmsTimer(t *testing.T) {
	// Tune must arm a timer that, when stopped before firing, leaves the state
	// in the settling (in-line) state. Uses a real WS server so the write path
	// is exercised; the timer is stopped by hand to keep the test fast.
	srv, up := newTestServer(t)
	dev := New(wsURL(srv), false, &testLogger{t})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-up.onReady:
		case <-time.After(2 * time.Second):
		}
	}()
	go func() { _ = dev.Run(ctx, func(State) {}) }()

	// Wait until connected, then issue a full tune.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := dev.Tune(true); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	dev.mu.Lock()
	armed := dev.timer != nil
	if armed && dev.timer != nil {
		dev.timer.Stop()
		dev.timer = nil
	}
	settling := dev.state.Settling
	dev.mu.Unlock()

	if !armed {
		t.Error("Tune should arm a timeout timer")
	}
	if !settling {
		t.Error("state should be settling immediately after Tune")
	}
}

func TestRelayClearsSettling(t *testing.T) {
	srv, up := newTestServer(t)
	dev := New(wsURL(srv), false, &testLogger{t})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	telemetry := make(chan State, 16)

	go func() {
		var conn *websocket.Conn
		select {
		case conn = <-up.onReady:
		case <-time.After(2 * time.Second):
			return
		}
		// Push a relay frame: L 12.34 µH, C 56 pF.
		r := make([]byte, 10)
		r[0], r[1] = 0xFF, scmdRelay
		binary.LittleEndian.PutUint16(r[6:8], 1234)
		binary.LittleEndian.PutUint16(r[8:10], 56)
		_ = conn.WriteMessage(websocket.BinaryMessage, r)
	}()

	go func() { _ = dev.Run(ctx, func(s State) { telemetry <- s }) }()
	<-telemetry // online

	// Arm a tune (settling true) by directly setting state, then let the relay
	// frame clear it.
	dev.mu.Lock()
	dev.state.Settling = true
	dev.mu.Unlock()

	var got State
	select {
	case got = <-telemetry:
	case <-time.After(2 * time.Second):
		t.Fatal("no relay telemetry")
	}
	if got.Settling {
		t.Error("settling should clear on relay frame")
	}
	if got.LUH != 12.34 {
		t.Errorf("l_uh = %v, want 12.34", got.LUH)
	}
	if got.CPF != 56 {
		t.Errorf("c_pf = %v, want 56", got.CPF)
	}
}
