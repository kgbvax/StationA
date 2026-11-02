package main

import (
	"bytes"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

func listenUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func recvPCM(t *testing.T, c *net.UDPConn, want []byte) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := c.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("%s: no PCM received: %v", c.LocalAddr(), err)
	}
	if !bytes.Equal(buf[:n], want) {
		t.Fatalf("%s: got %d bytes, want %x", c.LocalAddr(), n, want)
	}
}

// Every PCM chunk reaches every publish target (vhfcam + bmrec, 2026-10),
// and a target whose send fails does not starve the targets after it.
func TestAudioSinkFansOutPastAFailingTarget(t *testing.T) {
	a, b := listenUDP(t), listenUDP(t)
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	// Port 0 resolves but every sendto to it fails (EADDRNOTAVAIL/EINVAL)
	// — the stand-in for an unreachable consumer, listed first.
	sink, conn, err := newAudioSink([]string{
		"127.0.0.1:0",
		a.LocalAddr().String(),
		b.LocalAddr().String(),
	}, log)
	if err != nil {
		t.Fatalf("newAudioSink: %v", err)
	}
	defer conn.Close()

	for i := byte(1); i <= 3; i++ {
		pcm := []byte{i, 0x10, 0x20, 0x30}
		sink(pcm)
		recvPCM(t, a, pcm)
		recvPCM(t, b, pcm)
	}
	// The failing target really failed, and was logged once, not per chunk.
	if n := strings.Count(logBuf.String(), "audio publish write failed"); n != 1 {
		t.Fatalf("failing target logged %d times, want 1:\n%s", n, logBuf.String())
	}
}

func TestAudioSinkRejectsUnresolvableTarget(t *testing.T) {
	if _, _, err := newAudioSink([]string{"127.0.0.1:notaport"}, slog.Default()); err == nil {
		t.Fatal("newAudioSink accepted an unresolvable target")
	}
}
