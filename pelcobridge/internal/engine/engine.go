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
	// pollInterval is the sleep gap between position-query cycles. Polling is
	// self-paced, not wall-clock ticked: the engine sends ONE position query,
	// waits for its reply (bounded by this same duration as a response timeout),
	// then sleeps this long before the next query — so a query is never sent on
	// top of an in-flight reply on the 2400-baud half-duplex link. Each cycle
	// sends exactly ONE query (alternating pan/tilt), never the back-to-back
	// QueryPan+QueryTilt pair that collapsed readback on the 303Z/3050DZ (the pan
	// reply collided with the outgoing tilt query; observed live ~0-1% response
	// rate). The rotator is sensitive to excess traffic, so the gap is also the
	// minimum spacing between any two queries. Overridable via Options.PollInterval
	// (the in-memory sim uses a short interval to converge gotos fast).
	pollInterval   = 600 * time.Millisecond // sleep gap between query cycles (and response timeout); overridable via Options.PollInterval (sim)
	jogKeySpeed    = 0x20                   // fixed speed for keyboard/unwrap jogging
	moveTolerance  = 3.0                    // degrees: how close to the target ends an unwrap
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
	// one poll interval of real jogging produces.
	progressDeadband = 0.1

	// tiltSlewSafety biases the open-loop tilt slew deadline slightly short
	// (halt at ~90% of the planned travel) so a mis-calibrated slew rate
	// undershoots rather than overshoots into the mechanical stop. The
	// halt-and-confirm correction loop then closes the small remaining gap;
	// undershoot is safe, overshoot past a hard stop is not.
	tiltSlewSafety = 0.9
	// tiltConfirmSamples is how many consecutive within-deadband tilt readbacks
	// the open-loop halt-and-confirm path requires before declaring the tilt
	// axis settled. The motor is HALTED while confirming (no jog bit), so this
	// cannot overshoot — it only guards a single transient garbage frame that
	// might slip through right after the halt.
	tiltConfirmSamples = 2
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
	TiltCal      config.TiltCalConfig
	Goto         config.GotoConfig
	PollInterval time.Duration // per-cycle query/jog cadence override (0 → default pollInterval); tests use a short interval
	Sim          config.SimConfig
	SelfCheck    config.SelfCheckConfig
	Rotctld      config.ServerConfig
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
	Unwrapping            bool
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

	// tilt readback calibration: a raw tilt word maps to elevation degrees via
	// (raw - tiltOffset) / tiltScale. Pelco standard is hundredths of a degree
	// (offset 0, scale 100); a calibrated raw-encoder head (303Z/3050DZ) carries
	// an offset and a per-device scale. See config.TiltCalConfig.
	tiltOffset float64
	tiltScale  float64

	// closed-loop goto tuning. See config.GotoConfig.
	gotoTimeout     time.Duration
	tiltSlewRate    float64 // tilt open-loop slew rate (deg/s); >0 enables open-loop-slew-then-halt-and-confirm
	maxTiltConfirms int     // post-halt correction pulses before the goto timeout gives up

	// desired rotctld server at startup
	rotctldWant config.ServerConfig

	// runtime connection
	port         *serialio.Port
	frames       <-chan serialio.Event
	pollTimer    *time.Timer   // self-paced poll trigger: response timeout while awaitingResp, sleep gap otherwise
	awaitingResp bool          // a position query is in flight; the next query waits for its reply (or this timeout)
	pollInterval time.Duration // sleep gap between query cycles (default pollInterval const; shorter for the in-memory sim)
	pollFast     bool
	retry        *time.Timer // fires reconnectDelay after the link drops; stopped while connected
	retrying     bool        // disconnected and auto-retrying (throttles repeat failure logs)

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
	jogDir                     pelco.Direction
	jogPan, jogTilt            int
	jogging                    bool
	gotoing                    bool
	gotoTargetAz, gotoTargetEl float64
	gotoDeadline               time.Time // closed-loop goto safety timeout
	mvActive                   bool      // closed-loop goto: pan axis still jogging
	mvDir                      int       // pan jog direction: -1 CCW, +1 CW
	mvWrapStart, mvTravel      float64   // cable-wind accumulator start + planned travel (pan, unwrap path)
	unwrapping                 bool      // pan stop is accumulator-travel driven (cable-wrap relief) vs direct convergence
	mvTiltActive               bool      // closed-loop goto: tilt axis still jogging
	mvTiltDir                  int       // tilt jog direction: -1 down, +1 up
	mvTiltStart, mvTiltTravel  float64   // tilt position at goto start + planned travel (overshoot backstop)
	// Open-loop tilt slew (303Z/3050DZ: tilt readback is constant garbage while
	// the motor runs). The slew is jogged blind and halted on a planned-travel
	// deadline (mvTiltHaltDeadline), then confirmed against the now-stable idle
	// readback. mvTiltHalted marks the confirm phase; mvTiltConfirmCount counts
	// consecutive within-deadband samples; mvTiltConfirmTries bounds the
	// correction pulses that re-jog the remaining gap if the blind halt lands
	// short/long. tiltSlewRate == 0 keeps the legacy closed-loop path (these
	// fields stay zero and are never consulted).
	mvTiltHalted       bool
	mvTiltHaltDeadline time.Time
	mvTiltConfirmTries int
	mvTiltConfirmCount int

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
	// Refine the tilt calibration: two calibration points (raw at 0° and 90°)
	// define a linear raw = offset + scale*elev map. Without calibration
	// (raw_at_90 == 0) the Pelco-standard hundredths decode stays in effect
	// (offset 0, scale 100), so the in-memory simulator and any standard head
	// keep working unchanged. A calibrated raw-encoder head (303Z/3050DZ)
	// supplies the per-device offset and scale here.
	if o.TiltCal.RawAt90 > 0 {
		e.tiltOffset = o.TiltCal.RawAt0
		e.tiltScale = (o.TiltCal.RawAt90 - o.TiltCal.RawAt0) / 90.0
	} else {
		e.tiltOffset = 0
		e.tiltScale = 100
	}
	// Closed-loop goto tuning (defaults apply when unset, so the simulator and a
	// bare config keep working without a goto: block).
	e.gotoTimeout = time.Duration(o.Goto.TimeoutSec * float64(time.Second))
	if e.gotoTimeout <= 0 {
		e.gotoTimeout = 60 * time.Second
	}
	// Open-loop tilt slew rate. 0 (default) keeps the legacy closed-loop path
	// (works for the sim and any head with clean motion readback); >0 enables
	// the open-loop-slew-then-halt-and-confirm path for the 303Z/3050DZ, whose
	// tilt readback is constant garbage while the motor runs.
	e.tiltSlewRate = o.Goto.TiltSlewRate
	e.maxTiltConfirms = o.Goto.TiltMaxConfirms
	if e.maxTiltConfirms <= 0 {
		// Each correction is one open-loop pulse + halt-and-confirm (~1–2s on
		// real hardware), so 8 is well within the 60s goto timeout. A generous
		// default makes the loop converge for a roughly-calibrated rate
		// (tolerating a ~3x mis-calibration) instead of giving up early.
		e.maxTiltConfirms = 8
	}
	// Per-tick poll cadence. The default (pollInterval const, 500ms) gives the
	// 2400-baud 303Z/3050DZ a full tick to answer one query before the next. The
	// in-memory simulator has no such constraint, so tests override it with a
	// short interval to converge closed-loop gotos in milliseconds, not seconds.
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

// decodeTilt maps a raw tilt readback word to physical elevation degrees using
// the configured linear calibration. The Pelco standard is hundredths of a
// degree (offset 0, scale 100); a calibrated raw-encoder head (303Z/3050DZ)
// carries an offset and per-device scale so the raw count decodes to degrees.
func (e *Engine) decodeTilt(raw uint16) float64 {
	return (float64(raw) - e.tiltOffset) / e.tiltScale
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
	e.gotoing, e.mvActive, e.mvTiltActive, e.unwrapping = false, false, false, false
	e.mvTiltHalted, e.mvTiltConfirmTries, e.mvTiltConfirmCount = false, 0, 0
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

// startGoto commands an absolute move to (az, el) degrees. Absolute positioning
// on the 303Z/3050DZ is achieved by CLOSED-LOOP JOG: the device ignores the
// Pelco-D absolute SetPan/SetTilt opcodes (0x4B/0x4D) — confirmed live (zero
// movement for minutes after each set) and by every third-party controller of
// this rotor family — while jog + readback both work. So goto jogs each axis
// toward its target and stops on readback convergence, reusing the cable-wrap
// unwrap pattern (beginGoto/stepGoto/poll keepalive) generalized to both axes.
//
// Azimuth carries cable-wrap protection: a move that would over-wind the cable
// is driven the long way round (MoveUnwrap, accumulator-travel stop); otherwise
// the short path is taken (MoveShort, direct-convergence stop). Elevation jogs
// directly to the target. Repeated identical calls (e.g. key-repeat while held)
// are idempotent so an in-flight goto is not restarted.
func (e *Engine) startGoto(az, el float64) {
	az = e.physAz(az) // command the unit in physical coordinates
	if el < 0 {
		el = 0
	} else if el > 90 {
		el = 90 // keep the jog target inside the physical 0–90° travel
	}
	el = e.physTilt(el)
	if e.gotoing && e.gotoTargetAz == az && e.gotoTargetEl == el {
		return // already heading there; let it run
	}
	// Closed-loop goto is readback-driven, so both axes need a current position.
	if !e.havePan || !e.haveTilt {
		e.status = "goto: no position readback yet — move blocked"
		e.logf(config.LogWarn, "goto blocked: no readback (havePan=%v haveTilt=%v)", e.havePan, e.haveTilt)
		return
	}
	var plan MovePlan
	if e.wrapEnabled {
		plan = planMove(e.wrap, e.curPan, az, e.wrapLimit)
		if plan.Kind == MoveBlock {
			e.status = fmt.Sprintf("wrap: %.0f° unreachable within ±%.0f°", az, e.wrapLimit)
			e.logf(config.LogWarn, "wrap: blocked goto %.0f° (wind %+.0f°, limit ±%.0f°)", az, e.wrap, e.wrapLimit)
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
	e.beginGoto(az, el, plan)
}

// beginGoto arms a closed-loop jog of both axes toward the target. The pan axis
// jogs in the planned direction (accumulator-travel stop for a cable-wrap
// unwrap, direct-convergence stop for a short move); the tilt axis jogs
// directly toward the target elevation. poll() re-sends the combined jog
// keepalive each cycle; stepGoto (on readback) stops each axis as it converges.
func (e *Engine) beginGoto(az, el float64, plan MovePlan) {
	wasMoving := e.gotoing || e.jogging // retarget of live motion vs a standing start
	e.jogging = false
	e.gotoing, e.gotoTargetAz, e.gotoTargetEl = true, az, el

	// Pan axis.
	e.mvActive = plan.Kind == MoveShort || plan.Kind == MoveUnwrap
	e.unwrapping = plan.Kind == MoveUnwrap
	e.mvDir = plan.Dir
	e.mvWrapStart = e.wrap
	e.mvTravel = math.Abs(plan.Travel)

	// Tilt axis (direct convergence).
	dTilt := el - e.curTilt
	e.mvTiltActive = math.Abs(dTilt) > moveTolerance
	e.mvTiltDir = sign(dTilt)
	e.mvTiltStart = e.curTilt
	e.mvTiltTravel = math.Abs(dTilt)

	// Open-loop tilt slew: the 303Z/3050DZ tilt readback is a constant
	// valid-checksum garbage stream while the motor runs, so the closed loop
	// cannot watch it move. When a slew rate is configured, arm a
	// planned-travel deadline — jog blind, halt on the deadline, then confirm
	// against the now-stable idle readback with a bounded correction loop.
	// tiltSlewRate == 0 keeps the legacy closed-loop path (mvTiltHalted stays
	// false, the deadline is never consulted).
	e.mvTiltHalted = false
	e.mvTiltConfirmTries = 0
	e.mvTiltConfirmCount = 0
	if e.tiltSlewRate > 0 && e.mvTiltActive {
		e.armTiltHaltDeadline(e.mvTiltTravel)
	}

	// Stall watchdog. Armed when motion STARTS, not on every retarget: tracking
	// software streams a new target every second or two, and re-arming here each
	// time defeated the backstop completely — a jammed or unreadable axis would
	// jog for as long as the pass lasted. Motion already in flight keeps its
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
		e.gotoing, e.unwrapping = false, false
		e.setMovePoll(false)
		e.send(pelco.Stop(e.addr))
		e.status = "at target"
		return
	}
	if e.unwrapping {
		e.status = fmt.Sprintf("wrap: unwinding %s to reach %.0f°", dirArrow(plan.Dir), e.logicalPan(az))
		e.logf(config.LogInfo, "goto: unwinding %s %.0f° to reach %.0f° / %.0f°",
			dirArrow(plan.Dir), e.mvTravel, e.logicalPan(az), e.logicalTilt(el))
	} else {
		e.status = fmt.Sprintf("seeking %.0f° / %.0f°", e.logicalPan(az), e.logicalTilt(el))
		e.logf(config.LogInfo, "goto: seeking %.2f° / %.2f°", e.logicalPan(az), e.logicalTilt(el))
	}
	e.setMovePoll(true)
	e.sendGotoJog()
}

// armTiltHaltDeadline sets the open-loop tilt slew halt time for a planned
// travel (degrees) at the configured slew rate. The deadline is safety-biased
// short (tiltSlewSafety) so a mis-calibrated rate undershoots rather than
// overshoots into the mechanical stop, and capped at the goto timeout so a
// grossly wrong rate cannot run the slew past the safety backstop.
func (e *Engine) armTiltHaltDeadline(travel float64) {
	ns := travel / e.tiltSlewRate * float64(time.Second) * tiltSlewSafety
	d := time.Duration(ns)
	// Guarantee at least one jog keepalive so a correction for a gap just over
	// the deadband (whose planned deadline would be shorter than one poll)
	// actually moves the motor instead of halting before any jog is sent and
	// then re-reading the same off-target position until the goto times out.
	if d < e.pollInterval {
		d = e.pollInterval
	}
	if d > e.gotoTimeout {
		d = e.gotoTimeout
	}
	e.mvTiltHaltDeadline = time.Now().Add(d)
}

// sendGotoJog emits the combined pan/tilt jog for an in-flight closed-loop goto.
// Each active axis jogs in its planned direction at jogKeySpeed; a converged
// axis is dropped (no direction bit, speed 0) so the device halts that axis
// while the other keeps moving. Pelco-D has no per-axis stop, so a converged
// axis is signalled by omitting its direction bit. If neither axis is active an
// all-stop is sent. beginGoto already mapped the target into physical space, so
// the direction bits map directly to physical increase/decrease (the tiltInvert
// flip for logical/UI directions is NOT applied here).
func (e *Engine) sendGotoJog() {
	if !e.gotoing {
		return
	}
	dir := pelco.Direction{}
	panS, tiltS := 0, 0
	if e.mvActive {
		dir.Pan = e.mvDir
		panS = jogKeySpeed
	}
	// The open-loop tilt slew halts by dropping the tilt direction bit (the
	// device stops the tilt motor while the confirm phase reads the stable
	// idle position). mvTiltActive stays true during confirm so the goto is
	// not finished, but the jog bit is omitted so the motor is stopped.
	if e.mvTiltActive && !e.mvTiltHalted {
		dir.Tilt = e.mvTiltDir
		tiltS = jogKeySpeed
	}
	if !e.mvActive && !e.mvTiltActive {
		e.send(pelco.Stop(e.addr))
		return
	}
	e.send(pelco.Jog(e.addr, dir.Cmd2(), panS, tiltS))
}

// stepGoto advances the closed-loop goto: stops each axis when it reaches its
// target, and ends the goto when both axes have converged. Called from
// handleFrame after each pan/tilt readback; tiltRead is true when the readback
// that triggered this call was a tilt response. The tilt axis logic is gated
// on tiltRead because stepGoto also fires on pan readbacks, and during an
// open-loop tilt slew curTilt is frozen (readback ignored while the motor
// runs) — acting on a pan readback then would see the stale pre-slew curTilt
// and issue a full-travel correction. Pan stops on the cable-wind accumulator
// (unwrap) or on direct azimuth convergence (short move); tilt stops on
// direct elevation convergence with a travel backstop (legacy) or via the
// open-loop halt-and-confirm path. When one axis converges the combined jog
// is re-sent to halt that axis while the other continues; when both converge
// an all-stop ends the goto.
func (e *Engine) stepGoto(tiltRead bool) {
	if !e.gotoing {
		return
	}
	panDone := false
	if e.mvActive {
		travelled := math.Abs(e.wrap - e.mvWrapStart)
		// Accumulator-travel stop (both unwrap and short paths travel mvTravel
		// degrees; the wind accumulator integrates the observed travel either
		// way). For a short move, a direct deadband check stops it precisely.
		atTarget := !e.unwrapping && math.Abs(shortestDelta(e.curPan, e.gotoTargetAz)) < moveTolerance
		if travelled >= e.mvTravel-moveTolerance || atTarget {
			e.mvActive = false
			panDone = true
			e.logf(config.LogDebug, "goto: pan settled (travel %.1f°, at %.2f°)", travelled, e.curPan)
		}
	}
	tiltDone := false
	if e.mvTiltActive && tiltRead {
		if e.tiltSlewRate > 0 {
			// Open-loop tilt path (303Z/3050DZ). The slew is jogged blind and
			// timed by the configured rate; while slewing (not halted) the
			// readback is ignored (handleFrame) and curTilt is frozen, so there
			// is nothing to check here. Once halted, the stable idle readback
			// drives a halt-and-confirm: settle on consecutive within-deadband
			// samples (guards one transient post-halt frame), or issue a
			// bounded correction pulse back toward the target if the blind
			// halt landed short/long.
			if e.mvTiltHalted {
				travelled := math.Abs(e.curTilt - e.mvTiltStart)
				if math.Abs(e.curTilt-e.gotoTargetEl) < moveTolerance {
					e.mvTiltConfirmCount++
					if e.mvTiltConfirmCount >= tiltConfirmSamples {
						e.mvTiltActive = false
						tiltDone = true
						e.logf(config.LogDebug, "goto: tilt settled (travel %.1f°, at %.2f°, %d confirm)",
							travelled, e.curTilt, e.mvTiltConfirmCount)
					}
				} else if e.mvTiltConfirmTries < e.maxTiltConfirms {
					// Blind halt landed off-target: re-jog the remaining gap,
					// then halt-and-confirm again. The correction is itself an
					// open-loop pulse (same rate), so it converges geometrically
					// even with a mis-calibrated rate; bounded by
					// maxTiltConfirms so a grossly wrong rate gives up to the
					// goto timeout instead of hunting forever.
					e.mvTiltConfirmTries++
					e.mvTiltConfirmCount = 0
					corr := math.Abs(e.gotoTargetEl - e.curTilt)
					e.mvTiltDir = sign(e.gotoTargetEl - e.curTilt)
					e.mvTiltHalted = false
					e.armTiltHaltDeadline(corr)
					e.logf(config.LogDebug, "goto: tilt correction #%d %.1f° toward %.2f°",
						e.mvTiltConfirmTries, corr, e.gotoTargetEl)
				}
			}
		} else {
			// Legacy closed-loop tilt path (sim / any head with clean motion
			// readback): stop on direct convergence (deadband) or the travel
			// backstop. This path assumes clean motion readback — a head whose
			// tilt readback goes to garbage while the motor runs (303Z/3050DZ)
			// must use the open-loop path (tilt_slew_rate > 0) above instead.
			travelled := math.Abs(e.curTilt - e.mvTiltStart)
			if math.Abs(e.curTilt-e.gotoTargetEl) < moveTolerance {
				e.mvTiltActive = false
				tiltDone = true
				e.logf(config.LogDebug, "goto: tilt settled (travel %.1f°, at %.2f°)", travelled, e.curTilt)
			} else if travelled >= e.mvTiltTravel+moveTolerance {
				// Overshoot backstop: readback blew past the target without
				// hitting the deadband (a fast slew sampled coarsely).
				e.mvTiltActive = false
				tiltDone = true
				e.logf(config.LogDebug, "goto: tilt travel backstop (travel %.1f°, at %.2f°)", travelled, e.curTilt)
			}
		}
	}
	switch {
	case !e.mvActive && !e.mvTiltActive:
		e.send(pelco.Stop(e.addr))
		e.gotoing, e.unwrapping = false, false
		e.setMovePoll(false)
		e.status = "at target"
		e.logf(config.LogInfo, "goto: at target %.1f° / %.1f°", e.logicalPan(e.curPan), e.logicalTilt(e.curTilt))
	case panDone || tiltDone:
		// One axis converged: re-jog to halt it while the other keeps moving.
		e.sendGotoJog()
	}
}

// expireGoto is the safety backstop: if a closed-loop goto has not converged
// within gotoTimeout (a stuck axis, or persistent tilt readback noise that never
// reaches the deadband), stop all motion and abandon the move. Without this a
// runaway jog would slew the rotor into a mechanical stop.
func (e *Engine) expireGoto() {
	e.send(pelco.Stop(e.addr))
	e.logf(config.LogWarn, "goto: timed out after %v (pan %.1f°→%.1f°, tilt %.1f°→%.1f°)",
		e.gotoTimeout, e.curPan, e.gotoTargetAz, e.curTilt, e.gotoTargetEl)
	e.gotoing, e.mvActive, e.mvTiltActive, e.unwrapping = false, false, false, false
	e.mvTiltHalted, e.mvTiltConfirmTries, e.mvTiltConfirmCount = false, 0, 0
	e.setMovePoll(false)
	e.status = "goto timeout — readback did not converge"
}

// noteGotoProgress pushes the goto stall watchdog out when an axis that is
// being driven has visibly moved since the previous sample. This is what makes
// gotoDeadline a no-progress timer rather than a total-duration timer: a long
// tracking pass never trips it, while a jammed axis — or one whose readback has
// collapsed, so nothing ever converges — still expires within gotoTimeout.
func (e *Engine) noteGotoProgress(axisActive bool, moved float64) {
	if !e.gotoing || !axisActive || math.Abs(moved) < progressDeadband {
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
	e.gotoing, e.mvActive, e.mvTiltActive, e.unwrapping = false, false, false, false
	e.mvTiltHalted, e.mvTiltConfirmTries, e.mvTiltConfirmCount = false, 0, 0
	e.setMovePoll(false)
	e.send(pelco.Stop(e.addr))
}

// poll issues the readback query for one cycle and re-sends any active jog
// keepalive. Exactly ONE position query is sent per cycle — never the
// back-to-back QueryPan+QueryTilt pair the engine once sent. On the 303Z/3050DZ
// (2400 baud, ~58 ms per 7-byte frame) that pair's pan reply collides with the
// outgoing tilt query, collapsing readback to occasional random replies
// (observed live: ~0–1% response rate). One query per cycle, with the next
// query paced by reply completion + pollInterval (see run's pollTimer), gives
// the device a full gap to answer before the next query, restoring reliable
// readback at the cost of halving the per-axis query rate.
//
// During a closed-loop goto the axis being driven is queried: both axes moving
// → alternate pan/tilt (each axis read every other cycle, the safe cadence);
// one axis moving → query that axis every cycle for faster convergence. While
// idle or jogging (UI hold-to-move), pan and tilt alternate.
//
// The goto jog keepalive is sent BEFORE the query, not after. On the real rotator
// this is neutral — a sat rotator moves <0.1° in the ~58 ms frame gap between the
// jog and the query — but it makes the in-memory simulator's readback reflect the
// post-jog position. With query-then-jog the stepped sim would read its pre-jog
// (lagging) position, stop on a near-target sample, and then expose the one-step
// overshoot once idle polling resumes; jog-then-query makes the convergence
// readback the actual rest position.
func (e *Engine) poll() {
	if e.port == nil {
		return
	}
	// Closed-loop goto: re-send the combined jog keepalive first, then query the
	// result. Per-axis convergence is driven by stepGoto on the readback. The
	// safety timeout is checked here too; on expiry the move is abandoned and no
	// query is sent this tick.
	if e.gotoing {
		// Open-loop tilt slew halt (303Z/3050DZ): the tilt readback is constant
		// garbage while the motor runs, so the slew is timed blind by the
		// configured rate. Halt on the planned deadline and enter the confirm
		// phase. Checked BEFORE sendGotoJog so the halt tick emits the jog with
		// the tilt bit DROPPED (mvTiltHalted), stopping the motor, rather than
		// one more slew jog followed by a stop.
		if e.tiltSlewRate > 0 && e.mvTiltActive && !e.mvTiltHalted && time.Now().After(e.mvTiltHaltDeadline) {
			e.mvTiltHalted = true
			e.mvTiltConfirmCount = 0
			e.logf(config.LogDebug, "goto: tilt open-loop slew halted (deadline); confirming")
		}
		e.sendGotoJog()
		if time.Now().After(e.gotoDeadline) {
			e.expireGoto()
			return
		}
	}
	switch {
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
	// UI hold-to-move jog keepalive (separate from the goto jog; jog() clears gotoing).
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
		e.armNextPoll() // a reply (even garbage during a slew) ends the outstanding query
		decoded := e.decodeTilt(raw)
		// Open-loop tilt slew (303Z/3050DZ): while the tilt motor runs the
		// device emits a constant valid-checksum garbage stream (observed live:
		// raw 59776 for the entire slew), NEVER the true position. Readback is
		// trustworthy only once the motor halts (idle). So while the open-loop
		// slew runs, ignore tilt readback entirely — it cannot inform the move
		// and, accepted, would derail the loop with garbage. The halt-and-
		// confirm path reads the stable idle position after the deadline.
		if e.tiltSlewRate > 0 && e.gotoing && e.mvTiltActive && !e.mvTiltHalted {
			e.logf(config.LogTrace, "RX  tilt %02X %02X  (word 0x%04X) — ignored (open-loop slew; readback garbage while motor runs)", byte(raw>>8), byte(raw), raw)
			break
		}
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
// dropped link leaves the unit physically moving, and an active closed-loop
// unwrap (mvActive) must not be resumed against the cable-wind accumulator —
// that accumulator is integrated from observed readback and so misses all
// travel during the disconnect, which would let a resumed unwrap drive the
// cable past the wrap limit. stop() clears the motion state here; its Stop
// send is a no-op now that the port is closed, so a fresh Stop is issued on
// the new link in connect().
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
			Addr:                e.addr,
			StartPan:            e.sim.StartPan,
			StartTilt:           e.sim.StartTilt,
			JogStep:             e.sim.JogStep,
			WildTiltWhileMoving: e.sim.WildTiltWhileMoving,
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
		// gate in startGoto open, so an inbound move armed a closed-loop goto
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
	// closed-loop motion state, so a goto armed while the link was down cannot
	// resume on the fresh link and drive the unit toward a target computed from
	// a position that is now stale. linkFailed() clears the same state on the
	// other path, where a stale wrap accumulator would additionally have let an
	// unwrap run past wrapLimit.
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

func (e *Engine) setMovePoll(fast bool) {
	if e.pollTimer == nil || fast == e.pollFast {
		return
	}
	e.pollFast = fast
	// Closed-loop moves and idle both poll at the configured cadence; the fast/slow
	// distinction is kept for future tuning but currently identical.
	e.pollTimer.Reset(e.pollInterval)
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
		Unwrapping:   e.unwrapping,
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

func dirArrow(dir int) string {
	if dir < 0 {
		return "←"
	}
	return "→"
}
