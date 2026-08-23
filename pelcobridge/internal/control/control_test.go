package control

import (
	"strings"
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

func TestGS232Queries(t *testing.T) {
	s, _ := newTestServer(123.4, 45.6, true)
	if r := s.gs232("C\r"); r != "AZ=123\r" {
		t.Fatalf("C reply = %q", r)
	}
	if r := s.gs232("C2\r"); r != "AZ=123 EL=046\r" {
		t.Fatalf("C2 reply = %q", r)
	}
	if r := s.gs232("B\r"); r != "EL=046\r" {
		t.Fatalf("B reply = %q", r)
	}
}

func TestGS232QueriesNoData(t *testing.T) {
	s, _ := newTestServer(0, 0, false)
	for _, q := range []string{"C\r", "B\r", "C2\r"} {
		if r := s.gs232(q); r != "" {
			t.Fatalf("%q with no readback = %q, want empty", q, r)
		}
	}
}

func TestGS232Moves(t *testing.T) {
	s, got := newTestServer(0, 10, true)

	if r := s.gs232("W123 045\r"); r != "" {
		t.Fatalf("W reply = %q, want empty", r)
	}
	if c := last(got); c.Kind != KindSetPos || c.Az != 123 || c.El != 45 {
		t.Fatalf("W command = %+v", c)
	}

	s.gs232("M090\r") // azimuth-only: keeps current elevation (10)
	if c := last(got); c.Kind != KindSetPos || c.Az != 90 || c.El != 10 {
		t.Fatalf("M command = %+v", c)
	}

	s.gs232("R\r")
	if c := last(got); c.Kind != KindJog || c.Pan != 1 {
		t.Fatalf("R command = %+v", c)
	}
	s.gs232("S\r")
	if c := last(got); c.Kind != KindStop {
		t.Fatalf("S command = %+v", c)
	}
}

func TestGS232ContiguousW(t *testing.T) {
	s, got := newTestServer(0, 0, true)
	s.gs232("W123045\r") // spaceless 6-digit form
	if c := last(got); c.Kind != KindSetPos || c.Az != 123 || c.El != 45 {
		t.Fatalf("contiguous W command = %+v", c)
	}
}

func TestGS232AzWrap(t *testing.T) {
	// 359.6 rounds to 360, which is outside the GS-232 000-359 range; it must
	// wrap to 000 rather than report an invalid AZ=360.
	s, _ := newTestServer(359.6, 45.6, true)
	if r := s.gs232("C\r"); r != "AZ=000\r" {
		t.Fatalf("C reply for 359.6 = %q, want AZ=000", r)
	}
	if r := s.gs232("C2\r"); r != "AZ=000 EL=046\r" {
		t.Fatalf("C2 reply = %q, want AZ=000 EL=046", r)
	}
}

func last(got *[]Command) Command {
	if len(*got) == 0 {
		return Command{Kind: -1}
	}
	return (*got)[len(*got)-1]
}

// newTestUDPServer builds a UDPServer that records submitted commands, with a
// pre-seeded position for query replies.
func newTestUDPServer(az, el float64, haverb bool) (*UDPServer, *[]Command) {
	var got []Command
	pos := &Pos{}
	if haverb {
		pos.Set(az, el)
	}
	s := &UDPServer{pos: pos, submit: func(c Command) { got = append(got, c) }}
	return s, &got
}

func TestPstRotatorSetPos(t *testing.T) {
	s, got := newTestUDPServer(0, 0, true)
	if r := s.pstrotator("<PST><AZIMUTH>200</AZIMUTH><ELEVATION>30</ELEVATION></PST>"); r != "" {
		t.Fatalf("set reply = %q, want empty", r)
	}
	if c := last(got); c.Kind != KindSetPos || c.Az != 200 || c.El != 30 {
		t.Fatalf("set command = %+v", c)
	}
}

func TestPstRotatorAzOnly(t *testing.T) {
	s, got := newTestUDPServer(0, 10, true)
	s.pstrotator("<PST><AZIMUTH>90</AZIMUTH></PST>")
	if c := last(got); c.Kind != KindSetPos || c.Az != 90 || c.El != 10 {
		t.Fatalf("az-only command = %+v", c)
	}
}

func TestPstRotatorElOnly(t *testing.T) {
	s, got := newTestUDPServer(123, 0, true)
	s.pstrotator("<PST><ELEVATION>45</ELEVATION></PST>")
	if c := last(got); c.Kind != KindSetPos || c.Az != 123 || c.El != 45 {
		t.Fatalf("el-only command = %+v", c)
	}
}

func TestPstRotatorStop(t *testing.T) {
	s, got := newTestUDPServer(0, 0, true)
	s.pstrotator("<PST><STOP>1</STOP></PST>")
	if c := last(got); c.Kind != KindStop {
		t.Fatalf("stop command = %+v", c)
	}
}

func TestPstRotatorQueries(t *testing.T) {
	s, _ := newTestUDPServer(123.4, 45.6, true)
	if r := s.pstrotator("<PST>AZ?</PST>"); r != "AZ:123.4\r" {
		t.Fatalf("AZ? reply = %q", r)
	}
	if r := s.pstrotator("<PST>EL?</PST>"); r != "EL:45.6\r" {
		t.Fatalf("EL? reply = %q", r)
	}
}

func TestPstRotatorQueriesNoData(t *testing.T) {
	s, _ := newTestUDPServer(0, 0, false)
	if r := s.pstrotator("<PST>AZ?</PST>"); r != "" {
		t.Fatalf("AZ? with no readback = %q, want empty", r)
	}
}

func TestPstRotatorMalformed(t *testing.T) {
	s, got := newTestUDPServer(0, 0, true)
	if r := s.pstrotator("garbage"); r != "" {
		t.Fatalf("malformed reply = %q, want empty", r)
	}
	if len(*got) != 0 {
		t.Fatalf("malformed should not submit: %+v", *got)
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

func TestProtocolName(t *testing.T) {
	if !strings.Contains(GS232.Name(), "gs232") || !strings.Contains(Rotctld.Name(), "rotctld") ||
		!strings.Contains(PstRotator.Name(), "pstrotator") {
		t.Fatalf("names: %s %s %s", GS232.Name(), Rotctld.Name(), PstRotator.Name())
	}
}
