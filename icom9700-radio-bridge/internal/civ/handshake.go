package civ

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"
)

// Dial performs the full RS-BA1 handshake against the radio and returns a
// live transport (brief, "Connection sequence"):
//
//  1. are-you-there (16-byte control, type 0x03) every 500 ms until I-am-here
//  2. are-you-ready/I-am-ready exchange — establishes the 4-byte IDs
//  3. login (0x80) with substitution-table-obfuscated credentials; the radio
//     answers with the session token
//  4. token confirm (0x02) then token renewal (0x05) — renewals repeat every
//     60 s while live
//  5. stream request (0x90) carrying the client's chosen local CI-V port;
//     the radio answers with a status packet naming its own ports (CI-V
//     big-endian at 0x42) or error 0xffffffff = connection refused
//  6. the CI-V data socket opens from the chosen local port to the radio's
//     port and sends the open packet (0x16, data 0x01c0, magic 0x04)
//
// The returned Transport is live (Live() == true) or the error explains the
// failed step. Every phase is bounded by Opts.HandshakeTimeout; each
// handshake step's keepalive-free wait is fine because the real radio
// answers each step in single-digit milliseconds.
func Dial(ctx context.Context, o Opts) (*Transport, error) {
	o.fill()
	if o.Host == "" {
		return nil, errors.New("civ: radio host required")
	}
	if o.Username == "" || o.Password == "" {
		return nil, errors.New("civ: CI-V login credentials required (the login packet would authenticate with empty secrets)")
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}

	radio, err := net.ResolveUDPAddr("udp", net.JoinHostPort(o.Host, strconv.Itoa(o.ControlPort)))
	if err != nil {
		return nil, fmt.Errorf("civ: resolve radio: %w", err)
	}

	t := &Transport{
		o:   o,
		log: log,
	}
	t.ctx, t.cancel = context.WithCancel(ctx)
	t.hsCtrl = make(chan []byte, hsQueueCap)
	t.hsCiv = make(chan []byte, hsQueueCap)

	cleanup := func(e error) (*Transport, error) {
		t.closed.Store(true)
		t.shutdownSockets()
		return nil, e
	}

	// Control stream: socket, IDs, handshake-phase reader.
	ctrl, err := newStream(t, "control", radio)
	if err != nil {
		return cleanup(fmt.Errorf("civ: control socket: %w", err))
	}
	t.ctrl = ctrl
	go t.readLoop(t.ctrl, t.hsCtrl)

	// Step 1: are-you-there until I-am-here.
	if err := t.awaitIAmHere(ctx, t.ctrl, t.hsCtrl); err != nil {
		return cleanup(err)
	}
	t.log.Debug("radio answered are-you-there")

	// Step 2: are-you-ready -> I-am-ready (establishes the IDs' direction).
	_ = t.ctrl.sendUntracked(controlPacket(ptAreYouReady, 1, t.ctrl.myID, t.ctrl.remoteID))
	if _, err := t.awaitMatch(ctx, t.hsCtrl, t.o.HandshakeTimeout, "I-am-ready",
		func(d []byte, h header) bool { return h.len == ctrlLen && h.typ == ptIAmReady }); err != nil {
		return cleanup(err)
	}

	// Step 3: login with substituted credentials; the response carries the
	// session token (echo-matched on our token request nonce).
	ab := newAuthBytes(o.Username, o.Password, o.ClientName)
	t.ctrl.mu.Lock()
	tokReq := t.ctrl.sendLogin(ab.username, ab.password, ab.clientName, time.Now())
	t.ctrl.mu.Unlock()
	resp, err := t.awaitMatch(ctx, t.hsCtrl, t.o.HandshakeTimeout, "login response",
		func(d []byte, h header) bool {
			return h.len == loginRespLen && binary.LittleEndian.Uint16(d[tokReqOff:]) == tokReq
		})
	if err != nil {
		return cleanup(err)
	}
	t.ctrl.mu.Lock()
	t.ctrl.token, err = parseLoginResponse(resp, tokReq)
	t.ctrl.mu.Unlock()
	if err != nil {
		return cleanup(err)
	}

	// Step 4: token confirm (0x02), then the first renewal (0x05); the radio
	// answers the renewal and arms the 60 s cycle (wfview/kappanhang both
	// send the two back to back).
	now := time.Now()
	t.mu.Lock()
	t.ctrl.mu.Lock()
	t.ctrl.sendToken(reqTypeConfirm, now)
	t.ctrl.sendToken(reqTypeRenewal, now)
	t.ctrl.mu.Unlock()
	t.renewal.outstanding = true
	t.renewal.deadline = now.Add(t.o.RenewalTimeout)
	t.renewal.nextDue = now.Add(t.o.TokenRenewal)
	t.mu.Unlock()
	tokResp, err := t.awaitMatch(ctx, t.hsCtrl, t.o.RenewalTimeout, "token response",
		func(d []byte, h header) bool {
			_, ok := parseTokenResponse(d)
			return h.len == tokenLen && ok
		})
	if err != nil {
		return cleanup(err)
	}
	if response, _ := parseTokenResponse(tokResp); response != respOK {
		if response == errRefused {
			return cleanup(fmt.Errorf("%w: token renewal rejected (0xffffffff) during handshake", ErrRefused))
		}
		return cleanup(fmt.Errorf("civ: unexpected token response 0x%08x", response))
	}
	// The answer to the handshake renewal arms the 60 s cycle (wfview
	// restarts TOKEN_RENEWAL on every renewal response).
	t.mu.Lock()
	t.renewalOK(now)
	t.mu.Unlock()

	// Step 5: bind the CI-V data socket on the chosen local port (before the
	// stream request, which carries it), request the stream, read the radio's
	// status packet for its own port assignment.
	civ, err := newStream(t, "civ", nil)
	if err != nil {
		return cleanup(fmt.Errorf("civ: data socket: %w", err))
	}
	t.setCiv(civ)
	audioLocalPort := freeUDPPort() // requested but never opened (control-only v1, KTD-9)
	t.ctrl.mu.Lock()
	t.ctrl.sendStreamRequest(civ.localPort(), audioLocalPort, ab.username, time.Now())
	t.ctrl.mu.Unlock()
	statusPkt, err := t.awaitMatch(ctx, t.hsCtrl, t.o.HandshakeTimeout, "stream status",
		func(d []byte, h header) bool { return h.len == statusLen && h.typ == ptIdle })
	if err != nil {
		return cleanup(err)
	}
	st := parseStatus(statusPkt)
	if st.err == errRefused {
		return cleanup(fmt.Errorf("%w: status error 0xffffffff (stale/other session)", ErrRefused))
	}
	if st.disc {
		return cleanup(fmt.Errorf("%w: radio reports disconnected during handshake", ErrRefused))
	}
	if st.civPort == 0 {
		return cleanup(errors.New("civ: status packet carried no CI-V port"))
	}
	civAddr := &net.UDPAddr{IP: radio.IP, Port: st.civPort}
	civ.connect(civAddr)
	go t.readLoop(civ, t.hsCiv)

	// Step 6: the CI-V leg's own mini-handshake, then the open packet.
	if err := t.awaitIAmHere(ctx, civ, t.hsCiv); err != nil {
		return cleanup(err)
	}
	_ = civ.sendUntracked(controlPacket(ptAreYouReady, 1, civ.myID, civ.remoteID))
	if _, err := t.awaitMatch(ctx, t.hsCiv, t.o.HandshakeTimeout, "civ I-am-ready",
		func(d []byte, h header) bool { return h.len == ctrlLen && h.typ == ptIAmReady }); err != nil {
		return cleanup(err)
	}
	now = time.Now()
	civ.mu.Lock()
	_ = civ.sendTracked(openclosePacket(false, civ.nextSubSeq(), civ.myID, civ.remoteID), now)
	civ.openDue = now.Add(t.o.StartDataPeriod) // start-data repeats until data flows
	civ.mu.Unlock()

	t.live.Store(true)
	t.log.Info("CI-V session live",
		"radio", o.Host, "control_port", t.ctrl.localPort(), "civ_port", st.civPort,
		"local_civ_port", civ.localPort())
	go t.maintain()
	return t, nil
}

// awaitIAmHere runs the are-you-there loop (step 1): a 16-byte control probe
// every AreYouTherePeriod until the radio answers I-am-here, bounded by
// HandshakeTimeout.
func (t *Transport) awaitIAmHere(ctx context.Context, s *stream, hs chan []byte) error {
	s.mu.Lock()
	_ = s.sendUntracked(controlPacket(ptAreYouThere, 0, s.myID, s.remoteID))
	s.mu.Unlock()
	ticker := time.NewTicker(t.o.AreYouTherePeriod)
	defer ticker.Stop()
	timer := time.NewTimer(t.o.HandshakeTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("civ: %w (are-you-there phase)", ctx.Err())
		case <-timer.C:
			return fmt.Errorf("%w: radio did not answer are-you-there within %s", ErrTimeout, t.o.HandshakeTimeout)
		case <-ticker.C:
			s.mu.Lock()
			_ = s.sendUntracked(controlPacket(ptAreYouThere, 0, s.myID, s.remoteID))
			s.mu.Unlock()
		case d := <-hs:
			h := parseHeader(d)
			if h.len == ctrlLen && h.typ == ptIAmHere {
				s.mu.Lock()
				s.remoteID = h.sentID
				s.mu.Unlock()
				return nil
			}
		}
	}
}

// awaitMatch drains the handshake queue until a datagram matches, the
// deadline passes or ctx dies.
func (t *Transport) awaitMatch(ctx context.Context, hs chan []byte, timeout time.Duration, what string, match func([]byte, header) bool) ([]byte, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("civ: %w (awaiting %s)", ctx.Err(), what)
		case <-timer.C:
			return nil, fmt.Errorf("%w: no %s within %s", ErrTimeout, what, timeout)
		case d := <-hs:
			if match(d, parseHeader(d)) {
				return d, nil
			}
		}
	}
}

// freeUDPPort grabs a free UDP port for the audio-port field of the stream
// request (wfview's temp-bind dance; the port itself is never opened —
// control-only v1, KTD-9).
func freeUDPPort() int {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return 0
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}
