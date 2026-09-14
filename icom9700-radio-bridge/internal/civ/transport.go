package civ

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Transport is one live RS-BA1 session to the radio: the control stream and
// the CI-V data stream plus their timers. Build with Dial; send CI-V frames
// with SendCIV; consume radio CI-V traffic through Opts.OnCIVFrame; react to
// involuntary session end through Opts.OnSessionLoss (R3: session loss is a
// full re-login path — there is no resume, so the owner discards the
// transport and Dials again). Close disconnects cleanly.
type Transport struct {
	o          Opts
	log        *slog.Logger
	ctx        context.Context
	cancel     context.CancelFunc
	cancelOnce sync.Once

	ctrl  *stream // control stream, set by Dial before any reader runs
	civMu sync.Mutex
	civSt *stream // CI-V data stream, nil until the status packet

	hsCtrl chan []byte // handshake-phase datagrams (bounded, drops on flood)
	hsCiv  chan []byte

	live   atomic.Bool
	lost   atomic.Bool
	closed atomic.Bool

	mu        sync.Mutex // guards the renewal state
	renewal   renewalState
	malformed atomic.Uint64
}

// Opts configures the transport. The period fields carry the protocol
// brief's cadences as defaults; tests shrink them. Host, credentials and
// callbacks are mandatory in practice.
type Opts struct {
	// Host is the radio's LAN address (no discovery exists in the protocol).
	Host string
	// ControlPort is the radio's control port (50001; the CI-V data port is
	// assigned by the radio in the status packet, normally 50002).
	ControlPort int
	// Username and Password are the radio's remote-control login. They are
	// substitution-obfuscated onto the wire; they are never logged (never-log
	// pin, package doc).
	Username string
	Password string
	// ClientName rides the login packet's plain client-name field.
	ClientName string
	// Log receives lifecycle and error lines (no packet content, ever).
	Log *slog.Logger
	// OnCIVFrame receives raw CI-V chunks from the radio as they arrive, in
	// sequence order. Called on the read goroutine: it must not block and
	// must not call back into the transport (dispatch to a worker instead —
	// the stationa paho-handler rule).
	OnCIVFrame func(chunk []byte)
	// OnSessionLoss fires once when an established session dies (radio
	// silence past SessionTimeout, renewal rejection/timeout, radio
	// disconnect status, socket death). Called on its own goroutine.
	OnSessionLoss func(err error)

	// Cadence knobs (brief values as defaults).
	AreYouTherePeriod time.Duration // probe repeat while waiting for I-am-here (500 ms)
	HandshakeTimeout  time.Duration // bound for the whole connect (wfview: 20 probes)
	IdlePeriod        time.Duration // tracked idle keepalive (100 ms)
	PingPeriod        time.Duration // ping (500 ms)
	RetransmitPeriod  time.Duration // retransmit-request tick (100 ms)
	RetransmitTries   int           // give up on a hole after this many requests (4)
	MaxMissing        int           // flush the rx window beyond this many holes (50)
	TokenRenewal      time.Duration // renewal cycle (60 s)
	RenewalTimeout    time.Duration // bound for one renewal answer (3 s)
	CivSilence        time.Duration // CI-V watchdog: re-open after this much silence (2 s)
	StartDataPeriod   time.Duration // start-data (re-)send cadence (100 ms)
	SessionTimeout    time.Duration // no radio traffic at all -> session loss (10 s)
}

// fill applies the protocol-brief defaults to unset fields.
func (o *Opts) fill() {
	if o.ControlPort == 0 {
		o.ControlPort = ControlPort
	}
	if o.ClientName == "" {
		o.ClientName = "icom9700-radio-bridge"
	}
	if o.AreYouTherePeriod == 0 {
		o.AreYouTherePeriod = 500 * time.Millisecond
	}
	if o.HandshakeTimeout == 0 {
		o.HandshakeTimeout = 10 * time.Second
	}
	if o.IdlePeriod == 0 {
		o.IdlePeriod = 100 * time.Millisecond
	}
	if o.PingPeriod == 0 {
		o.PingPeriod = 500 * time.Millisecond
	}
	if o.RetransmitPeriod == 0 {
		o.RetransmitPeriod = 100 * time.Millisecond
	}
	if o.RetransmitTries == 0 {
		o.RetransmitTries = 4
	}
	if o.MaxMissing == 0 {
		o.MaxMissing = maxMissingCap
	}
	if o.TokenRenewal == 0 {
		o.TokenRenewal = 60 * time.Second
	}
	if o.RenewalTimeout == 0 {
		o.RenewalTimeout = 3 * time.Second
	}
	if o.CivSilence == 0 {
		o.CivSilence = 2 * time.Second
	}
	if o.StartDataPeriod == 0 {
		o.StartDataPeriod = 100 * time.Millisecond
	}
	if o.SessionTimeout == 0 {
		o.SessionTimeout = 10 * time.Second
	}
}

// Session outcome errors (errors.Is-able; R2/R3 surface these as facts).
var (
	// ErrTimeout: the radio did not answer a handshake phase in time.
	ErrTimeout = errors.New("civ handshake timeout")
	// ErrAuthFailed: the radio rejected the login credentials (0xfffffffe).
	ErrAuthFailed = errors.New("civ login rejected: invalid username/password")
	// ErrRefused: the radio refused the session (status 0xffffffff — stale or
	// other client holds it).
	ErrRefused = errors.New("civ connection refused by radio")
	// ErrSessionLost: an established session died involuntarily.
	ErrSessionLost = errors.New("civ session lost")
	// ErrClosed: the transport is closed (or was never dialed).
	ErrClosed = errors.New("civ transport closed")
)

// hsQueueCap bounds the handshake-phase datagram queues (bounded buffer
// rule; overflow drops — handshake steps match specific packets anyway).
const hsQueueCap = 64

// Live reports whether a session is established.
func (t *Transport) Live() bool { return t.live.Load() }

// SendCIV sends one raw CI-V frame to the radio on the data stream. Safe for
// concurrent use — sends serialize on the stream (the stationa /cmd worker
// contract: one wire, one writer at a time).
func (t *Transport) SendCIV(frame []byte) error {
	if t.closed.Load() {
		return ErrClosed
	}
	if !t.live.Load() {
		if t.lost.Load() {
			return fmt.Errorf("%w: SendCIV after session loss", ErrSessionLost)
		}
		return ErrClosed
	}
	s := t.civStream()
	if s == nil {
		return ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.civOut(frame, time.Now())
}

// Close disconnects cleanly: CI-V stream close (openclose magic 0x00), token
// removal (0x40 requesttype 0x01) and control type-0x05 disconnects on both
// streams (the wfview teardown order), then sockets close. Idempotent; a
// lost session skips the courtesy packets — there is nobody to send to.
func (t *Transport) Close() error {
	if !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	if t.live.CompareAndSwap(true, false) {
		now := time.Now()
		if s := t.civStream(); s != nil {
			s.mu.Lock()
			_ = s.sendTracked(openclosePacket(true, s.nextSubSeq(), s.myID, s.remoteID), now)
			_ = s.sendUntracked(controlPacket(ptLogout, 0, s.myID, s.remoteID))
			s.mu.Unlock()
		}
		t.ctrl.mu.Lock()
		t.ctrl.sendToken(reqTypeRemoval, now)
		_ = t.ctrl.sendUntracked(controlPacket(ptLogout, 0, t.ctrl.myID, t.ctrl.remoteID))
		t.ctrl.mu.Unlock()
	}
	t.shutdownSockets()
	return nil
}

// Stats exposes the window sizes for bound verification (tests and ops
// probing; the MemoryMax rule makes growth observable).
func (t *Transport) Stats() map[string]int {
	m := map[string]int{
		"malformed": int(t.malformed.Load()),
	}
	t.ctrl.mu.Lock()
	m["ctrl_tx"] = t.ctrl.tx.size()
	m["ctrl_pending"] = t.ctrl.rx.pendingCount()
	m["ctrl_missing"] = t.ctrl.rx.missingCount()
	m["ctrl_skip"] = t.ctrl.rx.skipCount()
	t.ctrl.mu.Unlock()
	if s := t.civStream(); s != nil {
		s.mu.Lock()
		m["civ_tx"] = s.tx.size()
		m["civ_pending"] = s.rx.pendingCount()
		m["civ_missing"] = s.rx.missingCount()
		m["civ_skip"] = s.rx.skipCount()
		s.mu.Unlock()
	}
	return m
}

// setCiv installs the CI-V stream (Dial only).
func (t *Transport) setCiv(s *stream) {
	t.civMu.Lock()
	t.civSt = s
	t.civMu.Unlock()
}

func (t *Transport) civStream() *stream {
	t.civMu.Lock()
	defer t.civMu.Unlock()
	return t.civSt
}

// shutdownSockets cancels the loops and closes both sockets (idempotent).
func (t *Transport) shutdownSockets() {
	t.cancelOnce.Do(func() {
		if t.cancel != nil {
			t.cancel()
		}
	})
	if t.ctrl != nil {
		t.ctrl.conn.Close()
	}
	if s := t.civStream(); s != nil {
		s.conn.Close()
	}
}

// sessionLoss ends an established session involuntarily (R3): no courtesy
// packets, sockets closed, one OnSessionLoss on its own goroutine.
func (t *Transport) sessionLoss(err error) {
	if t.closed.Load() {
		return
	}
	if !t.live.CompareAndSwap(true, false) {
		return
	}
	t.lost.Store(true)
	t.log.Warn("radio session lost", "err", err)
	t.shutdownSockets()
	if t.o.OnSessionLoss != nil {
		go t.o.OnSessionLoss(err)
	}
}

// --- readers ---------------------------------------------------------------------------

// readLoop drains one stream socket: sequence-tracking bookkeeping and
// retransmit/ping service happen in every phase; handshake-phase datagrams
// feed the bounded queue, live datagrams go to dispatch.
func (t *Transport) readLoop(s *stream, hs chan []byte) {
	buf := make([]byte, 2048)
	for {
		n, _, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			// Socket closed: intentional Close/teardown never reports loss.
			return
		}
		if n < headerLen {
			continue
		}
		d := make([]byte, n)
		copy(d, buf[:n])
		h := parseHeader(d)
		s.mu.Lock()
		s.lastRx = time.Now()
		s.mu.Unlock()

		if h.typ == ptRetransmit {
			s.mu.Lock()
			s.answerRetransmit(retxRequestSeqs(d, h))
			s.mu.Unlock()
			continue
		}
		if h.len == pingLen && h.typ == ptPing {
			t.handlePing(s, d, h)
			continue
		}
		if t.live.Load() {
			t.dispatch(s, d, h)
			continue
		}
		select {
		case hs <- d:
		default: // bounded: drop junk floods during handshake
		}
	}
}

// handlePing answers a radio ping request (echo seq and uptime with the
// reply flag set, wfview icomudpbase) and ignores replies to our own.
func (t *Transport) handlePing(s *stream, d []byte, h header) {
	if d[replyOff] != 0x00 {
		return
	}
	uptime := binary.LittleEndian.Uint32(d[pingTimeOff:])
	_ = s.sendUntracked(pingPacket(1, h.seq, uptime, s.myID, s.remoteID))
}

// dispatch routes live-phase datagrams per stream.
func (t *Transport) dispatch(s *stream, d []byte, h header) {
	switch {
	case s.name == "civ" && h.typ == ptIdle && h.len > civHeaderLen:
		t.handleCivData(s, d, h)
	case h.len == tokenLen && h.typ == ptIdle:
		if resp, ok := parseTokenResponse(d); ok {
			switch resp {
			case respOK:
				t.mu.Lock()
				t.renewalOK(time.Now())
				t.mu.Unlock()
				t.log.Debug("token renewal accepted")
			case errRefused:
				t.sessionLoss(fmt.Errorf("%w: radio rejected token renewal (0xffffffff)", ErrSessionLost))
			default:
				t.log.Warn("unexpected token renewal response", "response", fmt.Sprintf("0x%08x", resp))
			}
		}
	case h.len == statusLen && h.typ == ptIdle:
		st := parseStatus(d)
		if st.disc || st.err == errRefused {
			t.sessionLoss(fmt.Errorf("%w: radio disconnected (status packet)", ErrSessionLost))
		}
	}
}

// handleCivData frames one CI-V data packet by its sub-header datalen (never
// by scanning for FD), feeds the receive window and delivers in-order,
// duplicate-free payloads to OnCIVFrame.
func (t *Transport) handleCivData(s *stream, d []byte, h header) {
	datalen := int(binary.LittleEndian.Uint16(d[civDatalenOff:]))
	if datalen == 0 || civHeaderLen+datalen != int(h.len) {
		// Malformed length: counted and dropped, never parsed further.
		t.malformed.Add(1)
		return
	}
	payload := append([]byte(nil), d[civHeaderLen:civHeaderLen+datalen]...)
	s.mu.Lock()
	s.lastCivRx = time.Now()
	s.openDue = time.Time{} // data flowing: watchdog disarmed
	deliver := s.rx.offer(h.seq, payload, t.o.MaxMissing)
	s.mu.Unlock()
	t.deliverCIV(deliver)
}

func (t *Transport) deliverCIV(frames [][]byte) {
	if t.o.OnCIVFrame == nil {
		return
	}
	for _, f := range frames {
		t.o.OnCIVFrame(f)
	}
}

// --- maintenance -----------------------------------------------------------------------

// maintain runs the periodic jobs of both streams plus the token renewal and
// session-staleness checks until the context dies. One goroutine, one tick;
// every job keeps its own deadline.
func (t *Transport) maintain() {
	interval := t.o.IdlePeriod / 4
	for _, d := range []time.Duration{t.o.PingPeriod, t.o.RetransmitPeriod,
		t.o.StartDataPeriod, t.o.RenewalTimeout, t.o.CivSilence, t.o.SessionTimeout} {
		if d > 0 && d/4 < interval {
			interval = d / 4
		}
	}
	if min := 5 * time.Millisecond; interval < min {
		interval = min
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case now := <-ticker.C:
			if t.live.Load() {
				t.tick(now)
			}
		}
	}
}

// tick runs one maintenance pass.
func (t *Transport) tick(now time.Time) {
	// Renewal: send when due, die when an outstanding renewal times out.
	t.mu.Lock()
	switch {
	case t.renewal.outstanding && now.After(t.renewal.deadline):
		t.renewal.outstanding = false
		t.mu.Unlock()
		t.sessionLoss(fmt.Errorf("%w: token renewal timed out after %s", ErrSessionLost, t.o.RenewalTimeout))
		return
	case !t.renewal.outstanding && now.After(t.renewal.nextDue):
		t.renewNow(now)
	}
	t.mu.Unlock()

	for _, s := range []*stream{t.ctrl, t.civStream()} {
		if s == nil {
			continue
		}
		if deliver := t.tickStream(s, now); len(deliver) > 0 {
			t.deliverCIV(deliver)
		}
		if s.name == "civ" {
			t.tickCivWatchdog(s, now)
		}
	}

	// Staleness: a radio that stops answering ANYTHING is gone (keepalive
	// silence session loss; wfview STALE_CONNECTION posture).
	t.ctrl.mu.Lock()
	silentFor := now.Sub(t.ctrl.lastRx)
	t.ctrl.mu.Unlock()
	if silentFor > t.o.SessionTimeout {
		t.sessionLoss(fmt.Errorf("%w: no radio traffic for %s", ErrSessionLost, silentFor.Round(time.Millisecond)))
	}
}

// tickStream services one stream's idle keepalive, ping and retransmit
// request deadlines. Delivered payloads return to the caller so callbacks
// run without any lock held.
func (t *Transport) tickStream(s *stream, now time.Time) (deliver [][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.After(s.nextIdle) {
		_ = s.sendIdle(now) // tracked; also re-arms nextIdle
	}
	if now.After(s.nextPing) {
		_ = s.sendPing(now)
		s.nextPing = now.Add(t.o.PingPeriod)
	}
	if now.After(s.nextRetx) {
		s.nextRetx = now.Add(t.o.RetransmitPeriod)
		var reqs [][]byte
		deliver, reqs = s.rx.retxTick(t.o.RetransmitTries, t.o.MaxMissing, s.myID, s.remoteID)
		for _, r := range reqs {
			_ = s.sendRaw(r)
		}
	}
	return deliver
}

// tickCivWatchdog re-opens the CI-V data stream after CivSilence of no data
// (brief: "no CI-V data for 2 s -> re-send the start-data packet"), repeating
// at the start-data period until data flows again. Arming logs once at Warn;
// the repeat resends stay at Debug so a silent stream cannot flood the
// journal.
func (t *Transport) tickCivWatchdog(s *stream, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	silent := now.Sub(s.lastCivRx) > t.o.CivSilence
	if silent && s.openDue.IsZero() {
		s.openDue = now // watchdog arms the start-data resend loop
		t.log.Warn("CI-V watchdog: no CI-V data, re-opening the data stream",
			"stream", s.name, "silence", now.Sub(s.lastCivRx).Round(time.Millisecond))
	}
	if !s.openDue.IsZero() && now.After(s.openDue) {
		s.openDue = now.Add(t.o.StartDataPeriod)
		t.log.Debug("CI-V watchdog start-data resend", "stream", s.name)
		_ = s.sendTracked(openclosePacket(false, s.nextSubSeq(), s.myID, s.remoteID), now)
	}
}
