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

type Device struct {
	rw  byteReadWriteCloser
	mu  sync.Mutex
	seq byte

	closed bool
}

func NewDevice(rw byteReadWriteCloser) *Device {
	return &Device{rw: rw}
}

func (d *Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	return d.rw.Close()
}

func (d *Device) Exchange(ctx context.Context, com byte, data []byte, timeout time.Duration) (protocol.Packet, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return protocol.Packet{}, ErrClosed
	}

	seq := d.seq
	d.seq = (d.seq + 1) & 0x7F

	req := protocol.Packet{Seq: seq, Com: com, Data: data}
	if _, err := d.rw.Write(protocol.EncodePacket(req)); err != nil {
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
		if err != nil {
			if errors.Is(err, io.EOF) {
				return protocol.Packet{}, err
			}
			continue
		}
		if pkt.Seq != seq {
			continue
		}
		return pkt, nil
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
