package loggers

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestValidCall(t *testing.T) {
	for _, c := range []string{"EA8XYZ", "DL1ABC/P", "9A1A", "KH6/K1A"} {
		if !ValidCall(c) {
			t.Errorf("%q should be valid", c)
		}
	}
	for _, c := range []string{"", "ab", "ea8xyz", "EA8 XYZ", "EA8XYZ\n", "<x>", "A"} {
		if ValidCall(c) {
			t.Errorf("%q should be invalid", c)
		}
	}
}

// freeUDPPair returns a port p where both p and p+1 can be bound.
func freeUDPPair(t *testing.T) int {
	t.Helper()
	for p := 20000; p < 60000; p += 7 {
		a, err1 := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p})
		if err1 != nil {
			continue
		}
		b, err2 := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: p + 1})
		a.Close()
		if err2 == nil {
			b.Close()
			return p
		}
	}
	t.Fatal("no free port pair")
	return 0
}

// fakeLog4OM answers each request with the given Done value ("" = silence).
func fakeLog4OM(t *testing.T, port int, done string) <-chan string {
	t.Helper()
	got := make(chan string, 4)
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			req := string(buf[:n])
			got <- req
			if done == "" {
				continue
			}
			s := strings.Index(req, "<MessageId>") + len("<MessageId>")
			id := req[s:strings.Index(req, "</MessageId>")]
			out, _ := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port + 1})
			fmt.Fprintf(out, "<RemoteControlResponse><MessageId>%s</MessageId><Done>%s</Done></RemoteControlResponse>", id, done)
			out.Close()
		}
	}()
	return got
}

func TestLog4OMSetCallsign(t *testing.T) {
	port := freeUDPPair(t)
	got := fakeLog4OM(t, port, "True")
	r := NewLog4OM(fmt.Sprintf("127.0.0.1:%d", port), time.Second).SendCall(context.Background(), "EA8XYZ")
	if r.Status != OK {
		t.Fatalf("result = %+v", r)
	}
	req := <-got
	if !strings.Contains(req, "<RemoteControlMessage>SetCallsign</RemoteControlMessage>") || !strings.Contains(req, "<Callsign>EA8XYZ</Callsign>") {
		t.Errorf("request = %s", req)
	}
}

func TestLog4OMDoneFalseAndSilence(t *testing.T) {
	port := freeUDPPair(t)
	fakeLog4OM(t, port, "False")
	if r := NewLog4OM(fmt.Sprintf("127.0.0.1:%d", port), time.Second).SendCall(context.Background(), "EA8XYZ"); r.Status != Failed {
		t.Errorf("Done=False result = %+v", r)
	}
	port = freeUDPPair(t)
	fakeLog4OM(t, port, "")
	if r := NewLog4OM(fmt.Sprintf("127.0.0.1:%d", port), 150*time.Millisecond).SendCall(context.Background(), "EA8XYZ"); r.Status != NotConfirmed {
		t.Errorf("silent result = %+v", r)
	}
}

func TestLog4OMRefusesBadCall(t *testing.T) {
	if r := NewLog4OM("127.0.0.1:1", time.Second).SendCall(context.Background(), "ea8 <x>"); r.Status != Skipped {
		t.Errorf("result = %+v", r)
	}
}

func TestDXLogSender(t *testing.T) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go func() {
		buf := make([]byte, 64)
		n, from, _ := c.ReadFromUDP(buf)
		if string(buf[:n]) == "CALL EA8XYZ" {
			c.WriteToUDP([]byte("OK"), from)
		}
	}()
	addr := c.LocalAddr().String()
	if r := NewDXLog(addr, time.Second).SendCall(context.Background(), "EA8XYZ"); r.Status != OK {
		t.Errorf("result = %+v", r)
	}
	if r := NewDXLog(addr, time.Second).SendCall(context.Background(), "bad call"); r.Status != Skipped {
		t.Errorf("bad call result = %+v", r)
	}
}

func TestDXLogNobodyListening(t *testing.T) {
	c, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	addr := c.LocalAddr().String()
	c.Close() // port now unbound
	r := NewDXLog(addr, 500*time.Millisecond).SendCall(context.Background(), "EA8XYZ")
	if r.Status != Failed && r.Status != NotConfirmed {
		t.Errorf("result = %+v", r)
	}
	if r.Status == OK {
		t.Error("must not report ok")
	}
}
