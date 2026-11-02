package cluster

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s, ok := Parse("DX de SP5XYZ:     14025.0  EA8XYZ       CW 599 tnx QSO           1234Z JO32", now)
	if !ok || s.Spotter != "SP5XYZ" || s.Call != "EA8XYZ" || s.FreqHz != 14025000 || s.Band != "20m" || s.Comment != "CW 599 tnx QSO" || !s.At.Equal(now) {
		t.Fatalf("%+v %v", s, ok)
	}
	s, ok = Parse("DX de DL1ABC-#:  7012.3  ea8/dl2xx  CW 12 dB 25 WPM CQ  1200Z", now)
	if !ok || !s.Skimmer() || s.Call != "EA8/DL2XX" || s.Band != "40m" {
		t.Fatalf("%+v", s)
	}
	for _, l := range []string{"DX de K1ABC: 144300.0 W1AW FM 1200Z", "WWV de VE7CC <18>: SFI=150", "To ALL de DL1ABC: hi", ""} {
		if _, ok := Parse(l, now); ok {
			t.Errorf("parsed %q", l)
		}
	}
}

func TestClientLogsInAndReadsSpots(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	login := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.Write([]byte("Welcome to the node\r\nlogin: "))
		line, _ := bufio.NewReader(c).ReadString('\n')
		login <- strings.TrimSpace(line)
		c.Write([]byte("Hello\r\nDX de SP5XYZ:     14025.0  EA8XYZ       CW               1234Z\r\n"))
		time.Sleep(time.Second)
	}()
	cl := &Client{Addr: ln.Addr().String(), Call: "DL9ET"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan Spot, 1)
	go cl.Run(ctx, func(s Spot) { got <- s })
	if l := <-login; l != "DL9ET" {
		t.Fatalf("login %q", l)
	}
	select {
	case s := <-got:
		if s.Call != "EA8XYZ" || !cl.State().Connected {
			t.Fatalf("%+v %+v", s, cl.State())
		}
	case <-ctx.Done():
		t.Fatal("no spot")
	}
}
