package rotctld

import (
	"bufio"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"pelcobridge2/internal/control"
)

// stubRot records intents and returns canned results.
type stubRot struct {
	mu    sync.Mutex
	calls []control.Intent
	pan   control.Result
	tilt  control.Result
}

func (s *stubRot) Call(ctx context.Context, from control.Source, it control.Intent) control.Result {
	s.mu.Lock()
	s.calls = append(s.calls, it)
	s.mu.Unlock()
	switch it.(type) {
	case control.QueryPanIntent, control.SetPanIntent, control.GotoAzElIntent:
		return s.pan
	case control.QueryTiltIntent, control.SetTiltIntent:
		return s.tilt
	}
	return control.Result{}
}

func (s *stubRot) intents() []control.Intent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]control.Intent(nil), s.calls...)
}

func newStub(pan, tilt control.Result) *stubRot {
	return &stubRot{pan: pan, tilt: tilt}
}

func TestWireTable(t *testing.T) {
	tests := []struct {
		name string
		rot  *stubRot
		in   string
		want string
	}{
		{"get_pos short", newStub(control.Result{Deg: 123.4}, control.Result{Deg: 45}),
			"p\n", "123.40\n45.00\n"},
		{"get_pos long", newStub(control.Result{Deg: 0}, control.Result{Deg: 90}),
			"\\get_pos\n", "0.00\n90.00\n"},
		{"get_pos extended", newStub(control.Result{Deg: 359.99}, control.Result{Deg: 0.5}),
			"+p\n", "RPRT 0\n359.99\n0.50\n"},
		{"get_pos no readback", newStub(control.Result{Err: control.ErrNoFix}, control.Result{}),
			"p\n", "RPRT -11\n"},
		{"get_pos while moving", newStub(control.Result{Err: control.ErrMoving}, control.Result{}),
			"p\n", "RPRT -11\n"},

		{"set_pos ok", newStubOk(), "P 180 45\n", "RPRT 0\n"},
		{"set_pos long", newStubOk(), "\\set_pos 359.5 0\n", "RPRT 0\n"},
		{"set_pos comma decimal", newStubOk(), "P 12,5 3\n", "RPRT 0\n"},
		{"set_pos disarmed", newStub(control.Result{Err: control.ErrDisarmed}, control.Result{}),
			"P 180 45\n", "RPRT -9\n"},
		{"set_pos garbage az", newStubOk(), "P abc 45\n", "RPRT -1\n"},
		// ParseFloat("nan"/"inf") succeeds — DegToWord would park them at 0°,
		// real motion to a garbage target. Must be refused like any other junk.
		{"set_pos nan az", newStubOk(), "P nan 45\n", "RPRT -1\n"},
		{"set_pos inf az", newStubOk(), "P inf 45\n", "RPRT -1\n"},
		{"set_pos nan el", newStubOk(), "P 180 nan\n", "RPRT -1\n"},
		{"set_pos missing el", newStubOk(), "P 180\n", "RPRT -1\n"},
		{"set_pos short", newStubOk(), "S\n", "RPRT 0\n"},
		{"stop long", newStubOk(), "\\stop\n", "RPRT 0\n"},

		{"get_info", newStubOk(), "_\n", "mockhead\n"},
		{"get_info long", newStubOk(), "\\get_info\n", "mockhead\n"},

		{"dump_state", newStubOk(), "\\dump_state\n",
			"1\n" +
				"rot_model=901\n" +
				"min_az=0.000000\n" +
				"max_az=360.000000\n" +
				"min_el=0.000000\n" +
				"max_el=90.000000\n" +
				"south_zero=0\n" +
				"rot_type=AzEl\n" +
				"done\n"},

		{"quit", newStubOk(), "q\n", ""},
		{"quit upper", newStubOk(), "Q\n", ""},

		{"comment ignored", newStubOk(), "# a comment\n", ""},
		{"empty ignored", newStubOk(), "   \n", ""},
		{"unknown", newStubOk(), "x\n", "RPRT -4\n"},
		{"unknown long", newStubOk(), "\\set_conf foo bar\n", "RPRT -4\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.rot, "mockhead", DefaultLimits())
			got, closeConn := s.Handle(tc.in)
			if got != tc.want {
				t.Fatalf("Handle(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if tc.name == "quit" || tc.name == "quit upper" {
				if !closeConn {
					t.Fatal("q must close the connection")
				}
			} else if closeConn {
				t.Fatalf("%s must not close the connection", tc.name)
			}
		})
	}
}

func newStubOk() *stubRot { return newStub(control.Result{Deg: 10}, control.Result{Deg: 5}) }

func TestSetPosCarriesDegrees(t *testing.T) {
	stub := newStubOk()
	s := New(stub, "mockhead", DefaultLimits())

	s.Handle("P 179.5 12.25")
	got := stub.intents()
	if len(got) != 1 {
		t.Fatalf("set_pos issued %d intents, want 1 (atomic az+el)", len(got))
	}
	gotoIt, ok := got[0].(control.GotoAzElIntent)
	if !ok || !gotoIt.HasAz || !gotoIt.HasEl || gotoIt.Az != 179.5 || gotoIt.El != 12.25 {
		t.Fatalf("set_pos = %#v, want GotoAzElIntent{179.5, 12.25, both axes}", got[0])
	}
}

func TestGetPosIssuesTwoQueries(t *testing.T) {
	stub := newStub(control.Result{Deg: 200.25}, control.Result{Deg: 30})
	s := New(stub, "mockhead", DefaultLimits())

	got, _ := s.Handle("p")
	if got != "200.25\n30.00\n" {
		t.Fatalf("p = %q", got)
	}
	kinds := stub.intents()
	if _, ok := kinds[0].(control.QueryPanIntent); !ok {
		t.Fatalf("first intent = %T, want QueryPanIntent", kinds[0])
	}
	if _, ok := kinds[1].(control.QueryTiltIntent); !ok {
		t.Fatalf("second intent = %T, want QueryTiltIntent", kinds[1])
	}
}

// serveTCP runs a server on an ephemeral loopback port and returns its address.
func serveTCP(t *testing.T, s *Server) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.ListenAndServe(ctx, "127.0.0.1:0")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a := s.Addr(); a != nil {
			return a.String()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("listener never came up")
	return ""
}

func TestOverTheWire(t *testing.T) {
	s := New(newStubOk(), "mockhead", DefaultLimits())
	addr := serveTCP(t, s)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Multiple commands on one connection, then quit.
	if _, err := conn.Write([]byte("S\n\\get_pos\nq\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
	sc := bufio.NewScanner(conn)
	want := []string{"RPRT 0", "10.00", "5.00"}
	for _, w := range want {
		if !sc.Scan() {
			t.Fatalf("connection closed early, want %q", w)
		}
		if sc.Text() != w {
			t.Fatalf("line = %q, want %q", sc.Text(), w)
		}
	}
}

func TestClientCount(t *testing.T) {
	s := New(newStubOk(), "mockhead", DefaultLimits())
	addr := serveTCP(t, s)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("S\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if s.Clients() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("clients = %d, want 1", s.Clients())
		}
		time.Sleep(5 * time.Millisecond)
	}

	conn.Close()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && s.Clients() != 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Clients() != 0 {
		t.Fatalf("clients = %d after close, want 0", s.Clients())
	}
}
