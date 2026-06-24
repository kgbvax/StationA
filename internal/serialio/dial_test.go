package serialio

import (
	"net"
	"testing"
	"time"
)

// TestDialFramesOverTCP confirms the io.ReadWriteCloser transport path frames
// Pelco-D bytes arriving over a TCP serial bridge exactly like serial.
func TestDialFramesOverTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// A valid pan-position response for address 1 reading 0° (checksum 0x5A).
	frame := []byte{0xFF, 0x01, 0x00, 0x59, 0x00, 0x00, 0x5A}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write(frame)
	}()

	p, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer p.Close()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-p.Frames():
			if ev.Err == nil && ev.Raw == nil && ev.Frame.IsPanResponse() {
				return // framed successfully
			}
		case <-deadline:
			t.Fatal("timed out waiting for a framed pan response over TCP")
		}
	}
}
