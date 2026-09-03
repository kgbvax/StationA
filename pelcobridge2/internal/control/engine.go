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
// release gates, bound the one outstanding query, and chain ladder steps
// (tickSettle already TXes the ladder's set/check; tickBurst TXes the crawl's
// stop) — the recorded deviation from "a timer never transmits".
type tickKind int

const (
	tickNone      tickKind = iota
	tickFrameGap          // inter-frame silence after every TX
	tickReplyWait
	tickSettle // quiet-line window around absolute sets
	tickBurst  // crawl: one jog burst is over (stop the head)
)

// maxPending caps the gate-closed request queue. Without a cap a stuck line
// (head unplugged mid-move) grows the queue without bound and replays every
// stale command the moment the gate reopens.
const maxPending = 16

// readErr tags a transport read failure with the generation of the reader
// that produced it, so an error from a reader that has already been replaced
// (reopen started a fresh one) does not tear down the live one.
type readErr struct {
	gen int
	err error
}

// reopenCooldown throttles the auto-reopen: a flapping USB adapter or a dead
// TCP peer must not spin open/read/close in a tight loop.
const reopenCooldown = 2 * time.Second

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
// query → tolerance check → converge or fail (bench: sets only land on a
// quiet line, readback is garbage while moving). ONE set and ONE verify per
// step, no automatic re-send: a re-send moves the head again with no operator
// behind it (bench decision 2026-08-30) — the operator reads the failure and
// re-issues. Phase 1 exists for MULTI-STEP ladders (settle before the next
// step's set), not for retries.
//
// crawl mode is a ladder KIND, not a new pointer: steps carry physical
// targets and every unwind path (stopAll, tx failure, execJog) already nulls
// e.ladder. The crawl loop starts at phase 3 (read state first), then
// 4 (jog burst running, tickBurst armed) → stop → 2 (settle) → 3 (check
// query) → converge / next burst, until within CrawlTol or the burst cap.
// Phase 1 is never used by crawl.
type ladderState struct {
	steps   []ladderStep
	i       int
	phase   int // 1: next step's set TX due after settle · 2: verify TX due after settle · 3: verify in flight · 4: crawl burst running
	crawl   bool
	burst   int     // bursts spent on steps[i] (crawl only)
	lastDeg float64 // last readback for steps[i]; NaN before the first
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

	// The head's periodic self-check, as the engine models it: "on", "off",
	// or "unknown". RS-485 has no link-level ACK, so a write that left the
	// adapter proves nothing — a claim lands only once the head has proven
	// it is alive (a checksum-valid frame AFTER the preset frame went out).
	// selfCheckPend holds a sent-but-unproven claim until that proof arrives
	// (onRX); any link death drops the model back to "unknown" (the head may
	// have power-cycled to factory defaults: self-check on).
	selfCheck     string
	selfCheckPend string // "" none, else the claim awaiting proof of life

	// Reader generation: the transport reader dies on every read error (a
	// closed/re-enumerated USB fd, a dropped TCP mock). startReader spawns a
	// fresh one; late errors from a stale generation are ignored by tag.
	rxCh       chan []byte
	readErrCh  chan readErr
	readGen    int
	lastReopen time.Time

	physAz, physEl  float64
	panAt, tiltAt   time.Time
	havePan, haveEl bool
	deviceOn        bool
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

	e.rxCh = make(chan []byte, 64)
	e.readErrCh = make(chan readErr, 4)
	e.startReader()

	// The head's periodic self-check re-homes it UNPROMPTED — unacceptable for
	// an antenna rotor mid-contact. Disable it (preset set 105) once per
	// connect, before anything else uses the line. If the head is merely
	// powered off the TX can still succeed (RS-485 has no ACK), so the "off"
	// claim stays pending until the head proves it is alive.
	e.selfCheckUnknown()
	e.txSelfCheck(false)

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

		case chunk := <-e.rxCh:
			e.onRX(chunk)

		case re := <-e.readErrCh:
			e.onReadErr(re)

		case <-e.timer.C:
			e.onTick()
		}
	}
}

// startReader spawns one reader generation for the transport. The previous
// generation, if any, dies on its own error (or when a reopen closes the old
// fd under it); its late error arrives tagged with the stale generation and
// is ignored in onReadErr.
func (e *Engine) startReader() {
	e.readGen++
	gen := e.readGen
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := e.tr.Read(buf)
			if n > 0 {
				c := make([]byte, n)
				copy(c, buf[:n])
				e.rxCh <- c
			}
			if err != nil {
				select {
				case e.readErrCh <- readErr{gen: gen, err: err}:
				default: // engine gone (shutdown); nothing is reading anymore
				}
				return
			}
		}
	}()
}

// onReadErr handles a transport read failure: mark the head offline, then
// auto-heal by reopening and starting a fresh reader — a re-enumerated USB
// adapter or a dropped TCP mock must not leave the engine deaf until restart
// (the bench failure recorded in ptest and hit live by ultrabridge).
func (e *Engine) onReadErr(re readErr) {
	if re.gen != e.readGen {
		return // stale reader; a fresh one already owns the line
	}
	e.deviceOn = false
	e.selfCheckUnknown() // the link died: the head's self-check is unproven
	e.setError("serial read: " + re.err.Error())
	if e.reopen == nil {
		return // in-memory transport: nothing to heal
	}
	if time.Since(e.lastReopen) < reopenCooldown {
		return // flapping link: wait for the cooldown (ctrl+r always works)
	}
	e.lastReopen = time.Now()
	if err := e.reopen(); err != nil {
		e.setError("reopen: " + err.Error())
		return
	}
	e.startReader()
	e.resendSelfCheckDisable() // the head may have power-cycled meanwhile
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
		// Queued motion dies with the ladder — otherwise a set or jog parked
		// in the gate-closed window would drain and start moving right after
		// the all-stop frame.
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
			e.enqueue(req)
		} else {
			e.execJog(it.Dir)
			e.reply(req, Result{})
		}

	case QueryPanIntent, QueryTiltIntent:
		if !e.gateOpen {
			e.enqueue(req)
		} else {
			e.execQuery(req)
		}

	case SetPanIntent:
		if !e.armed {
			e.reply(req, Result{Err: ErrDisarmed})
		} else if !finiteDeg(it.Deg) {
			e.reply(req, Result{Err: fmt.Errorf("azimuth %v is not finite", it.Deg)})
		} else if !e.gateOpen || e.inFlight != nil || e.ladder != nil {
			// Queue while a query is outstanding too: execSet would overwrite
			// e.inFlight and orphan the caller's reply channel; queue while a
			// ladder runs so a new set cannot silently drop a goto-zero's
			// remaining steps.
			e.enqueue(req)
		} else {
			// true az → physical az; goto-zero bypasses the offset entirely
			e.reply(req, Result{Err: e.execSet([]ladderStep{{'p', pelco.Norm360(it.Deg + e.offset)}})})
		}

	case SetTiltIntent:
		if !e.armed {
			e.reply(req, Result{Err: ErrDisarmed})
		} else if !finiteDeg(it.Deg) {
			e.reply(req, Result{Err: fmt.Errorf("elevation %v is not finite", it.Deg)})
		} else if !elInRange(it.Deg) {
			e.reply(req, Result{Err: fmt.Errorf("elevation %v is outside 0..90", it.Deg)})
		} else if !e.gateOpen || e.inFlight != nil || e.ladder != nil {
			e.enqueue(req)
		} else {
			// true el → native tilt; the ladder target is physical (native),
			// like a pan target — the tilt scale is inverted (bench 2026-08-30)
			e.reply(req, Result{Err: e.execSet([]ladderStep{{'t', pelco.ElToTilt(it.Deg)}})})
		}

	case GotoPhysZeroIntent:
		// Manual "goto 0" from the TUI: allowed disarmed, like jog. Other
		// sources (rotctld) still need the arm gate.
		if req.From != SrcTUI && !e.armed {
			e.reply(req, Result{Err: ErrDisarmed})
		} else if !e.gateOpen || e.inFlight != nil || e.ladder != nil {
			e.enqueue(req)
		} else {
			// physical zero, offset never applied
			e.reply(req, Result{Err: e.execSet([]ladderStep{{'p', 0}, {'t', 0}})})
		}

	case GotoAzElIntent:
		// Manual target entry from the TUI ("g" prompt): same manual-
		// positioning class as goto-0 — allowed disarmed. It speaks TRUE
		// coordinates and ALWAYS crosses the current offset (the arm
		// calibration stays valid across a disarm; disarm never clears it).
		// Other sources need the arm gate.
		if req.From != SrcTUI && !e.armed {
			e.reply(req, Result{Err: ErrDisarmed})
		} else if (it.HasAz && !finiteDeg(it.Az)) || (it.HasEl && !finiteDeg(it.El)) {
			e.reply(req, Result{Err: fmt.Errorf("goto target %v/%v is not finite", it.Az, it.El)})
		} else if it.HasEl && !elInRange(it.El) {
			e.reply(req, Result{Err: fmt.Errorf("elevation %v is outside 0..90", it.El)})
		} else if !it.HasAz && !it.HasEl {
			e.reply(req, Result{Err: fmt.Errorf("goto with no axis selected")})
		} else if !e.gateOpen || e.inFlight != nil || e.ladder != nil {
			e.enqueue(req)
		} else {
			e.reply(req, Result{Err: e.execSet(gotoSteps(it, e.offset))})
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
		} else if e.moving() {
			e.reply(req, Result{Err: ErrMoving}) // never re-home under a ladder or jog
		} else if !e.gateOpen || e.inFlight != nil {
			// A frame sent inside a settle/reply window would replace the
			// ladder's timer and wedge it; a re-home is never urgent — retry.
			e.reply(req, Result{Err: ErrBusy})
		} else {
			e.log("SELF-TEST: head will re-home — pan sweeps right up to 720°, KEEP CABLES CLEAR")
			if e.tx(pelco.SelfTestFrame(e.cfg.Addr), "self-test (preset call 125)") {
				// The re-home invalidates every readback we hold: the arm
				// gate must not accept a pre-re-home position as fresh.
				// Factory defaults also restore the periodic self-check.
				e.havePan, e.haveEl = false, false
				e.selfCheck = "on"
				e.log("self-test sent: readback invalidated; periodic self-check is back ON — press c to re-disable")
				e.publish()
				e.reply(req, Result{})
			} else {
				// tx recorded the write error; the reply must not claim a
				// frame that never left the wire.
				e.reply(req, Result{Err: ErrTxFail})
			}
		}

	case SelfCheckIntent:
		if req.From != SrcTUI {
			e.reply(req, Result{Err: ErrSource})
		} else if it.Enable && e.armed {
			// An armed rotator is in operational use; enabling a periodic
			// un-prompted re-home under it is exactly the hazard the arm
			// gate exists for.
			e.reply(req, Result{Err: fmt.Errorf("enabling the periodic self-check is disarmed-only")})
		} else if e.moving() {
			// Same refusal as the self-test: a preset frame under motion is
			// "do not", not "retry" — ErrBusy alone means retry.
			e.reply(req, Result{Err: ErrMoving})
		} else if !e.gateOpen || e.inFlight != nil {
			e.reply(req, Result{Err: ErrBusy}) // a preset frame must not break a settle window
		} else if it.Enable {
			e.log("SELF-CHECK ENABLE: head will re-home itself UNPROMPTED while on")
			if e.txSelfCheck(true) {
				e.reply(req, Result{})
			} else {
				e.reply(req, Result{Err: ErrTxFail})
			}
		} else {
			if e.txSelfCheck(false) {
				e.reply(req, Result{})
			} else {
				e.reply(req, Result{Err: ErrTxFail})
			}
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
			e.lastReopen = time.Now()
			e.deviceOn = false
			e.selfCheckUnknown() // reopened line: the head's self-check is unproven
			e.publish()
			// The old reader died with the old fd: a reopened port is deaf
			// unless a fresh reader generation picks it up.
			e.startReader()
			e.resendSelfCheckDisable() // the head may have power-cycled meanwhile
		}
		e.reply(req, Result{Err: err})

	default:
		e.reply(req, Result{Err: fmt.Errorf("unhandled intent %T", req.Intent)})
	}
}

// txSelfCheck sends one periodic-self-check preset frame and models the
// result — the ONE site that may change e.selfCheck (never claim a state
// change the wire never saw). A write that left the USB adapter proves
// nothing on RS-485: the head can be unpowered. The claim lands only when
// the head has proven it is alive; otherwise it pends until the first
// checksum-valid frame arrives (onRX).
func (e *Engine) txSelfCheck(enable bool) bool {
	f, note, val := pelco.SelfCheckDisableFrame(e.cfg.Addr), "disable periodic self-check (preset set 105)", "off"
	if enable {
		f, note, val = pelco.SelfCheckEnableFrame(e.cfg.Addr), "enable periodic self-check (preset call 105)", "on"
	}
	if !e.tx(f, note) {
		return false // tx recorded the write error; claim nothing
	}
	if e.deviceOn {
		e.selfCheck = val
		e.publish()
	} else {
		e.selfCheckPend = val // land the claim when the head proves it is alive
	}
	return true
}

// selfCheckUnknown drops the model to "unknown": the head's actual self-check
// is unproven — the link died, the port reopened, the head may have
// power-cycled to factory defaults (self-check on).
func (e *Engine) selfCheckUnknown() {
	e.selfCheck, e.selfCheckPend = "unknown", ""
}

// resendSelfCheckDisable re-sends the connect-time disable after a successful
// reopen: the head may have power-cycled while the line was down. Skipped
// under motion or a closed gate — a preset frame must not break a ladder's
// settle window; the model then stays "unknown" until the operator re-sends
// with the TUI c key.
func (e *Engine) resendSelfCheckDisable() {
	if e.moving() || !e.gateOpen || e.inFlight != nil {
		return
	}
	e.txSelfCheck(false)
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
	e.setStat = "" // a jog moves the head: a previous "converged" is stale news
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

// execSet starts the verification ladder for the given physical targets —
// the single point every goto source funnels through, so crawl mode (cfg.Crawl)
// applies to all of them at once. Refused under a manual jog: a jog never
// self-stops, and a ladder's check readback would be garbage mid-motion —
// the caller replies ErrMoving and the operator's hold-stop ends the jog.
// Both modes report a failed first write (ErrWire) instead of claiming a
// ladder that tx already unwound.
func (e *Engine) execSet(steps []ladderStep) error {
	if e.jogOp != 0 {
		// Fire-and-forget callers (the TUI prompt) never see the Result —
		// the log line is the only place the operator learns of the refusal.
		e.log("goto refused: a manual jog is active (stop it first)")
		return ErrMoving
	}
	e.ladder = &ladderState{steps: steps, i: 0, crawl: e.cfg.Crawl, lastDeg: math.NaN()}
	if e.cfg.Crawl {
		return e.crawlStepStart() // read state first: one query, no set frame
	}
	e.ladder.phase = 2
	if !e.txSetStep() {
		return ErrWire // tx unwound the ladder; nothing is armed
	}
	return nil
}

// crawlStepStart TXes the position check query for steps[i] and leaves phase
// 3 — the frame-gap tick arms the reply wait for free, and a lost readback
// fails the crawl through the same tickReplyWait path as a set ladder.
func (e *Engine) crawlStepStart() error {
	st := e.ladder.steps[e.ladder.i]
	rsp := opForAxis(st.axis)
	e.inFlight = &query{op: rsp}
	if !e.tx(pelco.QueryFrame(e.cfg.Addr, opQueryFor(rsp)), "crawl query") {
		return ErrWire // tx unwound the ladder; nothing is armed
	}
	e.ladder.phase = 3
	return nil
}

// crawlVerify consumes one check readback: converge the step, move to the
// next axis, or jog one burst toward the target. Human wins at every
// readback: the drain runs a parked jog outright, and the queue scan below
// catches one that could not run yet — without these a mid-crawl jog would
// sit queued for the whole crawl (minutes) while the operator holds the key.
// The e-stop cut through earlier as always.
func (e *Engine) crawlVerify(deg float64) {
	// This readback just answered the check query: the gate is open and no
	// query is outstanding — the one moment in a crawl cycle where draining is
	// free. Without it the gate stays closed for whole cycles and parked
	// queries (rotctld get_pos polls whose callers long timed out) pile up
	// unanswered behind the ladder.
	e.drain()
	if e.ladder == nil {
		// A drained jog took over mid-drain — execJog killed the crawl and
		// already published; nothing here may touch the dead ladder.
		e.log("crawl cancelled: manual jog")
		return
	}
	// The reply can also land inside the frame gap (gate still closed, drain
	// a no-op): scan the WHOLE queue, not just the head — a jog parked behind
	// timed-out query zombies must still win.
	for _, pq := range e.pending {
		if _, isJog := pq.Intent.(JogIntent); isJog {
			e.log("crawl cancelled: manual jog")
			e.ladder = nil
			e.setStat = "" // a cancelled crawl must not inherit "converged"/"failed"
			e.publish()
			e.drain()
			return
		}
	}
	st := e.ladder.steps[e.ladder.i]
	if !math.IsNaN(e.ladder.lastDeg) {
		// Measured travel of the previous burst — the raw data the later
		// "learn deg/s per speed byte" version will consume. Keep the format.
		e.log("crawl %c: %+.2f° in %.1fs @ 0x%02X (err %.2f° → %.2f°)",
			st.axis, signedAxisDelta(st.axis, e.ladder.lastDeg, deg),
			e.cfg.CrawlBurst.Seconds(), e.cfg.CrawlSpeed,
			axisErr(st.axis, st.target, e.ladder.lastDeg), axisErr(st.axis, st.target, deg))
	}
	e.ladder.lastDeg = deg
	if axisErr(st.axis, st.target, deg) <= e.cfg.CrawlTol {
		e.ladder.i++
		e.ladder.burst = 0
		e.ladder.lastDeg = math.NaN() // per-step: never measure across axes
		if e.ladder.i >= len(e.ladder.steps) {
			e.ladder = nil
			e.setStat = "converged"
			e.publish()
			e.drain()
			return
		}
		if err := e.crawlStepStart(); err != nil {
			// The requester was already told success; tx logged the write
			// error and unwound the ladder. Nothing else to do here.
			e.log("crawl: next-axis read failed: %v", err)
		}
		return
	}
	if e.ladder.burst >= e.cfg.CrawlMaxBursts {
		e.ladderFail(fmt.Sprintf("crawl: %d bursts on %c, still %.2f° off target %.2f°",
			e.ladder.burst, st.axis, axisErr(st.axis, st.target, deg), st.target))
		return
	}
	if !e.tx(pelco.JogFrame(e.cfg.Addr, jogToward(st.axis, st.target, deg), e.cfg.CrawlSpeed),
		fmt.Sprintf("crawl %c toward %.2f (burst %d)", st.axis, st.target, e.ladder.burst+1)) {
		// tx unwound (invariant 5) and re-opened the gate, but armed no
		// timer — drain here or the queue strands until unrelated traffic.
		e.drain()
		return
	}
	e.ladder.burst++
	e.ladder.phase = 4
	e.armTick(e.cfg.CrawlBurst, tickBurst) // replaces the frame-gap tick
}

// gotoSteps shapes a GotoAzElIntent's TRUE az/el target into physical ladder
// steps: pan crosses the arm offset (physical = Norm360(true + offset),
// exactly where a SetPanIntent drives the head), elevation mirrors into the
// head's native tilt word. Axes the intent did not select get no step.
func gotoSteps(it GotoAzElIntent, offset float64) []ladderStep {
	var steps []ladderStep
	if it.HasAz {
		steps = append(steps, ladderStep{'p', pelco.Norm360(it.Az + offset)})
	}
	if it.HasEl {
		steps = append(steps, ladderStep{'t', pelco.ElToTilt(it.El)})
	}
	return steps
}

// txSetStep puts the current step's absolute-set frame on the wire and arms
// the settle window. False means the write failed and tx already unwound the
// ladder (invariant 5) — callers must not touch e.ladder afterwards.
func (e *Engine) txSetStep() bool {
	st := e.ladder.steps[e.ladder.i]
	var f pelco.Frame
	if st.axis == 'p' {
		f = pelco.SetPanFrame(e.cfg.Addr, st.target)
	} else {
		f = pelco.SetTiltFrame(e.cfg.Addr, st.target)
	}
	if !e.tx(f, fmt.Sprintf("set %c=%.2f", st.axis, st.target)) {
		return false // the write failed; tx unwound and killed the ladder
	}
	// The set frame itself needs quiet air around it: hold the gate and wait
	// out the settle window before the verification query (or the next step's
	// set, in a multi-step ladder).
	e.ladder.phase = 2 // set is on the wire; next settle tick verifies
	e.gateOpen = false
	e.armTick(e.cfg.Settle, tickSettle)
	return true
}

func (e *Engine) moving() bool { return e.jogOp != 0 || e.ladder != nil }

func stopAll(e *Engine) {
	e.jogOp = 0
	e.ladder = nil
	e.setStat = "" // the all-stop voids every set outcome; "converged" must not outlive it
	// Queued motion is motion that has not happened YET. An e-stop must kill
	// it, or the next drain replays a stale set/jog seconds after the all-stop
	// frame — motion with no operator input behind it.
	for _, req := range e.pending {
		e.reply(req, Result{Err: ErrCancelled})
	}
	e.pending = nil
}

// enqueue parks a request for the next drain, honouring the queue cap.
func (e *Engine) enqueue(req Request) {
	if len(e.pending) >= maxPending {
		e.reply(req, Result{Err: ErrBusy}) // a stuck line must not grow the queue
		return
	}
	e.pending = append(e.pending, req)
}

func (e *Engine) arm(req Request, trueAz float64) Result {
	if req.From != SrcTUI {
		return Result{Err: ErrSource} // no code path from MQTT/rotctld exists
	}
	if !finiteDeg(trueAz) || trueAz < 0 || trueAz > 360 {
		return Result{Err: fmt.Errorf("true azimuth %v is not a number in 0..360", trueAz)}
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
				e.ladderFail("no readback")
			}
		}
		e.drain()

	case tickSettle:
		if e.ladder != nil {
			switch e.ladder.phase {
			case 1:
				// Quiet line achieved: the NEXT step's set (multi-step
				// ladders only — single-step ladders never revisit phase 1).
				e.txSetStep()
			case 2:
				// Set (or crawl stop) sent and settled: one verification
				// query (bench: never query while moving, at most one query
				// outstanding). Crawl reuses this path after its burst.
				st := e.ladder.steps[e.ladder.i]
				op := byte(pelco.OpRspPan)
				if st.axis == 't' {
					op = byte(pelco.OpRspTilt)
				}
				e.inFlight = &query{op: op}
				if e.tx(pelco.QueryFrame(e.cfg.Addr, opQueryFor(op)), "verify") {
					e.ladder.phase = 3
				}
				// A failed write unwound the ladder (invariant 5) — never
				// touch e.ladder afterwards; the drain below releases the queue.
			}
		}
		e.drain()

	case tickBurst:
		// The crawl's jog burst is over: stop the head, then settle out
		// before the check query (garbage while moving).
		if e.ladder != nil && e.ladder.phase == 4 {
			if e.tx(pelco.StopFrame(e.cfg.Addr), "crawl burst end (stop)") {
				e.ladder.phase = 2
				e.armTick(e.cfg.Settle, tickSettle) // tickSettle case 2 TXes the check query
			}
			// A failed stop write unwound the ladder (invariant 5): fall
			// through to the drain — no timer is armed, the queue must not
			// strand on a line that just proved writable-failure.
		}
		e.drain()
	}
}

// drain releases queued motion intents while the gate stays open. Sets are
// left queued while a query is outstanding or a ladder runs — starting one
// would clobber e.inFlight or drop the ladder's remaining steps; the ladder's
// completion calls drain again.
func (e *Engine) drain() {
	for e.gateOpen && len(e.pending) > 0 {
		req := e.pending[0]
		if blocksSet(req.Intent) && (e.inFlight != nil || e.ladder != nil) {
			return // head of queue must wait; FIFO ordering keeps the rest
		}
		e.pending = e.pending[1:]
		e.execQueued(req)
	}
}

// blocksSet reports whether an intent must not run concurrently with an
// outstanding query or an active set ladder.
func blocksSet(it Intent) bool {
	switch it.(type) {
	case SetPanIntent, SetTiltIntent, GotoPhysZeroIntent, GotoAzElIntent:
		return true
	}
	return false
}

// execQueued runs a gate-held request; its source checks already passed. The
// armed check is repeated: state may have changed while the request sat
// queued (e.g. a disarm during a settle window).
func (e *Engine) execQueued(req Request) {
	var err error
	switch it := req.Intent.(type) {
	case JogIntent:
		e.execJog(it.Dir)
	case QueryPanIntent, QueryTiltIntent:
		e.execQuery(req) // answers via the in-flight reply channel
		return
	case SetPanIntent:
		if !e.armed {
			e.reply(req, Result{Err: ErrDisarmed})
			return
		}
		err = e.execSet([]ladderStep{{'p', pelco.Norm360(it.Deg + e.offset)}})
	case SetTiltIntent:
		if !e.armed {
			e.reply(req, Result{Err: ErrDisarmed})
			return
		}
		if !elInRange(it.Deg) {
			err = fmt.Errorf("elevation %v is outside 0..90", it.Deg)
		} else {
			// true el → native tilt (inverted scale, bench 2026-08-30)
			err = e.execSet([]ladderStep{{'t', pelco.ElToTilt(it.Deg)}})
		}
	case GotoPhysZeroIntent:
		err = e.execSet([]ladderStep{{'p', 0}, {'t', 0}})
	case GotoAzElIntent:
		if it.HasEl && !elInRange(it.El) {
			err = fmt.Errorf("elevation %v is outside 0..90", it.El)
		} else {
			err = e.execSet(gotoSteps(it, e.offset))
		}
	default:
		e.reply(req, Result{Err: fmt.Errorf("cannot queue %T", req.Intent)})
		return
	}
	e.reply(req, Result{Err: err})
}

// ladderFail ends the ladder as failed. No automatic re-send: an off-target
// verify or a missing readback is news for the operator, not a trigger to
// move the head again (bench decision 2026-08-30) — the operator re-issues.
func (e *Engine) ladderFail(why string) {
	e.log("set FAILED: %s", why)
	e.ladder = nil
	e.setStat = "failed"
	e.publish()
	e.drain() // the queue may proceed even though this ladder failed
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
		// First proof of life since a self-check frame went out: the
		// sent-but-unproven claim becomes fact.
		if e.selfCheckPend != "" {
			e.selfCheck = e.selfCheckPend
			e.selfCheckPend = ""
			e.publish()
		}
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
	e.emit(Event{Log: fmt.Sprintf("RX %s  %c %.2f°", rx.Hex(), axis, deg), RX: &rx, Dir: "RX"})

	q := e.inFlight
	matched := q != nil && q.op == opForAxis(axis)
	if matched {
		e.inFlight = nil
		if q.reply != nil {
			// USER queries are answered in TRUE degrees (pan: offset applied;
			// tilt: native word mirrored to elevation) so get_pos, the TUI's
			// 'a'/'e' readouts, and set_pos's argument convention all speak
			// the same coordinate frame. The ladder's own verification
			// queries (reply == nil) keep the raw physical readback — its
			// targets are physical.
			ans := deg
			if axis == 'p' {
				ans = pelco.Norm360(deg - e.offset)
			} else {
				ans = pelco.TiltToEl(deg)
			}
			select {
			case q.reply <- Result{Deg: ans}:
			default:
			}
		}
	}
	e.publish() // fresh readback is a state change

	// Only a readback that ANSWERS the ladder's outstanding query drives the
	// ladder: a wrong-axis frame, a duplicate reply, or the late answer to an
	// already-expired query must not be mistaken for the check readback — a
	// bogus error would jog the head off garbage data (and violate the one
	// outstanding query rule).
	if matched && e.ladder != nil && e.ladder.phase == 3 {
		if e.ladder.crawl {
			e.crawlVerify(deg)
		} else {
			e.ladderVerify(deg)
		}
	}
}

func (e *Engine) ladderVerify(deg float64) {
	st := e.ladder.steps[e.ladder.i]
	if axisErr(st.axis, st.target, deg) <= e.cfg.SetTolerance {
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
			e.drain() // sets queued behind the ladder may run now
			return
		}
		e.ladder.phase = 1 // settle out before the next step's set
		e.armTick(e.cfg.Settle, tickSettle)
		return
	}
	e.ladderFail(fmt.Sprintf("readback %.2f° off target %.2f°", deg, st.target))
}

// --- wire TX -----------------------------------------------------------------

// tx sends one frame on the wire. A failed write must unwind the
// state machine, not wedge it: the caller may already have stored an inFlight
// query or a ladder phase that would otherwise never resolve, because no
// timer is armed when nothing went out on the wire. The return reports whether
// the write succeeded, so callers that model device-side state (the periodic
// self-check) do not claim a frame that never left.
func (e *Engine) tx(f pelco.Frame, note string) bool {
	wire := []byte(f[:])
	if err := e.tr.Write(wire); err != nil {
		e.deviceOn = false
		if q := e.inFlight; q != nil {
			e.inFlight = nil
			if q.reply != nil {
				select {
				case q.reply <- Result{Err: ErrNoFix}:
				default:
				}
			}
		}
		if e.ladder != nil {
			e.ladder = nil
			e.setStat = "failed"
		}
		e.jogOp = 0
		e.gateOpen = true // no frame on the wire: the gate has nothing to protect
		e.setError("serial write: " + err.Error())
		return false
	}
	rx := pelco.RxFrame{Frame: f, Wire: wire}
	e.emit(Event{Log: "TX " + rx.Hex() + "  " + note, RX: &rx, Dir: "TX"})
	e.gateOpen = false
	e.armTick(serialio.IdleGap(e.cfg.Baud), tickFrameGap)
	return true
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
	selfCheck := e.selfCheck
	if selfCheck == "" {
		selfCheck = "unknown" // canonical tri-state: "" must never leave the engine
	}
	snap := Snapshot{
		Ts:           time.Now(),
		JogSpeed:     e.jogSpeed,
		Armed:        e.armed,
		Offset:       e.offset,
		Moving:       e.moving(),
		SetStatus:    e.setStat,
		SelfCheck:    selfCheck,
		DeviceOnline: e.deviceOn,
		Error:        e.errStr,
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
		snap.PhysEl = e.physEl             // raw native tilt word
		snap.El = pelco.TiltToEl(e.physEl) // true elevation (inverted scale)
		snap.TiltAge = time.Since(e.tiltAt)
		snap.ReadbackValid = true
	}
	if e.ladder != nil {
		st := e.ladder.steps[e.ladder.i]
		if st.axis == 'p' {
			snap.TargetAz = pelco.Norm360(st.target - e.offset) // snapshot carries TRUE targets
		} else {
			snap.TargetEl = pelco.TiltToEl(st.target)
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

// finiteDeg rejects the NaN/Inf values strconv.ParseFloat happily produces
// ("nan", "inf"): DegToWord would park them at 0° and arm would compute a NaN
// offset — both real motion with garbage targets.
func finiteDeg(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// elInRange rejects TRUE elevations the head cannot reach: the native tilt
// word spans 0..90 and el mirrors onto the same range. The set ladder clamps
// at the wire (SetTiltFrame, "overshooting the head's travel is the dangerous
// direction"), but a crawl never builds a set frame — without this gate an
// out-of-range el would be chased with jog bursts straight into the
// mechanical limit until the burst cap.
func elInRange(el float64) bool { return el >= 0 && el <= 90 }

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

// dirOpcode maps a jog direction onto its Pelco-D opcode. The tilt pair is
// SWAPPED: the jog opcodes speak the head's native (inverted) tilt scale, so
// OpUp raises native tilt — i.e. LOWERS the antenna (bench 2026-08-30). A jog
// "up" that means "raise elevation" must therefore send OpDown.
func dirOpcode(d Dir) byte {
	switch d {
	case DirUp:
		return pelco.OpDown
	case DirDown:
		return pelco.OpUp
	case DirLeft:
		return pelco.OpLeft
	case DirRight:
		return pelco.OpRight
	}
	return pelco.OpStop
}

// axisErr is the distance in degrees from a readback to a ladder target:
// wraparound-shortest for pan, absolute for the tilt word.
func axisErr(axis byte, target, deg float64) float64 {
	if axis != 'p' {
		return math.Abs(deg - target)
	}
	d := pelco.Norm360(deg - target)
	if d > 180 {
		return 360 - d
	}
	return d
}

// signedAxisDelta is the signed travel from a to b: wraparound-shortest for
// pan, plain difference for the tilt word.
func signedAxisDelta(axis byte, a, b float64) float64 {
	if axis != 'p' {
		return b - a
	}
	d := pelco.Norm360(b - a)
	if d > 180 {
		return d - 360
	}
	return d
}

// jogToward picks the jog opcode that moves the axis from cur toward target.
// Both speak the head's NATIVE frame — the tilt pair is deliberately NOT
// swapped here (unlike dirOpcode, which translates the user-facing "up").
func jogToward(axis byte, target, cur float64) byte {
	if axis == 'p' {
		if signedAxisDelta(axis, cur, target) > 0 {
			return pelco.OpRight
		}
		return pelco.OpLeft
	}
	if target > cur {
		return pelco.OpUp // OpUp RAISES native tilt (lowers the antenna)
	}
	return pelco.OpDown
}

// opQueryFor is the query opcode that provokes the given response opcode.
func opQueryFor(rsp byte) byte {
	if rsp == pelco.OpRspPan {
		return byte(pelco.OpQueryPan)
	}
	return byte(pelco.OpQueryTilt)
}
