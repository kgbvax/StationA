package engine

import (
	"net"
	"sync"
	"testing"
	"time"

	"pelcots/internal/config"
)

// waitFor polls cond up to timeout, returning whether it became true.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestAutoReconnect verifies the engine re-establishes a dropped link on its
// own: it connects to a TCP bridge, survives the peer closing the connection
// mid-session, and reconnects when the bridge accepts again — all without any
// explicit Reconnect call.
func TestAutoReconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var (
		mu       sync.Mutex
		accepted int
		conns    []net.Conn
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			mu.Lock()
			accepted++
			conns = append(conns, c)
			mu.Unlock()
			go func(c net.Conn) { // drain inbound polls until the conn drops
				buf := make([]byte, 64)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()

	eng := New(Options{
		Transport: config.TransportTCP,
		TCPAddr:   ln.Addr().String(),
		Addr:      1,
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool { return eng.Snapshot().Connected }) {
		t.Fatal("engine did not connect initially")
	}

	// Drop the first accepted connection to simulate a device/bridge failure.
	mu.Lock()
	first := conns[0]
	mu.Unlock()
	_ = first.Close()

	// The engine should notice the drop and enter the reconnecting state.
	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return !s.Connected || s.Reconnecting
	}) {
		t.Fatal("engine did not observe the dropped link")
	}

	// And then automatically re-establish the link (a second accept).
	if !waitFor(2*time.Second, func() bool {
		mu.Lock()
		n := accepted
		mu.Unlock()
		return n >= 2 && eng.Snapshot().Connected
	}) {
		t.Fatal("engine did not auto-reconnect after the drop")
	}
}

// TestReconnectWhileDeviceAbsent verifies the engine keeps retrying when the
// target is initially unreachable and connects once it appears, without a
// manual Reconnect.
func TestReconnectWhileDeviceAbsent(t *testing.T) {
	// Reserve a port, then close the listener so the address is (briefly) free.
	ln0, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln0.Addr().String()
	_ = ln0.Close()

	eng := New(Options{Transport: config.TransportTCP, TCPAddr: addr, Addr: 1})
	eng.Start()
	defer eng.Close()

	// Initial connect should fail and the engine should be retrying.
	if !waitFor(time.Second, func() bool { return eng.Snapshot().Reconnecting }) {
		t.Fatal("engine should be retrying while target is absent")
	}

	// Bring the target up; the retry loop should connect on its own.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("could not re-bind %s (race): %v", addr, err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 64)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()

	if !waitFor(2*time.Second, func() bool { return eng.Snapshot().Connected }) {
		t.Fatal("engine did not connect once the target appeared")
	}
}
