// SPDX-License-Identifier: AGPL-3.0-or-later

package rotor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ----- fake WRC + harness -----------------------------------------------------

type testLogger struct{ t *testing.T }

func (l *testLogger) Infof(format string, args ...any)  { l.t.Logf("INFO: "+format, args...) }
func (l *testLogger) Warnf(format string, args ...any)  { l.t.Logf("WARN: "+format, args...) }
func (l *testLogger) Errorf(format string, args ...any) { l.t.Logf("ERROR: "+format, args...) }
func (l *testLogger) Debugf(format string, args ...any) { l.t.Logf("DEBUG: "+format, args...) }

// fakeWRC serves one WebSocket endpoint that hands the server-side conn to fn
// (localhost only — the module's tests stay off the network).
func fakeWRC(t *testing.T, fn func(t *testing.T, c *websocket.Conn)) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		if fn != nil {
			fn(t, c)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") }

func waitOnline(t *testing.T, d *Device, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if d.Snapshot().DeviceOnline {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("device never came online")
}

// ----- tests -------------------------------------------------------------------

// The happy path: a command write lands on the wire and a streamed status
// frame reaches the telemetry callback as canonical State.
func TestRunTelemetryAndCommand(t *testing.T) {
	srv := fakeWRC(t, func(t *testing.T, c *websocket.Conn) {
		_, raw, err := c.ReadMessage()
		if err != nil {
			t.Errorf("read command: %v", err)
			return
		}
		var cmd RotorCommand
		if err := json.Unmarshal(raw, &cmd); err != nil {
			t.Errorf("command json: %v", err)
		} else if cmd.Az != "stop" {
			t.Errorf("command az = %v, want \"stop\"", cmd.Az)
		}
		b, _ := json.Marshal(RotorStatus{State: "rotating", Az: 123.5, TDeg: 180})
		if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
			t.Errorf("write status: %v", err)
		}
		for { // stay connected until the test tears down
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	})
	d := New(wsURL(srv), false, &testLogger{t})

	telemetry := make(chan State, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Run(ctx, func(st State) { telemetry <- st }) }()

	waitOnline(t, d, 3*time.Second)
	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case st := <-telemetry:
		if st.Az != 123.5 || !st.Moving || st.TargetAz != 180 || st.RotorState != "rotating" {
			t.Errorf("state = %+v", st)
		}
		if !st.DeviceOnline {
			t.Error("state.device_online = false while connected")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no telemetry within 3s")
	}
	cancel()
}

// The WRC drops the TCP conn mid-session: Run returns (so the caller's restart
// loop can redial) and the command path is REFUSED, not wedged.
func TestServerCloseTearsDown(t *testing.T) {
	srv := fakeWRC(t, func(t *testing.T, c *websocket.Conn) {
		_ = c.UnderlyingConn().Close() // abrupt drop right after the upgrade
	})
	d := New(wsURL(srv), false, &testLogger{t})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx, nil) }()

	waitOnline(t, d, 3*time.Second)
	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("Run returned nil after the server dropped the conn")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the server dropped the conn")
	}

	// Post-teardown the command path errors immediately — the pre-fix behavior
	// was a nil conn deref risk at best and a held writeMu at worst.
	if err := d.Stop(); err == nil {
		t.Fatal("Stop after teardown returned nil, want not-connected error")
	}
}

// A wedged peer must not wedge the command path: the per-write deadline bounds
// the write, and the failed write tears the conn down so the read loop
// unblocks and the restart loop can redial. The timeout is shrunk to a
// already-expired deadline here — that fails the write deterministically,
// which a live-but-non-reading peer cannot (the tiny frame drains into kernel
// socket buffers).
func TestWriteTimeoutTearsDown(t *testing.T) {
	srv := fakeWRC(t, func(t *testing.T, c *websocket.Conn) {
		for { // accept but never read anything
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	})
	old := writeTimeout
	writeTimeout = -time.Second
	defer func() { writeTimeout = old }()

	d := New(wsURL(srv), false, &testLogger{t})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx, nil) }()

	waitOnline(t, d, 3*time.Second)

	start := time.Now()
	err := d.Stop()
	if err == nil {
		t.Fatal("write past an expired deadline returned nil")
	}
	if !strings.Contains(err.Error(), "wrc write") {
		t.Errorf("err = %v, want a wrc write error", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("bounded write took %s; the deadline did not bound it", el)
	}

	// The teardown unblocked the read loop — Run returned for the restart loop.
	select {
	case <-runErr:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the write teardown")
	}
}
