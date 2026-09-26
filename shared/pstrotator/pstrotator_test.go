// SPDX-License-Identifier: AGPL-3.0-or-later

package pstrotator

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want Datagram
	}{
		{"azimuth only", "<PST><AZIMUTH>200</AZIMUTH></PST>", Datagram{AZ: 200, HasAZ: true, Known: true}},
		{"elevation only", "<PST><ELEVATION>45</ELEVATION></PST>", Datagram{EL: 45, HasEL: true, Known: true}},
		{"az and el", "<PST><AZIMUTH>200</AZIMUTH><ELEVATION>45</ELEVATION></PST>",
			Datagram{AZ: 200, EL: 45, HasAZ: true, HasEL: true, Known: true}},
		{"decimal", "<PST><AZIMUTH>123.5</AZIMUTH></PST>", Datagram{AZ: 123.5, HasAZ: true, Known: true}},
		{"stray spaces", "<PST>< AZIMUTH > 85 </ AZIMUTH ></PST>", Datagram{AZ: 85, HasAZ: true, Known: true}},
		{"lower case", "<pst><azimuth>85</azimuth></pst>", Datagram{AZ: 85, HasAZ: true, Known: true}},
		{"stop batched with azimuth", "<PST><STOP>1</STOP><AZIMUTH>85</AZIMUTH></PST>",
			Datagram{Stop: true, AZ: 85, HasAZ: true, Known: true}},
		{"park", "<PST><PARK>1</PARK></PST>", Datagram{Park: true, Known: true}},
		{"az query", "<PST>AZ?</PST>", Datagram{AZQuery: true, Known: true}},
		{"el query tag form", "<PST><EL?></PST>", Datagram{ELQuery: true, Known: true}},
		{"unknown tags", "<PST><TRACK>0</TRACK><FOO>1</FOO></PST>", Datagram{}},
		{"empty", "<PST></PST>", Datagram{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.msg); got != tc.want {
				t.Errorf("Parse(%q) = %+v, want %+v", tc.msg, got, tc.want)
			}
		})
	}
}

func TestFormatReply(t *testing.T) {
	if got := FormatReply(ReplyManual, AZ, 247.04); got != "AZ:247.0\r" {
		t.Errorf("manual az = %q", got)
	}
	if got := FormatReply("", EL, 12.25); got != "EL:12.2\r" && got != "EL:12.3\r" {
		t.Errorf("default el = %q", got)
	}
	if got := FormatReply(ReplyXML, AZ, 246.6); got != "<PST><AZIMUTH>247</AZIMUTH></PST>" {
		t.Errorf("xml az = %q", got)
	}
}

type fakeHandler struct {
	mu    sync.Mutex
	gotos []Datagram
	stops int
	parks int
	az    float64
	azOK  bool
}

func (f *fakeHandler) Goto(d Datagram) { f.mu.Lock(); f.gotos = append(f.gotos, d); f.mu.Unlock() }
func (f *fakeHandler) Stop()           { f.mu.Lock(); f.stops++; f.mu.Unlock() }
func (f *fakeHandler) Park()           { f.mu.Lock(); f.parks++; f.mu.Unlock() }
func (f *fakeHandler) Readback(ax Axis) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.az, ax == AZ && f.azOK
}
func (f *fakeHandler) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.gotos), f.stops, f.parks
}

func startServer(t *testing.T, h Handler) net.Addr {
	t.Helper()
	s := &Server{Bind: "127.0.0.1", Port: 0, H: h}
	pc, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx, pc) }()
	return pc.LocalAddr()
}

func send(t *testing.T, addr net.Addr, msg string) {
	t.Helper()
	c, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestServerPrecedence(t *testing.T) {
	h := &fakeHandler{}
	addr := startServer(t, h)

	send(t, addr, "<PST><STOP>1</STOP><AZIMUTH>85</AZIMUTH><PARK>1</PARK></PST>")
	eventually(t, "stop", func() bool { _, s, _ := h.counts(); return s == 1 })
	send(t, addr, "<PST><PARK>1</PARK><AZIMUTH>85</AZIMUTH></PST>")
	eventually(t, "park", func() bool { _, _, p := h.counts(); return p == 1 })
	send(t, addr, "<PST><AZIMUTH>85</AZIMUTH></PST>")
	eventually(t, "goto", func() bool { g, _, _ := h.counts(); return g == 1 })

	time.Sleep(50 * time.Millisecond)
	if g, s, p := h.counts(); g != 1 || s != 1 || p != 1 {
		t.Errorf("gotos=%d stops=%d parks=%d, want 1/1/1", g, s, p)
	}
	h.mu.Lock()
	if d := h.gotos[0]; !d.HasAZ || d.AZ != 85 || d.HasEL {
		t.Errorf("goto = %+v", d)
	}
	h.mu.Unlock()
}

// TestQueryReply checks the reply goes to the source IP at listen-port+1.
func TestQueryReply(t *testing.T) {
	h := &fakeHandler{az: 247, azOK: true}
	addr := startServer(t, h)
	port := addr.(*net.UDPAddr).Port

	rx, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port + 1})
	if err != nil {
		t.Skipf("cannot bind reply port %d: %v", port+1, err)
	}
	defer rx.Close()

	send(t, addr, "<PST>AZ?</PST>")
	_ = rx.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, _, err := rx.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no reply: %v", err)
	}
	if got := string(buf[:n]); got != "AZ:247.0\r" {
		t.Errorf("reply = %q", got)
	}

	// No valid EL readback: no reply at all.
	send(t, addr, "<PST>EL?</PST>")
	_ = rx.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, _, err := rx.ReadFrom(buf); err == nil {
		t.Errorf("unexpected reply %q", buf[:n])
	}
}
