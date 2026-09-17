package gs232

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"

	"spid-ercm-rotator-bridge/internal/config"
	"spid-ercm-rotator-bridge/internal/mount"
)

// fakeMount records façade calls and reports canned readbacks.
type fakeMount struct {
	az, el       float64
	azOK, elOK   bool
	targets      []mount.Target
	stops        int
	refuseReason map[mount.Axis]mount.RefusalReason // non-nil = refuse those axes
}

func (f *fakeMount) Goto(t mount.Target) []mount.Refusal {
	f.targets = append(f.targets, t)
	if f.refuseReason == nil {
		return nil
	}
	var out []mount.Refusal
	if t.HasAZ {
		if reason, ok := f.refuseReason[mount.AZ]; ok {
			out = append(out, mount.Refusal{Axis: mount.AZ, Reason: reason, Target: t.AZ, Detail: "scripted"})
		}
	}
	if t.HasEL {
		if reason, ok := f.refuseReason[mount.EL]; ok {
			out = append(out, mount.Refusal{Axis: mount.EL, Reason: reason, Target: t.EL, Detail: "scripted"})
		}
	}
	return out
}

func (f *fakeMount) Stop() []mount.AxisError { f.stops++; return nil }

func (f *fakeMount) Readback(ax mount.Axis) (float64, bool) {
	if ax == mount.AZ {
		return f.az, f.azOK
	}
	return f.el, f.elOK
}

func startTestServer(t *testing.T, m *fakeMount) (addr string, cancel func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(config.GS232Config{Bind: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}, m, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
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
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	b, err := r.ReadBytes('\r')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return string(b)
}

func TestQueryCReportsBothAxes(t *testing.T) {
	m := &fakeMount{az: 208, el: 45, azOK: true, elOK: true}
	addr, cancel := startTestServer(t, m)
	defer cancel()
	conn := dial(t, addr)
	r := bufio.NewReader(conn)

	if _, err := conn.Write([]byte("C\r")); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, r); got != "+0208+0045\r" {
		t.Errorf("C response = %q, want +0208+0045\\r", got)
	}

	if _, err := conn.Write([]byte("C2\r")); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, r); got != "+0208+0045\r" {
		t.Errorf("C2 response = %q, want +0208+0045\\r", got)
	}
}

// An axis without a valid readback yet reports 000 in the C reply — the
// legacy display surface needs a parseable answer, never a fabricated
// position.
func TestQueryCWithInvalidReadbacksReportsZeros(t *testing.T) {
	m := &fakeMount{az: 208, el: 45} // both invalid
	addr, cancel := startTestServer(t, m)
	defer cancel()
	conn := dial(t, addr)
	r := bufio.NewReader(conn)

	if _, err := conn.Write([]byte("C\r")); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, r); got != "+0000+0000\r" {
		t.Errorf("C response = %q, want +0000+0000\\r", got)
	}
}

// Waaa eee drives BOTH axes through one façade call (the PstRotator
// tracking-move shape).
func TestWGotoBothAxes(t *testing.T) {
	m := &fakeMount{azOK: true, elOK: true}
	addr, cancel := startTestServer(t, m)
	defer cancel()
	conn := dial(t, addr)
	r := bufio.NewReader(conn)

	if _, err := conn.Write([]byte("W123 045\r")); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, r); got != "\r" {
		t.Errorf("W response = %q, want a bare \\r", got)
	}
	if len(m.targets) != 1 {
		t.Fatalf("goto count = %d, want 1", len(m.targets))
	}
	tg := m.targets[0]
	if !tg.HasAZ || !tg.HasEL || tg.AZ != 123 || tg.EL != 45 {
		t.Errorf("target = %+v, want az 123 + el 45, both flagged", tg)
	}
}

// Maaa drives AZIMUTH ONLY: the elevation axis must never construct an
// intent from an az-only legacy command (R8).
func TestMGotoAzimuthOnly(t *testing.T) {
	m := &fakeMount{azOK: true, elOK: true}
	addr, cancel := startTestServer(t, m)
	defer cancel()
	conn := dial(t, addr)
	r := bufio.NewReader(conn)

	if _, err := conn.Write([]byte("M270\r")); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, r); got != "\r" {
		t.Errorf("M response = %q, want a bare \\r", got)
	}
	if len(m.targets) != 1 {
		t.Fatalf("goto count = %d, want 1", len(m.targets))
	}
	tg := m.targets[0]
	if !tg.HasAZ || tg.HasEL || tg.AZ != 270 {
		t.Errorf("target = %+v, want az-only 270 (HasAZ, no HasEL)", tg)
	}
}

func TestStopHaltsBoth(t *testing.T) {
	m := &fakeMount{azOK: true, elOK: true}
	addr, cancel := startTestServer(t, m)
	defer cancel()
	conn := dial(t, addr)
	r := bufio.NewReader(conn)

	for _, cmd := range []string{"S\r", "SA\r", "SE\r"} {
		if _, err := conn.Write([]byte(cmd)); err != nil {
			t.Fatal(err)
		}
		if got := readLine(t, r); got != "\r" {
			t.Errorf("%q response = %q, want a bare \\r", cmd, got)
		}
	}
	if m.stops != 3 {
		t.Errorf("stop count = %d, want 3", m.stops)
	}
}

// Refusals have no wire contract on this path: the CR still goes out and the
// refusal surfaces in the log only (the operator surface, KTD9 shape).
func TestRefusedGotoStillAcks(t *testing.T) {
	m := &fakeMount{azOK: true, elOK: true, refuseReason: map[mount.Axis]mount.RefusalReason{
		mount.AZ: mount.RefusalLiveness,
	}}
	addr, cancel := startTestServer(t, m)
	defer cancel()
	conn := dial(t, addr)
	r := bufio.NewReader(conn)

	if _, err := conn.Write([]byte("W045 010\r")); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, r); got != "\r" {
		t.Errorf("refused W response = %q, want a bare \\r", got)
	}
	if len(m.targets) != 1 {
		t.Errorf("goto count = %d, want 1 (the refusal is logged, not dropped)", len(m.targets))
	}
}

func TestUnknownCommandGetsErrorReply(t *testing.T) {
	m := &fakeMount{azOK: true, elOK: true}
	addr, cancel := startTestServer(t, m)
	defer cancel()
	conn := dial(t, addr)
	r := bufio.NewReader(conn)

	if _, err := conn.Write([]byte("ML\r")); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, r); got != "?>\r" {
		t.Errorf("unknown response = %q, want ?>\\r", got)
	}
	if len(m.targets) != 0 {
		t.Errorf("unknown command must not move anything: %+v", m.targets)
	}
}

// A real PstRotator batch: connect, query, move — over one connection.
func TestSessionFlow(t *testing.T) {
	m := &fakeMount{az: 30, el: 0, azOK: true, elOK: true}
	addr, cancel := startTestServer(t, m)
	defer cancel()
	conn := dial(t, addr)
	r := bufio.NewReader(conn)

	if _, err := conn.Write([]byte("C2\r")); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, r); got != "+0030+0000\r" {
		t.Errorf("initial query = %q, want +0030+0000\\r", got)
	}
	if _, err := conn.Write([]byte("W200 000\r")); err != nil {
		t.Fatal(err)
	}
	if got := readLine(t, r); got != "\r" {
		t.Errorf("W response = %q, want a bare \\r", got)
	}
	if !waitCond(time.Second, func() bool {
		return len(m.targets) == 1 && m.targets[0].AZ == 200
	}) {
		t.Fatalf("goto never landed on the façade: %+v", m.targets)
	}
}

func waitCond(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}
