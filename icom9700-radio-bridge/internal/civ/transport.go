package civ

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Tuning defaults. Values follow kappanhang unless the research brief pins
// something else; deviations carry a comment.
const (
	defaultPingInterval = time.Second // brief says 500 ms, kappanhang 3 s ("only a ping-line packet") — midpoint; the radio's own ~100 ms pings carry the liveness load
	defaultLossWatchdog = 3 * time.Second
	defaultReauthEvery  = time.Minute     // token renewal (both references)
	defaultReauthWait   = 3 * time.Second // kappanhang reauthTimeout
	defaultHandshakeTO  = time.Second     // per-step expect (kappanhang expectTimeoutDuration)
	defaultAreYouThere  = 500 * time.Millisecond
	defaultCIVSilence   = 2 * time.Second // brief: re-open the data stream after 2 s of CI-V silence
	defaultTxRetention  = 3 * time.Second // 10× the 300 ms buffer announced to the radio
	defaultRxBuffer     = 100 * time.Millisecond
	announcedTxBufferMs = 300

	// readQueueLen bounds each stream's datagram queue (overflow drops the
	// newest datagram; the rx reorder buffer heals the gap by requesting
	// retransmission).
	readQueueLen = 256

	// maxRetransmitRequestPackets bounds a single retransmit request; larger
	// gaps are skipped by the rx buffer's lock-timeout flush instead
	// (kappanhang maxRetransmitRequestPacketCount).
	maxRetransmitRequestPackets = 10
)

// Options configures a client. Zero durations take the defaults; tests
// shrink the timers against a fake radio on localhost.
type Options struct {
	Host     string
	Username string
	Password string

	// RigName is echoed to the radio in the request-stream packet; the
	// radio's answer carries the authoritative self-description.
	RigName string

	// Radio-side ports; defaults ControlPort/CIVDataPort.
	ControlPort int
	CIVPort     int
	AudioPort   int

	// Local bind ports; 0 (default) binds an ephemeral port.
	BindControl int
	BindCIV     int
	BindAudio   int

	// NoCIVData skips the CI-V data stream (:50002) entirely: the session
	// carries the control stream and (on demand) the audio stream only, no
	// CI-V frames flow over LAN, and SendCIV fails. The receive-only bridge
	// sets this (2026-09 pivot — no LAN CI-V other than audio); the wire
	// spec confirms the audio stream does not depend on the CI-V data
	// stream (docs/civ-wire-spec.md §5/§16).
	NoCIVData bool

	PingInterval    time.Duration
	LossWatchdog    time.Duration // control-stream silence that ends the session
	ReauthInterval  time.Duration
	ReauthTimeout   time.Duration
	HandshakeTO     time.Duration
	AreYouThere     time.Duration
	CIVSilence      time.Duration
	TxRetention     time.Duration
	RxBuffer        time.Duration
	HandshakeBudget time.Duration // overall bound on Dial

	Logger *slog.Logger
}

func (o *Options) fillDefaults() error {
	if o.Host == "" {
		return errors.New("civ: host required — the protocol has no discovery")
	}
	if o.Username == "" {
		return errors.New("civ: username required")
	}
	if o.ControlPort == 0 {
		o.ControlPort = ControlPort
	}
	if o.CIVPort == 0 {
		o.CIVPort = CIVDataPort
	}
	if o.AudioPort == 0 {
		o.AudioPort = AudioPort
	}
	if o.RigName == "" {
		o.RigName = "IC-9700"
	}
	if o.PingInterval == 0 {
		o.PingInterval = defaultPingInterval
	}
	if o.LossWatchdog == 0 {
		o.LossWatchdog = defaultLossWatchdog
	}
	if o.ReauthInterval == 0 {
		o.ReauthInterval = defaultReauthEvery
	}
	if o.ReauthTimeout == 0 {
		o.ReauthTimeout = defaultReauthWait
	}
	if o.HandshakeTO == 0 {
		o.HandshakeTO = defaultHandshakeTO
	}
	if o.AreYouThere == 0 {
		o.AreYouThere = defaultAreYouThere
	}
	if o.CIVSilence == 0 {
		o.CIVSilence = defaultCIVSilence
	}
	if o.TxRetention == 0 {
		o.TxRetention = defaultTxRetention
	}
	if o.RxBuffer == 0 {
		o.RxBuffer = defaultRxBuffer
	}
	if o.HandshakeBudget == 0 {
		o.HandshakeBudget = 15 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return nil
}

// Client is a live protocol session against one radio: control stream,
// CI-V data stream, keepalives, token renewal and session-loss detection.
// Create with Dial; a Client that has reported loss must be discarded —
// reconnect is always a fresh Dial (R3: no resume, full re-login).
type Client struct {
	opts Options
	log  *slog.Logger

	control *udpStream
	civ     *udpStream
	audio   *udpStream      // audio receive stream; nil unless OpenAudio ran
	auth    authState

	wmu sync.Mutex // serializes all tracked sends + inner/outer seq bookkeeping

	done     chan struct{} // closed on loss or Close; stops all timers/pumps
	loseOnce sync.Once
	loseErr  error
	Lost     chan error // exactly one delivery

	frames chan []byte // ordered inbound CI-V frames

	audioFrames chan []byte // inbound audio PCM chunks (nil unless audio open)

	// handshake-phase events (reader goroutine → Dial)
	authAck chan struct{}
	a8Got   chan struct{}

	// liveness bookkeeping
	mu            sync.Mutex // lastControlRx
	lastControlRx time.Time

	smu        sync.Mutex // timers + reauthDead
	civSilent  *time.Timer
	reauthDead *time.Timer

	closeOnce sync.Once
	clean     atomic.Bool // Close/abort ran — teardown errors are not losses

	rigName string
}

// Dial performs the full handshake (are-you-there → ready → login → token →
// auth → stream request → CI-V stream open) and returns a live client. It
// never steals a session held by another client: a radio-side refusal
// surfaces as ErrConnectionRefused, a wrong password as ErrLoginRejected
// (R2).
func Dial(ctx context.Context, opts Options) (c *Client, err error) {
	if err := opts.fillDefaults(); err != nil {
		return nil, err
	}
	c = &Client{
		opts:    opts,
		log:     opts.Logger.With("component", "civ"),
		done:    make(chan struct{}),
		Lost:    make(chan error, 1),
		frames:  make(chan []byte, 128),
		authAck: make(chan struct{}, 1),
		a8Got:   make(chan struct{}, 1),
		auth:    *newAuthState(),
	}
	// Credentials never reach the log: host/ports/username only.
	c.log.Info("dialing radio", "host", opts.Host,
		"control_port", opts.ControlPort, "civ_port", opts.CIVPort,
		"username", opts.Username)

	// The whole handshake is bounded by HandshakeBudget (the are-you-there
	// retry loop would otherwise spin forever against a silent radio).
	ctx, cancel := context.WithTimeout(ctx, opts.HandshakeBudget)
	defer cancel()

	if c.control, err = c.dialStream("control", opts.ControlPort, opts.BindControl); err != nil {
		return nil, err
	}
	// The abort target must be a local: `return nil, err` paths nil the
	// named return BEFORE defers run, and a `c != nil` guard on the named
	// return would then skip abort — leaking the control socket (no close,
	// no disconnect) on every failed handshake (bench 2026-09-20: six
	// dead control sockets, each leaving the radio's session busy).
	cl := c
	defer func() {
		if err != nil {
			cl.abort()
		}
	}()
	c.control.startReader()

	// 1-2. are-you-there / ready exchange (pkt3/pkt4/pkt6).
	if err = c.control.start(ctx); err != nil {
		return nil, c.hsErr(err)
	}

	// 3. Login. The radio answers 0x60; ff ff ff fe there is a rejected
	// credential — surfaced verbatim, never retried silently.
	login, err := c.auth.buildLogin(c.control.localSID, c.control.remoteSID, opts.Username, opts.Password)
	if err != nil {
		return nil, err
	}
	if err = c.sendTracked(c.control, login); err != nil {
		return nil, c.hsErr(err)
	}
	r, ok := c.expectAny(c.control, ctx, opts.HandshakeTO, sigLoginAnswer, sigBusyReject)
	if !ok {
		return nil, c.hsErr(ErrHandshakeTimeout)
	}
	if prefixEqual(r, sigBusyReject) {
		// Body check: only the 20-byte 81 ff ff ff form is the busy
		// rejection (a hypothetical 0x14 retransmit variant has no such
		// body — none is documented anywhere).
		if len(r) == 20 && bytes.Equal(r[16:20], sigBusyBody) {
			return nil, c.hsErr(ErrLoginBusy)
		}
		return nil, c.hsErr(ErrHandshakeTimeout)
	}
	if err = c.auth.parseLoginAnswer(r); err != nil {
		return nil, err
	}

	// 4. First auth (0x02) — a single immediate auth (wfview's order); the
	// 0x05 renewal rides the periodic timer. The radio answers 0x40 with a
	// 0 response and volunteers its 0xa8 capabilities (the radio name).
	if err = c.sendTracked(c.control, c.auth.buildAuth(c.control.localSID, c.control.remoteSID, 0x02)); err != nil {
		return nil, c.hsErr(err)
	}
	c.startKeepalives(c.control)
	if err = c.awaitAuthAck(ctx); err != nil {
		return nil, c.hsErr(err)
	}

	// 5. Request the CI-V data stream. The radio volunteers a 0x90 conninfo
	// around this point (wfview treats it as the "you may connect" trigger);
	// drain it best-effort so it doesn't shadow the actual answer. The
	// answer itself is either a 0x50 status (error 0 = granted) or the
	// kappanhang-style 144-byte 0x90 with the OK flag.
	select {
	case <-c.control.readCh:
	case <-time.After(c.opts.HandshakeTO):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	req := c.auth.buildRequestStream(c.control.localSID, c.control.remoteSID,
		opts.Username, opts.CIVPort, AudioPort, announcedTxBufferMs)
	if err = c.sendTracked(c.control, req); err != nil {
		return nil, c.hsErr(err)
	}
	ans, ok := c.expectAny(c.control, ctx, opts.HandshakeTO, sigStatus, sigRequestAnswer)
	if !ok {
		return nil, c.hsErr(ErrHandshakeTimeout)
	}
	if prefixEqual(ans, sigStatus) {
		if okAns, refused := parseStatusAnswer(ans); refused {
			return nil, ErrConnectionRefused
		} else if !okAns {
			return nil, c.hsErr(ErrHandshakeTimeout)
		}
	} else if len(ans) <= requestOKFlag || ans[requestOKFlag] != 0x01 {
		return nil, c.hsErr(ErrHandshakeTimeout)
	}
	// wfview keeps the i-am-here identity; no SID refresh happens here.
	c.rigName = c.auth.devName
	if c.rigName == "" {
		c.rigName = opts.RigName
	}
	c.log.Info("ci-v stream granted", "radio", c.rigName)

	// 6. Open the CI-V data socket, run its pkt3/4/6 start, and send the
	// open packet. The stream carries its own session IDs (derived from its
	// own socket, exchanged with its own handshake). Skipped under
	// NoCIVData (receive-only: no LAN CI-V at all).
	if !opts.NoCIVData {
		if c.civ, err = c.dialStream("civ", opts.CIVPort, opts.BindCIV); err != nil {
			return nil, err
		}
		c.civ.startReader()
		if err = c.civ.start(ctx); err != nil {
			return nil, c.hsErr(err)
		}
		c.startKeepalives(c.civ)
		if err = c.sendTracked(c.civ, c.buildOpenClose(false)); err != nil {
			return nil, c.hsErr(err)
		}

		// Ordered CI-V delivery + the session/stream watchdogs.
		c.civ.rx = newRxSeqBuf(opts.RxBuffer, readQueueLen, c.civ.requestRetransmit)
		go c.civPump()
	}
	c.startWatchdogs()
	return c, nil
}

// hsErr prefers a session-ending fact the reader already recorded (e.g. the
// radio refusing during login) over the generic timeout.
func (c *Client) hsErr(err error) error {
	if c.loseErr != nil {
		return c.loseErr
	}
	return err
}

// awaitAuthAck waits for the radio to accept the 0x02 auth. Real IC-9700
// firmware answers with the volunteered 0xa8 capabilities packet (echoing
// the auth's tokrequest and token) — there is no 0x40 auth answer; a 0x40
// with response 0 is accepted too for compatibility. The 0xa8 also carries
// the radio name used by /meta.
func (c *Client) awaitAuthAck(ctx context.Context) error {
	deadline := time.After(c.opts.HandshakeBudget)
	for {
		select {
		case <-c.a8Got:
			return nil
		case <-c.authAck:
			return nil
		case <-deadline:
			return ErrHandshakeTimeout
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// expectAny scans for any of the given prefixes.
func (c *Client) expectAny(s *udpStream, ctx context.Context, wait time.Duration, prefixes ...[]byte) ([]byte, bool) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case r := <-s.readCh:
			for _, p := range prefixes {
				if prefixEqual(r, p) {
					return r, true
				}
			}
		case <-timer.C:
			return nil, false
		case <-ctx.Done():
			return nil, false
		case <-c.done:
			return nil, false
		}
	}
}

// buildOpenClose assembles the data-stream open/close packet (wfview
// icomUdpCivData::sendOpenClose): sub-header flag 0xc0, one data byte —
// 0x04 opens, 0x00 closes. (kappanhang's 0x05 open was accepted-and-ignored
// by the real IC-9700: it ACKed the packet but never started CI-V flow —
// commands came back as echoes with no replies, bench 2026-09-20.)
// Caller holds wmu.
func (c *Client) buildOpenClose(close bool) []byte {
	magic := byte(0x04)
	if close {
		magic = 0x00
	}
	p := header([]byte{0x16, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		c.civ.localSID, c.civ.remoteSID)
	p = append(p, 0xc0, 0x01, 0x00,
		byte(c.civ.innerSeq>>8), byte(c.civ.innerSeq), magic)
	c.civ.innerSeq++
	return p
}

// startKeepalives launches the stream's idle-packet sender (the reference
// client idles every 100 ms under load, decaying to 1 s when quiet; this
// client has no traffic of its own beyond cmds, so a fixed interval keeps
// the radio's liveness picture) and the ping ticker. Idles are TRACKED —
// wfview's civ-stream idles ride sendTrackedPacket, and a data stream with
// no tracked traffic never had CI-V flow opened on the real radio (bench
// 2026-09-20).
func (c *Client) startKeepalives(s *udpStream) {
	go func() {
		t := time.NewTicker(c.opts.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-t.C:
				// SIDs are snapshotted under wmu — Dial refreshes the
				// control stream's remote SID from the request answer
				// while this loop is already running.
				c.wmu.Lock()
				p := header(sigIdle, s.localSID, s.remoteSID)
				_ = s.sendTracked(p)
				c.wmu.Unlock()
			}
		}
	}()
	go func() {
		var pingID [4]byte
		pingID[3] = 0x06 // kappanhang's ping-ID family tag
		inner := uint16(0x8304)
		seq := uint16(1)
		t := time.NewTicker(c.opts.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-t.C:
				c.wmu.Lock()
				p := make([]byte, pingLen)
				copy(p, []byte{0x15, 0x00, 0x00, 0x00, pingFamily, 0x00})
				binary.BigEndian.PutUint32(p[8:12], s.localSID)
				binary.BigEndian.PutUint32(p[12:16], s.remoteSID)
				binary.LittleEndian.PutUint16(p[6:8], seq)
				p[16] = pingRequest
				pingID[1] = byte(inner)
				pingID[2] = byte(inner >> 8)
				copy(p[17:21], pingID[:])
				c.wmu.Unlock()
				inner++
				_ = s.send(p)
				seq++
			}
		}
	}()
}

// handlePing replies to radio ping requests (mandatory — the radio pings
// constantly) and ignores replies to our own.
func (c *Client) handlePing(s *udpStream, r []byte) {
	if r[16] != pingRequest {
		return
	}
	seq, replyID := parsePingRequest(r)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	p := make([]byte, pingLen)
	copy(p, []byte{0x15, 0x00, 0x00, 0x00, pingFamily, 0x00})
	binary.BigEndian.PutUint32(p[8:12], s.localSID)
	binary.BigEndian.PutUint32(p[12:16], s.remoteSID)
	binary.LittleEndian.PutUint16(p[6:8], seq)
	p[16] = pingReplyFlg
	copy(p[17:21], replyID)
	_ = s.send(p)
}

// handleControlPacket dispatches control-stream-only packet families once
// the handshake is over. Returns true when the packet was consumed; an
// unconsumed packet falls through to the handshake's expectAny.
func (c *Client) handleControlPacket(r []byte) bool {
	switch {
	case prefixEqual(r, sigAuthAnswer):
		if resp, ok := authResponse(r); ok {
			if resp == 0 {
				select {
				case c.authAck <- struct{}{}:
				default:
				}
				c.smu.Lock()
				if c.reauthDead != nil {
					c.reauthDead.Stop()
					c.reauthDead = nil
				}
				c.smu.Unlock()
			} else {
				c.lose(ErrConnectionRefused)
			}
		}
		return true
	case prefixEqual(r, sigA8Reply):
		c.auth.parseA8(r)
		select {
		case c.a8Got <- struct{}{}:
		default:
		}
		return true
	case prefixEqual(r, sigStatus):
		switch {
		case statusRadiosDisconnected(r):
			// Error 0 with the disc flag — the radio ended the session.
			c.lose(errors.New("civ: radio reported disconnect"))
			return true
		default:
			if _, refused := parseStatusAnswer(r); refused {
				c.lose(ErrConnectionRefused)
				return true
			}
			// A success 0x50 (e.g. the stream-request answer during the
			// handshake) is consumed by expectAny instead.
			return false
		}
	}
	return false
}

// civPump feeds data-stream datagrams into the reorder buffer and delivers
// ordered CI-V frames to Frames.
func (c *Client) civPump() {
	for {
		e, retryIn, err := c.civ.rx.next()
		switch {
		case err == nil && retryIn == 0:
			// Idles occupy sequence space but carry no CI-V payload —
			// consume them without delivering (and without feeding the
			// silence watchdog, which tracks CI-V frames only).
			if isData(e.data) {
				c.resetCIVSilence()
				frame := dataPayload(e.data)
				select {
				case c.frames <- frame:
				default:
					// Newest-wins: a full queue drops the OLDEST frame so
					// state assembly always sees the freshest radio truth.
					select {
					case <-c.frames:
					default:
					}
					select {
					case c.frames <- frame:
					default:
					}
				}
			}
			continue
		case errors.Is(err, errRxOutOfOrder):
			continue
		}
		// Buffer empty or locked waiting for a retransmit: wait for new
		// data, the lock retry, or shutdown.
		var lock *time.Timer
		if retryIn > 0 {
			lock = time.NewTimer(retryIn)
		}
		if lock != nil {
			select {
			case pkt := <-c.civ.readCh:
				lock.Stop()
				c.feedRx(pkt)
			case <-lock.C:
			case <-c.done:
				return
			}
			continue
		}
		select {
		case pkt := <-c.civ.readCh:
			c.feedRx(pkt)
		case <-c.done:
			return
		}
	}
}

// feedRx adds one data-stream datagram to the reorder buffer. Idles occupy
// sequence space too, so both families enter (kappanhang handleRead);
// anything else the radio put on this socket is dropped.
func (c *Client) feedRx(pkt []byte) {
	if isIdle(pkt) || isData(pkt) {
		c.civ.rx.add(dataSeq(pkt), pkt)
	}
}

// startWatchdogs arms the session-loss and stream-health timers.
func (c *Client) startWatchdogs() {
	c.mu.Lock()
	c.lastControlRx = time.Now()
	c.mu.Unlock()
	// Control-stream silence: the radio pings/idles constantly; a quiet
	// control stream for LossWatchdog means the session is gone (R3).
	go func() {
		t := time.NewTicker(c.opts.LossWatchdog / 3)
		defer t.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-t.C:
				c.mu.Lock()
				idle := time.Since(c.lastControlRx)
				c.mu.Unlock()
				if idle > c.opts.LossWatchdog {
					c.lose(fmt.Errorf("civ: control stream silent for %s", idle.Truncate(time.Millisecond)))
					return
				}
			}
		}
	}()

	// Token renewal every ReauthInterval, bounded by ReauthTimeout: an
	// unanswered renewal is session loss (R3; kappanhang reauth).
	c.smu.Lock()
	c.reauthDead = nil
	c.smu.Unlock()
	time.AfterFunc(c.opts.ReauthInterval, c.renewToken)

	// CI-V silence: re-send the open packet after CIVSilence without a
	// frame (the research brief's 2 s watchdog) — NOT session loss. No CI-V
	// data stream (NoCIVData) → nothing to keep alive here.
	c.resetCIVSilence()
}

// renewToken sends the 0x05 auth renewal and arms the renewal deadline;
// re-arms itself while the session lives.
func (c *Client) renewToken() {
	if c.isDone() {
		return
	}
	c.wmu.Lock()
	p := c.auth.buildAuth(c.control.localSID, c.control.remoteSID, 0x05)
	err := c.sendTrackedLocked(c.control, p)
	c.wmu.Unlock()
	if err == nil {
		c.smu.Lock()
		c.reauthDead = time.AfterFunc(c.opts.ReauthTimeout, func() {
			c.lose(errors.New("civ: token renewal unanswered"))
		})
		c.smu.Unlock()
	}
	if !c.isDone() {
		time.AfterFunc(c.opts.ReauthInterval, c.renewToken)
	}
}

// resetCIVSilence re-arms the CI-V silence watchdog: 2 s without a frame
// re-sends the open packet (the research brief's watchdog) — NOT session
// loss.
func (c *Client) resetCIVSilence() {
	c.smu.Lock()
	defer c.smu.Unlock()
	if c.isDone() {
		return
	}
	if c.civ == nil {
		return // NoCIVData session: no data stream to keep alive
	}
	if c.civSilent == nil {
		c.civSilent = time.AfterFunc(c.opts.CIVSilence, func() {
			c.wmu.Lock()
			err := c.sendTrackedLocked(c.civ, c.buildOpenClose(false))
			c.wmu.Unlock()
			if err == nil {
				c.log.Info("ci-v silence watchdog re-opened the data stream")
			}
			// Re-arm: with no frames flowing the watchdog keeps re-opening
			// the stream at the configured cadence until delivery resumes.
			c.smu.Lock()
			c.civSilent = nil
			c.smu.Unlock()
			if !c.isDone() {
				c.resetCIVSilence()
			}
		})
		return
	}
	c.civSilent.Reset(c.opts.CIVSilence)
}

// rxSeen stamps control-stream liveness (called from reader goroutines).
func (c *Client) rxSeen(stream string) {
	if stream == "control" {
		c.mu.Lock()
		c.lastControlRx = time.Now()
		c.mu.Unlock()
	}
}

// lose records the first session-ending fact exactly once. Safe from any
// goroutine. Suppressed after a clean Close: teardown errors are not
// session losses.
func (c *Client) lose(err error) {
	if c.clean.Load() {
		return
	}
	c.loseOnce.Do(func() {
		c.loseErr = err
		c.log.Warn("ci-v session lost", "err", err)
		select {
		case c.Lost <- err:
		default:
		}
		c.shutdown()
	})
}

// sendTracked is the mutex-guarded tracked send.
func (c *Client) sendTracked(s *udpStream, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.sendTrackedLocked(s, p)
}

func (c *Client) sendTrackedLocked(s *udpStream, p []byte) error {
	return s.sendTracked(p)
}

// SendCIV writes one raw CI-V frame (`FE FE ... FD`) to the data stream as
// a tracked packet. Callers must hold a live session. A NoCIVData session
// has no data stream — the send fails (no LAN CI-V exists, receive-only
// posture).
func (c *Client) SendCIV(payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.civ == nil {
		return errors.New("civ: no CI-V data stream (NoCIVData session)")
	}
	return c.civ.sendTrackedData(payload)
}

// Frames returns the ordered inbound CI-V frames from the radio (replies
// and transceive broadcasts).
func (c *Client) Frames() <-chan []byte { return c.frames }

// RadioName returns the radio's self-description from the stream-request
// answer (e.g. "IC-9700").
func (c *Client) RadioName() string { return c.rigName }

// Close tears the session down cleanly: close the data stream, deauth,
// brief grace for the radio's retransmit requests, then disconnect both
// streams (kappanhang deinit order). Idempotent. done closes FIRST so the
// reader goroutines exit quietly while the teardown packets are still
// being sent.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.clean.Store(true)
		c.stopTimers()
		c.markDone()
		// Data stream first: close packet, then its disconnect one-shot.
		if c.civ != nil && c.civ.conn != nil {
			_ = c.sendTracked(c.civ, c.buildOpenClose(true))
			c.civ.disconnect()
			_ = c.civ.conn.Close()
		}
		// Deauth (0x01), then give the radio ~500 ms to ask for
		// retransmits before the control socket disappears.
		if c.control != nil && c.control.conn != nil {
			if c.auth.gotToken {
				_ = c.sendTracked(c.control, c.auth.buildAuth(c.control.localSID, c.control.remoteSID, 0x01))
				time.Sleep(500 * time.Millisecond)
			}
			c.control.disconnect()
			_ = c.control.conn.Close()
		}
	})
}

// abort tears down after a failed handshake (no clean disconnect is
// possible — the radio never granted the session). The control disconnect
// (type 0x05) still goes out: bench 2026-09-20 showed the radio keeps the
// control connection established (pinging a dead socket, staying busy for
// every later login) when an attempt just vanishes.
func (c *Client) abort() {
	c.closeOnce.Do(func() {
		c.clean.Store(true)
		c.stopTimers()
		if c.civ != nil && c.civ.conn != nil {
			_ = c.civ.conn.Close()
		}
		if c.control != nil && c.control.conn != nil {
			c.control.disconnect()
			_ = c.control.conn.Close()
		}
		c.shutdown()
	})
}

func (c *Client) stopTimers() {
	c.smu.Lock()
	if c.civSilent != nil {
		c.civSilent.Stop()
		c.civSilent = nil
	}
	if c.reauthDead != nil {
		c.reauthDead.Stop()
		c.reauthDead = nil
	}
	c.smu.Unlock()
}

// markDone closes done exactly once, stopping every timer/pump and waking
// all readers.
func (c *Client) markDone() {
	c.smu.Lock()
	defer c.smu.Unlock()
	if !c.isDone() {
		close(c.done)
	}
}

// shutdown closes done and the sockets, waking all readers.
func (c *Client) shutdown() {
	c.markDone()
	c.closeAudioLocked()
	if c.civ != nil && c.civ.conn != nil {
		_ = c.civ.conn.Close()
	}
	if c.control != nil && c.control.conn != nil {
		_ = c.control.conn.Close()
	}
}

func (c *Client) isDone() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}
