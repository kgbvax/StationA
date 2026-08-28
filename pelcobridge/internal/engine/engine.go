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
	"sync/atomic"
	"time"

	"pelcots/internal/config"
	"pelcots/internal/control"
	"pelcots/internal/pelco"
	"pelcots/internal/serialio"
)

const (
	// pollInterval is the sleep gap between poll ticks. Polling is self-paced,
	// not wall-clock ticked: the engine sends ONE frame (a position query, or
	// the pending SetTilt of a two-axis goto), waits for any reply (bounded by
	// this same duration as a response timeout), then sleeps this long before
	// the next tick — so a query is never sent on top of an in-flight reply on
	// the 2400-baud half-duplex link. Each tick sends exactly ONE query
	// (alternating pan/tilt), never the back-to-back QueryPan+QueryTilt pair
	// that collapsed readback on the 303Z/3050DZ (the pan reply collided with
	// the outgoing tilt query; observed live ~0-1% response rate). The rotator
	// is sensitive to excess traffic, so the gap is also the minimum spacing
	// between any two frames. At 500 ms the alternating queries read each axis
	// back once a second — the readback cadence the absolute-set gotos settle
	// on. Overridable via Options.PollInterval (the in-memory sim uses a short
	// interval to converge gotos fast).
	pollInterval   = 500 * time.Millisecond // sleep gap between poll ticks (and response timeout); overridable via Options.PollInterval (sim)
	jogKeySpeed    = 0x20                   // fixed speed for keyboard (hold-to-move) jogging
	moveTolerance  = 3.0                    // degrees: how close to the target ends a goto
	logSize        = 200                    // TX/RX ring-buffer depth
	reconnectDelay = 400 * time.Millisecond // retry cadence while the link is down

	// closeTimeout bounds how long Close waits for the actor goroutine to halt
	// motion and tear the link down. Close must not return before the final
	// all-stop has reached the port — Pelco-D Jog latches, so a caller that
	// exits the process first leaves the unit slewing — but neither may it hang
	// forever if the actor is wedged in a blocking write.
	closeTimeout = 2 * time.Second

	// progressDeadband is how much observed motion (degrees) counts as progress
	// for the goto stall watchdog: above readback dither, far below the travel
	// one poll interval of a real slew produces.
	progressDeadband = 0.1

	// Absolute-set pacing, in multiples of the poll tick (so the in-memory sim,
	// with its short tick, keeps the same proportions at test speed):
	//
	//   - setQuietTicks: after an absolute SetPan/SetTilt the line is held
	//     SILENT for this many ticks. Bench evidence (2026-08-28) shows the
	//     303Z/3050DZ ignoring byte-perfect SetPan frames when they are embedded
	//     in the query stream — the same frames move the head when ptest sends
	//     them onto a silent line, and third-party controllers of this family
	//     report the firmware mishandles traffic around the absolute-position
	//     command. The quiet window gives the head an uncontended slot to latch
	//     the set and start slewing.
	//   - setRetryTicks: if no axis has visibly moved this long after the last
	//     set went out, the set was lost — it is re-sent, up to maxSetResends
	//     times, each re-send followed by a fresh quiet window.
	setQuietTicks = 3
	setRetryTicks = 6
	maxSetResends = 3
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
	Transport    string
	SerialPort   string
	TCPAddr      string
	Baud         int
	Addr         byte
	Protocol     string // wire protocol sent to the PTZ: "d" (Pelco-D, default) | "p" (Pelco-P); anything else is Pelco-D
	Bind         string
	WrapEnabled  bool
	WrapLimit    float64
	WrapAccum    float64
	AzOffset     float64
	TiltInvert   bool
	Goto         config.GotoConfig
	PollInterval time.Duration // per-tick frame cadence override (0 → default pollInterval); tests use a short interval
	Sim          config.SimConfig
	SelfCheck    config.SelfCheckConfig
	Rotctld      config.ServerConfig
	AutoArm      bool // arm at start (headless/sim use; the TUI workflow starts disarmed)
	Logw         io.Writer
	LogLevel     config.LogLevel
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
	Reconnecting          bool
	Protocol              string // wire protocol sent to the PTZ ("d" | "p")
	HavePan, HaveTilt     bool
	CurPan, CurTilt       float64
	CurPanRaw, CurTiltRaw uint16
	LastPan, LastTilt     time.Time
	BytesIn               int
	Jogging               bool
	JogPan, JogTilt       int
	Gotoing               bool
	Armed                 bool
	WrapEnabled           bool
	WrapLimit             float64
	Wrap                  float64
	AzOffset              float64
	Bind                  string
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
	// proto is the wire envelope the engine SENDS (Pelco-D when zero). The
	// read side is adaptive: the framing reader accepts either protocol, and
	// the simulator answers in the protocol a query arrived in, so RX needs no
	// configuration. See pelco.Protocol and config.ProtocolD/ProtocolP.
	proto     pelco.Protocol
	bind      string
	sim       config.SimConfig
	selfCheck config.SelfCheckConfig

	// cable-wrap protection
	wrapEnabled bool
	wrapLimit   float64
	wrap        float64

	// azimuth zero offset: the physical azimuth (degrees) that reads as logical
	// 0°. A software calibration — Pelco-D has no re-zero command.
	azOffset float64

	// tiltInvert flips the elevation axis for an upside-down mount: logical
	// elevation = 90 - physical tilt, and the jog up/down direction is reversed.
	tiltInvert bool

	// goto tuning. See config.GotoConfig.
	gotoTimeout time.Duration

	// armed gates network-originated motion (the inbound rotctld server). It
	// starts false on every run and is set by the TUI arm workflow (confirmed
	// goto-0 + offset entry) or by AutoArm for headless/sim use. Armed state is
	// never persisted. The local TUI path (Jog/Goto/StopMotion) is deliberately
	// NOT gated — the person at the keyboard is trusted; the gate protects the
	// unattended interfaces from moving the unit before a safe azimuth has been
	// confirmed.
	armed bool

	// desired rotctld server at startup
	rotctldWant config.ServerConfig
	// autoArm arms during Start (before the servers accept traffic), for
	// headless/sim deployments. The TUI workflow always starts disarmed.
	autoArm bool

	// runtime connection
	port         *serialio.Port
	frames       <-chan serialio.Event
	pollTimer    *time.Timer   // self-paced poll trigger: response timeout while awaitingResp, sleep gap otherwise
	awaitingResp bool          // a position query is in flight; the next query waits for its reply (or this timeout)
	pollInterval time.Duration // sleep gap between poll ticks (default pollInterval const; shorter for the in-memory sim)
	retry        *time.Timer   // fires reconnectDelay after the link drops; stopped while connected
	retrying     bool          // disconnected and auto-retrying (throttles repeat failure logs)

	reqs chan func()
	quit chan struct{}
	// done is closed by run once the actor goroutine has halted motion and torn
	// down the link. Close waits on it so the all-stop is on the wire before the
	// caller exits the process; started gates that wait, since an engine that was
	// never Started has no actor goroutine to close it.
	done    chan struct{}
	started atomic.Bool
	once    sync.Once

	// readback
	curPan, curTilt       float64
	curPanRaw, curTiltRaw uint16
	havePan, haveTilt     bool
	lastPan, lastTilt     time.Time
	bytesIn               int
	// pollPan alternates which axis is queried each cycle: only one query is
	// sent per poll (pan, then tilt), never both back-to-back. See poll().
	pollPan bool

	// motion state
	jogDir          pelco.Direction
	jogPan, jogTilt int
	jogging         bool
	gotoing         bool
	// gotoTargetAz/El are the PHYSICAL coordinates of the in-flight goto
	// (already through physAz/physTilt). pendingPanSet/pendingTiltSet arm the
	// SetPan/SetTilt sends on successive poll ticks (one frame per tick, same
	// discipline as the query alternation — the two sets never go back-to-back
	// on the 2400-baud half-duplex link), each followed by a quiet window with
	// no queries: this head ignores absolute sets embedded in the query stream.
	gotoTargetAz, gotoTargetEl float64
	gotoDeadline               time.Time // goto safety timeout (stall watchdog)
	mvActive                   bool      // goto: pan axis still seeking (SetPan sent, readback not yet converged)
	mvTiltActive               bool      // goto: tilt axis still seeking (SetTilt sent, readback not yet converged)
	pendingPanSet              bool      // a SetPan for the in-flight goto is queued for the next poll tick
	pendingTiltSet             bool      // a SetTilt for the in-flight goto is queued for the next poll tick
	setsSentAt                 time.Time // last absolute set (or re-send) went out; drives the lost-set re-send
	travelSinceSets            float64   // observed readback travel since the last set went out
	setResends                 int       // lost-set re-sends issued for the current goto (bounded by maxSetResends)
	calZero                    bool      // goto targets physical 0° (calibration zero) and re-zeroes the wrap accumulator on arrival

	// inbound rotctld server
	rotctldSrv  *control.Server
	rotctldPort int
	rotctldOn   bool

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
		sim:         o.Sim,
		selfCheck:   o.SelfCheck,
		wrapEnabled: o.WrapEnabled,
		wrapLimit:   o.WrapLimit,
		wrap:        o.WrapAccum,
		azOffset:    o.AzOffset,
		tiltInvert:  o.TiltInvert,
		rotctldWant: o.Rotctld,
		autoArm:     o.AutoArm,
		rotctldPort: o.Rotctld.Port,
		pos:         &control.Pos{},
		logw:        o.Logw,
		logLevel:    o.LogLevel,
		reqs:        make(chan func(), 64),
		quit:        make(chan struct{}),
		done:        make(chan struct{}),
		pollPan:     true, // first poll queries pan — establishes havePan before any wrap move
		status:      "starting",
	}
	if e.wrapLimit <= 0 {
		e.wrapLimit = 270
	}
	if e.addr < 1 {
		e.addr = 1
	}
	// Wire protocol: "p" selects Pelco-P envelopes; anything else (including
	// empty) is Pelco-D. Readback is protocol-agnostic — the framing reader
	// accepts both, so no RX-side setting exists.
	if strings.EqualFold(strings.TrimSpace(o.Protocol), config.ProtocolP) {
		e.proto = pelco.ProtocolP
	}
	// Goto tuning (default applies when unset, so the simulator and a bare
	// config keep working without a goto: block).
	e.gotoTimeout = time.Duration(o.Goto.TimeoutSec * float64(time.Second))
	if e.gotoTimeout <= 0 {
		e.gotoTimeout = 60 * time.Second
	}
	// Per-tick poll cadence. The default (pollInterval const, 500ms) gives the
	// 2400-baud 303Z/3050DZ a full tick to answer one frame before the next —
	// alternating pan/tilt queries read each axis once a second. The in-memory
	// simulator has no such constraint, so tests override it with a short
	// interval to converge gotos in milliseconds, not seconds.
	e.pollInterval = pollInterval
	if o.PollInterval > 0 {
		e.pollInterval = o.PollInterval
	}
	return e
}

// Pos returns the thread-safe latest-position holder (for inbound servers).
func (e *Engine) Pos() *control.Pos { return e.pos }

// Start launches the actor goroutine: it connects, starts any enabled servers,
// and begins polling.
func (e *Engine) Start() {
	e.started.Store(true)
	go e.run()
}

// Close stops motion, shuts down servers and the port, and stops the loop.
//
// It BLOCKS until the actor goroutine has finished that teardown (bounded by
// closeTimeout), because the teardown is what puts the all-stop on the wire.
// Pelco-D Jog latches, so returning early let the caller (main, on SIGTERM)
// exit the process while the unit was still slewing, with nothing left running
// to halt it.
//
// Shutdown is signalled by closing e.quit rather than by queueing a cleanup
// request: a queued request would make Close itself block forever behind a
// wedged actor once the 64-deep request queue filled — exactly the situation in
// which the all-stop matters most. Idempotent and safe from any goroutine.
func (e *Engine) Close() error {
	e.once.Do(func() { close(e.quit) })
	if !e.started.Load() {
		return nil // never started: no actor goroutine will ever close done
	}
	select {
	case <-e.done:
	case <-time.After(closeTimeout):
	}
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
	defer close(e.done)                         // releases Close, once motion is halted and the link is down
	e.pollTimer = time.NewTimer(e.pollInterval) // first poll after the initial sleep gap
	defer e.pollTimer.Stop()
	e.retry = time.NewTimer(reconnectDelay)
	e.retry.Stop() // armed only while disconnected
	defer e.retry.Stop()

	e.reconnect(ConnSpec{Transport: e.transport, SerialPort: e.serialPort, Baud: e.baud, TCPAddr: e.tcpAddr})
	// auto_arm (headless/sim use): arm before the inbound servers accept
	// traffic, so no disarmed refusal window exists in unattended deployments.
	if e.autoArm {
		e.arm()
	}
	e.setServer(e.rotctldWant.Enabled, e.rotctldWant.Port)
	e.publish()
	for {
		select {
		case <-e.quit:
			e.shutdown()
			return
		case f := <-e.reqs:
			f()
			e.publish()
		case <-e.pollTimer.C:
			// Self-paced polling: the previous query's reply (or its timeout)
			// ended the wait, so the line is idle now. Send the next query and
			// arm a response timeout. Unlike a wall-clock ticker this paces
			// queries by I/O completion + a sleep gap, so a query is never sent
			// on top of an in-flight reply on the 2400-baud half-duplex link —
			// the rotator is sensitive to excess traffic, and a fixed ticker
			// can fire while a slow reply is still arriving and collide with it.
			e.awaitingResp = false
			e.poll()
			e.awaitingResp = e.port != nil // a query is in flight only if one was sent
			e.pollTimer.Reset(e.pollInterval)
			e.publish()
		case <-e.retry.C: // disconnected → attempt to re-open the link
			e.tryReconnect()
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

// Arm unlocks network-originated motion (the inbound rotctld server). The
// operator is expected to have run the calibration workflow first — confirmed
// goto-0 and the true-azimuth offset — so incoming network azimuths map onto
// the physical frame and the cable-wind accumulator starts from a known-safe
// zero. Armed state is never persisted: every run starts disarmed.
func (e *Engine) Arm() {
	e.do(func() { e.arm() })
}

// Arm sets armed on the actor goroutine.
func (e *Engine) arm() {
	if e.armed {
		return
	}
	e.armed = true
	e.status = "ARMED — network motion enabled"
	e.logf(config.LogInfo, "armed: inbound network motion enabled")
}

// GotoZero drives the pan axis to PHYSICAL 0° — the unit's own zero reference,
// deliberately bypassing the azimuth offset — and re-zeroes the cable-wind
// accumulator when the readback converges there. This is the calibration step
// of the arm workflow: the user confirms the antenna is safe to swing to the
// mechanical zero, and 0° becomes the cable-safe reference the ±wrap-limit
// accounting starts from. Elevation is untouched.
func (e *Engine) GotoZero() {
	e.do(func() { e.gotoZero() })
}

func (e *Engine) gotoZero() {
	if e.gotoing {
		e.status = "goto 0: motion already in flight"
		return
	}
	if !e.havePan {
		e.status = "goto 0: no position readback yet"
		e.logf(config.LogWarn, "goto 0 blocked: no pan readback")
		return
	}
	// Target physical 0° directly (no physAz — the offset does not apply).
	short := shortestDelta(e.curPan, 0)
	e.beginGoto(0, e.curTilt, MovePlan{Kind: MoveShort, Dir: sign(short), Travel: short, NewWrap: 0}, true)
	if e.gotoing {
		e.logf(config.LogInfo, "calibration: goto physical 0° (from %.2f°)", e.curPan)
	}
}

// SetAzimuthTrue calibrates the azimuth offset: the user reads what the antenna
// ACTUALLY points at (the logical azimuth) while the unit holds its current
// physical direction, and the offset is set so that direction reports the given
// value. All subsequent incoming azimuths map onto the physical frame through
// this offset (azOffset = curPan − true).
func (e *Engine) SetAzimuthTrue(trueAz float64) {
	e.do(func() { e.setAzimuthTrue(trueAz) })
}

func (e *Engine) setAzimuthTrue(trueAz float64) {
	if !e.havePan {
		e.status = "true-az: no position readback yet"
		e.logf(config.LogWarn, "true-az blocked: no pan readback")
		return
	}
	e.azOffset = norm360(e.curPan - norm360(trueAz))
	e.status = fmt.Sprintf("az offset set (true %.1f° = physical %.2f°)", norm360(trueAz), e.curPan)
	e.logf(config.LogInfo, "azimuth offset set: logical %.2f° = physical %.2f° (offset %.2f°)",
		norm360(trueAz), e.curPan, e.azOffset)
}

// ZeroWrap resets the cable-wind accumulator to zero (cable manually centered).
func (e *Engine) ZeroWrap() {
	e.do(func() {
		e.wrap = 0
		e.status = "wrap re-zeroed"
	})
}

// ZeroAzimuth re-zeroes the azimuth readout: the current physical direction
// becomes logical 0°. The offset is a software calibration (Pelco-D has no
// re-zero command) and persists across restarts via the config.
func (e *Engine) ZeroAzimuth() {
	e.do(func() {
		if !e.havePan {
			e.status = "zero-az: no position readback yet"
			return
		}
		e.azOffset = e.curPan
		e.status = fmt.Sprintf("azimuth zeroed (offset %.2f°)", e.azOffset)
		e.logf(config.LogInfo, "azimuth zeroed: logical 0° = physical %.2f°", e.azOffset)
	})
}

// logicalPan maps a physical azimuth to the zeroed (logical) frame: the
// direction the user zeroed reads as 0°.
func (e *Engine) logicalPan(phys float64) float64 { return norm360(phys - e.azOffset) }

// physAz maps a logical (zeroed) azimuth back to the physical frame for
// commanding the unit.
func (e *Engine) physAz(logical float64) float64 { return norm360(logical + e.azOffset) }

// logicalTilt maps a physical tilt to the logical elevation frame, inverting it
// for an upside-down mount.
func (e *Engine) logicalTilt(phys float64) float64 {
	if e.tiltInvert {
		return 90 - phys
	}
	return phys
}

// physTilt maps a logical elevation back to the physical tilt frame for
// commanding the unit.
func (e *Engine) physTilt(logical float64) float64 {
	if e.tiltInvert {
		return 90 - logical
	}
	return logical
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

// SetRotctld starts or stops the inbound rotctld server on a port.
func (e *Engine) SetRotctld(enabled bool, port int) {
	e.do(func() { e.setServer(enabled, port) })
}

// Snapshot returns the latest published engine state.
func (e *Engine) Snapshot() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

// --- actor-goroutine internals --------------------------------------------

// exec runs an inbound control command on the actor goroutine. Motion commands
// arriving from the rotctld server carry a Source tag set by the server; it is
// logged here so the trace shows what drove a move — distinguishing network
// moves from local TUI moves (Source empty → "local") and from the bare TX/RX
// hex the poll loop emits. Position queries never reach exec (the server
// answers those from the thread-safe Pos), so only motion commands appear as
// CMD lines.
//
// ARM GATE: while disarmed, network motion (KindSetPos) is refused and logged.
// Stop always passes (it is always safe), and the local TUI path never reaches
// exec at all — the gate protects only the unattended interfaces.
func (e *Engine) exec(c control.Command) {
	src := c.Source
	if src == "" {
		src = "local"
	}
	e.logf(config.LogInfo, "CMD [%s] %s", src, commandString(c))
	if c.Raw != "" {
		e.logf(config.LogTrace, "CMD [%s] raw %s", src, c.Raw)
	}
	switch c.Kind {
	case control.KindStop:
		e.stop()
	case control.KindSetPos:
		if !e.armed {
			e.status = "DISARMED — network move refused (arm via TUI 'A' or auto_arm)"
			e.logf(config.LogWarn, "network setpos refused: disarmed (az=%.1f el=%.1f)", c.Az, c.El)
			return
		}
		e.startGoto(c.Az, c.El)
	}
}

// commandString returns a human-readable decoded form of a control.Command.
func commandString(c control.Command) string {
	switch c.Kind {
	case control.KindStop:
		return "stop"
	case control.KindSetPos:
		return fmt.Sprintf("setpos az=%.1f el=%.1f", c.Az, c.El)
	default:
		return fmt.Sprintf("unknown kind=%d", c.Kind)
	}
}

func (e *Engine) jog(pan, tilt int, turbo bool) {
	if e.tiltInvert {
		tilt = -tilt // upside-down mount: logical up is physical down
	}
	e.jogDir, e.jogging = pelco.Direction{Pan: pan, Tilt: tilt}, true
	e.gotoing, e.mvActive, e.mvTiltActive, e.pendingPanSet, e.pendingTiltSet, e.calZero = false, false, false, false, false, false
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

// startGoto commands an absolute move to (az, el) degrees. The device honors
// the Pelco-D absolute SetPan/SetTilt opcodes (0x4B/0x4D) — confirmed on the
// bench 2026-08-28, but ONLY when the set is not embedded in the query stream:
// sets sent amid the readback polling were ignored (the same frames move the
// head when sent onto a silent line, as ptest does). The engine therefore
// queues the sets on the poll ticks (one per tick) and holds the line quiet
// around them, re-sending sets that produce no observed travel, and settles
// the goto on readback convergence at ~1 Hz per axis. No jog keepalive runs
// during a goto: the unit slews itself.
//
// Azimuth carries cable-wrap protection: only the shortest-path representation
// is ever commanded (SetPan is absolute shortest-path), and a move whose short
// path would over-wind the cable is REFUSED — over-winding is relieved manually
// (TUI jog + zero-wrap), never by driving the long way round. Repeated
// identical calls (e.g. tracking updates to an unchanged target) are idempotent
// so an in-flight goto is not restarted.
func (e *Engine) startGoto(az, el float64) {
	az = e.physAz(az) // command the unit in physical coordinates
	if el < 0 {
		el = 0
	} else if el > 90 {
		el = 90 // keep the target inside the physical 0–90° travel
	}
	el = e.physTilt(el)
	if e.gotoing && e.gotoTargetAz == az && e.gotoTargetEl == el {
		return // already heading there; let it run
	}
	// Goto settling is readback-driven, so both axes need a current position.
	if !e.havePan || !e.haveTilt {
		e.status = "goto: no position readback yet — move blocked"
		e.logf(config.LogWarn, "goto blocked: no readback (havePan=%v haveTilt=%v)", e.havePan, e.haveTilt)
		return
	}
	var plan MovePlan
	if e.wrapEnabled {
		plan = planMove(e.wrap, e.curPan, az, e.wrapLimit)
		if plan.Kind == MoveBlock {
			e.status = fmt.Sprintf("wrap: %.0f° unreachable within ±%.0f° — unwind manually (jog + 'z')", az, e.wrapLimit)
			e.logf(config.LogWarn, "wrap: refused goto %.0f° (wind %+.0f°, limit ±%.0f°)", az, e.wrap, e.wrapLimit)
			return
		}
	} else {
		// Wrap disabled: shortest-path direct move, synthesized as a plan so the
		// shared beginGoto/stepGoto path handles it uniformly.
		short := shortestDelta(e.curPan, az)
		if short == 0 {
			plan = MovePlan{Kind: MoveNone, NewWrap: e.wrap}
		} else {
			plan = MovePlan{Kind: MoveShort, Dir: sign(short), Travel: short, NewWrap: e.wrap + short}
		}
	}
	e.beginGoto(az, el, plan, false)
}

// beginGoto arms an absolute-set goto: SetPan and SetTilt are queued
// (pendingPanSet/pendingTiltSet) and sent one per poll tick, each followed by a
// quiet window (setQuietTicks) during which no query goes out — the 303Z/3050DZ
// ignores absolute sets embedded in the query stream (bench 2026-08-28: the
// same frames move the head when sent onto a silent line). The unit slews
// itself; stepGoto (on readback) ends the goto when both axes converge.
// calZero marks a calibration goto-0: on arrival the cable-wind accumulator is
// re-zeroed (the confirmed physical 0° is the cable-safe reference).
func (e *Engine) beginGoto(az, el float64, plan MovePlan, calZero bool) {
	wasMoving := e.gotoing || e.jogging // retarget of live motion vs a standing start
	e.jogging = false
	e.gotoing, e.gotoTargetAz, e.gotoTargetEl = true, az, el

	// Pan axis: a SetPan takes the shortest physical path by itself, so the
	// planned direction/travel is bookkeeping only — convergence is read from
	// the position readback. e.wrap is NOT set from the plan: the wind
	// accumulator is integrated from observed readback (handleFrame) so it
	// stays correct through interrupted or retargeted moves.
	e.mvActive = plan.Kind == MoveShort

	// Tilt axis.
	e.mvTiltActive = math.Abs(el-e.curTilt) > moveTolerance
	e.pendingTiltSet = e.mvTiltActive
	e.calZero = calZero
	// Set scheduling: both sets ride the poll ticks, and the re-send bookkeeping
	// starts fresh — a retargeted goto gets its own budget of lost-set retries.
	e.pendingPanSet = e.mvActive
	e.setsSentAt = time.Now()
	e.travelSinceSets = 0
	e.setResends = 0

	// Stall watchdog. Armed when motion STARTS, not on every retarget: tracking
	// software streams a new target every second or two, and re-arming here each
	// time defeated the backstop completely — a jammed or unreadable axis would
	// slew for as long as the pass lasted. Motion already in flight keeps its
	// existing deadline, and only OBSERVED travel pushes that deadline out (see
	// noteGotoProgress), so a stuck axis still expires within gotoTimeout no
	// matter how many targets arrive meanwhile.
	if !wasMoving {
		e.gotoDeadline = time.Now().Add(e.gotoTimeout)
	}

	if !e.mvActive && !e.mvTiltActive {
		// Already at the target on both axes. The all-stop is still sent: this
		// path is reached with motion possibly LATCHED — a hold-jog, or a goto
		// that was in flight when a tracking update retargeted it onto the
		// position the unit had just reached — and Pelco-D Jog has no auto-stop.
		// Clearing the flags without the frame left the unit slewing with no
		// keepalive left to correct it and no goto to time out.
		e.gotoing = false
		if e.calZero {
			e.calZero = false
			e.wrap = 0
			e.logf(config.LogInfo, "calibration: already at physical 0° — wrap accumulator re-zeroed")
		}
		e.send(pelco.Stop(e.addr))
		e.status = "at target"
		return
	}
	e.status = fmt.Sprintf("seeking %.0f° / %.0f°", e.logicalPan(az), e.logicalTilt(el))
	e.logf(config.LogInfo, "goto: seeking %.2f° / %.2f° (sets armed: pan=%v tilt=%v)",
		e.logicalPan(az), e.logicalTilt(el), e.pendingPanSet, e.pendingTiltSet)
}

// stepGoto advances the goto: ends it when both commanded axes have converged
// on readback. Called from handleFrame after each pan/tilt readback; tiltRead
// tells which axis the readback was. Each axis is checked only on its own
// readback (the other's stale value must not settle it). Absolute sets are not
// latched motion, so convergence needs no per-axis halt frame — the device has
// already stopped there; only the final all-stop is sent. calZero gotos
// (calibration goto-0) additionally re-zero the cable-wind accumulator: the
// confirmed physical 0° is the cable-safe reference.
func (e *Engine) stepGoto(tiltRead bool) {
	if !e.gotoing {
		return
	}
	if e.mvActive && !tiltRead {
		if math.Abs(shortestDelta(e.curPan, e.gotoTargetAz)) < moveTolerance {
			e.mvActive = false
			e.logf(config.LogDebug, "goto: pan settled (readback %.2f°)", e.curPan)
		}
	}
	if e.mvTiltActive && tiltRead {
		if math.Abs(e.curTilt-e.gotoTargetEl) < moveTolerance {
			e.mvTiltActive = false
			e.logf(config.LogDebug, "goto: tilt settled (readback %.2f°)", e.curTilt)
		}
	}
	if !e.mvActive && !e.mvTiltActive {
		e.send(pelco.Stop(e.addr))
		e.gotoing, e.pendingPanSet, e.pendingTiltSet = false, false, false
		if e.calZero {
			e.calZero = false
			e.wrap = 0
			e.logf(config.LogInfo, "calibration: at physical 0° — wrap accumulator re-zeroed")
		}
		e.status = "at target"
		e.logf(config.LogInfo, "goto: at target %.1f° / %.1f°", e.logicalPan(e.curPan), e.logicalTilt(e.curTilt))
	}
}

// expireGoto is the safety backstop: if a goto has not converged within
// gotoTimeout (a dropped set frame, a stuck axis, or readback that never
// reaches the deadband), stop all motion and abandon the move. Without this a
// runaway slew would drive the rotor into a mechanical stop.
func (e *Engine) expireGoto() {
	e.send(pelco.Stop(e.addr))
	e.logf(config.LogWarn, "goto: timed out after %v (pan %.1f°→%.1f°, tilt %.1f°→%.1f°)",
		e.gotoTimeout, e.curPan, e.gotoTargetAz, e.curTilt, e.gotoTargetEl)
	e.gotoing, e.mvActive, e.mvTiltActive, e.pendingPanSet, e.pendingTiltSet, e.calZero = false, false, false, false, false, false
	e.status = "goto timeout — readback did not converge"
}

// noteGotoProgress pushes the goto stall watchdog out when an axis that is
// being driven has visibly moved since the previous sample. This is what makes
// gotoDeadline a no-progress timer rather than a total-duration timer: a long
// tracking pass never trips it, while a jammed axis — or one whose readback has
// collapsed, so nothing ever converges — still expires within gotoTimeout.
// The same observed travel feeds travelSinceSets, the lost-set detector in
// poll() (no travel after the sets → re-send them).
func (e *Engine) noteGotoProgress(axisActive bool, moved float64) {
	if !e.gotoing || !axisActive {
		return
	}
	e.travelSinceSets += math.Abs(moved)
	if math.Abs(moved) < progressDeadband {
		return
	}
	e.gotoDeadline = time.Now().Add(e.gotoTimeout)
}

// shutdown is the actor goroutine's last act: halt motion, stop the inbound
// servers, close the link. Queued requests are deliberately NOT drained — a
// Goto or Jog that arrived just before shutdown must not start the unit moving
// on the way out — and Close is blocked waiting for this to return.
func (e *Engine) shutdown() {
	e.stop() // all-stop while the port is still open
	e.closeServers()
	if e.port != nil {
		_ = e.port.Close()
		e.port, e.frames = nil, nil
	}
	e.publish() // final snapshot: Close having returned now implies teardown is done
}

func (e *Engine) stop() {
	e.jogging = false
	e.jogPan, e.jogTilt = 0, 0
	e.jogDir = pelco.Direction{}
	e.gotoing, e.mvActive, e.mvTiltActive, e.pendingPanSet, e.pendingTiltSet, e.calZero = false, false, false, false, false, false
	e.send(pelco.Stop(e.addr))
}

// poll issues the ONE frame for this tick — a position query, or the pending
// SetTilt of a two-axis goto — and re-sends any active jog keepalive. Exactly
// ONE query is sent per cycle — never the back-to-back QueryPan+QueryTilt pair
// the engine once sent. On the 303Z/3050DZ (2400 baud, ~58 ms per 7-byte frame)
// that pair's pan reply collides with the outgoing tilt query, collapsing
// readback to occasional random replies (observed live: ~0–1% response rate).
// One frame per cycle, with the next paced by reply completion + pollInterval
// (see run's pollTimer), gives the device a full gap to answer before the next,
// restoring reliable readback at the cost of halving the per-axis query rate.
//
// During a goto there is no jog keepalive (the unit slews itself after the
// absolute sets). The pending SetPan/SetTilt take their own ticks (one frame
// per tick, never back-to-back), and each set is followed by a SILENT window
// (setQuietTicks): the 303Z/3050DZ ignores absolute sets embedded in the query
// stream (bench 2026-08-28 — the same frames move the head when ptest sends
// them onto a quiet line), so queries are held off while the head latches the
// set. If no axis has visibly moved setRetryTicks after the last set went out,
// the set was lost and is re-sent (bounded by maxSetResends). Once the sets are
// away and the quiet window has passed, the axis still being sought is
// queried: both axes moving → alternate pan/tilt (each axis read every other
// cycle, ~1 Hz per axis at the default cadence); one axis moving → query that
// axis every cycle for faster convergence. While idle or jogging (UI
// hold-to-move), pan and tilt alternate.
func (e *Engine) poll() {
	if e.port == nil {
		return
	}
	// Goto safety timeout: on expiry the move is abandoned and no query is sent
	// this tick.
	if e.gotoing && time.Now().After(e.gotoDeadline) {
		e.expireGoto()
		return
	}
	switch {
	case e.pendingPanSet:
		// SetPan of the in-flight goto, sent on its own tick.
		e.pendingPanSet = false
		e.send(pelco.SetPan(e.addr, e.gotoTargetAz))
		e.setsSentAt, e.travelSinceSets = time.Now(), 0
	case e.pendingTiltSet:
		// SetTilt of the in-flight goto, deferred one tick so the two sets
		// never go back-to-back.
		e.pendingTiltSet = false
		e.send(pelco.SetTilt(e.addr, e.gotoTargetEl))
		e.setsSentAt, e.travelSinceSets = time.Now(), 0
	case e.gotoing && time.Since(e.setsSentAt) < setQuietTicks*e.pollInterval:
		// Quiet window after the last absolute set: hold the line silent so the
		// head can latch the set without contending with a query.
	case e.gotoing && e.setResends < maxSetResends &&
		time.Since(e.setsSentAt) >= setRetryTicks*e.pollInterval &&
		e.travelSinceSets < progressDeadband:
		// No axis has moved since the sets went out: they were lost (this head
		// drops a set that lands in traffic). Re-send and open a fresh quiet
		// window; the overall gotoDeadline still bounds the move.
		e.setResends++
		e.pendingPanSet, e.pendingTiltSet = e.mvActive, e.mvTiltActive
		e.logf(config.LogWarn, "goto: no travel after sets — re-sending (attempt %d/%d, pan %.2f°→%.2f°, tilt %.2f°→%.2f°)",
			e.setResends, maxSetResends, e.curPan, e.gotoTargetAz, e.curTilt, e.gotoTargetEl)
	case e.mvActive && e.mvTiltActive:
		// Two-axis goto: alternate one query per cycle.
		if e.pollPan {
			e.send(pelco.QueryPan(e.addr))
			e.pollPan = false
		} else {
			e.send(pelco.QueryTilt(e.addr))
			e.pollPan = true
		}
	case e.mvActive:
		e.send(pelco.QueryPan(e.addr))
		e.pollPan = false // bias to tilt next so it refreshes after the pan settles
	case e.mvTiltActive:
		e.send(pelco.QueryTilt(e.addr))
		e.pollPan = true
	case e.pollPan:
		e.send(pelco.QueryPan(e.addr))
		e.pollPan = false
	default:
		e.send(pelco.QueryTilt(e.addr))
		e.pollPan = true
	}
	// UI hold-to-move jog keepalive (jog() clears gotoing, so this never
	// interleaves with a goto).
	if e.jogging {
		e.send(pelco.Jog(e.addr, e.jogDir.Cmd2(), e.jogPan, e.jogTilt))
	}
}

func (e *Engine) handleFrame(ev serialio.Event, ok bool) {
	if !ok {
		e.linkFailed("link closed")
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
			moved := shortestDelta(e.curPan, newPan)
			e.wrap += moved
			e.noteGotoProgress(e.mvActive, moved)
		}
		e.curPanRaw, e.curPan, e.havePan, e.lastPan = raw, newPan, true, time.Now()
		e.pos.Set(e.logicalPan(e.curPan), e.logicalTilt(e.curTilt))
		e.logf(config.LogDebug, "RX  pan  %02X %02X  (word 0x%04X)", byte(raw>>8), byte(raw), raw)
		e.stepGoto(false)
		e.armNextPoll()
	case ev.Frame.IsTiltResponse():
		raw := ev.Frame.Word()
		e.armNextPoll() // a reply (even garbage) ends the outstanding query
		// Textbook Pelco-D: the tilt word is elevation in hundredths of a
		// degree, exactly like pan (confirmed on the bench 2026-08-28 — the
		// earlier raw-encoder calibration model was a misread).
		decoded := pelco.HundredthsToDeg(raw)
		if e.haveTilt {
			e.noteGotoProgress(e.mvTiltActive, decoded-e.curTilt)
		}
		e.curTiltRaw, e.curTilt, e.haveTilt, e.lastTilt = raw, decoded, true, time.Now()
		e.pos.Set(e.logicalPan(e.curPan), e.logicalTilt(e.curTilt))
		e.logf(config.LogDebug, "RX  tilt %02X %02X  (word 0x%04X)", byte(raw>>8), byte(raw), raw)
		e.stepGoto(true)
	default:
		e.logf(config.LogTrace, "RX  % X", ev.Frame.Bytes())
	}
}

// reconnect closes the current link and opens a new one per spec. It is the
// explicit (user-initiated) path, so it resets the auto-retry throttle and logs
// normally.
func (e *Engine) reconnect(spec ConnSpec) {
	e.closePort()
	e.transport, e.serialPort, e.baud, e.tcpAddr = spec.Transport, spec.SerialPort, spec.Baud, spec.TCPAddr
	e.retrying = false
	e.connect()
}

// tryReconnect is the timer-driven path: re-open the current target if (still)
// disconnected. A no-op once a link is up.
func (e *Engine) tryReconnect() {
	if e.port != nil {
		return
	}
	e.connect()
}

// closePort halts motion and tears down the current link (best-effort). Used
// before an explicit reconnect; safe when already disconnected.
func (e *Engine) closePort() {
	if e.port == nil {
		return
	}
	e.stop()           // best-effort halt before swapping links
	if e.port != nil { // stop()'s write may have already dropped a dead link
		_ = e.port.Close()
		e.port, e.frames = nil, nil
	}
}

// linkFailed drops a link that has died mid-session (peer close or write error)
// and schedules an automatic reconnect. Idempotent: a no-op if already down.
//
// It also abandons any in-flight motion. Pelco-D Jog has no auto-stop, so a
// dropped link leaves the unit physically moving, and an active goto must not
// resume against a stale position — the readback gate would re-arm the move
// from coordinates computed before the disconnect. stop() clears the motion
// state here; its Stop send is a no-op now that the port is closed, so a fresh
// Stop is issued on the new link in connect().
func (e *Engine) linkFailed(reason string) {
	if e.port == nil {
		return
	}
	_ = e.port.Close()
	e.port, e.frames = nil, nil
	e.stop()
	e.havePan, e.haveTilt = false, false
	e.retrying = true
	e.status = "disconnected — reconnecting"
	e.logf(config.LogWarn, "%s; reconnecting every %v", reason, reconnectDelay)
	e.armRetry()
}

// connect opens the currently configured target. On failure it schedules a
// retry; repeated failures while retrying are logged only once (on the first)
// to avoid flooding the trace when a device is absent.
func (e *Engine) connect() {
	var (
		p   *serialio.Port
		err error
	)
	switch e.transport {
	case config.TransportTCP:
		p, err = serialio.Dial(e.tcpAddr)
	case config.TransportSim:
		p = serialio.OpenSim(serialio.SimOptions{
			Addr:      e.addr,
			StartPan:  e.sim.StartPan,
			StartTilt: e.sim.StartTilt,
			JogStep:   e.sim.JogStep,
		})
	default:
		p, err = serialio.Open(e.serialPort, e.baud)
	}
	if err != nil {
		e.status = "connect failed: " + err.Error()
		// The position is not knowable while the link is down. linkFailed clears
		// these when a live link dies; clearing them here covers the other way in
		// — an explicit Reconnect (TUI 'r'/'m') to a target that is down, or a
		// first connect that never succeeded — which otherwise left the readback
		// gate in startGoto open, so an inbound move armed a goto
		// against a stale position that then drove the unit on the fresh link.
		e.havePan, e.haveTilt = false, false
		if !e.retrying {
			e.logf(config.LogError, "connect failed (%s %s): %v; retrying every %v",
				e.transport, e.endpoint(), err, reconnectDelay)
			e.retrying = true
		}
		e.armRetry()
		return
	}
	e.port, e.frames = p, p.Frames()
	e.havePan, e.haveTilt = false, false // fresh readback for the new link
	e.retry.Stop()                       // connected: cancel any pending retry
	if e.retrying {
		e.logf(config.LogInfo, "reconnected (%s %s)", e.transport, e.endpoint())
		e.retrying = false
	} else {
		e.logf(config.LogInfo, "connected (%s %s)", e.transport, e.endpoint())
	}
	e.status = "connected"
	// Halt any motion the unit is still executing. Pelco-D Jog has no auto-stop,
	// so a dropped link leaves it moving; issuing Stop on the fresh link (every
	// connect, including the first) is defensive and idempotent.
	//
	// stop() rather than a bare Stop frame: it also clears any in-flight
	// motion state, so a goto armed while the link was down cannot resume on
	// the fresh link and drive the unit toward a target computed from a
	// position that is now stale. linkFailed() clears the same state on the
	// other path.
	e.stop()
	// Disable the PTZ self-check once per successful connect (set preset 105 on
	// the 303Z/3050DZ). The setting is persistent on the unit, so this keeps it
	// disabled across power-ups; re-sending on each (re)connect is idempotent.
	// In sim mode the emulator ignores preset commands (harmless no-op).
	if e.selfCheck.Disable {
		e.send(pelco.DisableSelfCheck(e.addr))
		e.logf(config.LogInfo, "sent disable-self-check (set preset %d)", pelco.SelfCheckPreset)
	}
}

// armRetry (re)schedules the reconnect timer. Called only from the actor
// goroutine, so the timer is never touched concurrently.
func (e *Engine) armRetry() {
	if e.retry != nil {
		e.retry.Reset(reconnectDelay)
	}
}

// endpoint returns the active transport's human-readable target.
func (e *Engine) endpoint() string {
	switch e.transport {
	case config.TransportTCP:
		return e.tcpAddr
	case config.TransportSim:
		return "in-memory simulator"
	default:
		return e.serialPort
	}
}

// setServer starts or stops the inbound rotctld server.
func (e *Engine) setServer(enabled bool, port int) {
	if e.rotctldSrv != nil {
		_ = e.rotctldSrv.Close()
		e.rotctldSrv, e.rotctldOn = nil, false
		e.logf(config.LogInfo, "rotctld stopped")
	}
	e.rotctldPort = port
	if !enabled {
		return
	}
	srv, err := control.Start(e.bind, port, e.pos, e.Submit)
	if err != nil {
		e.status = fmt.Sprintf("rotctld: %v", err)
		e.logf(config.LogError, "rotctld start failed: %v", err)
		return
	}
	e.rotctldSrv, e.rotctldOn = srv, true
	e.status = fmt.Sprintf("rotctld listening on %s", srv.Addr())
	e.logf(config.LogInfo, "rotctld listening on %s", srv.Addr())
}

func (e *Engine) closeServers() {
	if e.rotctldSrv != nil {
		_ = e.rotctldSrv.Close()
		e.rotctldSrv, e.rotctldOn = nil, false
	}
}

// armNextPoll is called when a position-query reply arrives. Only the FIRST
// reply to the outstanding query cancels the response-timeout and starts the
// sleep gap before the next query; further (unsolicited) frames received while
// sleeping are processed for position but do not extend the gap. This keeps the
// rotator's garbage stream during motion from resetting the sleep repeatedly
// and starving the jog keepalive — the next query always goes out one
// pollInterval after the first reply, never sooner, never delayed indefinitely.
func (e *Engine) armNextPoll() {
	if e.awaitingResp {
		e.awaitingResp = false
		e.pollTimer.Reset(e.pollInterval)
	}
}

func (e *Engine) send(f pelco.Frame) {
	if e.port == nil {
		return // disconnected: the retry loop is responsible for restoring the link
	}
	f.Proto = e.proto // tag the wire envelope (Pelco-D when unset)
	if err := e.port.Send(f); err != nil {
		e.linkFailed("write failed: " + err.Error())
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
	endpoint := e.endpoint()
	logCopy := make([]string, len(e.logLines))
	copy(logCopy, e.logLines)

	e.mu.Lock()
	e.state = State{
		Status:       e.status,
		Connected:    e.port != nil,
		Reconnecting: e.retrying,
		Transport:    e.transport,
		Endpoint:     endpoint,
		SerialPort:   e.serialPort,
		TCPAddr:      e.tcpAddr,
		Baud:         e.baud,
		Addr:         e.addr,
		Protocol:     e.proto.String(),
		HavePan:      e.havePan,
		HaveTilt:     e.haveTilt,
		CurPan:       e.logicalPan(e.curPan),
		CurTilt:      e.logicalTilt(e.curTilt),
		CurPanRaw:    e.curPanRaw,
		CurTiltRaw:   e.curTiltRaw,
		LastPan:      e.lastPan,
		LastTilt:     e.lastTilt,
		BytesIn:      e.bytesIn,
		Jogging:      e.jogging,
		JogPan:       e.jogPan,
		JogTilt:      e.jogTilt,
		Gotoing:      e.gotoing,
		Armed:        e.armed,
		WrapEnabled:  e.wrapEnabled,
		WrapLimit:    e.wrapLimit,
		Wrap:         e.wrap,
		AzOffset:     e.azOffset,
		Bind:         e.bind,
		RotctldOn:    e.rotctldOn,
		RotctldPort:  e.rotctldPort,
		Log:          logCopy,
	}
	e.mu.Unlock()
}
