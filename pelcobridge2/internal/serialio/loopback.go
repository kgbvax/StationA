package serialio

import (
	"errors"
	"sync"
	"time"
)

// Pipe is one end of an in-memory full-duplex transport pair. Write delivers
// to the peer's Read; Close unblocks a blocked Read with ErrClosed, mirroring
// what a serial port does when the adapter is pulled.
type Pipe struct {
	mu     sync.Mutex
	peer   *Pipe
	ch     chan []byte
	closed bool
	// latency emulates wire turnaround (2400 baud is ~30 ms per frame) so
	// tests exercise the same ordering windows the real link has.
	latency time.Duration
}

// ErrClosed is returned by Read/Write after Close, like a pulled serial adapter.
var ErrClosed = errors.New("transport closed")

// Pair returns two connected pipes: what one side writes, the other reads.
func Pair() (*Pipe, *Pipe) {
	a := &Pipe{ch: make(chan []byte, 64)}
	b := &Pipe{ch: make(chan []byte, 64)}
	a.peer, b.peer = b, a
	return a, b
}

// SetLatency sets the emulated wire turnaround delay for writes from this end.
func (p *Pipe) SetLatency(d time.Duration) {
	p.mu.Lock()
	p.latency = d
	p.mu.Unlock()
}

// Write delivers b to the peer's Read queue. The latency delay runs under the
// write lock: consecutive writes must arrive in order (a serial wire cannot
// reorder), so a delayed write blocks the next one from starting.
func (p *Pipe) Write(b []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if p.latency > 0 {
		time.Sleep(p.latency)
	}
	buf := make([]byte, len(b))
	copy(buf, b)
	p.peer.ch <- buf
	return nil
}

// Read blocks until data or Close. Closed → (0, ErrClosed).
func (p *Pipe) Read(out []byte) (int, error) {
	buf, ok := <-p.ch
	if !ok {
		return 0, ErrClosed
	}
	n := copy(out, buf)
	return n, nil
}

// Close unblocks a blocked Read (both sides' Close are independent, like two
// separate serial devices).
func (p *Pipe) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	close(p.ch)
	return nil
}
