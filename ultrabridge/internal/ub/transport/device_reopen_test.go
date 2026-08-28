package transport

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"ultrabridge/internal/ub/protocol"
)

// errRW is a minimal byteReadWriteCloser whose Write/Read/Close behaviour is
// scripted, so a test can simulate a USB-serial adapter that drops mid-exchange
// and recovers on reopen.
type errRW struct {
	mu      sync.Mutex
	writeFn func(p []byte) (int, error)
	readFn  func(p []byte) (int, error)
	closeFn func() error
}

func (w *errRW) Write(p []byte) (int, error) { return w.writeFn(p) }
func (w *errRW) Read(p []byte) (int, error)  { return w.readFn(p) }
func (w *errRW) Close() error {
	if w.closeFn != nil {
		return w.closeFn()
	}
	return nil
}

// replyRW is a read-write-closer that, on each Write, decodes the framed
// request and stages a ReplyOK reply carrying the same sequence number. Reading
// drains the staged reply. This mirrors a healthy RCU-06 responding to a
// status query with the matching seq.
type replyRW struct {
	reply []byte
}

func (r *replyRW) Write(p []byte) (int, error) {
	// p is the full framed request: STX + escaped payload + ETX. Strip the
	// STX/ETX framing and decode the payload to recover the request seq.
	pkt, err := protocol.DecodeFramedBytes(p[1 : len(p)-1])
	if err != nil {
		return len(p), nil // don't fail the write on a decode hiccup
	}
	r.reply = protocol.EncodePacket(protocol.Packet{Seq: pkt.Seq, Com: protocol.ReplyOK})
	return len(p), nil
}

func (r *replyRW) Read(p []byte) (int, error) {
	if len(r.reply) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.reply)
	r.reply = r.reply[n:]
	return n, nil
}

func (r *replyRW) Close() error { return nil }

// TestExchangeReopensOnWriteError simulates the live failure: the first write
// hits a dead port (EIO) and must transparently reopen and retry rather than
// surface "write request: Input/output error".
func TestExchangeReopensOnWriteError(t *testing.T) {
	dead := &errRW{writeFn: func(p []byte) (int, error) { return 0, errors.New("input/output error") }}
	healthy := &replyRW{}

	// First opener call (construction) returns the dead port; later calls
	// (reopen) return the healthy one. A fresh handle each reopen mirrors a
	// real OpenSerial re-resolving the by-id symlink to the fresh tty.
	healthyPort := healthy
	opener := func() (byteReadWriteCloser, error) {
		if healthyPort != nil {
			h := healthyPort
			healthyPort = nil
			return h, nil
		}
		return dead, nil
	}

	d := NewDevice(dead, opener)
	pkt, err := d.Exchange(context.Background(), protocol.CmdStatusQuery, nil, time.Second)
	if err != nil {
		t.Fatalf("expected transparent reopen+retry, got error: %v", err)
	}
	if pkt.Com != protocol.ReplyOK {
		t.Fatalf("expected ReplyOK, got com=%d", pkt.Com)
	}
}

// TestExchangeReopensOnReadError covers the read-side variant: the write
// succeeds but the read returns a port fault, which must also self-heal.
func TestExchangeReopensOnReadError(t *testing.T) {
	gone := &errRW{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		readFn:  func(p []byte) (int, error) { return 0, errors.New("input/output error") },
	}
	healthy := &replyRW{}

	healthyPort := healthy
	opener := func() (byteReadWriteCloser, error) {
		if healthyPort != nil {
			h := healthyPort
			healthyPort = nil
			return h, nil
		}
		return gone, nil
	}

	d := NewDevice(gone, opener)
	pkt, err := d.Exchange(context.Background(), protocol.CmdStatusQuery, nil, time.Second)
	if err != nil {
		t.Fatalf("expected transparent reopen+retry, got error: %v", err)
	}
	if pkt.Com != protocol.ReplyOK {
		t.Fatalf("expected ReplyOK, got com=%d", pkt.Com)
	}
}

// TestExchangeFailsWhenReopenFails ensures a persistently broken link surfaces
// the error (no infinite reopen loop) and leaves d.rw nil so the next call
// retries from a clean slate.
func TestExchangeFailsWhenReopenFails(t *testing.T) {
	dead := &errRW{writeFn: func(p []byte) (int, error) { return 0, errors.New("input/output error") }}
	opener := func() (byteReadWriteCloser, error) { return nil, errors.New("no such device") }

	d := NewDevice(dead, opener)
	_, err := d.Exchange(context.Background(), protocol.CmdStatusQuery, nil, time.Second)
	if err == nil {
		t.Fatal("expected error when reopen fails, got nil")
	}
	if d.rw != nil {
		t.Fatalf("expected d.rw nil after failed reopen, got %T", d.rw)
	}
}
