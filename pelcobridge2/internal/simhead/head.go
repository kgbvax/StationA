// Package simhead is an in-process model of the PTS-303Z/3050DZ head for `go
// test` and `-port sim`, with no hardware and no pty. It models the head AS
// THE ENGINE MEETS IT, quirks included, each individually toggleable so a test
// can pin exactly the behaviour it exercises:
//
//   - absolute sets (0x4B/0x4D) are IGNORED unless the line has been quiet for
//     a configurable silence window;
//   - readback while a motor runs is checksum-valid garbage (unless disabled).
//
// The head's tilt is modeled in its NATIVE (inverted) frame — the word the
// wire speaks, not elevation. SetAzEl/ElDeg are test conveniences that
// convert TRUE azimuth/elevation to and from that frame; PanDeg/TiltDeg stay
// raw native words.
package simhead

import (
	"errors"
	"math"
	"sync"
	"time"

	"pelcobridge2/internal/pelco"
)

var errClosed = errors.New("simhead closed")

// Options configures a simulated head.
type Options struct {
	Addr            byte
	PanDeg          float64
	TiltDeg         float64
	SilenceRequired time.Duration // sets arriving sooner after any frame are ignored (default 300ms)
	// HonestReadback disables the garbage-while-moving quirk: readback then
	// tracks the true position even while a motor runs. Default (false) is
	// the bench-documented behaviour: garbage while moving.
	HonestReadback bool
	RateAzDegPerS  float64 // default 4.0
	RateElDegPerS  float64 // default 2.0
}

// Head is a canned rotator. It implements serialio.Transport directly: the
// engine writes frames to it and reads replies, exactly as with a real port.
type Head struct {
	mu sync.Mutex

	addr byte
	pan  float64
	tilt float64

	panDir, tiltDir int // jog motion: -1, +1, 0 stopped
	panTarget       float64
	tiltTarget      float64
	panHasTgt       bool
	tiltHasTgt      bool

	rateAz, rateEl  float64
	silenceRequired time.Duration
	garbage         bool
	selfCheck       bool // periodic self-check enabled (factory default on)

	// wire state (engine side)
	lastWrite time.Time

	replies chan []byte
	closed  chan struct{}
}

// New returns a simulated head; the engine uses the returned Head directly as
// its serialio.Transport.
func New(opts Options) *Head {
	if opts.RateAzDegPerS == 0 {
		opts.RateAzDegPerS = 4.0
	}
	if opts.RateElDegPerS == 0 {
		opts.RateElDegPerS = 2.0
	}
	if opts.SilenceRequired == 0 {
		opts.SilenceRequired = 300 * time.Millisecond
	}
	h := &Head{
		addr:            opts.Addr,
		pan:             pelco.Norm360(opts.PanDeg),
		tilt:            clampTilt(opts.TiltDeg),
		rateAz:          opts.RateAzDegPerS,
		rateEl:          opts.RateElDegPerS,
		silenceRequired: opts.SilenceRequired,
		garbage:         !opts.HonestReadback,
		selfCheck:       true, // factory default: periodic self-check on
		replies:         make(chan []byte, 64),
		closed:          make(chan struct{}),
	}
	go h.animate()
	return h
}

// PanDeg reads the simulated pan position.
func (h *Head) PanDeg() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pan
}

// TiltDeg reads the simulated tilt position — the NATIVE word the wire
// speaks, not elevation (use ElDeg for that).
func (h *Head) TiltDeg() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tilt
}

// SetAzEl teleports the head to the given PHYSICAL azimuth (no arm offset —
// the head knows nothing about the engine's user frame; the control tests'
// harness gotoAzEl crosses it) and TRUE elevation. A
// test-setup primitive, not motion: no frames, no rates, no settle window.
// The elevation is mirrored into the native tilt frame (pelco.ElToTilt) and
// both axes clamp to their travel, matching what a clamped wire set would
// leave behind. Any jog or set in progress is dropped.
func (h *Head) SetAzEl(az, el float64) {
	h.mu.Lock()
	h.pan = pelco.Norm360(az)
	h.tilt = clampTilt(pelco.ElToTilt(el))
	h.panDir, h.tiltDir = 0, 0
	h.panHasTgt, h.tiltHasTgt = false, false
	h.mu.Unlock()
}

// ElDeg reads the current position as TRUE elevation — the mirror of the
// native tilt word (pelco.TiltToEl).
func (h *Head) ElDeg() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return pelco.TiltToEl(h.tilt)
}

// Moving reports whether either axis is animating.
func (h *Head) Moving() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.panDir != 0 || h.tiltDir != 0 || h.panHasTgt || h.tiltHasTgt
}

// SelfCheck reports whether the periodic self-check is enabled. The head
// itself is NOT modeled as re-homing on its periodic self-check: tests stay
// deterministic, and the engine has no defence against that hazard anyway —
// which is exactly why the engine disables it once per connect.
func (h *Head) SelfCheck() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.selfCheck
}

// Write decodes one frame from the engine and acts on it. Undecodable and
// wrong-address frames are silently ignored, like the real head.
func (h *Head) Write(b []byte) error {
	rx, ok := DecodeAny(b)
	if !ok {
		return nil
	}
	if rx.Addr() != h.addr {
		return nil
	}

	h.mu.Lock()
	quietFor := time.Since(h.lastWrite)
	h.lastWrite = time.Now()
	h.mu.Unlock()

	switch rx.Op() {
	case pelco.OpStop:
		h.stopAll()

	case pelco.OpRight:
		h.jogAxis(+1, 0)
	case pelco.OpLeft:
		h.jogAxis(-1, 0)
	case pelco.OpUp:
		h.jogAxis(0, +1)
	case pelco.OpDown:
		h.jogAxis(0, -1)

	case pelco.OpQueryPan:
		h.emit(pelco.OpRspPan, h.readback(quietFor, h.PanDeg()))
	case pelco.OpQueryTilt:
		h.emit(pelco.OpRspTilt, h.readback(quietFor, h.TiltDeg()))

	case pelco.OpSetPan:
		if quietFor >= h.silenceRequired {
			h.goTo(pelco.WordToDeg(rx.Frame.Word()), math.NaN())
		}
	case pelco.OpSetTilt:
		if quietFor >= h.silenceRequired {
			h.goTo(math.NaN(), pelco.WordToDeg(rx.Frame.Word()))
		}

	case pelco.OpPresetSet:
		if rx.Frame[5] == pelco.PresetSelfCheckOff {
			h.setSelfCheck(false)
		}

	case pelco.OpPresetCall:
		switch rx.Frame[5] {
		case pelco.PresetSelfCheckOff:
			h.setSelfCheck(true)
		case pelco.PresetSelfTest:
			// Factory defaults + self-test: re-home to mechanical zero,
			// and the factory defaults restore the periodic self-check.
			h.setSelfCheck(true)
			h.goTo(0, 0)
		}
	}
	return nil
}

func (h *Head) setSelfCheck(on bool) {
	h.mu.Lock()
	h.selfCheck = on
	h.mu.Unlock()
}

// Read blocks until the head emits a reply or Close.
func (h *Head) Read(p []byte) (int, error) {
	select {
	case buf, ok := <-h.replies:
		if !ok {
			return 0, errClosed
		}
		return copy(p, buf), nil
	case <-h.closed:
		return 0, errClosed
	}
}

// Close stops the simulation.
func (h *Head) Close() error {
	select {
	case <-h.closed:
	default:
		close(h.closed)
	}
	return nil
}

func (h *Head) jogAxis(panDir, tiltDir int) {
	h.mu.Lock()
	if panDir != 0 {
		h.panDir, h.panHasTgt = panDir, false
	}
	if tiltDir != 0 {
		h.tiltDir, h.tiltHasTgt = tiltDir, false
	}
	h.mu.Unlock()
}

func (h *Head) goTo(pan, tilt float64) {
	h.mu.Lock()
	if !math.IsNaN(pan) {
		h.panTarget, h.panHasTgt, h.panDir = pelco.Norm360(pan), true, 0
	}
	if !math.IsNaN(tilt) {
		h.tiltTarget, h.tiltHasTgt, h.tiltDir = clampTilt(tilt), true, 0
	}
	h.mu.Unlock()
}

func (h *Head) stopAll() {
	h.mu.Lock()
	h.panDir, h.tiltDir = 0, 0
	h.panHasTgt, h.tiltHasTgt = false, false
	h.mu.Unlock()
}

// readback is the word the head reports for a query. While a motor runs the
// readback is garbage — checksum-valid, position-unrelated (bench finding);
// otherwise it is the textbook degrees×100 of the current position.
func (h *Head) readback(quietFor time.Duration, cur float64) uint16 {
	h.mu.Lock()
	moving := h.panDir != 0 || h.tiltDir != 0 || h.panHasTgt || h.tiltHasTgt
	garbage := h.garbage
	h.mu.Unlock()
	if moving && garbage {
		return garbageWord()
	}
	return degWord(cur)
}

func (h *Head) emit(op byte, word uint16) {
	f := pelco.Build(h.addr, 0x00, op, byte(word>>8), byte(word))
	select {
	case h.replies <- f[:]:
	case <-h.closed:
	}
}

func degWord(deg float64) uint16 { return uint16(pelco.Norm360(deg) * 100) }

func garbageWord() uint16 {
	garbageSeq += 0x2A1F
	return garbageSeq
}

var garbageSeq uint16 = 0x9B81

// animate advances simulated motion. This ticker models the PHYSICAL head — a
// rotor keeps turning under motor power — not the engine, whose no-timer
// discipline applies to its own serial traffic only.
func (h *Head) animate() {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-h.closed:
			return
		case <-t.C:
			h.mu.Lock()
			if h.panDir != 0 {
				h.pan = pelco.Norm360(h.pan + float64(h.panDir)*h.rateAz*0.02)
			}
			if h.tiltDir != 0 {
				h.tilt += float64(h.tiltDir) * h.rateEl * 0.02
				if h.tilt <= 0 {
					h.tilt, h.tiltDir = 0, 0
				} else if h.tilt >= 90 {
					h.tilt, h.tiltDir = 90, 0
				}
			}
			if h.panHasTgt {
				h.pan = approachDeg(h.pan, h.panTarget, h.rateAz*0.02)
				if h.pan == h.panTarget {
					h.panHasTgt = false
				}
			}
			if h.tiltHasTgt {
				h.tilt = approachTilt(h.tilt, h.tiltTarget, h.rateEl*0.02)
				if h.tilt == h.tiltTarget {
					h.tiltHasTgt = false
				}
			}
			h.mu.Unlock()
		}
	}
}

func approachDeg(cur, target, maxStep float64) float64 {
	d := math.Mod(target-cur, 360)
	if d > 180 {
		d -= 360
	}
	if d < -180 {
		d += 360
	}
	if math.Abs(d) <= maxStep {
		return pelco.Norm360(target)
	}
	if d < 0 {
		return pelco.Norm360(cur - maxStep)
	}
	return pelco.Norm360(cur + maxStep)
}

func approachTilt(cur, target, maxStep float64) float64 {
	d := target - cur
	if math.Abs(d) <= maxStep {
		return target
	}
	if d < 0 {
		return cur - maxStep
	}
	return cur + maxStep
}

func clampTilt(d float64) float64 {
	if d < 0 {
		return 0
	}
	if d > 90 {
		return 90
	}
	return d
}
