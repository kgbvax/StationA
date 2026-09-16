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
	mu       sync.Mutex
	writeFn  func(p []byte) (int, error)
	readFn   func(p []byte) (int, error)
	closeFn  func() error
	replyBuf []byte // staged reply bytes, drained by reads (for silent→healthy transitions)
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

// TestExchangeTimesOutOnSilentDevice covers the 2026-09-14 incident: the
// controller goes silent (wedged firmware, powered off head) while the USB
// adapter stays healthy. Every Read returns the read-window timeout, and the
// exchange must surface a timeout error WITHOUT reopening — cycling a healthy
// port on every silent poll tick would churn the handle forever — so the poll
// loop can mark the device offline and keep trying.
func TestExchangeTimesOutOnSilentDevice(t *testing.T) {
	var reopenCalls int
	silent := &errRW{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		readFn:  func(p []byte) (int, error) { return 0, errReadTimeout },
	}
	opener := func() (byteReadWriteCloser, error) {
		reopenCalls++
		return silent, nil
	}

	d := NewDevice(silent, opener)
	start := time.Now()
	_, err := d.Exchange(context.Background(), protocol.CmdStatusQuery, nil, 150*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error on silent device, got nil")
	}
	if !errors.Is(err, errReadTimeout) {
		t.Fatalf("expected errReadTimeout in chain, got: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("exchange took %v — read not bounded by the deadline", elapsed)
	}
	if reopenCalls != 0 {
		t.Fatalf("expected no reopen on silence (not a port fault), got %d calls", reopenCalls)
	}
}

// TestExchangeRecoversAfterSilence simulates the outage ending: the controller
// is silent through one exchange (device marked offline), then answers again.
// The very next exchange must succeed without any reopen — the poll loop's
// own retry is the recovery, and Refresh rebuilds the state wholesale.
func TestExchangeRecoversAfterSilence(t *testing.T) {
	rw := &errRW{
		writeFn: func(p []byte) (int, error) { return len(p), nil },
		readFn:  func(p []byte) (int, error) { return 0, errReadTimeout },
	}

	openerCalls := 0
	opener := func() (byteReadWriteCloser, error) {
		openerCalls++
		return rw, nil
	}

	d := NewDevice(rw, opener)
	if _, err := d.Exchange(context.Background(), protocol.CmdStatusQuery, nil, 100*time.Millisecond); err == nil {
		t.Fatal("expected timeout while device silent, got nil")
	}

	// Controller comes back (power-cycled): writes stage a reply, reads
	// deliver it — same mechanics as replyRW, on the same handle the device
	// already holds, mirroring a resync without any reopen.
	rw.writeFn = func(p []byte) (int, error) {
		pkt, err := protocol.DecodeFramedBytes(p[1 : len(p)-1])
		if err == nil {
			rw.replyBuf = protocol.EncodePacket(protocol.Packet{Seq: pkt.Seq, Com: protocol.ReplyOK})
		}
		return len(p), nil
	}
	rw.readFn = func(p []byte) (int, error) {
		if len(rw.replyBuf) == 0 {
			return 0, errReadTimeout
		}
		n := copy(p, rw.replyBuf)
		rw.replyBuf = rw.replyBuf[n:]
		return n, nil
	}

	pkt, err := d.Exchange(context.Background(), protocol.CmdStatusQuery, nil, time.Second)
	if err != nil {
		t.Fatalf("expected recovery on next poll after device returns, got: %v", err)
	}
	if pkt.Com != protocol.ReplyOK {
		t.Fatalf("expected ReplyOK after recovery, got com=%d", pkt.Com)
	}
	if openerCalls != 0 {
		t.Fatalf("expected no reopen across the outage (silence is not a port fault), got %d calls", openerCalls)
	}
}
