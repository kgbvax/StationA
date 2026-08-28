package control

import (
	"sync"
	"testing"
)

// newTestServer builds a Server that records submitted commands, with a
// pre-seeded position for query replies.
func newTestServer(az, el float64, haverb bool) (*Server, *[]Command) {
	var got []Command
	pos := &Pos{}
	if haverb {
		pos.Set(az, el)
	}
	s := &Server{pos: pos, submit: func(c Command) { got = append(got, c) }}
	return s, &got
}

func TestRotctldGetPos(t *testing.T) {
	s, _ := newTestServer(123.0, 45.0, true)
	reply, closeConn := s.rotctld("p\n")
	if closeConn {
		t.Fatal("get_pos should not close")
	}
	if reply != "123.000000\n45.000000\n" {
		t.Fatalf("get_pos reply = %q", reply)
	}
}

func TestRotctldGetPosNoData(t *testing.T) {
	s, _ := newTestServer(0, 0, false)
	if reply, _ := s.rotctld("p\n"); reply != "RPRT -1\n" {
		t.Fatalf("get_pos with no readback = %q, want RPRT -1", reply)
	}
}

func TestRotctldSetPos(t *testing.T) {
	s, got := newTestServer(0, 0, true)
	reply, _ := s.rotctld("P 200 30\n")
	if reply != "RPRT 0\n" {
		t.Fatalf("set_pos reply = %q", reply)
	}
	if len(*got) != 1 || (*got)[0].Kind != KindSetPos || (*got)[0].Az != 200 || (*got)[0].El != 30 {
		t.Fatalf("set_pos command = %+v", *got)
	}
}

func TestRotctldStopAndUnknown(t *testing.T) {
	s, got := newTestServer(0, 0, true)
	if reply, _ := s.rotctld("S\n"); reply != "RPRT 0\n" {
		t.Fatalf("stop reply = %q", reply)
	}
	if len(*got) != 1 || (*got)[0].Kind != KindStop {
		t.Fatalf("stop command = %+v", *got)
	}
	if reply, _ := s.rotctld("frobnicate\n"); reply != "RPRT -1\n" {
		t.Fatalf("unknown reply = %q", reply)
	}
	if _, closeConn := s.rotctld("q\n"); !closeConn {
		t.Fatal("q should close the connection")
	}
}

func TestPosConcurrency(t *testing.T) {
	p := &Pos{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); p.Set(10, 20) }()
		go func() { defer wg.Done(); p.Get() }()
	}
	wg.Wait()
	if az, el, ok := p.Get(); !ok || az != 10 || el != 20 {
		t.Fatalf("pos = %g,%g,%v", az, el, ok)
	}
}
