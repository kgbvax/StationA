package log4om

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"chase-queue/internal/adif"
)

func TestQSOFromRecord(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	q, ok := QSOFromRecord(adif.Record{"CALL": "ea8xyz/p", "BAND": "20M", "MODE": "ssb", "QSO_DATE": "20261101", "TIME_ON": "1015"}, now)
	if !ok || q.Call != "EA8XYZ" || q.Band != "20m" || q.Mode != "SSB" || !q.At.Equal(time.Date(2026, 11, 1, 10, 15, 0, 0, time.UTC)) {
		t.Fatalf("%+v %v", q, ok)
	}
	q, ok = QSOFromRecord(adif.Record{"CALL": "DL1ABC", "FREQ": "7.150"}, now)
	if !ok || q.Band != "40m" || !q.At.Equal(now) {
		t.Fatalf("%+v %v", q, ok)
	}
	if _, ok := QSOFromRecord(adif.Record{"CALL": "DL1ABC", "BAND": "30m"}, now); ok {
		t.Fatal("30m is not tracked")
	}
}

func TestListenerAllowList(t *testing.T) {
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
	addr := pc.LocalAddr().String()
	pc.Close()
	l := New(addr, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	time.Sleep(100 * time.Millisecond)
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("<CALL:6>EA8XYZ<BAND:3>20m<MODE:3>SSB<EOR>"))
	select {
	case q := <-l.QSOs():
		if q.Call != "EA8XYZ" {
			t.Fatal(q)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no QSO")
	}
}
