package tuner

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// A silently dead link must end Run: the ATR streams meter frames
// continuously and never legitimately goes quiet, so the read-deadline
// silence bound is honest (review S4). Pre-fix, Run blocked forever and
// retained /state kept device_online:true with frozen values.
func TestRunReadDeadlineOnSilentPeer(t *testing.T) {
	srv, u := newTestServer(t)

	dev := New(wsURL(srv), false, &testLogger{t})
	dev.readTimeout = 150 * time.Millisecond // shrunk: set before Run, read only in Run's loop
	done := make(chan error, 1)
	go func() { done <- dev.Run(context.Background(), nil) }()

	select {
	case <-u.onReady: // handshake complete; the silence clock is running
	case <-time.After(3 * time.Second):
		t.Fatal("never connected to the test server")
	}
	start := time.Now()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil against a silent peer")
		}
		if el := time.Since(start); el > 3*time.Second {
			t.Errorf("silent peer detected only after %s; deadline not honored", el)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return against a silent peer")
	}
}

// The per-Run ctx watcher must exit when Run returns (review S3): the bare
// <-ctx.Done() form parked on the ROOT context until app shutdown, stranding
// one goroutine per reconnect cycle (systemd TasksMax=64).
func TestRunCtxWatcherExits(t *testing.T) {
	srv, u := newTestServer(t)
	dev := New(wsURL(srv), false, &testLogger{t})
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dev.Run(ctx, nil) }()

	select {
	case <-u.onReady:
	case <-time.After(3 * time.Second):
		t.Fatal("never connected to the test server")
	}

	cancel() // Run returns (read error via closeConn); the watcher exits via stop
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("goroutine count did not return to baseline; the ctx watcher leaked")
}
