// SPDX-License-Identifier: AGPL-3.0-or-later

package pstrotator

// Datagram-table tests for the PstRotator native UDP listener (plan U7).
//
// Two layers of proof:
//
//   - Grammar/precedence: a fake façade records the exact mount calls each
//     datagram produces, pinning the Appendix C grammar (R7), the
//     present-axes-only dispatch (R8/AE2), the STOP-beats-motion precedence
//     (KTD11) and the reply pins.
//   - Semantics: the REAL mount façade over fake per-axis controllers (and,
//     in the end-to-end test, the real in-process SPID/ERC-M mocks) pins the
//     refusal logging (R9/R10), the park dispatch and the deadband no-op —
//     the server must never re-implement those (KTD12).

import (
	"context"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"spid-ercm-rotator-bridge/internal/config"
	"spid-ercm-rotator-bridge/internal/ercm"
	"spid-ercm-rotator-bridge/internal/mount"
	"spid-ercm-rotator-bridge/internal/spid"
)

// --- test scaffolding -----------------------------------------------------------

// freePortPair finds a UDP port on 127.0.0.1 whose port+1 is free too — the
// reply socket convention (source IP at listen-port+1) makes both needed.
func freePortPair(t *testing.T) int {
	t.Helper()
	for i := 0; i < 25; i++ {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			continue
		}
		port := c.LocalAddr().(*net.UDPAddr).Port
		_ = c.Close()
		c2, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port+1)))
		if err != nil {
			continue
		}
		_ = c2.Close()
		return port
	}
	t.Fatal("no free UDP port pair found")
	return 0
}

// startTestServer runs the server on a free port pair and returns the listen
// address and the listen port (query replies arrive on port+1). The single
// bind attempt can lose a race with an ephemeral-port UDP dial from an
// earlier test (the OS recycles recently freed ports quickly), so a Run that
// dies before binding is retried on a fresh port pair.
func startTestServer(t *testing.T, m Mount, log *slog.Logger) (addr string, port int) {
	t.Helper()
	const attempts = 5
outer:
	for attempt := 0; attempt < attempts; attempt++ {
		port = freePortPair(t)
		cfg := config.PstRotatorConfig{Bind: "127.0.0.1", Port: port}
		srv := New(cfg, m, log)
		addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() { errCh <- srv.Run(ctx) }()

		// Wait for the bind: a fresh ListenPacket on the same address fails
		// with EADDRINUSE once the server owns it (a UDP dial cannot detect
		// a bind). A Run error before the bind means a lost port race.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case err := <-errCh:
				cancel()
				if attempt == attempts-1 {
					t.Fatalf("server Run failed to bind after %d attempts: %v", attempts, err)
				}
				continue outer
			default:
			}
			c, err := net.ListenPacket("udp", addr)
			if err != nil {
				// Address in use: the server has it.
				t.Cleanup(func() {
					cancel()
					select {
					case <-errCh:
					case <-time.After(2 * time.Second):
						t.Error("Server.Run did not return after ctx cancel")
					}
				})
				return addr, port
			}
			_ = c.Close()
			time.Sleep(2 * time.Millisecond)
		}
		cancel()
		<-errCh
	}
	t.Fatal("server did not bind")
	return "", 0
}

func sendDatagram(t *testing.T, addr, msg string) {
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

// bindReply binds the reply socket at listen-port+1 (the PstRotator reply
// convention, Appendix C).
func bindReply(t *testing.T, port int) net.PacketConn {
	t.Helper()
	pc, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port+1)))
	if err != nil {
		t.Fatalf("bind reply socket: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

// readReply reads one reply datagram with a generous deadline.
func readReply(t *testing.T, pc net.PacketConn) string {
	t.Helper()
	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no reply on port+1: %v", err)
	}
	return string(buf[:n])
}

// expectNoReply asserts silence within d — the pinned no-reply behaviors
// (motion datagrams, queries with no valid readback).
func expectNoReply(t *testing.T, pc net.PacketConn, d time.Duration) {
	t.Helper()
	_ = pc.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 512)
	if n, _, err := pc.ReadFrom(buf); err == nil {
		t.Errorf("unexpected reply %q", string(buf[:n]))
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// lockedBuffer is an io.Writer safe for the slog handler goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// --- fakes ----------------------------------------------------------------------

// fakeMount records the exact façade calls each datagram produces.
type fakeMount struct {
	mu      sync.Mutex
	gotos   []mount.Target
	stops   int
	parks   int
	az, el  float64
	azValid bool
	elValid bool
}

func (f *fakeMount) Goto(t mount.Target) []mount.Refusal {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotos = append(f.gotos, t)
	return nil
}

func (f *fakeMount) Stop() []mount.AxisError {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return nil
}

func (f *fakeMount) Park() []mount.Refusal {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parks++
	return nil
}

func (f *fakeMount) Readback(ax mount.Axis) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ax == mount.AZ {
		return f.az, f.azValid
	}
	return f.el, f.elValid
}

func (f *fakeMount) Gotos() []mount.Target {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]mount.Target, len(f.gotos))
	copy(out, f.gotos)
	return out
}

func (f *fakeMount) Stops() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

func (f *fakeMount) Parks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.parks
}

// fakeAxis is a minimal per-axis controller for driving the REAL mount façade
// in the semantics tests (refusal logging, park, deadband).
type fakeAxis struct {
	mu      sync.Mutex
	online  bool
	rb      float64
	valid   bool
	targets []float64
	stops   int
}

func (f *fakeAxis) SetTarget(deg float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = append(f.targets, deg)
	return nil
}

func (f *fakeAxis) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return nil
}

func (f *fakeAxis) Readback() (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rb, f.valid
}

func (f *fakeAxis) Online() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online
}

func (f *fakeAxis) Targets() []float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]float64, len(f.targets))
	copy(out, f.targets)
	return out
}

func (f *fakeAxis) StopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

func (f *fakeAxis) setReadback(deg float64, valid bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rb, f.valid = deg, valid
}

func (f *fakeAxis) setOnline(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.online = on
}

// newRealMount runs the real façade over two fake axes until the test ends.
func newRealMount(t *testing.T, az, el *fakeAxis) *mount.Mount {
	t.Helper()
	az.setOnline(true)
	el.setOnline(true)
	m := mount.New(az, el, config.Defaults().Control, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)
	return m
}

// --- grammar and precedence (fake façade) ----------------------------------------

func TestDatagramTable(t *testing.T) {
	cases := []struct {
		name       string
		dgram      string
		wantGotos  []mount.Target
		wantStops  int
		wantParks  int
		wantNoCall bool
	}{
		{
			name:      "azimuth only (AE2): el structurally omitted",
			dgram:     "<PST><AZIMUTH>200</AZIMUTH></PST>",
			wantGotos: []mount.Target{{AZ: 200, HasAZ: true}},
		},
		{
			name:      "elevation only: az structurally omitted",
			dgram:     "<PST><ELEVATION>45</ELEVATION></PST>",
			wantGotos: []mount.Target{{EL: 45, HasEL: true}},
		},
		{
			name:      "az and el together",
			dgram:     "<PST><AZIMUTH>200</AZIMUTH><ELEVATION>45</ELEVATION></PST>",
			wantGotos: []mount.Target{{AZ: 200, EL: 45, HasAZ: true, HasEL: true}},
		},
		{
			name:      "decimal values",
			dgram:     "<PST><AZIMUTH>123.5</AZIMUTH><ELEVATION>10.25</ELEVATION></PST>",
			wantGotos: []mount.Target{{AZ: 123.5, EL: 10.25, HasAZ: true, HasEL: true}},
		},
		{
			name:      "stray-space tolerance (manual shape)",
			dgram:     "<PST><AZIMUTH> 85 </AZIMUTH></PST>",
			wantGotos: []mount.Target{{AZ: 85, HasAZ: true}},
		},
		{
			name:      "case-insensitive tags",
			dgram:     "<pst><azimuth>85</azimuth></pst>",
			wantGotos: []mount.Target{{AZ: 85, HasAZ: true}},
		},
		{
			name:      "negative value parses (limits are the facade's job)",
			dgram:     "<PST><AZIMUTH>-5</AZIMUTH></PST>",
			wantGotos: []mount.Target{{AZ: -5, HasAZ: true}},
		},
		{
			name:       "STOP alone",
			dgram:      "<PST><STOP>1</STOP></PST>",
			wantStops:  1,
			wantNoCall: true,
		},
		{
			// The manual's batched example: STOP wins, azimuth NOT dispatched.
			name:       "STOP beats AZIMUTH in one datagram (manual batched example)",
			dgram:      "<PST><STOP>1</STOP><AZIMUTH>85</AZIMUTH></PST>",
			wantStops:  1,
			wantNoCall: true,
		},
		{
			name:       "STOP beats PARK in one datagram",
			dgram:      "<PST><STOP>1</STOP><PARK>1</PARK></PST>",
			wantStops:  1,
			wantNoCall: true,
		},
		{
			name:       "PARK dispatches through the facade",
			dgram:      "<PST><PARK>1</PARK></PST>",
			wantParks:  1,
			wantNoCall: true,
		},
		{
			name:       "unknown tags ignored (TRACK/SATSELECT/LLH/FOO)",
			dgram:      "<PST><TRACK>0</TRACK><SATSELECT>ISS</SATSELECT><LLH>52,13,10</LLH><FOO>1</FOO></PST>",
			wantNoCall: true,
		},
		{
			name:       "empty PST body ignored",
			dgram:      "<PST></PST>",
			wantNoCall: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fm := &fakeMount{}
			addr, _ := startTestServer(t, fm, nil)

			sendDatagram(t, addr, tc.dgram)

			// Wait for the positive expectation, then settle so a late,
			// unwanted dispatch cannot slip past unnoticed.
			if tc.wantStops > 0 {
				eventually(t, "stop dispatched", func() bool { return fm.Stops() == tc.wantStops })
			}
			if tc.wantParks > 0 {
				eventually(t, "park dispatched", func() bool { return fm.Parks() == tc.wantParks })
			}
			if len(tc.wantGotos) > 0 {
				eventually(t, "goto dispatched", func() bool { return len(fm.Gotos()) == len(tc.wantGotos) })
			}
			time.Sleep(100 * time.Millisecond)

			if got := fm.Gotos(); len(got) != len(tc.wantGotos) {
				t.Errorf("gotos = %+v, want %+v", got, tc.wantGotos)
			} else {
				for i := range got {
					if got[i] != tc.wantGotos[i] {
						t.Errorf("gotos[%d] = %+v, want %+v", i, got[i], tc.wantGotos[i])
					}
				}
			}
			if fm.Stops() != tc.wantStops {
				t.Errorf("stops = %d, want %d", fm.Stops(), tc.wantStops)
			}
			if fm.Parks() != tc.wantParks {
				t.Errorf("parks = %d, want %d", fm.Parks(), tc.wantParks)
			}
		})
	}
}

// --- queries ---------------------------------------------------------------------

func TestQueryReplies(t *testing.T) {
	fm := &fakeMount{az: 247, azValid: true, el: 45.5, elValid: true}
	addr, port := startTestServer(t, fm, nil)
	pc := bindReply(t, port)

	// AZ? — the pinned reply string (Appendix C / KTD11: manual shape,
	// one decimal, trailing CR; bench-reconciled against the real instance).
	sendDatagram(t, addr, "<PST>AZ?</PST>")
	if got := readReply(t, pc); got != "AZ:247.0\r" {
		t.Errorf("AZ? reply = %q, want %q", got, "AZ:247.0\r")
	}

	// EL?
	sendDatagram(t, addr, "<PST>EL?</PST>")
	if got := readReply(t, pc); got != "EL:45.5\r" {
		t.Errorf("EL? reply = %q, want %q", got, "EL:45.5\r")
	}

	// Case-insensitive query (wrc precedent keeps matching tolerant).
	sendDatagram(t, addr, "<pst>az?</pst>")
	if got := readReply(t, pc); got != "AZ:247.0\r" {
		t.Errorf("az? reply = %q, want %q", got, "AZ:247.0\r")
	}

	// Batched queries in one datagram: one reply per query, AZ first.
	sendDatagram(t, addr, "<PST><AZ?><EL?></PST>")
	if got := readReply(t, pc); got != "AZ:247.0\r" {
		t.Errorf("batched AZ? reply = %q, want %q", got, "AZ:247.0\r")
	}
	if got := readReply(t, pc); got != "EL:45.5\r" {
		t.Errorf("batched EL? reply = %q, want %q", got, "EL:45.5\r")
	}

	// Queries are pure reads: no motion dispatched from a query datagram.
	time.Sleep(100 * time.Millisecond)
	if len(fm.Gotos()) != 0 || fm.Stops() != 0 || fm.Parks() != 0 {
		t.Errorf("query datagram must not dispatch: gotos=%v stops=%d parks=%d",
			fm.Gotos(), fm.Stops(), fm.Parks())
	}
}

// KTD9 no-fabrication on the query path: with no valid cached readback the
// server stays silent rather than inventing a position.
func TestQueryNoValidReadbackStaysSilent(t *testing.T) {
	fm := &fakeMount{azValid: false, elValid: false}
	addr, port := startTestServer(t, fm, nil)
	pc := bindReply(t, port)

	sendDatagram(t, addr, "<PST><AZ?><EL?></PST>")
	expectNoReply(t, pc, 300*time.Millisecond)
}

// Appendix C: no reply datagram exists for motion commands.
func TestNoReplyForMotionDatagram(t *testing.T) {
	fm := &fakeMount{azValid: true}
	addr, port := startTestServer(t, fm, nil)
	pc := bindReply(t, port)

	sendDatagram(t, addr, "<PST><AZIMUTH>180</AZIMUTH></PST>")
	expectNoReply(t, pc, 300*time.Millisecond)

	// ...and for STOP and PARK likewise.
	sendDatagram(t, addr, "<PST><STOP>1</STOP></PST>")
	expectNoReply(t, pc, 300*time.Millisecond)
	sendDatagram(t, addr, "<PST><PARK>1</PARK></PST>")
	expectNoReply(t, pc, 300*time.Millisecond)

	eventually(t, "all three dispatched", func() bool {
		return len(fm.Gotos()) == 1 && fm.Stops() == 1 && fm.Parks() == 1
	})
}

// --- semantics through the REAL façade --------------------------------------------

// R9/R10 on the PstRotator path: UDP motion datagrams have no reply contract,
// so limit and liveness refusals are LOGGED (Warn, component/slot attrs per
// the logging convention) and never reach the wire.
func TestRefusalsLogged(t *testing.T) {
	az := &fakeAxis{}
	az.setReadback(30, true)
	el := &fakeAxis{}
	el.setReadback(10, true)
	m := newRealMount(t, az, el)
	el.setOnline(false) // dead el link (AE3 shape) — after construction; the
	// façade reads liveness at admit time, not at construction.

	sink := &lockedBuffer{}
	lg := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	addr, _ := startTestServer(t, m, lg)

	// az 999 is outside [0,360] (limit); el 45 toward a dead link (liveness).
	sendDatagram(t, addr, "<PST><AZIMUTH>999</AZIMUTH><ELEVATION>45</ELEVATION></PST>")

	eventually(t, "both refusals logged", func() bool {
		out := sink.String()
		return strings.Contains(out, "reason=limit") && strings.Contains(out, "reason=liveness")
	})

	out := sink.String()
	if !strings.Contains(out, "refused") {
		t.Errorf("refusal log line missing 'refused':\n%s", out)
	}
	for _, want := range []string{"axis=az", "axis=el", "target=999"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal log missing %s:\n%s", want, out)
		}
	}
	// Refused targets never reach the controllers (R10: before any write).
	time.Sleep(100 * time.Millisecond)
	if len(az.Targets()) != 0 {
		t.Errorf("limit-refused az target must not write, got %v", az.Targets())
	}
	if len(el.Targets()) != 0 {
		t.Errorf("liveness-refused el target must not write, got %v", el.Targets())
	}
}

// PARK slews BOTH axes to their configured park positions; its stop phase
// does not suppress its own park slew (mount.Park owns that, KTD11).
func TestParkSlewsBothAxes(t *testing.T) {
	az := &fakeAxis{}
	az.setReadback(100, true)
	el := &fakeAxis{}
	el.setReadback(40, true)
	m := newRealMount(t, az, el) // parks: az 0, el 0 (config defaults)

	addr, _ := startTestServer(t, m, nil)
	sendDatagram(t, addr, "<PST><PARK>1</PARK></PST>")

	eventually(t, "az parked", func() bool { return reflect.DeepEqual(az.Targets(), []float64{0}) })
	eventually(t, "el parked", func() bool { return reflect.DeepEqual(el.Targets(), []float64{0}) })
	if az.StopCount() == 0 || el.StopCount() == 0 {
		t.Errorf("park's stop phase must halt both axes: az stops=%d el stops=%d",
			az.StopCount(), el.StopCount())
	}
}

// Already-at-park: the deadband (R12) makes the park slew a serial no-op —
// the stop phase runs, the targets never reach the controllers.
func TestParkAlreadyAtParkDeadbandNoop(t *testing.T) {
	az := &fakeAxis{}
	az.setReadback(0, true) // at park
	el := &fakeAxis{}
	el.setReadback(0, true)
	m := newRealMount(t, az, el)

	addr, _ := startTestServer(t, m, nil)
	sendDatagram(t, addr, "<PST><PARK>1</PARK></PST>")

	eventually(t, "stop phase ran", func() bool {
		return az.StopCount() == 1 && el.StopCount() == 1
	})
	time.Sleep(100 * time.Millisecond)
	if len(az.Targets()) != 0 || len(el.Targets()) != 0 {
		t.Errorf("at-park PARK must be a deadband no-op: az targets=%v el targets=%v",
			az.Targets(), el.Targets())
	}
}

// --- mock-mode end-to-end: real datagrams → real façade → real mocks --------------

// The full U7 stack against the in-process device mocks (KTD7 mock mode):
// the UDP listener drives the REAL mount façade into the REAL SPID and ERC-M
// mocks, and the query path reads live positions back out of them.
func TestMockModeEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Real SPID mock axis (fast poll cadence).
	azMock := spid.NewMock(spid.Opts{
		PollInterval:   2 * time.Millisecond,
		ReopenCooldown: 5 * time.Millisecond,
		WritePace:      time.Millisecond,
		ReadTimeout:    50 * time.Millisecond,
	})
	go azMock.RunPoll(ctx)

	// Real ERC-M mock behind the real elevation driver.
	elDev := ercm.NewMock()
	elDrv := ercm.New(ercm.Config{
		PollInterval:   5 * time.Millisecond,
		ReopenCooldown: 5 * time.Millisecond,
		ReplyTimeout:   time.Second,
		Opener:         func() (io.ReadWriteCloser, error) { return elDev.Port(), nil },
	}, nil)
	go elDrv.Run(ctx)

	m := mount.New(azMock, elDrv, config.Defaults().Control, nil)
	go m.Run(ctx)

	addr, port := startTestServer(t, m, nil)

	// AE2 through the real stack: azimuth-only datagram moves the SPID and
	// leaves the ERC-M/GS-500 untouched — no W command ever reaches it.
	sendDatagram(t, addr, "<PST><AZIMUTH>180</AZIMUTH></PST>")
	eventually(t, "spid mock at 180", func() bool {
		deg, valid := azMock.Readback()
		return valid && deg == 180
	})
	if azMock.Target() != 180 {
		t.Errorf("spid mock commanded target = %v, want 180", azMock.Target())
	}
	time.Sleep(50 * time.Millisecond)
	for _, w := range elDev.Writes() {
		if strings.HasPrefix(w, "W") {
			t.Errorf("azimuth-only datagram must not move elevation; saw ERC-M command %q", w)
		}
	}

	// Then a full az+el datagram: both axes move.
	sendDatagram(t, addr, "<PST><AZIMUTH>200</AZIMUTH><ELEVATION>45</ELEVATION></PST>")
	eventually(t, "spid mock at 200", func() bool {
		deg, valid := azMock.Readback()
		return valid && deg == 200
	})
	eventually(t, "erc-m mock at 45", func() bool {
		el, valid := elDrv.Readback()
		return valid && el == 45
	})

	// Live positions flow back out on the query path (port+1, pinned strings).
	pc := bindReply(t, port)
	sendDatagram(t, addr, "<PST><AZ?><EL?></PST>")
	if got := readReply(t, pc); got != "AZ:200.0\r" {
		t.Errorf("e2e AZ? reply = %q, want %q", got, "AZ:200.0\r")
	}
	if got := readReply(t, pc); got != "EL:45.0\r" {
		t.Errorf("e2e EL? reply = %q, want %q", got, "EL:45.0\r")
	}

	// STOP halts both axes through the real stack.
	sendDatagram(t, addr, "<PST><STOP>1</STOP></PST>")
	eventually(t, "both axes halted", func() bool {
		_, azOK := m.Target(mount.AZ)
		_, elOK := m.Target(mount.EL)
		return !azOK && !elOK
	})
}
