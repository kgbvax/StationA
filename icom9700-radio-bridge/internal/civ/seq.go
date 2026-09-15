package civ

import (
	"errors"
	"sync"
	"time"
)

// Sequence numbers are 16-bit with natural wrap (the protocol's streams run
// 0..0xffff); all comparisons are half-range.
type seqRange [2]uint16

func seqCompare(seq, to uint16) seqCmp {
	d1 := seq - to // forward distance from to to seq (wraps)
	d2 := to - seq
	switch {
	case d1 == d2:
		return cmpEqual
	case d1 > d2:
		return cmpSmaller
	default:
		return cmpLarger
	}
}

type seqCmp int

const (
	cmpLarger seqCmp = iota
	cmpSmaller
	cmpEqual
)

// rxSeqBuf reorders incoming tracked packets and requests retransmission of
// gaps (a Go port of kappanhang's seqbuf.go, maxSeqNum fixed at 0xffff and
// the RTT feedback reduced to a fixed multiple of the buffer length).
//
// In-order entries are delivered to entries; on a gap the buffer locks for
// one buffer length (during which the retransmit request is outstanding),
// then flushes: if the retransmit range arrived in full, delivery resumes
// in order; otherwise entries beyond the requested range end are delivered
// and the missing packets are skipped — CI-V state keeps flowing on a lossy
// link instead of freezing (the plan's telemetry-freeze prohibition).
type rxSeqBuf struct {
	length      time.Duration
	requestRetx func(r seqRange) error
	// ready wakes the delivery pump when new data arrived (buffered 1 —
	// coalesces bursts).
	ready chan struct{}

	mu      sync.Mutex
	entries []rxEntry // front = newest, back = oldest
	// delivery state
	returnedFirst  bool
	lastReturned   uint16
	locked         bool
	lockedAt       time.Time
	requested      bool
	requestedRange seqRange
	ignoreUntilSet bool
	ignoreUntil    uint16
	// bounded-growth guard: a wedged consumer must not grow entries forever.
	maxEntries int
}

type rxEntry struct {
	seq  uint16
	data []byte
}

var errRxEmpty = errors.New("civ: rx buffer empty")
var errRxOutOfOrder = errors.New("civ: rx out of order")

func newRxSeqBuf(length time.Duration, maxEntries int, requestRetx func(seqRange) error) *rxSeqBuf {
	if maxEntries <= 0 {
		maxEntries = 512
	}
	return &rxSeqBuf{
		length:      length,
		maxEntries:  maxEntries,
		requestRetx: requestRetx,
		ready:       make(chan struct{}, 1),
	}
}

// signal wakes a parked pump.
func (b *rxSeqBuf) signal() {
	select {
	case b.ready <- struct{}{}:
	default:
	}
}

// add inserts a received packet. Duplicate and (half-range) too-old packets
// are dropped.
func (b *rxSeqBuf) add(seq uint16, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	e := rxEntry{seq: seq, data: data}
	if len(b.entries) == 0 {
		b.entries = append(b.entries, e)
		return
	}
	if b.entries[0].seq == seq {
		return // duplicate of newest
	}
	if seqCompare(seq, b.entries[0].seq) == cmpLarger {
		b.entries = append([]rxEntry{e}, b.entries...)
		b.trim()
		return
	}
	for i := 1; i < len(b.entries); i++ {
		if b.entries[i].seq == seq {
			return // duplicate
		}
		if seqCompare(seq, b.entries[i].seq) == cmpLarger {
			b.entries = append(b.entries[:i], append([]rxEntry{e}, b.entries[i:]...)...)
			return
		}
	}
	// No place found scanning from the front: it belongs at the back
	// (kappanhang addToBack) — a retransmit for an old gap lands here.
	b.entries = append(b.entries, e)
	b.signal()
}

// trim bounds growth while delivery is wedged: beyond maxEntries the OLDEST
// entries (the ones a retransmit could still recover) are dropped first —
// memory stays bounded (MemoryMax lesson) at the cost of skipping very old
// frames, which the bridge's poll/refresh model tolerates.
func (b *rxSeqBuf) trim() {
	for len(b.entries) > b.maxEntries {
		b.entries = b.entries[:len(b.entries)-1]
	}
}

// next pops the next in-order entry. It returns errRxOutOfOrder when an
// out-of-order entry was dropped (caller retries), and a retry delay
// (> 0) when delivery is locked waiting for a retransmit.
func (b *rxSeqBuf) next() (rxEntry, time.Duration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.entries) == 0 {
		return rxEntry{}, 0, errRxEmpty
	}
	last := len(b.entries) - 1
	e := b.entries[last]

	if b.returnedFirst {
		if b.locked {
			if b.requested && b.gotRange() {
				b.locked = false // retransmit range arrived in full
			} else if !b.lockTimedOut() {
				return rxEntry{}, b.length, nil
			}
			// timed out: fall through and flush the entry (checkLockTimeout)
		} else {
			if seqCompare(e.seq, b.lastReturned) != cmpLarger {
				b.entries = b.entries[:last]
				return rxEntry{}, 0, errRxOutOfOrder
			}
			if b.ignoreUntilSet && seqCompare(e.seq, b.ignoreUntil) != cmpLarger {
				// Inside a skipped gap — deliver past it.
			} else if want := b.lastReturned + 1; e.seq != want {
				// Gap: lock and request the missing range.
				b.locked = true
				b.lockedAt = time.Now()
				b.requested = false
				b.ignoreUntilSet = false
				b.requestedRange = seqRange{want, e.seq - 1}
				if b.requestRetx != nil {
					if b.requestRetx(b.requestedRange) == nil {
						b.requested = true
					}
				}
				return rxEntry{}, b.length, nil
			}
		}
	}

	b.lastReturned = e.seq
	b.returnedFirst = true
	b.entries = b.entries[:last]
	return e, 0, nil
}

// lockTimedOut mirrors checkLockTimeout: the lock lasts the buffer length
// (at least two lengths in effect — kappanhang doubles via RTT feedback);
// on expiry the lock clears and, if a retransmit was requested, entries up
// to the requested range end are skipped when they arrive.
func (b *rxSeqBuf) lockTimedOut() bool {
	if time.Since(b.lockedAt) < b.length {
		return false
	}
	b.locked = false
	if b.requested {
		b.ignoreUntilSet = true
		b.ignoreUntil = b.requestedRange[1]
	}
	return true
}

// gotRange reports whether every seq of the requested retransmit range has
// arrived.
func (b *rxSeqBuf) gotRange() bool {
	i := len(b.entries)
	s := b.requestedRange[0]
	for {
		i--
		if i < 0 {
			return false
		}
		if b.entries[i].seq != s {
			return false
		}
		if s == b.requestedRange[1] {
			return true
		}
		s++
	}
}

// txSeqBuf retains recently sent tracked packets so radio retransmit
// requests can be served (kappanhang txseqbuf.go; retention is ten times
// the buffer length announced to the radio).
type txSeqBuf struct {
	retention time.Duration

	mu      sync.Mutex
	entries []txEntry
}

type txEntry struct {
	seq  uint16
	data []byte
	at   time.Time
}

func newTxSeqBuf(retention time.Duration) *txSeqBuf {
	return &txSeqBuf{retention: retention}
}

func (b *txSeqBuf) add(seq uint16, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = append(b.entries, txEntry{seq: seq, data: data, at: time.Now()})
	b.purge()
}

func (b *txSeqBuf) get(seq uint16) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.entries) - 1; i >= 0; i-- {
		if b.entries[i].seq == seq {
			return b.entries[i].data
		}
	}
	b.purge()
	return nil
}

func (b *txSeqBuf) purge() {
	cut := 0
	for cut < len(b.entries) && time.Since(b.entries[cut].at) > b.retention {
		cut++
	}
	if cut > 0 {
		b.entries = append([]txEntry(nil), b.entries[cut:]...)
	}
}
