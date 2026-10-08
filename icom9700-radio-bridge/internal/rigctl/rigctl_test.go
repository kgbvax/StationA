package rigctl

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
)

// fakeRigctld answers each line with reply and records the commands.
func fakeRigctld(t *testing.T, reply func(cmd string) string) (addr string, cmds func() []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var got []string
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
					mu.Lock()
					got = append(got, sc.Text())
					mu.Unlock()
					c.Write([]byte(reply(sc.Text()) + "\n"))
				}
			}()
		}
	}()
	return ln.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func TestTuneSendsModeThenFreq(t *testing.T) {
	addr, cmds := fakeRigctld(t, func(string) string { return "RPRT 0" })
	c := &Client{Addr: addr}
	if err := c.Tune(context.Background(), 432_200_000, "usb"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmds(), "|"); got != "M USB 0|F 432200000" {
		t.Errorf("sent %q, want mode then freq", got)
	}
}

func TestTuneRefusalSurfaces(t *testing.T) {
	addr, cmds := fakeRigctld(t, func(cmd string) string {
		if strings.HasPrefix(cmd, "F ") {
			return "RPRT -1"
		}
		return "RPRT 0"
	})
	err := (&Client{Addr: addr}).Tune(context.Background(), 432_200_000, "fm")
	if err == nil || !strings.Contains(err.Error(), "RPRT -1") {
		t.Fatalf("err = %v, want the rigctld refusal", err)
	}
	if len(cmds()) != 2 {
		t.Errorf("sent %v", cmds())
	}
}

func TestTuneValidatesBeforeDialing(t *testing.T) {
	// An unroutable address would hang if Validate did not run first.
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
