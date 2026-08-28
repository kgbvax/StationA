package control

import (
	"context"
	"fmt"
	"math"
	"time"

	"pelcobridge2/internal/pelco"
	"pelcobridge2/internal/serialio"
)

// tickKind tags what the engine's single one-shot timer is waiting on. Timers
// only release gates or bound a wait — they never transmit on their own.
type tickKind int

const (
	tickNone     tickKind = iota
	tickFrameGap          // inter-frame silence after every TX
	tickReplyWait
	tickSettle // quiet-line window around absolute sets
)

// query is the one outstanding position query. reply is nil for the engine's
// own verification queries inside the set ladder.
type query struct {
	op    byte
	reply chan Result
}

// ladderStep is one physical axis target of a set ladder.
type ladderStep struct {
	axis   byte // 'p' | 't'
	target float64
}

// ladderState is the set_pos verification ladder: set → settle → one verify
// query → tolerance check → converge or re-send (bench: sets only land on a
// quiet line, readback is garbage while moving).
type ladderState struct {
	steps []ladderStep
	i     int
	tries int
	phase int // 1: set TX due after settle · 2: verify TX due after settle · 3: verify in flight
}

// Engine owns the serial link and all rotator state. Exactly one goroutine
// runs Run; everyone else talks through Request channels and receives Event
// fan-out on the sink.
type Engine struct {
	tr     serialio.Transport
	cfg    Config
	asm    *pelco.Assembler
	reqCh  <-chan Request
	sendCh chan<- Request // same channel as reqCh, sendable view for Call
	sink   chan<- Event
	reopen func() error // nil for in-memory transports

	// state below belongs to the Run goroutine — no mutexes.
	gateOpen bool      // false from every TX until the frame gap elapses
	pending  []Request // motion intents held while the gate is closed
	inFlight *query    // the one outstanding query, if any
	timer    *time.Timer
	tickK    tickKind

	armed    bool
	offset   float64
	jogOp    byte // commanded jog opcode, 0 = stopped
	ladder   *ladderState
	jogSpeed byte

	physAz, physEl  float64
	panAt, tiltAt   time.Time
	havePan, haveEl bool
	deviceOn        bool
	protocol        string // envelope of the last RX frame: "D" or "P"
	errStr          string
	setStat         string // "", "converged", "failed"
}

// New wires an engine to a transport. reopen (real serial only) re-opens the
// port after USB re-enumeration; pass nil for in-memory transports.
func New(cfg Config, tr serialio.Transport, reopen func() error, reqCh chan Request, sink chan<- Event) *Engine {
	return &Engine{
		tr: tr, cfg: cfg, asm: &pelco.Assembler{},
		reqCh: reqCh, sendCh: reqCh, sink: sink, reopen: reopen,
		jogSpeed: cfg.JogSpeed,
		gateOpen: true, // the gate only closes on TX; start with an open line
	}
}

// Run is the engine loop; it returns when ctx is cancelled, after one
// best-effort all-stop.
func (e *Engine) Run(ctx context.Context) {
	e.cfg.fillDefaults()
	e.timer = time.NewTimer(time.Hour)
	if !e.timer.Stop() {
		<-e.timer.C
	}

	rxCh := make(chan []byte, 64)
	errCh := make(chan error, 1)
	go readLoop(e.tr, rxCh, errCh)

	// Publish once BEFORE the loop: with no polling, nothing else guarantees a
	// snapshot — without this the TUI sits on "waiting for engine…" and MQTT
	// has no /state until the first frame happens to arrive.
	e.publish()

	for {
		select {
		case <-ctx.Done():
			e.tx(pelco.StopFrame(e.cfg.Addr), "shutdown all-stop")
			return

		case req := <-e.reqCh:
			e.handle(req)

		case chunk := <-rxCh:
			e.onRX(chunk)

		case err := <-errCh:
			e.deviceOn = false
			e.setError("serial read: " + err.Error())

		case <-e.timer.C:
			e.onTick()
		}
	}
}

// Call submits a request and waits for its reply or the ctx deadline.
func (e *Engine) Call(ctx context.Context, from Source, it Intent) Result {
	return Call(ctx, e.sendCh, from, it)
}

// --- request handling --------------------------------------------------------

func (e *Engine) handle(req Request) {
	switch it := req.Intent.(type) {
	case StopIntent:
		// Always allowed, every source, gate open or closed: human wins.
		stopAll(e)
		e.tx(pelco.StopFrame(e.cfg.Addr), "all-stop")
		e.publish() // Moving just changed
		e.reply(req, Result{})

	case JogIntent:
		// TUI jog is MANUAL movement: allowed disarmed (the operator needs it
		// to position the head before arming). Every other source needs the
		// arm gate — rotctld motion stays a post-arm capability.
		if req.From != SrcTUI && !e.armed {
			e.reply(req, Result{Err: ErrDisarmed})
		} else if !e.gateOpen {
			e.pending = append(e.pending, req)
		} else {
			e.execJog(it.Dir)
		}

	case QueryPanIntent, QueryTiltIntent:
		if !e.gateOpen {
			e.pending = append(e.pending, req)
		} else {
			e.execQuery(req)
		}

	case SetPanIntent:
		if !e.armed {
			e.reply(req, Result{Err: ErrDisarmed})
		} else if !e.gateOpen {
			e.pending = append(e.pending, req)
		} else {
			// true az → physical az; goto-zero bypasses the offset entirely
			e.execSet([]ladderStep{{'p', pelco.Norm360(it.Deg + e.offset)}})
			e.reply(req, Result{})
		}

	case SetTiltIntent:
		if !e.armed {
			e.reply(req, Result{Err: ErrDisarmed})
		} else if !e.gateOpen {
			e.pending = append(e.pending, req)
		} else {
			e.execSet([]ladderStep{{'t', it.Deg}})
			e.reply(req, Result{})
		}

	case GotoPhysZeroIntent:
		// Manual "goto 0" from the TUI: allowed disarmed, like jog. Other
		// sources (rotctld) still need the arm gate.
		if req.From != SrcTUI && !e.armed {
			e.reply(req, Result{Err: ErrDisarmed})
		} else if !e.gateOpen {
			e.pending = append(e.pending, req)
		} else {
			e.execSet([]ladderStep{{'p', 0}, {'t', 0}}) // physical zero, offset never applied
			e.reply(req, Result{})
		}

	case ArmIntent:
		e.reply(req, e.arm(req, it.TrueAz))

	case DisarmIntent:
		if req.From != SrcTUI {
			e.reply(req, Result{Err: ErrSource})
		} else {
			if e.armed {
				e.armed = false
				e.log("disarmed")
				e.publish()
			}
			e.reply(req, Result{})
		}

	case SelfTestIntent:
		if req.From != SrcTUI {
			e.reply(req, Result{Err: ErrSource})
		} else if e.armed {
			e.reply(req, Result{Err: fmt.Errorf("self-test is disarmed-only")})
		} else {
			e.log("SELF-TEST: head will re-home — KEEP CABLES CLEAR")
			e.tx(pelco.SelfTestFrame(e.cfg.Addr), "self-test (preset call 125)")
			e.reply(req, Result{})
		}

	case JogSpeedIntent:
		e.jogSpeed = pelco.ClampSpeed(it.Speed)
		e.publish()
		e.reply(req, Result{})

	case ReopenIntent:
		if e.reopen == nil {
			e.reply(req, Result{})
			return
		}
		err := e.reopen()
		if err != nil {
			e.setError("reopen: " + err.Error())
		} else {
			e.deviceOn = false
			e.publish()
		}
		e.reply(req, Result{Err: err})

	default:
		e.reply(req, Result{Err: fmt.Errorf("unhandled intent %T", req.Intent)})
	}
}

func (e *Engine) reply(req Request, r Result) {
	if req.Reply == nil {
		return
	}
	select {
	case req.Reply <- r:
	default: // caller gave up; never block the engine
	}
}

// --- executors (ungated; callers already passed the gate) --------------------

func (e *Engine) execJog(dir Dir) {
	e.ladder = nil // human wins over any in-flight ladder
	e.jogOp = dirOpcode(dir)
	e.tx(pelco.JogFrame(e.cfg.Addr, e.jogOp, e.jogSpeed), "jog "+dir.String())
	e.publish() // Moving just changed
}

func (e *Engine) execQuery(req Request) {
	if e.inFlight != nil {
		e.reply(req, Result{Err: ErrBusy}) // at most one outstanding query
		return
	}
	if e.moving() {
		e.reply(req, Result{Err: ErrMoving}) // readback is garbage while moving
		return
	}
	// The in-flight record holds the EXPECTED RESPONSE opcode (0x59/0x5B):
	// replies are matched against it, the TX frame uses the query opcodes.
	rsp := byte(pelco.OpRspPan)
	if _, isTilt := req.Intent.(QueryTiltIntent); isTilt {
		rsp = byte(pelco.OpRspTilt)
	}
	e.inFlight = &query{op: rsp, reply: req.Reply}
	e.tx(pelco.QueryFrame(e.cfg.Addr, opQueryFor(rsp)), "query")
}

// execSet starts the verification ladder for the given physical targets.
func (e *Engine) execSet(steps []ladderStep) {
	e.jogOp = 0
	e.ladder = &ladderState{steps: steps, i: 0, tries: e.cfg.SetAttempts, phase: 2}
	e.txSetStep()
}

func (e *Engine) txSetStep() {
	st := e.ladder.steps[e.ladder.i]
	var f pelco.Frame
	if st.axis == 'p' {
		f = pelco.SetPanFrame(e.cfg.Addr, st.target)
	} else {
		f = pelco.SetTiltFrame(e.cfg.Addr, st.target)
	}
	e.tx(f, fmt.Sprintf("set %c=%.2f", st.axis, st.target))
	// The set frame itself needs quiet air around it: hold the gate and wait
	// out the settle window before the verification query (or a re-send).
	e.ladder.phase = 2 // set is on the wire; next settle tick verifies
	e.gateOpen = false
	e.armTick(e.cfg.Settle, tickSettle)
}

func (e *Engine) moving() bool { return e.jogOp != 0 || e.ladder != nil }

func stopAll(e *Engine) {
	e.jogOp = 0
	e.ladder = nil
}

func (e *Engine) arm(req Request, trueAz float64) Result {
	if req.From != SrcTUI {
		return Result{Err: ErrSource} // no code path from MQTT/rotctld exists
	}
	if e.moving() {
		return Result{Err: ErrMoving}
	}
	age := time.Since(e.panAt)
	if !e.havePan || age > e.cfg.ArmMaxReadbackAge {
		e.log("arm refused: pan readback missing or stale (age %s)", fmtAge(age))
		return Result{Err: ErrStale}
	}
	e.offset = e.physAz - trueAz
	e.armed = true
	e.log("ARMED: offset %.2f° (phys %.2f° ↔ true %.2f°)", e.offset, e.physAz, trueAz)
	e.publish()
	return Result{}
}

// --- the single one-shot timer -----------------------------------------------

func (e *Engine) armTick(d time.Duration, k tickKind) {
	if !e.timer.Stop() {
		select {
		case <-e.timer.C:
		default:
		}
	}
	e.timer.Reset(d)
	e.tickK = k
}

func (e *Engine) onTick() {
	switch e.tickK {
	case tickFrameGap:
		e.gateOpen = true
		if e.inFlight != nil {
			// A query is outstanding: bound its wait before releasing the
			// gate to further traffic.
			e.armTick(e.cfg.ReplyWait, tickReplyWait)
			return
		}
		e.drain()

	case tickReplyWait:
		if e.inFlight != nil {
			// No (usable) answer in time: surface the stalled partial first,
			// then give up on the reply.
			e.flushPartials()
			if q := e.inFlight; q.reply != nil {
				select {
				case q.reply <- Result{Err: ErrNoFix}:
				default:
				}
			}
			e.inFlight = nil
			if e.ladder != nil && e.ladder.phase == 3 {
				e.ladderRetry("no readback")
			}
		}
		e.drain()

	case tickSettle:
		if e.ladder != nil {
			switch e.ladder.phase {
			case 1:
				// Quiet line achieved: re-send the absolute set.
				e.txSetStep()
			case 2:
				// Set sent and settled: one verification query (bench: never
				// query while moving, at most one query outstanding).
				st := e.ladder.steps[e.ladder.i]
				op := byte(pelco.OpRspPan)
				if st.axis == 't' {
					op = byte(pelco.OpRspTilt)
				}
				e.inFlight = &query{op: op}
				e.tx(pelco.QueryFrame(e.cfg.Addr, opQueryFor(op)), "verify")
				e.ladder.phase = 3
			}
		}
		e.drain()
	}
}

// drain releases queued motion intents while the gate stays open.
func (e *Engine) drain() {
	for e.gateOpen && len(e.pending) > 0 {
		req := e.pending[0]
		e.pending = e.pending[1:]
		e.execQueued(req)
	}
}

// execQueued runs a gate-held request; its source/armed checks already passed.
func (e *Engine) execQueued(req Request) {
	switch it := req.Intent.(type) {
	case JogIntent:
		e.execJog(it.Dir)
	case QueryPanIntent, QueryTiltIntent:
		e.execQuery(req) // answers via the in-flight reply channel
		return
	case SetPanIntent:
		e.execSet([]ladderStep{{'p', pelco.Norm360(it.Deg + e.offset)}})
	case SetTiltIntent:
		e.execSet([]ladderStep{{'t', it.Deg}})
	case GotoPhysZeroIntent:
		e.execSet([]ladderStep{{'p', 0}, {'t', 0}})
	default:
		e.reply(req, Result{Err: fmt.Errorf("cannot queue %T", req.Intent)})
		return
	}
	e.reply(req, Result{})
}

// ladderRetry waits out another quiet window and re-sends, or fails the ladder.
func (e *Engine) ladderRetry(why string) {
	e.ladder.tries--
	if e.ladder.tries > 0 {
		e.log("set verify: %s — re-sending (%d tries left)", why, e.ladder.tries)
		e.ladder.phase = 1 // quiet first, then re-send (bench: sets need a quiet line)
		e.armTick(e.cfg.Settle, tickSettle)
		return
	}
	e.log("set FAILED: %s", why)
	e.ladder = nil
	e.setStat = "failed"
	e.publish()
}

// --- RX ----------------------------------------------------------------------

func (e *Engine) onRX(chunk []byte) {
	for _, ev := range e.asm.Feed(chunk) {
		if ev.IsNoise() {
			tag := "noise"
			if ev.Partial {
				tag = "partial"
			}
			e.emit(Event{Log: tag + ": " + ev.Noise.Hex()})
			continue
		}
		rx := ev.Frame
		e.deviceOn = true
		switch rx.Op() {
		case pelco.OpRspPan:
			e.gotReadback('p', pelco.WordToDeg(rx.Word()), rx)
		case pelco.OpRspTilt:
			e.gotReadback('t', pelco.WordToDeg(rx.Word()), rx)
		default:
			e.emit(Event{Log: "RX unexpected: " + rx.Hex(), RX: &rx, Dir: "RX"})
		}
	}
}

func (e *Engine) gotReadback(axis byte, deg float64, rx pelco.RxFrame) {
	now := time.Now()
	if axis == 'p' {
		e.physAz, e.panAt, e.havePan = deg, now, true
	} else {
		e.physEl, e.tiltAt, e.haveEl = deg, now, true
	}
	e.protocol = envName(rx.P)
	e.emit(Event{Log: fmt.Sprintf("RX %s  %c %.2f°", rx.Hex(), axis, deg), RX: &rx, Dir: "RX"})

	q := e.inFlight
	if q != nil && q.op == opForAxis(axis) {
		e.inFlight = nil
		if q.reply != nil {
			select {
			case q.reply <- Result{Deg: deg}:
			default:
			}
		}
	}
	e.publish() // fresh readback is a state change

	if e.ladder != nil && e.ladder.phase == 3 {
		e.ladderVerify(deg)
	}
}

func (e *Engine) ladderVerify(deg float64) {
	st := e.ladder.steps[e.ladder.i]
	d := math.Abs(deg - st.target)
	if st.axis == 'p' {
		if d = pelco.Norm360(deg - st.target); d > 180 {
			d = 360 - d
		}
	}
	if d <= e.cfg.SetTolerance {
		if st.axis == 'p' {
			e.physAz = deg
		} else {
			e.physEl = deg
		}
		e.ladder.i++
		if e.ladder.i >= len(e.ladder.steps) {
			e.ladder = nil
			e.setStat = "converged"
			e.publish()
			return
		}
		e.ladder.tries = e.cfg.SetAttempts
		e.ladder.phase = 1
		e.armTick(e.cfg.Settle, tickSettle)
		return
	}
	e.ladderRetry(fmt.Sprintf("readback %.2f° off target %.2f°", deg, st.target))
}

// --- wire TX -----------------------------------------------------------------

func (e *Engine) tx(f pelco.Frame, note string) {
	wire := f[:]
	if err := e.tr.Write(wire); err != nil {
		e.deviceOn = false
		e.setError("serial write: " + err.Error())
		return
	}
	rx := pelco.RxFrame{Frame: f, Wire: wire}
	e.emit(Event{Log: "TX " + f.Hex() + "  " + note, RX: &rx, Dir: "TX"})
	e.gateOpen = false
	e.armTick(serialio.IdleGap(e.cfg.Baud), tickFrameGap)
}

func (e *Engine) flushPartials() {
	for _, ev := range e.asm.FlushIdle() {
		if ev.IsNoise() {
			e.emit(Event{Log: "partial: " + ev.Noise.Hex()})
		}
	}
}

// --- snapshot fan-out ----------------------------------------------------------

func (e *Engine) publish() {
	snap := Snapshot{
		Ts:           time.Now(),
		JogSpeed:     e.jogSpeed,
		Armed:        e.armed,
		Offset:       e.offset,
		Moving:       e.moving(),
		SetStatus:    e.setStat,
		DeviceOnline: e.deviceOn,
		Error:        e.errStr,
		Protocol:     e.protocol,
		Az:           math.NaN(), El: math.NaN(),
		PhysAz: math.NaN(), PhysEl: math.NaN(),
		TargetAz: math.NaN(), TargetEl: math.NaN(),
	}
	if e.havePan {
		snap.PhysAz = e.physAz
		snap.Az = pelco.Norm360(e.physAz - e.offset)
		snap.PanAge = time.Since(e.panAt)
		snap.ReadbackValid = true
		snap.ArmFresh = snap.PanAge <= e.cfg.ArmMaxReadbackAge
	}
	if e.haveEl {
		snap.PhysEl = e.physEl
		snap.El = e.physEl
		snap.TiltAge = time.Since(e.tiltAt)
		snap.ReadbackValid = true
	}
	if e.ladder != nil {
		st := e.ladder.steps[e.ladder.i]
		if st.axis == 'p' {
			snap.TargetAz = pelco.Norm360(st.target - e.offset) // snapshot carries TRUE targets
		} else {
			snap.TargetEl = st.target
		}
		snap.SetStatus = "setting"
	}
	e.emit(Event{Snap: &snap})
}

func (e *Engine) emit(ev Event) {
	select {
	case e.sink <- ev:
	default: // slow consumer: the latest snapshot matters, not the backlog
	}
}

func (e *Engine) log(format string, args ...any) {
	e.emit(Event{Log: fmt.Sprintf(format, args...)})
}

func (e *Engine) setError(msg string) {
	e.errStr = msg
	e.publish()
}

// readLoop pumps the transport into the engine. No timers, no polling — it
// only ever forwards what actually arrived.
func readLoop(tr serialio.Transport, ch chan<- []byte, errCh chan<- error) {
	buf := make([]byte, 256)
	for {
		n, err := tr.Read(buf)
		if n > 0 {
			c := make([]byte, n)
			copy(c, buf[:n])
			ch <- c
		}
		if err != nil {
			errCh <- err
			return
		}
	}
}

// envName names a wire envelope.
func envName(p bool) string {
	if p {
		return "P"
	}
	return "D"
}

// opForAxis is the position opcode for a ladder axis.
func opForAxis(axis byte) byte {
	if axis == 'p' {
		return pelco.OpRspPan
	}
	return pelco.OpRspTilt
}

func fmtAge(d time.Duration) string {
	if d < 0 {
		return "0s"
	}
	return d.Round(time.Millisecond).String()
}

// dirOpcode maps a jog direction onto its Pelco-D opcode.
func dirOpcode(d Dir) byte {
	switch d {
	case DirUp:
		return pelco.OpUp
	case DirDown:
		return pelco.OpDown
	case DirLeft:
		return pelco.OpLeft
	case DirRight:
		return pelco.OpRight
	}
	return pelco.OpStop
}

// opQueryFor is the query opcode that provokes the given response opcode.
func opQueryFor(rsp byte) byte {
	if rsp == pelco.OpRspPan {
		return byte(pelco.OpQueryPan)
	}
	return byte(pelco.OpQueryTilt)
}
