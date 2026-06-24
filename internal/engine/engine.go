// Package engine is the headless control core. It owns the connection to the
// PTZ, the position-polling loop, command execution, and cable-wrap protection,
// and runs with or without a UI: the TUI is a thin view/controller over it and
// the daemon runs it alone.
//
// The engine is an actor: a single goroutine owns all mutable state and is the
// only writer to the port. Callers interact through thread-safe methods that
// enqueue work onto that goroutine, and read a published snapshot via Snapshot.
package engine

import (
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"pelcots/internal/config"
	"pelcots/internal/control"
	"pelcots/internal/pelco"
	"pelcots/internal/serialio"
)

const (
	pollInterval     = 400 * time.Millisecond // query + jog keepalive cadence
	movePollInterval = 100 * time.Millisecond // faster cadence during a managed unwrap
	jogKeySpeed      = 0x20                   // fixed speed for keyboard/unwrap jogging
	moveTolerance    = 2.0                    // degrees: how close to the target ends an unwrap
	logSize          = 200                    // TX/RX ring-buffer depth
)

// ConnSpec describes an outbound connection target.
type ConnSpec struct {
	Transport  string
	SerialPort string
	Baud       int
	TCPAddr    string
}

// Options configures a new Engine.
type Options struct {
	Transport   string
	SerialPort  string
	TCPAddr     string
	Baud        int
	Addr        byte
	Bind        string
	WrapEnabled bool
	WrapLimit   float64
	WrapAccum   float64
	GS232       config.ServerConfig
	Rotctld     config.ServerConfig
	Logw        io.Writer
	LogLevel    config.LogLevel
}

// State is an immutable snapshot of the engine for rendering / persistence.
type State struct {
	Status                string
	Connected             bool
	Transport             string
	Endpoint              string // active transport's endpoint
	SerialPort            string
	TCPAddr               string
	Baud                  int
	Addr                  byte
	HavePan, HaveTilt     bool
	CurPan, CurTilt       float64
	CurPanRaw, CurTiltRaw uint16
	LastPan, LastTilt     time.Time
	BytesIn               int
	Jogging               bool
	JogPan, JogTilt       int
	Gotoing               bool
	Unwrapping            bool
	WrapEnabled           bool
	WrapLimit             float64
	Wrap                  float64
	Bind                  string
	GS232On               bool
	GS232Port             int
	RotctldOn             bool
	RotctldPort           int
	Log                   []string
}

// Engine is the headless control core. Construct with New, start with Start,
// and shut down with Close.
type Engine struct {
	// connection config (mutated only in the actor goroutine)
	transport  string
	serialPort string
	tcpAddr    string
	baud       int
	addr       byte
	bind       string

	// cable-wrap protection
	wrapEnabled bool
	wrapLimit   float64
	wrap        float64

	// desired servers at startup
	gs232Want   config.ServerConfig
	rotctldWant config.ServerConfig

	// runtime connection
	port     *serialio.Port
	frames   <-chan serialio.Event
	ticker   *time.Ticker
	pollFast bool

	reqs chan func()
	quit chan struct{}
	once sync.Once

	// readback
	curPan, curTilt       float64
	curPanRaw, curTiltRaw uint16
	havePan, haveTilt     bool
	lastPan, lastTilt     time.Time
	bytesIn               int

	// motion state
	jogDir                     pelco.Direction
	jogPan, jogTilt            int
	jogging                    bool
	gotoing                    bool
	gotoTargetAz, gotoTargetEl float64
	mvActive                   bool // closed-loop unwrap in progress
	mvDir                      int
	mvWrapStart, mvTravel      float64
	unwrapping                 bool

	// inbound servers
	gs232Srv, rotctldSrv   *control.Server
	gs232Port, rotctldPort int
	gs232On, rotctldOn     bool

	pos *control.Pos // latest position published for the servers

	logw     io.Writer
	logLevel config.LogLevel
	logLines []string
	status   string

	mu    sync.Mutex
	state State
}

// New builds an Engine from the given options (does not connect yet).
func New(o Options) *Engine {
	e := &Engine{
		transport:   o.Transport,
		serialPort:  o.SerialPort,
		tcpAddr:     o.TCPAddr,
		baud:        o.Baud,
		addr:        o.Addr,
		bind:        o.Bind,
		wrapEnabled: o.WrapEnabled,
		wrapLimit:   o.WrapLimit,
		wrap:        o.WrapAccum,
		gs232Want:   o.GS232,
		rotctldWant: o.Rotctld,
		gs232Port:   o.GS232.Port,
		rotctldPort: o.Rotctld.Port,
		pos:         &control.Pos{},
		logw:        o.Logw,
		logLevel:    o.LogLevel,
		reqs:        make(chan func(), 64),
		quit:        make(chan struct{}),
		status:      "starting",
	}
	if e.wrapLimit <= 0 {
		e.wrapLimit = 270
	}
	if e.addr < 1 {
		e.addr = 1
	}
	return e
}

// Pos returns the thread-safe latest-position holder (for inbound servers).
func (e *Engine) Pos() *control.Pos { return e.pos }

// Start launches the actor goroutine: it connects, starts any enabled servers,
// and begins polling.
func (e *Engine) Start() { go e.run() }

// Close stops motion, shuts down servers and the port, and stops the loop.
func (e *Engine) Close() error {
	e.once.Do(func() {
		e.do(func() {
			e.stop()
			e.closeServers()
			if e.port != nil {
				_ = e.port.Close()
			}
		})
		close(e.quit)
	})
	return nil
}

// do enqueues f to run on the actor goroutine (no-op if the engine is stopping).
func (e *Engine) do(f func()) {
	select {
	case e.reqs <- f:
	case <-e.quit:
	}
}

func (e *Engine) run() {
	e.reconnect(ConnSpec{Transport: e.transport, SerialPort: e.serialPort, Baud: e.baud, TCPAddr: e.tcpAddr})
	e.setServer(control.GS232, e.gs232Want.Enabled, e.gs232Want.Port)
	e.setServer(control.Rotctld, e.rotctldWant.Enabled, e.rotctldWant.Port)
	e.ticker = time.NewTicker(pollInterval)
	defer e.ticker.Stop()
	e.publish()
	for {
		select {
		case <-e.quit:
			for { // drain pending requests (incl. Close cleanup), then exit
				select {
				case f := <-e.reqs:
					f()
				default:
					return
				}
			}
		case f := <-e.reqs:
			f()
			e.publish()
		case <-e.ticker.C:
			e.poll()
			e.publish()
		case ev, ok := <-e.frames: // nil channel when disconnected → never selected
			e.handleFrame(ev, ok)
			e.publish()
		}
	}
}

// --- public command API ---------------------------------------------------

// Submit executes a decoded network command (runs to completion).
func (e *Engine) Submit(c control.Command) { e.do(func() { e.exec(c) }) }

// Jog starts continuous motion in the given pan/tilt direction (UI hold-to-move).
func (e *Engine) Jog(pan, tilt int, turbo bool) { e.do(func() { e.jog(pan, tilt, turbo) }) }

// Goto commands an absolute move to (az, el) degrees, applying wrap protection.
func (e *Engine) Goto(az, el float64) { e.do(func() { e.startGoto(az, el) }) }

// Home commands a move to (0, 0).
func (e *Engine) Home() { e.do(func() { e.startGoto(0, 0) }) }

// StopMotion halts all motion.
func (e *Engine) StopMotion() { e.do(func() { e.stop() }) }

// ZeroWrap resets the cable-wind accumulator to zero (cable manually centered).
func (e *Engine) ZeroWrap() {
	e.do(func() {
		e.wrap = 0
		e.status = "wrap re-zeroed"
	})
}

// SetWrap enables/disables wrap protection and sets the ± limit (degrees).
func (e *Engine) SetWrap(enabled bool, limit float64) {
	e.do(func() {
		e.wrapEnabled = enabled
		if limit > 0 {
			e.wrapLimit = limit
		}
	})
}

// SetAddr changes the Pelco-D camera address.
func (e *Engine) SetAddr(a byte) {
	e.do(func() {
		if a >= 1 {
			e.addr = a
		}
	})
}

// Reconnect closes the current link and opens a new one per spec.
func (e *Engine) Reconnect(spec ConnSpec) { e.do(func() { e.reconnect(spec) }) }

// SetServer starts or stops the given inbound-control server on a port.
func (e *Engine) SetServer(proto control.Protocol, enabled bool, port int) {
	e.do(func() { e.setServer(proto, enabled, port) })
}

// Snapshot returns the latest published engine state.
func (e *Engine) Snapshot() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

// --- actor-goroutine internals --------------------------------------------

func (e *Engine) exec(c control.Command) {
	switch c.Kind {
	case control.KindStop:
		e.stop()
	case control.KindSetPos:
		e.startGoto(c.Az, c.El)
	case control.KindJog:
		e.jog(c.Pan, c.Tilt, false)
	}
}

func (e *Engine) jog(pan, tilt int, turbo bool) {
	e.jogDir, e.jogging, e.gotoing, e.mvActive, e.unwrapping = pelco.Direction{Pan: pan, Tilt: tilt}, true, false, false, false
	e.setMovePoll(false)
	panS, tiltS := jogKeySpeed, jogKeySpeed
	if turbo {
		panS, tiltS = pelco.TurboSpeed, pelco.MaxSpeed
	}
	e.jogPan, e.jogTilt = 0, 0
	if pan != 0 {
		e.jogPan = panS
	}
	if tilt != 0 {
		e.jogTilt = tiltS
	}
	e.send(pelco.Jog(e.addr, e.jogDir.Cmd2(), e.jogPan, e.jogTilt))
}

// startGoto commands an absolute move, applying cable-wrap protection to the
// azimuth axis. Elevation is always set directly. Repeated identical calls
// (e.g. from key-repeat while held) are idempotent so an in-flight unwrap is
// not restarted.
func (e *Engine) startGoto(az, el float64) {
	if e.gotoing && e.gotoTargetAz == az && e.gotoTargetEl == el {
		return // already heading there; let it run
	}
	if e.wrapEnabled {
		if !e.havePan {
			e.status = "wrap: no position readback yet — move blocked"
			return
		}
		plan := planMove(e.wrap, e.curPan, az, e.wrapLimit)
		switch plan.Kind {
		case MoveBlock:
			e.status = fmt.Sprintf("wrap: %.0f° unreachable within ±%.0f°", az, e.wrapLimit)
			e.logf(config.LogWarn, "wrap: blocked goto %.0f° (wind %+.0f°, limit ±%.0f°)", az, e.wrap, e.wrapLimit)
			return
		case MoveUnwrap:
			e.beginUnwrap(az, el, plan)
			return
		}
	}
	// Wrap disabled, or a short/none move: command absolute position directly.
	e.jogging, e.mvActive, e.unwrapping = false, false, false
	e.gotoing, e.gotoTargetAz, e.gotoTargetEl = true, az, el
	e.setMovePoll(false)
	e.send(pelco.SetPan(e.addr, az))
	e.send(pelco.SetTilt(e.addr, el))
}

// beginUnwrap drives the long way round under closed-loop control: jog the pan
// in the planned direction (re-sent by poll) until the accumulator has moved
// the planned distance, then stop. Elevation is set absolutely up front.
func (e *Engine) beginUnwrap(az, el float64, plan MovePlan) {
	e.jogging = false
	e.gotoing, e.gotoTargetAz, e.gotoTargetEl = true, az, el
	e.mvActive, e.unwrapping = true, true
	e.mvDir = plan.Dir
	e.mvWrapStart = e.wrap
	e.mvTravel = math.Abs(plan.Travel)
	e.status = fmt.Sprintf("wrap: unwinding %s to reach %.0f°", dirArrow(plan.Dir), az)
	e.logf(config.LogInfo, "wrap: unwinding %s %.0f° to reach %.0f°", dirArrow(plan.Dir), e.mvTravel, az)
	e.send(pelco.SetTilt(e.addr, el))
	e.send(pelco.Jog(e.addr, pelco.Direction{Pan: plan.Dir}.Cmd2(), jogKeySpeed, 0))
	e.setMovePoll(true)
}

// stepMove ends a closed-loop unwrap once the accumulator has travelled far
// enough. Called after each pan readback is integrated.
func (e *Engine) stepMove() {
	if !e.mvActive {
		return
	}
	if math.Abs(e.wrap-e.mvWrapStart) >= e.mvTravel-moveTolerance {
		e.send(pelco.Stop(e.addr))
		e.mvActive, e.gotoing, e.unwrapping = false, false, false
		e.setMovePoll(false)
		e.status = "wrap unwind complete"
		e.logf(config.LogInfo, "wrap: unwind complete (wind %+.0f°)", e.wrap)
	}
}

func (e *Engine) stop() {
	e.jogging = false
	e.jogPan, e.jogTilt = 0, 0
	e.jogDir = pelco.Direction{}
	e.gotoing, e.mvActive, e.unwrapping = false, false, false
	e.setMovePoll(false)
	e.send(pelco.Stop(e.addr))
}

func (e *Engine) poll() {
	if e.port == nil {
		return
	}
	e.send(pelco.QueryPan(e.addr))
	e.send(pelco.QueryTilt(e.addr))
	if e.jogging {
		e.send(pelco.Jog(e.addr, e.jogDir.Cmd2(), e.jogPan, e.jogTilt))
	}
	if e.mvActive {
		e.send(pelco.Jog(e.addr, pelco.Direction{Pan: e.mvDir}.Cmd2(), jogKeySpeed, 0))
	}
}

func (e *Engine) handleFrame(ev serialio.Event, ok bool) {
	if !ok {
		e.status = "port closed"
		e.logf(config.LogWarn, "port closed")
		e.frames = nil
		return
	}
	switch {
	case ev.Raw != nil:
		e.bytesIn += len(ev.Raw)
		e.logf(config.LogTrace, "RX  raw % X", ev.Raw)
	case ev.Err != nil:
		e.logf(config.LogWarn, "RX  ! %v", ev.Err)
	case ev.Frame.IsPanResponse():
		raw := ev.Frame.Word()
		newPan := pelco.HundredthsToDeg(raw)
		if e.havePan { // integrate observed travel into the cable-wind accumulator
			e.wrap += shortestDelta(e.curPan, newPan)
		}
		e.curPanRaw, e.curPan, e.havePan, e.lastPan = raw, newPan, true, time.Now()
		e.pos.Set(e.curPan, e.curTilt)
		e.logf(config.LogDebug, "RX  pan=%.2f° (raw %d)", e.curPan, raw)
		e.stepMove()
	case ev.Frame.IsTiltResponse():
		raw := ev.Frame.Word()
		e.curTiltRaw, e.curTilt, e.haveTilt, e.lastTilt = raw, pelco.HundredthsToDeg(raw), true, time.Now()
		e.pos.Set(e.curPan, e.curTilt)
		e.logf(config.LogDebug, "RX  tilt=%.2f° (raw %d)", e.curTilt, raw)
	default:
		e.logf(config.LogTrace, "RX  % X", ev.Frame.Bytes())
	}
}

func (e *Engine) reconnect(spec ConnSpec) {
	if e.port != nil {
		e.stop()
		_ = e.port.Close()
		e.port, e.frames = nil, nil
	}
	e.transport, e.serialPort, e.baud, e.tcpAddr = spec.Transport, spec.SerialPort, spec.Baud, spec.TCPAddr
	var (
		p   *serialio.Port
		err error
	)
	if spec.Transport == config.TransportTCP {
		p, err = serialio.Dial(spec.TCPAddr)
	} else {
		p, err = serialio.Open(spec.SerialPort, spec.Baud)
	}
	if err != nil {
		e.status = "connect failed: " + err.Error()
		e.logf(config.LogError, "connect failed (%s %s): %v", spec.Transport, e.endpoint(), err)
		return
	}
	e.port, e.frames = p, p.Frames()
	e.havePan, e.haveTilt = false, false // fresh readback for the new link
	e.status = "connected"
	e.logf(config.LogInfo, "connected (%s %s)", spec.Transport, e.endpoint())
}

// endpoint returns the active transport's human-readable target.
func (e *Engine) endpoint() string {
	if e.transport == config.TransportTCP {
		return e.tcpAddr
	}
	return e.serialPort
}

func (e *Engine) setServer(proto control.Protocol, enabled bool, port int) {
	switch proto {
	case control.GS232:
		if e.gs232Srv != nil {
			_ = e.gs232Srv.Close()
			e.gs232Srv, e.gs232On = nil, false
			e.logf(config.LogInfo, "gs232 stopped")
		}
		e.gs232Port = port
		if !enabled {
			return
		}
		srv, err := control.Start(proto, e.bind, port, e.pos, e.Submit)
		if err != nil {
			e.status = fmt.Sprintf("gs232: %v", err)
			e.logf(config.LogError, "gs232 start failed: %v", err)
			return
		}
		e.gs232Srv, e.gs232On = srv, true
		e.status = fmt.Sprintf("gs232 listening on %s", srv.Addr())
		e.logf(config.LogInfo, "gs232 listening on %s", srv.Addr())
	case control.Rotctld:
		if e.rotctldSrv != nil {
			_ = e.rotctldSrv.Close()
			e.rotctldSrv, e.rotctldOn = nil, false
			e.logf(config.LogInfo, "rotctld stopped")
		}
		e.rotctldPort = port
		if !enabled {
			return
		}
		srv, err := control.Start(proto, e.bind, port, e.pos, e.Submit)
		if err != nil {
			e.status = fmt.Sprintf("rotctld: %v", err)
			e.logf(config.LogError, "rotctld start failed: %v", err)
			return
		}
		e.rotctldSrv, e.rotctldOn = srv, true
		e.status = fmt.Sprintf("rotctld listening on %s", srv.Addr())
		e.logf(config.LogInfo, "rotctld listening on %s", srv.Addr())
	}
}

func (e *Engine) closeServers() {
	if e.gs232Srv != nil {
		_ = e.gs232Srv.Close()
		e.gs232Srv, e.gs232On = nil, false
	}
	if e.rotctldSrv != nil {
		_ = e.rotctldSrv.Close()
		e.rotctldSrv, e.rotctldOn = nil, false
	}
}

func (e *Engine) setMovePoll(fast bool) {
	if e.ticker == nil || fast == e.pollFast {
		return
	}
	e.pollFast = fast
	if fast {
		e.ticker.Reset(movePollInterval)
	} else {
		e.ticker.Reset(pollInterval)
	}
}

func (e *Engine) send(f pelco.Frame) {
	if e.port == nil {
		e.logf(config.LogWarn, "TX  ! not connected")
		return
	}
	if err := e.port.Send(f); err != nil {
		e.logf(config.LogError, "TX  ! %v", err)
		return
	}
	e.logf(config.LogDebug, "TX  % X", f.Bytes())
}

// logf records a line at the given level. Lines more verbose than the
// configured level are dropped entirely (neither buffered for the TUI panel nor
// written to the log file/stderr), so the level controls verbosity uniformly.
func (e *Engine) logf(level config.LogLevel, format string, args ...any) {
	if level > e.logLevel {
		return
	}
	line := fmt.Sprintf("%s%-5s %s", time.Now().Format("15:04:05.000 "),
		strings.ToUpper(level.String()), fmt.Sprintf(format, args...))
	e.logLines = append(e.logLines, line)
	if len(e.logLines) > logSize {
		e.logLines = e.logLines[len(e.logLines)-logSize:]
	}
	if e.logw != nil {
		_, _ = io.WriteString(e.logw, line+"\n")
	}
}

func (e *Engine) publish() {
	endpoint := e.serialPort
	if e.transport == config.TransportTCP {
		endpoint = e.tcpAddr
	}
	logCopy := make([]string, len(e.logLines))
	copy(logCopy, e.logLines)

	e.mu.Lock()
	e.state = State{
		Status:      e.status,
		Connected:   e.port != nil,
		Transport:   e.transport,
		Endpoint:    endpoint,
		SerialPort:  e.serialPort,
		TCPAddr:     e.tcpAddr,
		Baud:        e.baud,
		Addr:        e.addr,
		HavePan:     e.havePan,
		HaveTilt:    e.haveTilt,
		CurPan:      e.curPan,
		CurTilt:     e.curTilt,
		CurPanRaw:   e.curPanRaw,
		CurTiltRaw:  e.curTiltRaw,
		LastPan:     e.lastPan,
		LastTilt:    e.lastTilt,
		BytesIn:     e.bytesIn,
		Jogging:     e.jogging,
		JogPan:      e.jogPan,
		JogTilt:     e.jogTilt,
		Gotoing:     e.gotoing,
		Unwrapping:  e.unwrapping,
		WrapEnabled: e.wrapEnabled,
		WrapLimit:   e.wrapLimit,
		Wrap:        e.wrap,
		Bind:        e.bind,
		GS232On:     e.gs232On,
		GS232Port:   e.gs232Port,
		RotctldOn:   e.rotctldOn,
		RotctldPort: e.rotctldPort,
		Log:         logCopy,
	}
	e.mu.Unlock()
}

func dirArrow(dir int) string {
	if dir < 0 {
		return "←"
	}
	return "→"
}
