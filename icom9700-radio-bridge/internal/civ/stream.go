package civ

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// udpStream is one protocol stream: a UDP socket bound locally, its reader
// goroutine, and the tracked-send machinery shared by every stream. The
// control stream carries the handshake and auth; the CI-V data stream
// carries the framed CI-V bytes. (kappanhang streamCommon/pkt0, with the
// globals made per-client state.)
type udpStream struct {
	cli  *Client
	name string
	conn *net.UDPConn

	localSID  uint32
	remoteSID uint32
	gotRemote bool

	// outbound sequence (outer [6..7]) and data inner sequence ([19..20]).
	sendSeq  uint16
	innerSeq uint16

	rx *rxSeqBuf // nil on the control stream
	tx *txSeqBuf

	readCh chan []byte
}

// dialStream binds the stream's UDP socket. A localPort of 0 (the default
// everywhere, tests and production) binds an ephemeral port — the radio
// accepts any source port (wfview uses ephemeral ports too; only kappanhang
// pins the same-numbered local port, and nothing in the protocol requires
// it).
func (c *Client) dialStream(name string, radioPort, localPort int) (*udpStream, error) {
	raddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", c.opts.Host, radioPort))
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", &net.UDPAddr{Port: localPort}, raddr)
	if err != nil {
		return nil, fmt.Errorf("dial %s udp %d->%d: %w", name, localPort, radioPort, err)
	}
	s := &udpStream{
		cli:    c,
		name:   name,
		conn:   conn,
		readCh: make(chan []byte, readQueueLen),
		tx:     newTxSeqBuf(c.opts.TxRetention),
	}
	// Local session ID: the socket's IPv4 bytes shifted left 16, OR the
	// local port (kappanhang streamCommon.init — derived, not random).
	laddr := conn.LocalAddr().(*net.UDPAddr)
	s.localSID = binary.BigEndian.Uint32(laddr.IP[len(laddr.IP)-4:])<<16 | uint32(laddr.Port&0xffff)
	return s, nil
}

// startReader launches the stream's receive loop.
func (s *udpStream) startReader() {
	go func() {
		buf := make([]byte, 2048)
		for {
			n, err := s.conn.Read(buf)
			if err != nil {
				// Socket closed by Close(): a clean shutdown, not a loss.
				select {
				case <-s.cli.done:
					return
				default:
				}
				s.cli.lose(fmt.Errorf("%s stream read: %w", s.name, err))
				return
			}
			// Any control-stream datagram proves the radio link alive.
			s.cli.rxSeen(s.name)
			pkt := make([]byte, n)
			copy(pkt, buf[:n])

			if isPing(pkt) {
				s.cli.handlePing(s, pkt)
				continue
			}
			if prefixEqual(pkt, sigRetransmitSingle) || prefixEqual(pkt, sigRetransmitRange) {
				s.handleRetransReq(pkt)
				continue
			}
			if s.name == "control" {
				// Control idles are liveness only; the rxSeen stamp above
				// already recorded them.
				if isIdle(pkt) {
					continue
				}
				if s.cli.handleControlPacket(pkt) {
					continue
				}
			}
			// Everything else (civ data + idles, control handshake/stray
			// answers) goes to the stream consumer; a full queue drops the
			// NEWEST packet, which the rx reorder buffer heals by
			// requesting retransmission of the resulting gap.
			select {
			case s.readCh <- pkt:
			default:
				s.cli.log.Warn("civ read queue full, dropping packet", "stream", s.name, "seq", dataSeq(pkt))
			}
		}
	}()
}

// handleRetransReq serves the radio's retransmit requests from the tx
// buffer. (kappanhang pkt0Type.handle — each requested packet is resent
// twice, and unknown seqs are answered with two untracked idles bearing the
// requested seq so the radio's own gap detection can close.)
func (s *udpStream) handleRetransReq(r []byte) {
	switch {
	case prefixEqual(r, sigRetransmitSingle):
		s.retransmitOne(binary.LittleEndian.Uint16(r[6:8]))
	case prefixEqual(r, sigRetransmitRange):
		body := r[headerLen:]
		for len(body) >= 4 {
			start := binary.LittleEndian.Uint16(body[0:2])
			end := binary.LittleEndian.Uint16(body[2:4])
			for {
				s.retransmitOne(start)
				if start == end {
					break
				}
				start++
			}
			body = body[4:]
		}
	}
}

func (s *udpStream) retransmitOne(seq uint16) {
	if d := s.tx.get(seq); d != nil {
		_ = s.sendRaw(d)
		_ = s.sendRaw(d)
		return
	}
	idle := header(sigIdle, s.localSID, s.remoteSID)
	binary.LittleEndian.PutUint16(idle[6:8], seq)
	_ = s.sendRaw(idle)
	_ = s.sendRaw(idle)
}

// send writes a datagram (untracked).
func (s *udpStream) send(p []byte) error {
	if _, err := s.conn.Write(p); err != nil {
		s.cli.lose(fmt.Errorf("%s stream write: %w", s.name, err))
		return err
	}
	return nil
}

func (s *udpStream) sendRaw(p []byte) error { return s.send(p) }

// sendTracked stamps the stream sequence, retains the packet for possible
// retransmit, and sends it (kappanhang sendTrackedPacket). The CLIENT's
// wmu must be held: sequence bookkeeping is serialized there.
func (s *udpStream) sendTracked(p []byte) error {
	binary.LittleEndian.PutUint16(p[6:8], s.sendSeq)
	s.tx.add(s.sendSeq, p)
	if err := s.send(p); err != nil {
		return err
	}
	s.sendSeq++
	return nil
}

// sendTrackedData stamps the inner data sequence and sends a CI-V payload
// as a tracked data packet.
func (s *udpStream) sendTrackedData(payload []byte) error {
	p := buildData(s.localSID, s.remoteSID, s.innerSeq, payload)
	s.innerSeq++
	return s.sendTracked(p)
}

// --- handshake one-shots (kappanhang streamCommon) ------------------------------

// start runs the pkt3/pkt4/pkt6 exchange that establishes sequence tracking
// and the remote SID. are-you-there is retried at the brief's 500 ms cadence
// until the radio answers or the context ends.
func (s *udpStream) start(ctx context.Context) error {
	p3 := header(sigAreYouThere, s.localSID, s.remoteSID)
	for {
		_ = s.send(p3)
		_ = s.send(p3)
		if r, ok := s.cli.expect(s, ctx, s.cli.opts.AreYouThere, sigIAmHere); ok {
			s.remoteSID = binary.BigEndian.Uint32(r[8:12])
			s.gotRemote = true
			break
		}
		if ctx.Err() != nil {
			return ErrHandshakeTimeout
		}
	}
	p6 := header(sigReady, s.localSID, s.remoteSID)
	_ = s.send(p6)
	_ = s.send(p6)
	if _, ok := s.cli.expect(s, ctx, s.cli.opts.HandshakeTO, sigReady); !ok {
		return ErrHandshakeTimeout
	}
	return nil
}

// disconnect sends the one-shot disconnect (type 0x05) twice.
func (s *udpStream) disconnect() {
	if !s.gotRemote {
		return
	}
	p := header(sigDisconnect, s.localSID, s.remoteSID)
	_ = s.send(p)
	_ = s.send(p)
}

// requestRetransmit asks the radio to resend a missing range: single-packet
// form for one seq, the range form otherwise (kappanhang requestRetransmit;
// ranges larger than the request budget are refused — the rx buffer's
// lock-timeout flush covers them instead).
func (s *udpStream) requestRetransmit(r seqRange) error {
	diff := int(r[1]) - int(r[0])
	if r[1] < r[0] {
		diff += 0x10000
	}
	if diff > maxRetransmitRequestPackets {
		return fmt.Errorf("retransmit range too large (%d)", diff)
	}
	var p []byte
	if diff == 0 {
		p = header(sigRetransmitSingle, s.localSID, s.remoteSID)
		binary.LittleEndian.PutUint16(p[6:8], r[0])
	} else {
		p = header(sigRetransmitRange, s.localSID, s.remoteSID)
		var body [4]byte
		binary.LittleEndian.PutUint16(body[0:2], r[0])
		binary.LittleEndian.PutUint16(body[2:4], r[1])
		p = append(p, body[:]...)
	}
	_ = s.send(p)
	_ = s.send(p)
	return nil
}

// expect scans the stream's read queue for a datagram of the exact length
// with the given prefix, for at most wait (kappanhang expect with the
// timeout made explicit). Non-matching datagrams are dropped — only the
// handshake consumes this path.
func (c *Client) expect(s *udpStream, ctx context.Context, wait time.Duration, prefix []byte) ([]byte, bool) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case r := <-s.readCh:
			if len(r) >= len(prefix) && bytes.Equal(r[:len(prefix)], prefix) {
				return r, true
			}
		case <-timer.C:
			return nil, false
		case <-ctx.Done():
			return nil, false
		}
	}
}
