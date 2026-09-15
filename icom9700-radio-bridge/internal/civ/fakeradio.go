package civ

// The fake radio: an in-process UDP server speaking the same packet grammar
// as the IC-9700's RS-BA1 LAN interface (see docs/civ-research-brief.md),
// scripted per test. It is the only radio the tests meet — no network
// beyond loopback, no hardware.
//
// The fake mirrors the wire grammar the transport implements (wfview
// icomudpbase/icomudphandler/icomudpcivdata shapes): 16-byte header packets
// on a control socket and a CI-V data socket, handshake responses, tracked
// data packets with a tx window for answering retransmit requests, and ping
// request/reply. Scripting knobs (silence, drops, error injection, frame
// injection) plus receive logs let the tests assert exact packet sequences,
// loss recovery and bounded buffers.
//
// This file is regular (non-test) source so the session manager's and the
// bridge slot's tests (internal/radio, internal/bridge) can drive the fake
// over real loopback UDP through the small exported surface below — the
// spid-ercm internal/spid mock.go precedent. It imports nothing test-only;
// production builds carry the dead code (bounded, self-contained).
// Package-internal tests keep using the unexported methods and fields
// directly.

import (
	"encoding/binary"
	"net"
	"sync"
	"time"
)

// fakeRadioID is the 4-byte ID the fake radio presents as sentid until a
// Reboot rotates it (the real radio picks a random one per boot; the value
// itself is irrelevant to the client).
const fakeRadioID = uint32(0x11223344)

// fakeToken is the session token the fake radio hands out on login.
const fakeToken = uint32(0x21436587)

// sock names for the two UDP legs.
const (
	sockCtrl = "ctrl"
	sockCiv  = "civ"
)

// recPkt is one datagram the fake radio received or sent, with its socket
// and arrival time.
type recPkt struct {
	sock string
	data []byte
	at   time.Time
}

// FakeRadio is the scripted in-process IC-9700.
type FakeRadio struct {
	ctrlConn *net.UDPConn
	civConn  *net.UDPConn

	mu           sync.Mutex
	radioID      uint32          // sentid the fake presents (Reboot rotates)
	silent       bool            // never answer are-you-there
	noReady      bool            // answer I-am-here but never I-am-ready
	stopped      bool            // answer nothing at all (keepalive silence)
	dropIn       map[string]int  // remaining client datagrams to drop per sock
	dropInData   map[string]int  // remaining client CI-V data datagrams to drop per sock
	dropOut      map[string]int  // remaining radio datagrams to drop per sock
	lostCiv      map[uint16]bool // radio "lost" these own packets permanently
	loginErr     uint32
	statusErr    uint32
	autoCivEvery time.Duration // 0 = no automatic CI-V data

	sent      []recPkt
	recv      []recPkt
	renew     int // 0x40 requesttype 0x05 received (renewals)
	logout    int // 0x40 requesttype 0x01 received (token removal)
	opens     int // 0x16 magic 0x04 received
	closes    int // 0x16 magic 0x00 received
	closeOnce sync.Once

	ctrlAddr   *net.UDPAddr // client's control source address
	civAddr    *net.UDPAddr // client's civ source address
	ctrlSeq    uint16       // radio's tracked seq on the control stream
	civSeq     uint16       // radio's tracked seq on the civ stream
	civSubSeq  uint16       // radio's sub-header sendseq on the civ stream
	ctrlTx     map[uint16][]byte
	civTx      map[uint16][]byte
	gotFrames  [][]byte // CI-V payloads that reached the radio (deduped)
	gotSubSeqs []uint16 // sub-header sendseqs of received client data packets

	// Client-sequence tracking on the civ stream, so the fake can request
	// retransmission of lost client packets like the real radio does.
	sawClientCiv bool
	clientSeq    uint16
	clientSeen   map[uint16]bool

	civResp func(frame []byte) [][]byte // scripted CI-V command responder

	autoStop chan struct{}
}

// NewFakeRadio binds the fake's two loopback UDP sockets and starts serving.
// Call Close when the test is done (the exported surface's constructor —
// package-internal tests use the newFakeRadio adapter, which adds the
// testing.T plumbing).
func NewFakeRadio() (*FakeRadio, error) {
	fr := &FakeRadio{
		radioID:    fakeRadioID,
		dropIn:     map[string]int{},
		dropInData: map[string]int{},
		dropOut:    map[string]int{},
		lostCiv:    map[uint16]bool{},
		ctrlTx:     map[uint16][]byte{},
		civTx:      map[uint16][]byte{},
		clientSeen: map[uint16]bool{},
		autoStop:   make(chan struct{}),
	}
	var err error
	if fr.ctrlConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}); err != nil {
		return nil, err
	}
	if fr.civConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}); err != nil {
		fr.ctrlConn.Close()
		return nil, err
	}
	go fr.serve(fr.ctrlConn, sockCtrl)
	go fr.serve(fr.civConn, sockCiv)
	return fr, nil
}

// Close shuts both sockets down and stops the scripted timers. Idempotent.
func (fr *FakeRadio) Close() {
	fr.ctrlConn.Close()
	fr.civConn.Close()
	fr.closeOnce.Do(func() { close(fr.autoStop) })
}

func (fr *FakeRadio) ctrlPort() int { return fr.ctrlConn.LocalAddr().(*net.UDPAddr).Port }
func (fr *FakeRadio) civPort() int  { return fr.civConn.LocalAddr().(*net.UDPAddr).Port }

// --- exported scripting + observation surface (cross-package tests) --------

// CtrlPort returns the fake's control-stream port (the client dials it).
func (fr *FakeRadio) CtrlPort() int { return fr.ctrlPort() }

// SetSilent makes the fake never answer are-you-there.
func (fr *FakeRadio) SetSilent() { fr.setSilent() }

// SetNoReady makes the fake answer are-you-there but never I-am-ready.
func (fr *FakeRadio) SetNoReady() { fr.setNoReady() }

// StopAnswering models the radio vanishing mid-session (reboot, network
// drop). Reboot is the variant that comes back with a fresh session ID.
func (fr *FakeRadio) StopAnswering() { fr.stopAnswering() }

// SetLoginErr scripts the login response error field (0xfffffffe = invalid
// username/password).
func (fr *FakeRadio) SetLoginErr(err uint32) { fr.setLoginErr(err) }

// SetRefused scripts the status-packet error 0xffffffff: connection refused
// (a stale or other client holds the radio's single session — wfview holds).
func (fr *FakeRadio) SetRefused() { fr.setRefused() }

// Reboot models a radio power cycle: it stops answering immediately and,
// after back, answers again presenting a FRESH session ID — a client's old
// session dies in keepalive silence and its next connect is a full re-login
// against the new ID (R3: no resume).
func (fr *FakeRadio) Reboot(back time.Duration) {
	fr.mu.Lock()
	fr.stopped = true
	fr.radioID = fr.radioID + 1 // fresh sentid, deterministic and != the old one
	// A rebooted radio forgot its sequence windows and client tracking; the
	// scripted responder persists (the operator's radio settings would).
	fr.ctrlTx = map[uint16][]byte{}
	fr.civTx = map[uint16][]byte{}
	fr.clientSeen = map[uint16]bool{}
	fr.sawClientCiv = false
	fr.mu.Unlock()
	if back <= 0 {
		fr.comeBack()
		return
	}
	go func() {
		select {
		case <-fr.autoStop:
			return
		case <-time.After(back):
			fr.comeBack()
		}
	}()
}

func (fr *FakeRadio) comeBack() {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.stopped = false
}

// RadioID returns the session ID the fake currently presents (a Reboot
// rotates it — the assertion that a reconnect met the new radio).
func (fr *FakeRadio) RadioID() uint32 {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return fr.radioID
}

// LoginAttempts returns the arrival times of the login packets the fake
// received — the retry-spacing assertion primitive (R2: attempts >=30s
// apart, no retry storm).
func (fr *FakeRadio) LoginAttempts() []time.Time {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	var out []time.Time
	for _, p := range fr.recv {
		if p.sock == sockCtrl && len(p.data) == loginLen {
			out = append(out, p.at)
		}
	}
	return out
}

// CivFramesReceived returns the CI-V payloads that reached the radio, in
// arrival order — the frame-order assertion primitive (e.g. PTT-off sent
// first on a safety reconnect).
func (fr *FakeRadio) CivFramesReceived() [][]byte {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	out := make([][]byte, len(fr.gotFrames))
	copy(out, fr.gotFrames)
	return out
}

// Counts returns how many token renewals, token removals (clean
// disconnects), CI-V stream opens and closes the fake has seen.
func (fr *FakeRadio) Counts() (renew, logout, opens, closes int) {
	return fr.counts()
}

// RespondCIV scripts the CI-V command responder: for every CI-V frame the
// client sends on the data stream, fn returns the reply payloads to push
// back (nil = stay silent). fn runs on the fake's serve goroutine with
// fr.mu held — it must be pure and must not call the fake's locking methods.
func (fr *FakeRadio) RespondCIV(fn func(frame []byte) [][]byte) { fr.respondCIV(fn) }

// InjectCIV pushes one CI-V payload to the client as a tracked data packet
// (a transceive broadcast simulation).
func (fr *FakeRadio) InjectCIV(payload []byte) { fr.injectCIV(payload) }

// StartAutoCiv makes the fake emit a small CI-V frame every interval once
// the data stream is open, until StopAutoCiv.
func (fr *FakeRadio) StartAutoCiv(every time.Duration) { fr.startAutoCiv(every) }

// StopAutoCiv stops the automatic CI-V data emission.
func (fr *FakeRadio) StopAutoCiv() { fr.stopAutoCiv() }

// --- scripting surface (package-internal; the civ tests use these) --------

func (fr *FakeRadio) setSilent() {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.silent = true
}

func (fr *FakeRadio) setNoReady() {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.noReady = true
}

// stopAnswering models the radio vanishing mid-session (reboot, network drop).
func (fr *FakeRadio) stopAnswering() {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.stopped = true
}

// dropIncoming drops the next n client datagrams on the named socket.
func (fr *FakeRadio) dropIncoming(sock string, n int) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.dropIn[sock] += n
}

// dropIncomingData drops the next n client CI-V DATA datagrams on the named
// socket (keepalives and control packets pass — the loss hits exactly the
// test's data frames).
func (fr *FakeRadio) dropIncomingData(sock string, n int) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.dropInData[sock] += n
}

// dropOutgoing drops the next n radio->client datagrams on the named socket
// (radio-side transmission loss).
func (fr *FakeRadio) dropOutgoing(sock string, n int) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.dropOut[sock] += n
}

// loseCivSeq marks one of the radio's own civ packets as permanently lost —
// retransmit requests for it go unanswered, like a datagram that never made
// it onto the wire in either direction.
func (fr *FakeRadio) loseCivSeq(seq uint16) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.lostCiv[seq] = true
}

func (fr *FakeRadio) setLoginErr(err uint32) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.loginErr = err
}

func (fr *FakeRadio) setRefused() {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.statusErr = 0xffffffff
}

// startAutoCiv makes the fake emit a small CI-V frame every interval once
// the data stream is open, until stopAutoCiv.
func (fr *FakeRadio) startAutoCiv(every time.Duration) {
	fr.mu.Lock()
	fr.autoCivEvery = every
	fr.mu.Unlock()
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		var b byte
		for {
			select {
			case <-fr.autoStop:
				return
			case <-t.C:
				fr.mu.Lock()
				open := fr.opens > 0 && !fr.stopped
				fr.mu.Unlock()
				if !open {
					continue
				}
				b++
				fr.injectCIV([]byte{0xfe, 0xfe, 0xa2, 0xe0, 0x00, b, 0xfd})
			}
		}
	}()
}

func (fr *FakeRadio) stopAutoCiv() {
	fr.mu.Lock()
	fr.autoCivEvery = 0
	fr.mu.Unlock()
}

// --- observation surface ---------------------------------------------------

func (fr *FakeRadio) sentOn(sock string) []recPkt {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	var out []recPkt
	for _, p := range fr.sent {
		if p.sock == sock {
			out = append(out, p)
		}
	}
	return out
}

func (fr *FakeRadio) recvOn(sock string) []recPkt {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	var out []recPkt
	for _, p := range fr.recv {
		if p.sock == sock {
			out = append(out, p)
		}
	}
	return out
}

func (fr *FakeRadio) counts() (renew, logout, opens, closes int) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return fr.renew, fr.logout, fr.opens, fr.closes
}

func (fr *FakeRadio) civFramesReceived() [][]byte {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	out := make([][]byte, len(fr.gotFrames))
	copy(out, fr.gotFrames)
	return out
}

func (fr *FakeRadio) civSubSeqsReceived() []uint16 {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	out := make([]uint16, len(fr.gotSubSeqs))
	copy(out, fr.gotSubSeqs)
	return out
}

// framesSentCount is how many CI-V payloads the radio has pushed (attempts,
// including dropped ones).
func (fr *FakeRadio) framesSentCount() int {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return len(fr.civTx)
}

// --- the fake protocol server ----------------------------------------------

func (fr *FakeRadio) serve(conn *net.UDPConn, sock string) {
	buf := make([]byte, 2048)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return // socket closed
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		fr.handle(sock, src, data)
	}
}

func (fr *FakeRadio) handle(sock string, src *net.UDPAddr, data []byte) {
	fr.mu.Lock()
	defer fr.mu.Unlock()

	// Record the client's source address per socket (radio replies to it).
	switch sock {
	case sockCtrl:
		fr.ctrlAddr = src
	case sockCiv:
		fr.civAddr = src
	}

	if n := fr.dropIn[sock]; n > 0 {
		fr.dropIn[sock] = n - 1
		return // datagram lost in transit
	}
	fr.recv = append(fr.recv, recPkt{sock: sock, data: data, at: time.Now()})

	if len(data) < headerLen {
		return
	}
	h := parseHeader(data)

	switch {
	case h.len == ctrlLen:
		switch h.typ {
		case ptAreYouThere:
			if fr.silent || fr.stopped {
				return
			}
			fr.send(sock, controlPacket(ptIAmHere, 0, fr.radioID, h.sentID))
		case ptAreYouReady:
			if fr.noReady || fr.stopped {
				return
			}
			fr.send(sock, controlPacket(ptIAmReady, 1, fr.radioID, h.sentID))
		case ptRetransmit:
			// Single-packet retransmit request: seq rides the header.
			fr.answerRetransmit(sock, []uint16{h.seq})
		case ptIdle:
			if sock == sockCiv {
				// Tracked idle: it shares the client's tracked sequence
				// space, so it counts for gap detection.
				fr.trackClientSeq(sock, h)
			}
		case ptLogout:
			// Disconnects need no answer.
		}
	case h.typ == ptRetransmit && h.len > ctrlLen:
		// Bulk (range) retransmit request: (start,end) u16 LE pairs at 0x10.
		var seqs []uint16
		for off := headerLen; off+4 <= len(data); off += 4 {
			start := binary.LittleEndian.Uint16(data[off : off+2])
			end := binary.LittleEndian.Uint16(data[off+2 : off+4])
			for s := start; ; s++ {
				seqs = append(seqs, s)
				if s == end || len(seqs) >= 128 {
					break
				}
			}
		}
		fr.answerRetransmit(sock, seqs)
	case h.len == pingLen && h.typ == ptPing:
		if data[replyOff] == 0x00 && !fr.stopped {
			// Ping request: echo seq and uptime with reply flag set.
			p := make([]byte, pingLen)
			putHeader(p, header{len: pingLen, typ: ptPing, seq: h.seq, sentID: fr.radioID, rcvdID: h.sentID})
			p[replyOff] = 0x01
			copy(p[pingTimeOff:], data[pingTimeOff:pingTimeOff+4])
			fr.send(sock, p)
		}
	case h.len == loginLen:
		if fr.stopped {
			return
		}
		fr.replyLogin(sock, h, data)
	case h.len == tokenLen && data[reqTypeOff] == 0x05:
		if fr.stopped {
			return
		}
		fr.renew++
		fr.replyToken(sock, h, data)
	case h.len == tokenLen && data[reqTypeOff] == 0x02:
		if fr.stopped {
			return
		}
		fr.replyToken(sock, h, data)
	case h.len == tokenLen && data[reqTypeOff] == 0x01:
		fr.logout++
	case h.len == streamReqLen:
		if fr.stopped {
			return
		}
		fr.replyStreamReq(sock, h, data)
	case h.len == openCloseLen && h.typ == ptIdle:
		if sock == sockCiv {
			// Open/close packets are tracked on the same client seq space.
			fr.trackClientSeq(sock, h)
		}
		if data[magicOff] == 0x04 {
			fr.opens++
		} else {
			fr.closes++
		}
	case h.len > civHeaderLen && h.typ == ptIdle:
		// CI-V data from the client. Scripted loss first (a dropped datagram
		// is unseen: no seq tracking, no recording — like the wire).
		if n := fr.dropInData[sock]; n > 0 {
			fr.dropInData[sock] = n - 1
			return
		}
		// Gap detection: the radio requests retransmission of the client
		// packets it never received.
		fr.trackClientSeq(sock, h)
		if !fr.clientSeen[h.seq] {
			// Record the payload after the sub-header; duplicates (re-sent
			// packets) are ignored — the real radio deduplicates too.
			fr.clientSeen[h.seq] = true
			datalen := binary.LittleEndian.Uint16(data[civDatalenOff : civDatalenOff+2])
			if int(civHeaderLen)+int(datalen) <= len(data) {
				payload := make([]byte, datalen)
				copy(payload, data[civHeaderLen:civHeaderLen+int(datalen)])
				fr.gotFrames = append(fr.gotFrames, payload)
				fr.gotSubSeqs = append(fr.gotSubSeqs, binary.BigEndian.Uint16(data[civSendSeqOff:civSendSeqOff+2]))
				// Tracked: the radio keeps it for retransmit answers.
				fr.civTx[h.seq] = data
				if fr.civResp != nil {
					for _, rep := range fr.civResp(payload) {
						fr.injectCIVLocked(rep)
					}
				}
			}
		}
	}
}

// send emits a radio datagram, honoring the outgoing-drop script and the
// permanent-loss script for civ packets.
func (fr *FakeRadio) send(sock string, data []byte) {
	if n := fr.dropOut[sock]; n > 0 {
		fr.dropOut[sock] = n - 1
		return
	}
	fr.sent = append(fr.sent, recPkt{sock: sock, data: data, at: time.Now()})
	var conn *net.UDPConn
	var dst *net.UDPAddr
	switch sock {
	case sockCtrl:
		conn, dst = fr.ctrlConn, fr.ctrlAddr
	case sockCiv:
		conn, dst = fr.civConn, fr.civAddr
	}
	if conn == nil || dst == nil {
		return
	}
	if _, err := conn.WriteToUDP(data, dst); err != nil {
		return // client socket gone; tests poll state, not this error
	}
}

// sendTracked assigns the radio's next tracked seq on the socket and keeps
// the packet for retransmit answers. Caller holds fr.mu (all callers run on
// the serve goroutine inside handle).
func (fr *FakeRadio) sendTracked(sock string, data []byte) {
	var seq uint16
	switch sock {
	case sockCtrl:
		fr.ctrlSeq++
		seq = fr.ctrlSeq
		fr.ctrlTx[seq] = data
	case sockCiv:
		fr.civSeq++
		seq = fr.civSeq
		fr.civTx[seq] = data
	}
	data[seqOff] = byte(seq)
	data[seqOff+1] = byte(seq >> 8)
	fr.send(sock, data)
}

func (fr *FakeRadio) answerRetransmit(sock string, seqs []uint16) {
	if fr.stopped {
		return
	}
	var win map[uint16][]byte
	switch sock {
	case sockCtrl:
		win = fr.ctrlTx
	case sockCiv:
		win = fr.civTx
	}
	for _, s := range seqs {
		if sock == sockCiv && fr.lostCiv[s] {
			continue // this packet never reaches the client, ever
		}
		if d, ok := win[s]; ok {
			fr.send(sock, d)
		} else {
			// Unknown seq: answer with a 16-byte idle carrying the seq
			// (the wfview behavior).
			fr.send(sock, controlPacket(ptIdle, s, fr.radioID, 0))
		}
	}
}

func (fr *FakeRadio) replyLogin(sock string, h header, req []byte) {
	tokReq := binary.LittleEndian.Uint16(req[tokReqOff : tokReqOff+2])
	p := make([]byte, loginRespLen)
	putHeader(p, header{len: loginRespLen, sentID: fr.radioID, rcvdID: h.sentID})
	putBE32(p, payloadOff, uint32(loginRespLen-headerLen))
	p[reqReplyOff] = 0x02
	putBE16(p, innerSeqOff, reqBE16(req, innerSeqOff))
	binary.LittleEndian.PutUint16(p[tokReqOff:], tokReq)
	binary.LittleEndian.PutUint32(p[tokenOff:], fakeToken)
	putLE32(p, errOff, fr.loginErr)
	copy(p[connTypeOff:], "FTTH")
	fr.sendTracked(sock, p)
}

func (fr *FakeRadio) replyToken(sock string, h header, req []byte) {
	tokReq := binary.LittleEndian.Uint16(req[tokReqOff : tokReqOff+2])
	p := make([]byte, tokenLen)
	putHeader(p, header{len: tokenLen, sentID: fr.radioID, rcvdID: h.sentID})
	putBE32(p, payloadOff, uint32(tokenLen-headerLen))
	p[reqReplyOff] = 0x02
	p[reqTypeOff] = 0x05
	putBE16(p, innerSeqOff, reqBE16(req, innerSeqOff))
	binary.LittleEndian.PutUint16(p[tokReqOff:], tokReq)
	binary.LittleEndian.PutUint32(p[tokenOff:], fakeToken)
	putLE32(p, errOff, 0) // response 0x00000000 = renewal accepted
	fr.sendTracked(sock, p)
}

func (fr *FakeRadio) replyStreamReq(sock string, h header, req []byte) {
	p := make([]byte, statusLen)
	putHeader(p, header{len: statusLen, sentID: fr.radioID, rcvdID: h.sentID})
	putBE32(p, payloadOff, uint32(statusLen-headerLen))
	p[reqReplyOff] = 0x02
	p[reqTypeOff] = 0x03
	putBE16(p, innerSeqOff, reqBE16(req, innerSeqOff))
	binary.LittleEndian.PutUint16(p[tokReqOff:], binary.LittleEndian.Uint16(req[tokReqOff:tokReqOff+2]))
	binary.LittleEndian.PutUint32(p[tokenOff:], binary.LittleEndian.Uint32(req[tokenOff:tokenOff+4]))
	putLE32(p, errOff, fr.statusErr)
	p[discOff] = 0x00
	if fr.statusErr == 0 {
		binary.BigEndian.PutUint16(p[civPortOff:], uint16(fr.civPort()))
		binary.BigEndian.PutUint16(p[audioPortOff:], 50003)
	}
	fr.sendTracked(sock, p)
}

// injectCIV pushes one CI-V payload to the client as a tracked data packet.
func (fr *FakeRadio) injectCIV(payload []byte) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.injectCIVLocked(payload)
}

func (fr *FakeRadio) injectCIVLocked(payload []byte) {
	fr.civSeq++
	seq := fr.civSeq
	fr.civSubSeq++
	p := make([]byte, civHeaderLen+len(payload))
	putHeader(p, header{len: uint32(len(p)), sentID: fr.radioID})
	p[civReplyOff] = 0xc1
	binary.LittleEndian.PutUint16(p[civDatalenOff:], uint16(len(payload)))
	binary.BigEndian.PutUint16(p[civSendSeqOff:], fr.civSubSeq)
	copy(p[civHeaderLen:], payload)
	p[seqOff] = byte(seq)
	p[seqOff+1] = byte(seq >> 8)
	// The radio HAS this packet even if the transmission drops — register it
	// for retransmit answers before the scripted send loss.
	fr.civTx[seq] = p
	fr.send(sockCiv, p)
}

// injectCIVAt sends a payload with an arbitrary tracked seq (gap/flush tests).
func (fr *FakeRadio) injectCIVAt(seq uint16, payload []byte) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.civSubSeq++
	p := make([]byte, civHeaderLen+len(payload))
	putHeader(p, header{len: uint32(len(p)), sentID: fr.radioID, seq: seq})
	p[civReplyOff] = 0xc1
	binary.LittleEndian.PutUint16(p[civDatalenOff:], uint16(len(payload)))
	binary.BigEndian.PutUint16(p[civSendSeqOff:], fr.civSubSeq)
	copy(p[civHeaderLen:], payload)
	fr.civTx[seq] = p
	fr.send(sockCiv, p)
}

// injectPingRequest makes the radio ping the client (unsolicited request).
func (fr *FakeRadio) injectPingRequest(seq uint16, uptimeMS uint32) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	p := make([]byte, pingLen)
	putHeader(p, header{len: pingLen, typ: ptPing, seq: seq, sentID: fr.radioID})
	p[replyOff] = 0x00
	binary.LittleEndian.PutUint32(p[pingTimeOff:], uptimeMS)
	fr.send(sockCtrl, p)
}

// respondCIV scripts the CI-V command responder: for every CI-V frame the
// client sends on the data stream, fn returns the reply payloads to push
// back (nil = stay silent). fn runs on the fake's serve goroutine with
// fr.mu held — it must be pure and must not call the fake's locking methods.
func (fr *FakeRadio) respondCIV(fn func(frame []byte) [][]byte) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.civResp = fn
}

// script runs fn under the fake's mutex — the same lock the CI-V responder
// runs under — so test-side mutations of the responder's captured state are
// properly synchronized with the serve goroutine.
func (fr *FakeRadio) script(fn func()) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fn()
}

// reqBE16 reads a big-endian u16 out of a request packet (innerseq echo).
func reqBE16(req []byte, off int) uint16 {
	return uint16(req[off])<<8 | uint16(req[off+1])
}

// trackClientSeq follows the client's tracked-sequence space on the civ
// stream (idles and data share one counter) and requests retransmission of
// any gap, the way the real radio does. Caller holds fr.mu.
func (fr *FakeRadio) trackClientSeq(sock string, h header) {
	if fr.sawClientCiv {
		if d := seqDiff(h.seq, fr.clientSeq); d >= 1 && d <= maxMissingCap {
			for s := fr.clientSeq; s != h.seq; s++ {
				fr.send(sock, controlPacket(ptRetransmit, s, fr.radioID, h.sentID))
			}
		}
	}
	fr.sawClientCiv = true
	if seqDiff(h.seq+1, fr.clientSeq) < 32768 {
		fr.clientSeq = h.seq + 1
	}
}
