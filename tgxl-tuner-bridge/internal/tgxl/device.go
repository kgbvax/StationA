package tgxl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	writeTimeout = 3 * time.Second
	replyTimeout = 2 * time.Second
	bannerWait   = 5 * time.Second

	// defaultReadTimeout bounds the silence on the link. The bridge polls
	// `status` every few hundred ms and the TGXL answers each one, so silence
	// beyond this means the link is dead: Run returns and the outer loop
	// reconnects (atr1k review S4: a dead link must not leave device_online
	// true with frozen values).
	defaultReadTimeout = 3 * time.Second

	// tuneStartGrace: a memory recall finishes in <20 ms and may never show
	// tuning=1 in a poll; if tuning has not been seen this long after autotune
	// we assume it completed and clear settling.
	// statusStale: an unanswered status request is abandoned after this.
	statusStale = time.Second

	tuneStartGrace = 3 * time.Second
	// tuneTimeout: tuning=1 held longer than this is a fault (first-time
	// solutions take 2-12 s per the user guide).
	tuneTimeout = 20 * time.Second
)

// Commander is the tuner control surface the bridge drives from /cmd.
type Commander interface {
	SetInline(inline bool) error // in line (operate, not bypassed) vs bypass
	SetOperate(operate bool) error
	SetAntenna(ant int) error // 1..3
	Tune() error              // autotune
}

// Logger is the minimal logging surface the device uses.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

// State is the canonical tuner state the bridge publishes (integration model
// §7.1, slot muehle/hf/tuner).
type State struct {
	Inline       bool    `json:"inline"`  // in the RF path: operate and not bypassed
	Operate      bool    `json:"operate"` // OPERATE (true) vs STANDBY (false, antennas disconnected)
	Antenna      int     `json:"antenna"` // selected antenna 1..3
	SWR          float64 `json:"swr"`     // ratio, from the API's return-loss figure
	Fwd          uint16  `json:"fwd"`     // forward watts, from dBm
	PTT          bool    `json:"ptt"`
	Settling     bool    `json:"settling"` // a tune cycle is in progress
	Fault        string  `json:"fault,omitempty"`
	Message      string  `json:"message,omitempty"` // TGXL warning/info text (M| line)
	RelayC1      int     `json:"relay_c1"`          // diagnostic 0-255
	RelayL       int     `json:"relay_l"`
	RelayC2      int     `json:"relay_c2"`
	DeviceOnline bool    `json:"device_online"`
	Error        string  `json:"error,omitempty"`
}

// Info is what the `info` command reports.
type Info struct {
	Serial, Version, Nickname string
	ThreeWay                  bool
}

// Device owns the TCP connection to the TGXL.
type Device struct {
	addr     string
	authCode string
	poll     time.Duration // status interval while receiving
	txPoll   time.Duration // status interval while PTT is asserted
	debug    bool
	log      Logger

	readTimeout time.Duration
	seq         atomic.Int64
	pollKick    chan struct{} // PTT changed: re-arm the poll timer at the new rate
	inflight    atomic.Int64  // unix-nano of the outstanding status request, 0 = none

	writeMu sync.Mutex
	conn    net.Conn

	pmu     sync.Mutex
	pending map[int]chan Line

	mu          sync.Mutex // guards state + tune tracking + callbacks
	state       State
	tuneAt      time.Time // autotune requested
	tuningSince time.Time // tuning=1 first seen (zero = not seen)
	onTelemetry func(State)
	onInfo      func(Info)
	lastRaw     RawStatus // last full status, base for `state` pushes
}

// New constructs a Device for host:port.
func New(addr, authCode string, poll, txPoll time.Duration, debug bool, log Logger) *Device {
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	if txPoll <= 0 || txPoll > poll {
		txPoll = poll
	}
	return &Device{addr: addr, authCode: authCode, poll: poll, txPoll: txPoll, debug: debug, log: log,
		readTimeout: defaultReadTimeout, pollKick: make(chan struct{}, 1), pending: map[int]chan Line{}}
}

// SetInfoHandler registers a callback for the one-time `info` result of each
// connection (the bridge puts serial/firmware in /meta).
func (d *Device) SetInfoHandler(f func(Info)) {
	d.mu.Lock()
	d.onInfo = f
	d.mu.Unlock()
}

// Snapshot returns a copy of the current state.
func (d *Device) Snapshot() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

// Run dials the TGXL, handles the banner/auth, then polls `status` and reads
// lines until the link fails or ctx is cancelled. Returns the ending error.
func (d *Device) Run(ctx context.Context, onTelemetry func(State)) error {
	d.mu.Lock()
	d.onTelemetry = onTelemetry
	d.mu.Unlock()

	dialer := net.Dialer{Timeout: 10 * time.Second}
	d.mu.Lock()
	addr := d.addr
	d.mu.Unlock()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial tgxl: %w", err)
	}
	d.writeMu.Lock()
	d.conn = conn
	d.writeMu.Unlock()
	defer d.closeConn()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			d.closeConn()
		case <-stop:
		}
	}()

	rd := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(bannerWait))
	banner, err := rd.ReadString('\n')
	if err != nil {
		return fmt.Errorf("tgxl banner: %w", err)
	}
	banner = strings.TrimSpace(banner)
	d.log.Infof("TGXL connected: %s (%s)", addr, banner)
	if strings.Contains(banner, "AUTH") {
		if d.authCode == "" {
			return errors.New("tgxl requires auth but tuner.auth_code is not configured")
		}
		if err := d.sendRaw(fmt.Sprintf("auth %s", d.authCode)); err != nil {
			return err
		}
	}

	d.setOnline(true, "")

	// Reader dispatches replies to waiting callers; this goroutine below does
	// info + polling so the read loop never blocks on a command.
	go d.initAndPoll(ctx, stop)

	for {
		_ = conn.SetReadDeadline(time.Now().Add(d.readTimeout))
		raw, err := rd.ReadString('\n')
		if err != nil {
			return fmt.Errorf("tgxl read: %w", err)
		}
		if d.debug {
			d.log.Debugf("RX from TGXL: %q", raw)
		}
		ln, perr := ParseLine(raw)
		if perr != nil {
			d.log.Debugf("tgxl: %v", perr)
			continue
		}
		d.dispatch(ln)
	}
}

func (d *Device) initAndPoll(ctx context.Context, stop <-chan struct{}) {
	if r, err := d.call("info"); err == nil && r.Code == 0 {
		_, kv := ParseKV(r.Msg)
		inf := Info{Serial: kv["serial"], Version: kv["version"],
			Nickname: strings.ReplaceAll(kv["nickname"], "_", " "), ThreeWay: kv["3way"] == "1"}
		d.log.Infof("TGXL info: serial=%s version=%s 3way=%v", inf.Serial, inf.Version, inf.ThreeWay)
		if !inf.ThreeWay {
			d.log.Warnf("TGXL did not report 3way=1: this bridge targets the 1x3 variant (SO2R channel switching is not implemented)")
		}
		d.mu.Lock()
		cb := d.onInfo
		d.mu.Unlock()
		if cb != nil {
			cb(inf)
		}
	} else if err != nil {
		d.log.Warnf("TGXL info: %v", err)
	}
	t := time.NewTimer(d.pollInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-d.pollKick:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(d.pollInterval())
		case <-t.C:
			// One status request in flight at a time (a lost reply is abandoned
			// after statusStale), so a fast TX rate never queues requests.
			if at := d.inflight.Load(); at == 0 || time.Since(time.Unix(0, at)) > statusStale {
				d.inflight.Store(time.Now().UnixNano())
				if err := d.sendRaw("status"); err != nil {
					d.closeConn() // unblocks the reader; Run returns the read error
					return
				}
			}
			t.Reset(d.pollInterval())
		}
	}
}

// pollInterval is the fast TX rate while PTT is asserted (a keyed carrier moves
// fwd every few ms, and voice peaks are missed at 250 ms), else the RX rate.
func (d *Device) pollInterval() time.Duration {
	d.mu.Lock()
	ptt := d.state.PTT
	d.mu.Unlock()
	if ptt {
		return d.txPoll
	}
	return d.poll
}

func (d *Device) dispatch(ln Line) {
	switch ln.Kind {
	case KindReply:
		d.pmu.Lock()
		ch := d.pending[ln.Seq]
		d.pmu.Unlock()
		if ch != nil {
			select {
			case ch <- ln:
			default:
			}
		}
	case KindStatus:
		d.inflight.Store(0)
		if name, kv := ParseKV(ln.Msg); name == "state" {
			d.handleStatePush(kv, time.Now())
			return
		}
		st, err := ParseStatus(ln.Msg)
		if err != nil {
			d.log.Warnf("tgxl status: %v", err)
			return
		}
		d.handleStatus(st, time.Now())
	case KindMessage:
		d.mu.Lock()
		d.state.Message = ln.Msg
		snap := d.state
		d.mu.Unlock()
		if ln.Msg != "" {
			d.log.Infof("TGXL message: %s", ln.Msg)
		}
		d.push(snap)
	}
}

// handleStatePush applies an unsolicited `S0|state k=v ...` frame (sent on
// state changes: tuning, bypass, operate, antenna, relays). It carries no
// meters, so it overlays only the keys present onto the last full status.
func (d *Device) handleStatePush(kv map[string]string, now time.Time) {
	d.mu.Lock()
	st := d.lastRaw
	d.mu.Unlock()
	num := func(k string) (int, bool) {
		v, ok := kv[k]
		if !ok {
			return 0, false
		}
		n, err := strconv.Atoi(v)
		return n, err == nil
	}
	if n, ok := num("state"); ok {
		st.Operate = n == 1
	}
	if n, ok := num("bypass"); ok {
		st.Bypass = n == 1
	}
	if n, ok := num("tuning"); ok {
		st.Tuning = n == 1
	}
	if n, ok := num("antA"); ok {
		st.Antenna = n
	}
	if n, ok := num("relayC1"); ok {
		st.RelayC1 = n
	}
	if n, ok := num("relayL"); ok {
		st.RelayL = n
	}
	if n, ok := num("relayC2"); ok {
		st.RelayC2 = n
	}
	d.handleStatus(st, now)
}

// handleStatus folds one status poll into the canonical state.
func (d *Device) handleStatus(st RawStatus, now time.Time) {
	d.mu.Lock()
	d.lastRaw = st
	s := &d.state
	if s.PTT != st.PTT {
		select {
		case d.pollKick <- struct{}{}:
		default:
		}
	}
	s.Operate = st.Operate
	s.Inline = st.Operate && !st.Bypass
	s.Antenna = st.Antenna
	s.SWR = ReturnLossToSWR(st.SWRdB)
	s.Fwd = DBmToWatts(st.FwdDBm)
	s.PTT = st.PTT
	s.RelayC1, s.RelayL, s.RelayC2 = st.RelayC1, st.RelayL, st.RelayC2

	switch {
	case st.Tuning:
		if d.tuningSince.IsZero() {
			d.tuningSince = now
		}
		s.Settling = true
		if now.Sub(d.tuningSince) > tuneTimeout {
			s.Settling = false
			s.Fault = "tune timeout"
			d.log.Warnf("tuner tune timeout")
		}
	case !d.tuningSince.IsZero():
		// tuning ended
		d.tuningSince, d.tuneAt = time.Time{}, time.Time{}
		s.Settling = false
	case !d.tuneAt.IsZero() && now.Sub(d.tuneAt) > tuneStartGrace:
		// requested but never observed: a fast memory recall
		d.tuneAt = time.Time{}
		s.Settling = false
	case d.tuneAt.IsZero():
		s.Settling = false
	}
	snap := *s
	d.mu.Unlock()
	d.push(snap)
}

// SetInline puts the tuner in line (operate + not bypassed) or bypasses it.
// Standby is never used for "bypass": it disconnects all antennas.
func (d *Device) SetInline(inline bool) error {
	if !inline {
		return d.cmd("bypass set=1")
	}
	if !d.Snapshot().Operate {
		if err := d.cmd("operate set=1"); err != nil {
			return err
		}
	}
	return d.cmd("bypass set=0")
}

// SetOperate switches OPERATE / STANDBY.
func (d *Device) SetOperate(operate bool) error {
	v := 0
	if operate {
		v = 1
	}
	return d.cmd(fmt.Sprintf("operate set=%d", v))
}

// SetAntenna selects antenna 1..3 (1x3 variant).
func (d *Device) SetAntenna(ant int) error {
	if ant < 1 || ant > 3 {
		return fmt.Errorf("antenna %d out of range 1..3", ant)
	}
	return d.cmd(fmt.Sprintf("activate ant=%d", ant))
}

// Tune starts an autotune. NOTE: the TGXL keys the FLEX itself over the LAN;
// there is no PA gate here (see CLAUDE.md hazard).
func (d *Device) Tune() error {
	// Raise settling before the carrier so the PLC #1 gate engages first.
	d.mu.Lock()
	d.tuneAt = time.Now()
	d.tuningSince = time.Time{}
	d.state.Settling = true
	d.state.Fault = ""
	snap := d.state
	d.mu.Unlock()
	d.push(snap)
	if err := d.cmd("autotune"); err != nil {
		d.mu.Lock()
		d.tuneAt = time.Time{}
		d.state.Settling = false
		snap = d.state
		d.mu.Unlock()
		d.push(snap)
		return err
	}
	return nil
}

// cmd runs a command and turns a non-zero reply into a fault.
func (d *Device) cmd(c string) error {
	r, err := d.call(c)
	if err != nil {
		return err
	}
	name, _, _ := strings.Cut(c, " ")
	d.mu.Lock()
	if r.Code != 0 {
		d.state.Fault = fmt.Sprintf("%s: code %d", name, r.Code)
	} else if strings.HasPrefix(d.state.Fault, name+": ") {
		d.state.Fault = ""
	}
	snap := d.state
	d.mu.Unlock()
	d.push(snap)
	if r.Code != 0 {
		return fmt.Errorf("tgxl %s: code %d %s", name, r.Code, r.Msg)
	}
	return nil
}

// call sends a command and waits for its R reply.
func (d *Device) call(c string) (Line, error) {
	seq := int(d.seq.Add(1))
	ch := make(chan Line, 1)
	d.pmu.Lock()
	d.pending[seq] = ch
	d.pmu.Unlock()
	defer func() {
		d.pmu.Lock()
		delete(d.pending, seq)
		d.pmu.Unlock()
	}()
	if err := d.write(seq, c); err != nil {
		return Line{}, err
	}
	t := time.NewTimer(replyTimeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r, nil
	case <-t.C:
		return Line{}, fmt.Errorf("tgxl %q: reply timeout", c)
	}
}

func (d *Device) sendRaw(c string) error {
	return d.write(int(d.seq.Add(1)), c)
}

func (d *Device) write(seq int, c string) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	if d.conn == nil {
		return errors.New("tgxl: not connected")
	}
	_ = d.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := d.conn.Write([]byte(FormatCmd(seq, c))); err != nil {
		return fmt.Errorf("tgxl write: %w", err)
	}
	if d.debug {
		d.log.Debugf("TX to TGXL: %q", FormatCmd(seq, c))
	}
	return nil
}

// setOnline flips device_online, keeping the last measurements.
func (d *Device) setOnline(online bool, errMsg string) {
	d.mu.Lock()
	d.state.DeviceOnline = online
	d.state.Error = errMsg
	if !online {
		d.state.Settling = false
		d.tuneAt, d.tuningSince = time.Time{}, time.Time{}
	}
	snap := d.state
	d.mu.Unlock()
	d.push(snap)
}

func (d *Device) closeConn() {
	d.writeMu.Lock()
	c := d.conn
	d.conn = nil
	d.writeMu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

func (d *Device) push(snap State) {
	d.mu.Lock()
	cb := d.onTelemetry
	d.mu.Unlock()
	if cb != nil {
		cb(snap)
	}
}

// SetAddr overrides the dial address (used after UDP discovery).
func (d *Device) SetAddr(addr string) {
	d.mu.Lock()
	d.addr = addr
	d.mu.Unlock()
}
