package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// The sweep recorder is the one automated sequence in ptest, and it lives
// outside the TUI on purpose: the interactive tool stays strictly manual.
//
// It exists because the meaning of the tilt readback word (cmd2 0x5B) is not
// known — the manual's hundredths claim is disproved and so is the linear
// raw-encoder-count model, since elevation does not appear in the word. Finding
// out what the word actually tracks needs a recorded series of known mechanical
// moves paired with the raw bytes that came back, which is precisely what this
// produces. It asserts no decode of its own; it writes raw data.
//
// One step of the loop:
//
//	TX jog <dir> at <speed>      → wait postTX
//	(motor runs so total on-time == -sweep-move)
//	TX stop                      → wait postTX
//	wait -sweep-settle           (readback is only trustworthy once halted)
//	TX tilt query                → wait postTX
//	read the reply, record the raw frame
//
// It repeats until the tilt word has been identical for -sweep-stable
// consecutive readings (the head has reached a mechanical stop, or the word
// does not track the move at all — both are results worth having), or until
// -sweep-max-steps.

type sweepOpts struct {
	dir       string // "up" or "down"
	out       string // CSV path
	move      time.Duration
	settle    time.Duration
	postTX    time.Duration
	replyWait time.Duration
	stable    int
	maxSteps  int
	speed     byte
}

// sweepRow is one recorded step. Everything is raw: the exact bytes sent and
// received, plus the fields mechanically extracted from them. No decode.
type sweepRow struct {
	step      int
	at        time.Time
	elapsedMS int64
	motorOnMS int64
	txHex     string
	rxHex     string
	chkOK     bool
	haveWord  bool
	word      uint16
	d1, d2    byte
	delta     int
	haveDelta bool
	replyMS   int64
	note      string
}

var sweepHeader = []string{
	"step", "iso_time", "elapsed_ms", "dir", "motor_on_ms", "reply_ms",
	"tx_hex", "rx_hex", "chk_ok", "word_dec", "word_hex", "d1_dec", "d2_dec",
	"delta_counts", "note",
}

func (r sweepRow) record(dir string) []string {
	word, wordHex, d1, d2 := "", "", "", ""
	if r.haveWord {
		word = strconv.Itoa(int(r.word))
		wordHex = fmt.Sprintf("%04X", r.word)
		d1 = strconv.Itoa(int(r.d1))
		d2 = strconv.Itoa(int(r.d2))
	}
	delta := ""
	if r.haveDelta {
		delta = strconv.Itoa(r.delta)
	}
	return []string{
		strconv.Itoa(r.step),
		r.at.Format("2006-01-02T15:04:05.000Z07:00"),
		strconv.FormatInt(r.elapsedMS, 10),
		dir,
		strconv.FormatInt(r.motorOnMS, 10),
		strconv.FormatInt(r.replyMS, 10),
		r.txHex, r.rxHex, strconv.FormatBool(r.chkOK),
		word, wordHex, d1, d2, delta, r.note,
	}
}

// jogCmd2 maps a sweep direction to its Pelco-D jog opcode and speed byte
// position. Up/down ride on data2 (the tilt speed byte).
func jogCmd2(dir string) (byte, error) {
	switch dir {
	case "up":
		return 0x08, nil
	case "down":
		return 0x10, nil
	}
	return 0, fmt.Errorf("direction must be \"up\" or \"down\", got %q", dir)
}

// stabilityTracker decides when the tilt word has stopped changing. It is the
// loop's termination condition, so it is separated out to be testable without
// hardware. A step that produced no reading breaks the run rather than counting
// as "identical": a missing reply is not evidence of stability.
type stabilityTracker struct {
	need      int
	prev      uint16
	havePrev  bool
	identical int
}

func newStability(need int) *stabilityTracker {
	return &stabilityTracker{need: need, identical: 1}
}

// observe records a reading and reports the delta from the previous one (when
// there is one) and whether the word has now been identical `need` times.
func (s *stabilityTracker) observe(word uint16) (delta int, haveDelta bool, settled bool) {
	if s.havePrev {
		delta, haveDelta = int(word)-int(s.prev), true
		if word == s.prev {
			s.identical++
		} else {
			s.identical = 1
		}
	} else {
		s.identical = 1
	}
	s.prev, s.havePrev = word, true
	return delta, haveDelta, s.identical >= s.need
}

// miss records a step that produced no reading.
func (s *stabilityTracker) miss() {
	s.havePrev = false
	s.identical = 1
}

// Identical is how many consecutive readings have matched.
func (s *stabilityTracker) Identical() int { return s.identical }

// runSweep drives the loop and writes the CSV. It always stops the motor before
// returning, including on SIGINT — leaving a rotator jogging is not acceptable.
func runSweep(sp *SerialPort, addr byte, useP bool, o sweepOpts) error {
	cmd2, err := jogCmd2(o.dir)
	if err != nil {
		return err
	}
	if o.stable < 1 {
		return fmt.Errorf("-sweep-stable must be at least 1")
	}
	if o.move < o.postTX {
		return fmt.Errorf("-sweep-move (%v) must be at least -sweep-post-tx (%v)", o.move, o.postTX)
	}

	f, err := os.Create(o.out)
	if err != nil {
		return fmt.Errorf("create %s: %v", o.out, err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write(sweepHeader); err != nil {
		return err
	}

	// Bounded reads so a silent head cannot wedge the loop.
	if err := sp.SetReadTimeout(o.replyWait); err != nil {
		return fmt.Errorf("set read timeout: %v", err)
	}

	stopFrame, _, err := BuildWire(addr, Command{Cmd2: 0x00}, "", useP)
	if err != nil {
		return err
	}
	jogFrame, _, err := BuildWire(addr, Command{Cmd2: cmd2, D2: o.speed}, "", useP)
	if err != nil {
		return err
	}
	queryFrame, _, err := BuildWire(addr, Command{Cmd2: 0x53}, "", useP)
	if err != nil {
		return err
	}

	// Always halt the motor on the way out.
	halted := false
	haltOnce := func() {
		if halted {
			return
		}
		halted = true
		_ = sp.Write(stopFrame)
	}
	defer haltOnce()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	interrupted := false
	go func() {
		<-sig
		interrupted = true
		haltOnce()
		w.Flush()
		_ = f.Close()
		fmt.Fprintln(os.Stderr, "\ninterrupted — motor stopped, CSV flushed")
		os.Exit(130)
	}()

	var asm Assembler
	start := time.Now()
	stab := newStability(o.stable)

	fmt.Printf("sweep %s: move %v, settle %v, post-TX %v, stop after %d identical readings, max %d steps\n",
		o.dir, o.move, o.settle, o.postTX, o.stable, o.maxSteps)
	fmt.Printf("writing %s\n\n", o.out)

	for step := 1; step <= o.maxSteps && !interrupted; step++ {
		row := sweepRow{step: step, at: time.Now()}
		row.elapsedMS = time.Since(start).Milliseconds()

		// Move for exactly o.move of motor-on time, counting the mandated
		// post-TX pause as part of it.
		motorStart := time.Now()
		if err := sp.Write(jogFrame); err != nil {
			return fmt.Errorf("step %d: jog write: %v", step, err)
		}
		halted = false
		time.Sleep(o.postTX)
		if rest := o.move - time.Since(motorStart); rest > 0 {
			time.Sleep(rest)
		}
		if err := sp.Write(stopFrame); err != nil {
			return fmt.Errorf("step %d: stop write: %v", step, err)
		}
		halted = true
		row.motorOnMS = time.Since(motorStart).Milliseconds()
		time.Sleep(o.postTX)

		// Readback is only trustworthy once the motor has halted.
		time.Sleep(o.settle)

		queryAt := time.Now()
		if err := sp.Write(queryFrame); err != nil {
			return fmt.Errorf("step %d: query write: %v", step, err)
		}
		row.txHex = hexBytesSweep(queryFrame)
		time.Sleep(o.postTX)

		frame, extra, err := readTiltReply(sp, &asm, o.replyWait)
		row.replyMS = time.Since(queryAt).Milliseconds()
		switch {
		case err != nil:
			row.note = "read error: " + err.Error()
		case frame == nil:
			row.note = "no tilt reply within " + o.replyWait.String()
		default:
			row.rxHex = frame.Hex()
			row.chkOK = frame.ChkOK()
			row.haveWord = true
			row.word = frame.Word()
			row.d1, row.d2 = frame.Frame[4], frame.Frame[5]
		}
		if extra != "" {
			if row.note != "" {
				row.note += "; "
			}
			row.note += extra
		}

		settled := false
		if row.haveWord {
			row.delta, row.haveDelta, settled = stab.observe(row.word)
		} else {
			stab.miss()
		}

		if err := w.Write(row.record(o.dir)); err != nil {
			return fmt.Errorf("step %d: csv write: %v", step, err)
		}
		w.Flush() // flush per step: a sweep that is killed keeps its data
		if err := w.Error(); err != nil {
			return fmt.Errorf("step %d: csv flush: %v", step, err)
		}

		printSweepStep(row)

		if settled {
			fmt.Printf("\ntilt word unchanged for %d consecutive readings (%d / 0x%04X) — stopping\n",
				stab.Identical(), row.word, row.word)
			return nil
		}
	}
	if !interrupted {
		fmt.Printf("\nreached -sweep-max-steps (%d) without the tilt word settling\n", o.maxSteps)
	}
	return nil
}

func printSweepStep(r sweepRow) {
	switch {
	case !r.haveWord:
		fmt.Printf("step %3d  %6d ms  motor %4d ms  --  %s\n", r.step, r.elapsedMS, r.motorOnMS, r.note)
	case r.haveDelta:
		fmt.Printf("step %3d  %6d ms  motor %4d ms  reply %3d ms  %s  word=%5d (0x%04X)  Δ %+d  %s\n",
			r.step, r.elapsedMS, r.motorOnMS, r.replyMS, r.rxHex, r.word, r.word, r.delta, r.note)
	default:
		fmt.Printf("step %3d  %6d ms  motor %4d ms  reply %3d ms  %s  word=%5d (0x%04X)  %s\n",
			r.step, r.elapsedMS, r.motorOnMS, r.replyMS, r.rxHex, r.word, r.word, r.note)
	}
}

// readTiltReply reads until a 0x5B frame arrives or the deadline passes. It
// returns the frame, plus a note describing anything else seen on the wire
// (noise, partials, other opcodes) so nothing is silently dropped from the
// record.
func readTiltReply(sp *SerialPort, asm *Assembler, wait time.Duration) (*RxFrame, string, error) {
	deadline := time.Now().Add(wait)
	buf := make([]byte, 256)
	note := ""
	addNote := func(s string) {
		if note != "" {
			note += "; "
		}
		note += s
	}
	for time.Now().Before(deadline) {
		n, err := sp.Read(buf)
		if err != nil && err != io.EOF {
			return nil, note, err
		}
		if n == 0 {
			continue // read timeout tick
		}
		for _, e := range asm.Feed(buf[:n]) {
			if e.IsNoise() {
				addNote(fmt.Sprintf("unframed %s", hexBytesSweep(e.Noise)))
				continue
			}
			if e.Frame.Frame[3] == 0x5B {
				fr := e.Frame
				return &fr, note, nil
			}
			addNote(fmt.Sprintf("other frame %s", e.Frame.Hex()))
		}
	}
	// Surface anything stalled in the assembler rather than carrying it into
	// the next step, where it could merge with that reply.
	for _, e := range asm.FlushIdle() {
		if e.IsNoise() {
			label := "unframed"
			if e.Partial {
				label = "partial"
			}
			addNote(fmt.Sprintf("%s %s", label, hexBytesSweep(e.Noise)))
		}
	}
	return nil, note, nil
}

func hexBytesSweep(b []byte) string {
	out := make([]byte, 0, len(b)*3)
	const h = "0123456789ABCDEF"
	for i, v := range b {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, h[v>>4], h[v&0x0F])
	}
	return string(out)
}
