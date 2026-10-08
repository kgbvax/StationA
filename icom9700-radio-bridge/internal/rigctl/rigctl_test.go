package rigctl

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// fakeRadio is a rigctld stand-in with real state: it records every command,
// applies M/F/U SATMODE, and answers f/m from that state. ignoreMode makes
// it accept "M" with RPRT 0 but keep its old mode (the live failure: the
// 9700 stayed LSB). refuse names a command prefix answered with RPRT -1.
type fakeRadio struct {
	mu         sync.Mutex
	cmds       []string
	freq       int64
	mode       string
	sat        bool
	ignoreMode bool
	refuse     string
}

func (r *fakeRadio) handle(cmd string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, cmd)
	if r.refuse != "" && strings.HasPrefix(cmd, r.refuse) {
		return "RPRT -1"
	}
	f := strings.Fields(cmd)
	switch f[0] {
	case "U":
		r.sat = f[2] == "1"
	case "M":
		if !r.ignoreMode {
			r.mode = f[1]
		}
	case "F":
		fmt.Sscan(f[1], &r.freq)
	case "f":
		return fmt.Sprint(r.freq)
	case "m":
		return r.mode + "\n12000"
	}
	return "RPRT 0"
}

func (r *fakeRadio) sent() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.cmds, "|")
}

func serve(t *testing.T, r *fakeRadio) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					c.Write([]byte(r.handle(sc.Text()) + "\n"))
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func TestTuneParksOutOfSatModeThenModeThenFreq(t *testing.T) {
	r := &fakeRadio{sat: true, mode: "LSB", freq: 435_856_762}
	if err := (&Client{Addr: serve(t, r)}).Tune(context.Background(), 145_212_500, "fm"); err != nil {
		t.Fatal(err)
	}
	if got := r.sent(); got != "U SATMODE 0|M FM 0|F 145212500|f|m" {
		t.Errorf("sent %q, want satmode off, mode, freq, then readbacks", got)
	}
	if r.sat || r.mode != "FM" || r.freq != 145_212_500 {
		t.Errorf("radio state sat=%v mode=%s freq=%d", r.sat, r.mode, r.freq)
	}
}

// The live failure: rigctld said RPRT 0 but the radio stayed LSB. The
// readback must turn that into an error instead of a silent success.
func TestTuneDetectsRadioIgnoringMode(t *testing.T) {
	r := &fakeRadio{mode: "LSB", ignoreMode: true}
	err := (&Client{Addr: serve(t, r)}).Tune(context.Background(), 145_212_500, "fm")
	if err == nil || !strings.Contains(err.Error(), "reads mode LSB") {
		t.Fatalf("err = %v, want the readback mismatch", err)
	}
}

func TestTuneRefusalSurfaces(t *testing.T) {
	r := &fakeRadio{}
	err := (&Client{Addr: serve(t, r)}).Tune(context.Background(), 145_212_500, "fm")
	if err != nil {
		t.Fatal(err)
	}
	r2 := &fakeRadio{refuse: "U SATMODE"}
	err = (&Client{Addr: serve(t, r2)}).Tune(context.Background(), 145_212_500, "fm")
	if err == nil || !strings.Contains(err.Error(), "RPRT -1") {
		t.Fatalf("err = %v, want the rigctld refusal", err)
	}
	if got := r2.sent(); got != "U SATMODE 0" {
		t.Errorf("after a refused satmode, sent %q — must stop there", got)
	}
}

func TestTuneValidatesBeforeDialing(t *testing.T) {
	c := &Client{Addr: "127.0.0.1:1"}
	for _, tc := range []struct {
		hz   int64
		mode string
	}{{0, "fm"}, {432_200_000, "dv"}, {9_000_000_000, "fm"}, {432_200_000, "FM"}} {
		if err := c.Tune(context.Background(), tc.hz, tc.mode); err == nil || strings.Contains(err.Error(), "rigctld 127") {
			t.Errorf("Tune(%d,%q) = %v, want a validation error before any dial", tc.hz, tc.mode, err)
		}
	}
}

func TestTuneUnreachable(t *testing.T) {
	if err := (&Client{Addr: "127.0.0.1:1"}).Tune(context.Background(), 432_200_000, "fm"); err == nil {
		t.Fatal("want a dial error")
	}
}
