package transport

import (
	"context"
	"errors"
	"testing"
	"time"

	"ubctrl/internal/ub/protocol"
)

type fakeClient struct {
	reply protocol.Packet
	err   error
	calls int
}

func (f *fakeClient) Exchange(_ context.Context, _ byte, _ []byte, _ time.Duration) (protocol.Packet, error) {
	f.calls++
	return f.reply, f.err
}
func (f *fakeClient) Close() error { return nil }

func TestTracingDisabledEmitsNothing(t *testing.T) {
	inner := &fakeClient{reply: protocol.Packet{Com: protocol.ReplyOK}}
	var got []Trace
	c := NewTracing(inner, func() bool { return false }, func(tr Trace) { got = append(got, tr) })

	if _, err := c.Exchange(context.Background(), protocol.CmdStatusQuery, nil, time.Second); err != nil {
		t.Fatalf("Exchange error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("disabled tracer emitted %d entries, want 0", len(got))
	}
	if inner.calls != 1 {
		t.Fatalf("inner called %d times, want 1", inner.calls)
	}
}

func TestTracingEnabledEmitsTxThenRx(t *testing.T) {
	inner := &fakeClient{reply: protocol.Packet{Com: protocol.ReplyOK, Data: []byte{0xAA, 0xBB}}}
	var got []Trace
	c := NewTracing(inner, func() bool { return true }, func(tr Trace) { got = append(got, tr) })

	data := []byte{0x01, 0x02}
	if _, err := c.Exchange(context.Background(), protocol.CmdChangeFrequency, data, time.Second); err != nil {
		t.Fatalf("Exchange error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (tx, rx)", len(got))
	}
	tx, rx := got[0], got[1]
	if tx.Dir != "tx" || tx.Com != protocol.CmdChangeFrequency || len(tx.Data) != 2 || tx.Data[0] != 0x01 {
		t.Errorf("unexpected tx trace: %+v", tx)
	}
	if rx.Dir != "rx" || rx.Com != protocol.ReplyOK || len(rx.Data) != 2 || rx.Data[1] != 0xBB || rx.Err != "" {
		t.Errorf("unexpected rx trace: %+v", rx)
	}
	// The tx trace must be a copy: mutating the caller's slice must not change it.
	data[0] = 0xFF
	if tx.Data[0] != 0x01 {
		t.Errorf("tx trace data was not cloned; got %#v", tx.Data)
	}
}

func TestTracingRecordsRxError(t *testing.T) {
	inner := &fakeClient{err: errors.New("timeout waiting for response")}
	var got []Trace
	c := NewTracing(inner, func() bool { return true }, func(tr Trace) { got = append(got, tr) })

	_, err := c.Exchange(context.Background(), protocol.CmdStatusQuery, nil, time.Second)
	if err == nil {
		t.Fatal("expected error from inner")
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[1].Dir != "rx" || got[1].Err != "timeout waiting for response" {
		t.Errorf("rx trace did not record error: %+v", got[1])
	}
}
