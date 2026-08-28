package pelco

// The assembler reassembles a raw byte stream into frames. It syncs on the
// start byte (0xFF → 7-byte Pelco-D, 0xA0 → 8-byte Pelco-P), validates the
// matching checksum (additive sum vs XOR), and on a bad checksum drops the
// leading byte and resumes scanning. Both protocols are always accepted on RX —
// the head answers in the protocol the frame arrived in.
//
// Two invariants are load-bearing, both paid for on the bench with ptest:
//
//  1. Noise runs are emitted BEFORE the frame that follows them, in wire order.
//     Returning frames and noise separately logged noise-first and made the
//     transcript contradict the wire for exactly the corrupted traffic being
//     investigated.
//
//  2. An incomplete frame is held only until the next receive gap (FlushIdle).
//     Without that bound a truncated reply stayed at the head of the buffer and
//     merged with the next reply, whose 0xFF start byte landed where the lost
//     checksum byte had been — producing a checksum-VALID frame carrying a
//     fabricated position word while the genuine reply was walked off byte by
//     byte.
type Assembler struct {
	buf []byte
	bad []byte // bytes consumed while resyncing, reported as one noise run
}

// Noise is a run of bytes that could not be assembled into a valid frame.
type Noise []byte

// Event is one thing that came off the wire, in wire order.
type Event struct {
	Frame   RxFrame
	Noise   Noise // non-nil ⇒ this event is an unframeable run, Frame unset
	Partial bool  // a stalled incomplete frame flushed after a receive gap
}

// IsNoise reports whether the event is an unframeable run rather than a frame.
func (e Event) IsNoise() bool { return e.Noise != nil }

// Hex renders the noise run as space-separated upper-case hex.
func (n Noise) Hex() string { return hexSpaced(n) }

// badCap chunks a very long resync run inside a single Feed so one huge read of
// garbage (a wrong-baud link) is reported in readable pieces.
const badCap = 256

// Feed consumes new bytes and returns the frames and noise runs it can, in
// wire order.
func (a *Assembler) Feed(data []byte) []Event {
	a.buf = append(a.buf, data...)
	var ev []Event
	for len(a.buf) > 0 {
		isP := a.buf[0] == STX
		need := FrameLen
		if isP {
			need = FrameLenP
		} else if a.buf[0] != 0xFF {
			a.drop()
			ev = a.capBad(ev)
			continue
		}
		if len(a.buf) < need {
			break // wait for the rest of the frame (or for FlushIdle)
		}
		wire := make([]byte, need)
		copy(wire, a.buf[:need])
		var f Frame
		ok := false
		if isP {
			ok = PChkOK(wire)
			f = Frame{wire[0], wire[1], wire[2], wire[3], wire[4], wire[5], wire[7]}
		} else {
			copy(f[:], wire)
			ok = f.ChkOK()
		}
		if !ok {
			a.drop()
			ev = a.capBad(ev)
			continue
		}
		ev = a.flushBad(ev) // noise precedes the frame that follows it
		ev = append(ev, Event{Frame: RxFrame{Frame: f, P: isP, Wire: wire}})
		a.buf = a.buf[need:]
	}
	// Bytes already definitively rejected are never withheld: buffering them
	// made a single corrupted reply produce no output at all, which read as
	// "no answer".
	return a.flushBad(ev)
}

// FlushIdle reports an incomplete frame that has been sitting in the assembler
// with no further traffic. Call it after a receive gap (~1.5 frame times at
// the configured baud) so a truncated reply is surfaced as a partial instead
// of merging with the next one.
func (a *Assembler) FlushIdle() []Event {
	ev := a.flushBad(nil)
	if len(a.buf) > 0 {
		ev = append(ev, Event{Noise: Noise(append([]byte(nil), a.buf...)), Partial: true})
		a.buf = nil
	}
	return ev
}

// Pending reports whether the assembler is holding anything that a receive
// gap should flush.
func (a *Assembler) Pending() bool { return len(a.buf) > 0 || len(a.bad) > 0 }

func (a *Assembler) drop() {
	a.bad = append(a.bad, a.buf[0])
	a.buf = a.buf[1:]
}

// flushBad emits the accumulated resync bytes as one noise run, if any.
func (a *Assembler) flushBad(ev []Event) []Event {
	if len(a.bad) == 0 {
		return ev
	}
	ev = append(ev, Event{Noise: Noise(a.bad)})
	a.bad = nil
	return ev
}

// capBad chunks a resync run that grows past badCap within a single Feed.
func (a *Assembler) capBad(ev []Event) []Event {
	if len(a.bad) < badCap {
		return ev
	}
	return a.flushBad(ev)
}
