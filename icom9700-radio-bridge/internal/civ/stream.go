package civ

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"time"
)

// stream is one UDP leg of the session — the control stream (:50001) and the
// CI-V data stream (:50002) today, with room for a future audio sidecar to
// open a third (KTD-9). It carries the per-stream state the protocol tracks
// independently per leg: the 4-byte IDs derived from the local endpoint, the
// tracked-send window the radio can request retransmits from, the receive
// window that orders, deduplicates and retransmission-requests inbound
// tracked packets, and the keepalive/ping deadlines the maintenance loop
// services.
//
// Every buffer here is bounded: the tx window by txWindowCap (wfview's
// BUFSIZE 500 posture), the rx pending/missing/skip sets by the
// flush-if->maxMissing rule (wfview MAX_MISSING 50) — the MemoryMax lesson
// (plan R18: no radio-silence-growable structure).
type stream struct {
	name      string
	conn      *net.UDPConn
	radioAddr *net.UDPAddr
	tr        *Transport

	mu        sync.Mutex
	myID      uint32
	remoteID  uint32
	sendSeq   uint16 // next tracked header sequence
	subSeq    uint16 // BE sub-header send sequence (civ data + openclose)
	pingSeq   uint16
	authSeq   uint16 // BE inner sequence for auth-family packets (control)
	tokReq    uint16 // token request nonce the radio echoes
	token     uint32 // session token from the login response
	nextIdle  time.Time
	nextPing  time.Time
	nextRetx  time.Time
	lastRx    time.Time
	lastCivRx time.Time // last CI-V DATA packet (watchdog key, civ stream)
	openDue   time.Time // next start-data (re-)send, zero = disarmed

	tx txWindow
	rx rxWindow
}

// Window bounds. txWindowCap follows wfview's BUFSIZE (500, rounded up);
// maxMissingCap is wfview's MAX_MISSING — more simultaneous holes than this
// flushes the receive window and resyncs (brief: "flush if >50 missing").
const (
	txWindowCap   = 512
	maxMissingCap = 50
)

// txWindow remembers recently sent tracked packets so a retransmit request
// from the radio can be answered. Drop-oldest at the cap.
type txWindow struct {
	entries map[uint16][]byte
	order   []uint16 // FIFO of stored seqs for drop-oldest
}

func (w *txWindow) add(seq uint16, data []byte) {
	if w.entries == nil {
		w.entries = make(map[uint16][]byte, 64)
	}
	if len(w.order) >= txWindowCap {
		delete(w.entries, w.order[0])
		w.order = w.order[1:]
	}
	w.entries[seq] = data
	w.order = append(w.order, seq)
}

func (w *txWindow) get(seq uint16) ([]byte, bool) {
	d, ok := w.entries[seq]
	return d, ok
}

func (w *txWindow) size() int { return len(w.order) }

func (w *txWindow) clear() {
	w.entries = nil
	w.order = nil
}

// rxWindow reassembles the inbound tracked-packet sequence: payloads are
// delivered strictly in sequence order, duplicates are dropped (the radio
// may double-send retransmit answers), gaps raise retransmit requests via
// the missing set, holes are given up after the configured tries (the skip
// set lets the sequence advance past a permanently lost packet), and a jump
// beyond maxMissing flushes everything and resyncs (wfview MAX_MISSING).
type rxWindow struct {
	started bool
	next    uint16
	pending map[uint16][]byte
	missing map[uint16]int // seq -> retransmit requests issued
	skip    map[uint16]bool
	recent  []uint16 // ring of recently delivered seqs (stale-dup filter)
	recentI int
}

// seqDiff is a-b in u16 sequence space (0..65535).
func seqDiff(a, b uint16) int {
	d := int(a) - int(b)
	if d < 0 {
		d += 65536
	}
	return d
}

// offer feeds one tracked data payload into the window and returns the
// payloads deliverable now (in order). Caller holds the stream mutex.
func (w *rxWindow) offer(seq uint16, payload []byte, maxMissing int) [][]byte {
	if w.containsRecent(seq) {
		return nil // stale duplicate of an already-delivered packet
	}
	if !w.started {
		w.started = true
		w.next = seq
	}
	switch d := seqDiff(seq, w.next); {
	case d == 0:
		w.noteRecent(seq)
		w.next = seq + 1
		return append([][]byte{payload}, w.drain()...)
	case d <= maxMissing:
		if w.pending == nil {
			w.pending = make(map[uint16][]byte)
		}
		if _, dup := w.pending[seq]; dup {
			return nil // duplicate of an out-of-order packet we still hold
		}
		w.pending[seq] = payload
		for s := w.next; s != seq; s++ {
			if w.missing == nil {
				w.missing = make(map[uint16]int)
			}
			if _, ok := w.missing[s]; !ok {
				w.missing[s] = 0
			}
		}
		// Hard bound the pending set: drop the oldest out-of-order payload
		// if a pathological flood ever exceeds the flush threshold without
		// triggering the resync path.
		for len(w.pending) > maxMissing {
			var lowest uint16
			first := true
			for s := range w.pending {
				if first || s < lowest {
					lowest, first = s, false
				}
			}
			delete(w.pending, lowest)
			delete(w.missing, lowest)
		}
		return nil
	default:
		// Far ahead (or behind — recent already filtered stale dups): flush
		// and resync on the new sequence (wfview's "large gap" clear).
		w.flushLocked()
		w.started = true
		w.next = seq + 1
		w.noteRecent(seq)
		return [][]byte{payload}
	}
}

// drain releases consecutive payloads waiting behind an advanced next.
func (w *rxWindow) drain() [][]byte {
	var out [][]byte
	for {
		if p, ok := w.pending[w.next]; ok {
			delete(w.pending, w.next)
			delete(w.missing, w.next)
			delete(w.skip, w.next)
			w.noteRecent(w.next)
			out = append(out, p)
			w.next++
			continue
		}
		if w.skip[w.next] {
			delete(w.skip, w.next)
			delete(w.missing, w.next)
			w.next++
			continue
		}
		return out
	}
}

// retxTick is the 100 ms retransmit-request pass (brief: "100 ms timer, give
// up after 4 tries"): every still-missing seq under the try bound joins a
// request; exhausted holes are skipped past and the stream advances. Returns
// the payloads now deliverable and the request datagrams to send (a single
// 16-byte request for one hole, a bulk range packet otherwise). A hole count
// beyond the flush bound clears the window instead — the peer resyncs us.
func (w *rxWindow) retxTick(tries, maxMissing int, sentID, rcvdID uint32) (deliver [][]byte, reqs [][]byte) {
	if len(w.missing) > maxMissing {
		w.flushLocked()
		return nil, nil
	}
	var seqs []uint16
	for s := range w.missing {
		seqs = append(seqs, s)
	}
	sortSeqs(seqs)
	var flat []uint16
	for _, s := range seqs {
		if w.missing[s] < tries {
			w.missing[s]++
			flat = append(flat, s)
		} else {
			// Give up on this hole: mark it skipped so the sequence (and the
			// buffered payloads behind it) can advance.
			delete(w.missing, s)
			if w.skip == nil {
				w.skip = make(map[uint16]bool)
			}
			w.skip[s] = true
			for len(w.skip) > maxMissing {
				for sk := range w.skip {
					delete(w.skip, sk)
					break
				}
			}
		}
	}
	switch len(flat) {
	case 0:
	case 1:
		reqs = append(reqs, controlPacket(ptRetransmit, flat[0], sentID, rcvdID))
	default:
		reqs = append(reqs, bulkRetransmitPacket(flat, sentID, rcvdID))
	}
	return w.drain(), reqs
}

// pendingCount/missingCount/skipCount size probes (Stats, bound tests).
func (w *rxWindow) pendingCount() int { return len(w.pending) }
func (w *rxWindow) missingCount() int { return len(w.missing) }
func (w *rxWindow) skipCount() int    { return len(w.skip) }

func (w *rxWindow) flushLocked() {
	w.pending = nil
	w.missing = nil
	w.skip = nil
	w.started = false
}

func (w *rxWindow) noteRecent(seq uint16) {
	if w.recent == nil {
		w.recent = make([]uint16, 256)
	}
	w.recent[w.recentI%len(w.recent)] = seq
	w.recentI++
}

func (w *rxWindow) containsRecent(seq uint16) bool {
	if w.recent == nil {
		return false
	}
	for _, s := range w.recent {
		if s == seq {
			return true
		}
	}
	return false
}

// sortSeqs orders sequence numbers in u16 sequence space (wrap-aware) — the
// sets are bounded by maxMissingCap, so insertion sort is plenty.
func sortSeqs(s []uint16) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && seqDiff(s[j], s[j-1]) < 32768; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// --- stream construction and helpers -------------------------------------------------

// newStream binds a UDP socket on an OS-chosen port and derives myID from
// the local endpoint the way wfview does: two octets of the client IP plus
// the local port (brief: "sentid derived from client IP+port").
func newStream(tr *Transport, name string, radio *net.UDPAddr) (*stream, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, err
	}
	_ = conn.SetReadBuffer(256 * 1024)
	_ = conn.SetWriteBuffer(64 * 1024)
	laddr := conn.LocalAddr().(*net.UDPAddr)
	ip4 := laddr.IP.To4()
	var myID uint32
	if ip4 != nil {
		myID = uint32(ip4[2])<<24 | uint32(ip4[3])<<16 | uint32(laddr.Port)&0xffff
	} else {
		myID = uint32(laddr.Port)&0xffff | 0x0a000000
	}
	s := &stream{
		name:      name,
		conn:      conn,
		radioAddr: radio,
		tr:        tr,
		myID:      myID,
		lastRx:    time.Now(),
	}
	return s, nil
}

func (s *stream) localPort() int {
	return s.conn.LocalAddr().(*net.UDPAddr).Port
}

// nextSubSeq advances and returns the big-endian sub-header send sequence
// (shared by CI-V data packets and openclose packets, wfview's sendSeqB).
// Caller holds s.mu.
func (s *stream) nextSubSeq() uint16 {
	s.subSeq++
	return s.subSeq
}

// connect points the stream at the radio's datagram address (the CI-V data
// socket only learns it from the status packet; stdlib cannot connect() an
// existing bound socket, so sends go out addressed, not piped).
func (s *stream) connect(radio *net.UDPAddr) {
	s.radioAddr = radio
}

// sendRaw writes one datagram to the radio. Caller holds s.mu (sends
// serialize per stream, which is what keeps concurrent CI-V commands
// intact).
func (s *stream) sendRaw(b []byte) error {
	if s.radioAddr == nil {
		return errors.New("civ: stream has no radio address yet")
	}
	_, err := s.conn.WriteToUDP(b, s.radioAddr)
	return err
}

// sendUntracked sends a packet without sequence tracking (handshake
// controls, disconnects, ping answers).
func (s *stream) sendUntracked(b []byte) error {
	return s.sendRaw(b)
}

// sendTracked stamps the next tracked sequence, remembers the packet for
// retransmit answers and resets the idle-keepalive deadline (a real packet
// counts as a keepalive — wfview restarts the idle timer on every tracked
// send).
func (s *stream) sendTracked(b []byte, now time.Time) error {
	b[seqOff] = byte(s.sendSeq)
	b[seqOff+1] = byte(s.sendSeq >> 8)
	s.tx.add(s.sendSeq, b)
	s.sendSeq++
	s.nextIdle = now.Add(s.tr.o.IdlePeriod)
	return s.sendRaw(b)
}

// sendIdle sends the tracked idle keepalive (16-byte type 0x00) — the
// 100 ms "keepalive/idle packet" of the brief.
func (s *stream) sendIdle(now time.Time) error {
	return s.sendTracked(controlPacket(ptIdle, 0, s.myID, s.remoteID), now)
}

// sendPing sends the 500 ms ping (21-byte, type 0x07; the time field carries
// milliseconds since start of day, wfview-style).
func (s *stream) sendPing(now time.Time) error {
	s.pingSeq++
	ms := uint32(now.Sub(now.Truncate(24*time.Hour)) / time.Millisecond)
	return s.sendUntracked(pingPacket(0, s.pingSeq, ms, s.myID, s.remoteID))
}

// civOut wraps one CI-V payload into a tracked data packet with the next
// big-endian sub-header sequence and sends it.
func (s *stream) civOut(payload []byte, now time.Time) error {
	s.subSeq++
	return s.sendTracked(civDataPacket(payload, s.subSeq, s.myID, s.remoteID), now)
}

// answerRetransmit replies to a radio retransmit request: packets still in
// the tx window are re-sent (twice, kappanhang's doubling), unknown ones get
// a 16-byte idle carrying the requested sequence so the radio's own
// retransmit loop can advance.
func (s *stream) answerRetransmit(seqs []uint16) {
	for _, seq := range seqs {
		if d, ok := s.tx.get(seq); ok {
			_ = s.sendRaw(d)
			_ = s.sendRaw(d)
			continue
		}
		_ = s.sendUntracked(controlPacket(ptIdle, seq, s.myID, s.remoteID))
	}
}

// retxRequestSeqs extracts the requested sequences from a retransmit-request
// datagram: a 16-byte packet carries one seq in its header; longer ones
// carry (start,end) u16 LE range pairs at 0x10 (capped for sanity).
func retxRequestSeqs(d []byte, h header) []uint16 {
	if h.len == ctrlLen {
		return []uint16{h.seq}
	}
	var out []uint16
	for off := headerLen; off+4 <= len(d); off += 4 {
		start := binary.LittleEndian.Uint16(d[off:])
		end := binary.LittleEndian.Uint16(d[off+2:])
		for s := start; ; s++ {
			out = append(out, s)
			if s == end || len(out) >= 4*maxMissingCap {
				break
			}
		}
	}
	return out
}
