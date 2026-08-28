package main

import (
	"strings"
	"testing"
	"time"
)

func TestJogCmd2(t *testing.T) {
	if c, err := jogCmd2("up"); err != nil || c != 0x08 {
		t.Errorf("up -> %02X, %v; want 08", c, err)
	}
	if c, err := jogCmd2("down"); err != nil || c != 0x10 {
		t.Errorf("down -> %02X, %v; want 10", c, err)
	}
	for _, bad := range []string{"", "UP", "left", "0x08"} {
		if _, err := jogCmd2(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// The sweep's jog and query frames must be the doc's frames.
func TestSweepFramesMatchDoc(t *testing.T) {
	cases := []struct {
		name string
		cmd  Command
		want string
	}{
		{"jog up @20", Command{Cmd2: 0x08, D2: 0x20}, "FF 01 00 08 00 20 29"},
		{"jog down @20", Command{Cmd2: 0x10, D2: 0x20}, "FF 01 00 10 00 20 31"},
		{"stop", Command{Cmd2: 0x00}, "FF 01 00 00 00 00 01"},
		{"tilt query", Command{Cmd2: 0x53}, "FF 01 00 53 00 00 54"},
	}
	for _, c := range cases {
		w, _, err := BuildWire(1, c.cmd, "", false)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := hexBytesSweep(w); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestStabilityTracker(t *testing.T) {
	t.Run("settles after need identical readings", func(t *testing.T) {
		s := newStability(3)
		for i, w := range []uint16{100, 100, 100} {
			d, haveD, settled := s.observe(w)
			switch i {
			case 0:
				if haveD {
					t.Error("first reading has no delta")
				}
				if settled {
					t.Error("one reading must not settle a need-3 tracker")
				}
			case 1:
				if !haveD || d != 0 {
					t.Errorf("delta = %d/%v, want 0/true", d, haveD)
				}
				if settled {
					t.Error("two identical readings must not settle a need-3 tracker")
				}
			case 2:
				if !settled {
					t.Error("three identical readings must settle")
				}
			}
		}
	})

	t.Run("a change resets the run", func(t *testing.T) {
		s := newStability(3)
		s.observe(100)
		s.observe(100)
		if _, d, settled := s.observe(150); settled {
			t.Error("a changed word must not settle")
		} else if !d {
			t.Error("expected a delta")
		}
		if s.Identical() != 1 {
			t.Errorf("identical = %d, want 1 after a change", s.Identical())
		}
		// and the delta is signed
		if d, _, _ := s.observe(120); d != -30 {
			t.Errorf("delta = %d, want -30", d)
		}
	})

	t.Run("a missing reply is not evidence of stability", func(t *testing.T) {
		s := newStability(2)
		s.observe(100)
		s.miss()
		if _, haveD, settled := s.observe(100); settled {
			t.Error("a reading after a missed step must not settle")
		} else if haveD {
			t.Error("no delta across a missed step")
		}
	})

	t.Run("need 1 settles on the first reading", func(t *testing.T) {
		if _, _, settled := newStability(1).observe(42); !settled {
			t.Error("need-1 must settle immediately")
		}
	})

	t.Run("deltas wrap correctly near the 16-bit ends", func(t *testing.T) {
		s := newStability(9)
		s.observe(65530)
		if d, _, _ := s.observe(5); d != -65525 {
			t.Errorf("delta = %d, want -65525 (no uint16 wraparound)", d)
		}
	})
}

// The CSV row must carry the raw frame and leave unknown fields empty rather
// than writing a misleading zero.
func TestSweepRowRecord(t *testing.T) {
	at := time.Date(2026, 8, 27, 23, 35, 17, 695_000_000, time.UTC)
	full := sweepRow{
		step: 4, at: at, elapsedMS: 1209, motorOnMS: 301, replyMS: 51,
		txHex: "FF 01 00 53 00 00 54", rxHex: "FF 01 00 5B 57 B8 6B",
		chkOK: true, haveWord: true, word: 22456, d1: 0x57, d2: 0xB8,
		delta: -30, haveDelta: true,
	}
	got := full.record("up")
	if len(got) != len(sweepHeader) {
		t.Fatalf("row has %d fields, header has %d", len(got), len(sweepHeader))
	}
	want := map[string]string{
		"step": "4", "elapsed_ms": "1209", "dir": "up", "motor_on_ms": "301",
		"reply_ms": "51", "rx_hex": "FF 01 00 5B 57 B8 6B", "chk_ok": "true",
		"word_dec": "22456", "word_hex": "57B8", "d1_dec": "87", "d2_dec": "184",
		"delta_counts": "-30", "note": "",
	}
	for i, h := range sweepHeader {
		if w, ok := want[h]; ok && got[i] != w {
			t.Errorf("%s = %q, want %q", h, got[i], w)
		}
	}
	if !strings.HasPrefix(got[1], "2026-08-27T23:35:17.695") {
		t.Errorf("iso_time = %q", got[1])
	}

	// A step with no reply must leave the value columns empty.
	miss := sweepRow{step: 5, at: at, note: "no tilt reply within 2s"}
	got = miss.record("up")
	for i, h := range sweepHeader {
		switch h {
		case "word_dec", "word_hex", "d1_dec", "d2_dec", "delta_counts", "rx_hex":
			if got[i] != "" {
				t.Errorf("%s = %q for a missed step, want empty", h, got[i])
			}
		case "note":
			if got[i] == "" {
				t.Error("a missed step must carry a note")
			}
		}
	}
}

func TestSweepOptValidation(t *testing.T) {
	base := sweepOpts{dir: "up", out: "/dev/null", move: time.Second,
		settle: 200 * time.Millisecond, postTX: 50 * time.Millisecond,
		replyWait: time.Second, stable: 3, maxSteps: 10}

	bad := base
	bad.dir = "sideways"
	if err := runSweep(nil, 1, false, bad); err == nil {
		t.Error("an invalid direction must be rejected before touching the port")
	}

	bad = base
	bad.stable = 0
	if err := runSweep(nil, 1, false, bad); err == nil {
		t.Error("-sweep-stable 0 must be rejected")
	}

	bad = base
	bad.move = 10 * time.Millisecond // shorter than postTX
	if err := runSweep(nil, 1, false, bad); err == nil {
		t.Error("a move shorter than the post-TX pause must be rejected")
	}
}

func TestHexBytesSweep(t *testing.T) {
	if got := hexBytesSweep([]byte{0xFF, 0x01, 0x00}); got != "FF 01 00" {
		t.Errorf("got %q", got)
	}
	if got := hexBytesSweep(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
