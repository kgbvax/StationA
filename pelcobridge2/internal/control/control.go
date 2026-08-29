// Package control is pelcobridge2's engine: a single goroutine that owns all
// serial TX and all rotator state, with no mutexes. Everyone else — TUI,
// rotctld, MQTT — talks to it exclusively through Request values carrying an
// Intent; the engine checks the request's Source and refuses forbidden
// combinations (arming is TUI-only; MQTT has no motion intents at all).
//
// No timer polling: the engine's only timers are one-shots that RELEASE gates
// (frame gap after TX, reply wait around one outstanding query, settle window
// after an absolute set). No timer ever transmits on its own.
package control

import (
	"context"
	"errors"
	"time"

	"pelcobridge2/internal/pelco"
)

// Source identifies who issued a Request. The engine gates on it: Arm and
// SelfTest are accepted only from SrcTUI.
type Source int

const (
	SrcTUI Source = iota
	SrcRotctld
	SrcMQTT
	SrcEngine // engine-internal (verification queries, ladder re-sends)
)

func (s Source) String() string {
	switch s {
	case SrcTUI:
		return "tui"
	case SrcRotctld:
		return "rotctld"
	case SrcMQTT:
		return "mqtt"
	default:
		return "engine"
	}
}

// Request is one intent delivered to the engine loop.
type Request struct {
	From   Source
	Intent Intent
	// Reply is optional; buffered size-1. nil for fire-and-forget.
	Reply chan Result
}

// Result is the reply to one Request.
type Result struct {
	Err error
	// Deg carries the position for query intents (NaN if none): TRUE degrees
	// (offset-corrected) for user queries, matching set_pos's argument
	// convention; the ladder's internal verification stays physical.
	Deg float64
	// Age is the age of that readback at reply time.
	Age time.Duration
}

// Submit hands a request to the engine without waiting. Non-blocking: a full
// queue refuses the request rather than blocking the caller (a stuck TUI must
// never stall the rotctld path).
func Submit(reqCh chan<- Request, from Source, it Intent) error {
	select {
	case reqCh <- Request{From: from, Intent: it}:
		return nil
	default:
		return ErrBusy
	}
}

// Call submits and waits for the reply or ctx. A 2 s caller timeout is the
// convention (rotctld uses exactly that); the engine abandons nothing — an
// unanswered Call simply leaves the buffered reply to be collected or not.
func Call(ctx context.Context, reqCh chan<- Request, from Source, it Intent) Result {
	req := Request{From: from, Intent: it, Reply: make(chan Result, 1)}
	select {
	case reqCh <- req:
	case <-ctx.Done():
		return Result{Err: errors.New("request aborted")}
	}
	select {
	case r := <-req.Reply:
		return r
	case <-ctx.Done():
		return Result{Err: errors.New("request timed out")}
	}
}

// Config is the engine slice of the application config (internal/config maps
// TOML onto it). Zero values are the bench-tuned defaults.
type Config struct {
	Addr              byte          // head's DIP address
	Baud              int           // 2400 on the bench link
	JogSpeed          byte          // 0x00–0x3F, default 0x12
	Settle            time.Duration // quiet-line window around absolute sets
	SetAttempts       int           // verification-ladder re-sends
	SetTolerance      float64       // degrees
	ArmMaxReadbackAge time.Duration // pan readback must be fresher to arm
	ReplyWait         time.Duration // bound on the one outstanding query
}

func (c *Config) fillDefaults() {
	if c.Baud == 0 {
		c.Baud = 2400
	}
	if c.JogSpeed == 0 {
		c.JogSpeed = pelco.DefaultJogSpeed
	}
	if c.Settle == 0 {
		c.Settle = 2 * time.Second
	}
	if c.SetAttempts == 0 {
		c.SetAttempts = 3
	}
	if c.SetTolerance == 0 {
		c.SetTolerance = 0.3
	}
	if c.ArmMaxReadbackAge == 0 {
		c.ArmMaxReadbackAge = 10 * time.Second
	}
	if c.ReplyWait == 0 {
		c.ReplyWait = 400 * time.Millisecond
	}
}

// Errors returned as Result.Err.
var (
	ErrBusy      = errors.New("engine busy")
	ErrCancelled = errors.New("request cancelled (all-stop)")
	ErrDisarmed  = errors.New("rotator is not armed")
	ErrMoving    = errors.New("rotator is moving")
	ErrNoFix     = errors.New("no valid readback")
	ErrStale     = errors.New("readback too old to arm")
	ErrSource    = errors.New("intent not allowed from this source")
	ErrFailed    = errors.New("set did not converge")
	ErrTxFail    = errors.New("frame never reached the wire")
)

// Snapshot is the engine's published state, fanned out to the TUI and MQTT
// after every change. Deg fields are true (offset-corrected) positions;
// Phys* are the head's raw readbacks.
type Snapshot struct {
	Ts time.Time

	Az, El         float64 // true az/el, NaN when no readback
	PhysAz, PhysEl float64 // raw readback, NaN when none
	PanAge         time.Duration
	TiltAge        time.Duration
	ReadbackValid  bool // both axes have a fresh-enough readback
	ArmFresh       bool // pan readback fresh enough to arm (engine policy)

	Armed              bool
	Offset             float64
	Moving             bool
	TargetAz, TargetEl float64 // ladder targets, NaN when none
	SetStatus          string  // "", "setting", "converged", "failed"

	// SelfCheck is the head's periodic self-check as the engine models it:
	// "on" (head may re-home itself unprompted — maintenance only), "off",
	// or "unknown" (canonical tri-state — the claim is liveness-gated and
	// dropped on every link death, so consumers render it verbatim).
	SelfCheck string

	JogSpeed byte

	DeviceOnline bool // a checksum-valid frame was seen recently
	Error        string
}

// Event is one item fanned out to the sink (TUI log pane + MQTT).
type Event struct {
	// Snapshot is set when the engine state changed.
	Snap *Snapshot
	// Log is a wire-log line for the TUI (assembler noise, frames, engine notes).
	Log string
	// RX is a decoded frame event (nil for pure-log events).
	RX *pelco.RxFrame
	// Dir is "TX" or "RX" for RX frames.
	Dir string
}
