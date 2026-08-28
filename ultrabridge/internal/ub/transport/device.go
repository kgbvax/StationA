package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"ultrabridge/internal/ub/protocol"
)

var (
	ErrClosed = errors.New("device closed")
)

type byteReadWriteCloser interface {
	io.Reader
	io.Writer
	io.Closer
}

type Client interface {
	Exchange(ctx context.Context, com byte, data []byte, timeout time.Duration) (protocol.Packet, error)
	Close() error
}

// Device is a request/response client over a byte-oriented serial link.
//
// It owns an opener so it can self-heal when the underlying USB-serial
// adapter drops and the kernel re-enumerates it under a different tty name:
// the process keeps a stable by-id path in the opener, and on a port-level
// write/read fault Exchange closes the stale handle, re-opens the by-id path
// (which now resolves to the fresh tty), and retries the exchange once. This
// recovers from the cable/hub glitches that would otherwise leave the bridge
// writing to a deleted device node ("write request: Input/output error") until
// a manual restart. opener may be nil for transports that never need to
// reopen (e.g. the in-process mock).
type Device struct {
	rw     byteReadWriteCloser
	opener func() (byteReadWriteCloser, error)
	mu     sync.Mutex
	seq    byte

	closed bool
}

func NewDevice(rw byteReadWriteCloser, opener func() (byteReadWriteCloser, error)) *Device {
	return &Device{rw: rw, opener: opener}
}

func (d *Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	if d.rw == nil {
		return nil
	}
	return d.rw.Close()
}

// reopen swaps the underlying port for a fresh handle. It closes the stale
// handle best-effort (it is likely already gone) and opens a new one via the
// opener captured at construction. On failure d.rw is left nil so the next
// Exchange attempts to reopen again. Must be called with d.mu held.
func (d *Device) reopen() error {
	if d.opener == nil {
		return ErrClosed
	}
	if d.rw != nil {
		_ = d.rw.Close()
	}
	rw, err := d.opener()
	if err != nil {
		d.rw = nil
		return err
	}
	d.rw = rw
	return nil
}

func (d *Device) Exchange(ctx context.Context, com byte, data []byte, timeout time.Duration) (protocol.Packet, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return protocol.Packet{}, ErrClosed
	}

	// A previous exchange may have left d.rw nil after a failed reopen (the
	// adapter was still gone). Try again before giving up — the poll loop
	// drives this every 2s, so the link recovers within a tick or two of the
	// adapter reappearing.
	if d.rw == nil {
		if err := d.reopen(); err != nil {
			return protocol.Packet{}, fmt.Errorf("reopen serial: %w", err)
		}
	}

	// retried bounds self-heal to one reopen per Exchange call. A second port
	// fault within the same exchange is returned to the caller and retried on
	// the next poll tick; this prevents a tight reopen loop against a
	// persistently broken link.
	retried := false
	for {
		seq := d.seq
		d.seq = (d.seq + 1) & 0x7F

		req := protocol.Packet{Seq: seq, Com: com, Data: data}
		if _, err := d.rw.Write(protocol.EncodePacket(req)); err != nil {
			if !retried && d.reopen() == nil {
				retried = true
				continue // re-send with a fresh sequence number
			}
			return protocol.Packet{}, fmt.Errorf("write request: %w", err)
		}

		deadline := time.Now().Add(timeout)
		for {
			if ctx.Err() != nil {
				return protocol.Packet{}, ctx.Err()
			}
			if time.Now().After(deadline) {
				return protocol.Packet{}, fmt.Errorf("timeout waiting for response")
			}

			pkt, err := readOnePacket(d.rw, deadline)
			if err == nil {
				if pkt.Seq != seq {
					continue
				}
				return pkt, nil
			}

			// A non-nil read error is a port-level fault, not a timeout:
			// the serial port is opened without a read timeout, so Read
			// blocks until data arrives or the link drops. Reopen and retry
			// the exchange once; if the link is still gone, surface the
			// error so the next poll tick retries rather than spinning.
			if !retried && d.reopen() == nil {
				retried = true
				break // restart the outer loop, re-send the request
			}
			if errors.Is(err, io.EOF) {
				return protocol.Packet{}, err
			}
			return protocol.Packet{}, fmt.Errorf("read response: %w", err)
		}
	}
}

func readOnePacket(r io.Reader, deadline time.Time) (protocol.Packet, error) {
	buf := make([]byte, 1)
	inPacket := false
	escaped := false
	decoded := make([]byte, 0, 64)

	for {
		if time.Now().After(deadline) {
			return protocol.Packet{}, fmt.Errorf("read timeout")
		}

		n, err := r.Read(buf)
		if err != nil {
			return protocol.Packet{}, err
		}
		if n == 0 {
			continue
		}
		b := buf[0]

		if b == protocol.STX {
			inPacket = true
			escaped = false
			decoded = decoded[:0]
			continue
		}
		if !inPacket {
			continue
		}
		if b == protocol.ETX {
			return protocol.DecodeDecodedPacket(decoded)
		}
		if escaped {
			decoded = append(decoded, b|0x80)
			escaped = false
			continue
		}
		if b == protocol.DLE {
			escaped = true
			continue
		}
		decoded = append(decoded, b)
	}
}
