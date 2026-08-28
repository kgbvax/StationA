package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// testModel builds a model over a closed SerialPort: Name/Baud work, and Write
// fails safely so no test can transmit.
func testModel(w, h int) model {
	in := textinput.New()
	in.Prompt = "> "
	m := model{
		port:       &SerialPort{name: "/dev/tty.usbserial-TEST", baud: 2400},
		addr:       1,
		tiltCal:    TiltCal{}, // default: no elevation hypothesis
		input:      in,
		paramCmd:   -1,
		confirmIdx: -1,
		rxCh:       make(chan []byte, 8),
		errCh:      make(chan readErr, 4),
	}
	m.logView = viewport.New(60, 20)
	m.width, m.height = w, h
	m.layout()
	return m
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func press(m model, keys ...tea.KeyMsg) model {
	var cur tea.Model = m
	for _, k := range keys {
		cur, _ = cur.(model).handleKey(k)
	}
	return cur.(model)
}

func idx(t *testing.T, name string) int {
	t.Helper()
	for i, c := range Commands {
		if c.Name == name {
			return i
		}
	}
	t.Fatalf("no command %q", name)
	return -1
}

func stripANSI(s string) string {
	out := make([]rune, 0, len(s))
	esc := false
	for _, r := range s {
		if r == 0x1b {
			esc = true
			continue
		}
		if esc {
			if r == 'm' {
				esc = false
			}
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// The rendered view must fit the terminal. All 32 menu entries used to be
// rendered unconditionally, making the frame 36 rows tall — on 80x24 the header
// and the first entries (the tilt and pan commands) were pushed off the top.
func TestViewFitsTerminal(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {100, 30}, {120, 40}, {60, 20}} {
		m := testModel(sz[0], sz[1])
		rows := strings.Count(m.View(), "\n") + 1
		if rows > sz[1] {
			t.Errorf("%dx%d: view is %d rows, overflows the terminal", sz[0], sz[1], rows)
		}
	}
}

// The menu window follows the cursor so the selected command is always visible.
func TestMenuWindowFollowsCursor(t *testing.T) {
	m := testModel(80, 24)
	for _, cur := range []int{0, 5, len(Commands) / 2, len(Commands) - 1} {
		m.cursor = cur
		start, end := m.menuWindow()
		if cur < start || cur >= end {
			t.Errorf("cursor %d outside window [%d,%d)", cur, start, end)
		}
		if !strings.Contains(stripANSI(m.View()), Commands[cur].Name) {
			t.Errorf("selected command %q not rendered", Commands[cur].Name)
		}
	}
}

// The raw position word and the checksum verdict must survive on an 80-column
// terminal. The log pane is 52 columns there and the viewport hard-truncates,
// so the one-line breakdown used to lose both — leaving no tilt number at all.
func TestRawWordVisibleOn80Columns(t *testing.T) {
	f := Build(1, 0x00, 0x5B, 0xD4, 0xD5) // head at 90°, raw 54485
	for _, cols := range []int{80, 100, 120} {
		m := testModel(cols, 40)
		m.appendLog(Decode(RxFrame{Frame: f, Wire: f[:]}, "RX", hypTiltCal), styleRX)
		pane := stripANSI(m.logView.View())
		if !strings.Contains(pane, "54485") {
			t.Errorf("%d cols: raw count 54485 truncated off the pane:\n%s", cols, pane)
		}
		if !strings.Contains(pane, "chk=05") {
			t.Errorf("%d cols: checksum verdict truncated off the pane", cols)
		}
		if !strings.Contains(pane, "UNKNOWN") {
			t.Errorf("%d cols: unknown-meaning caveat truncated off the pane", cols)
		}
	}
}

// layout must never hand the viewport a negative width.
func TestLogWidthClamped(t *testing.T) {
	for _, cols := range []int{1, 10, 20, 28, 40} {
		m := testModel(cols, 24)
		if m.logView.Width < minLogWidth {
			t.Errorf("%d cols: logView.Width = %d, below the %d clamp", cols, m.logView.Width, minLogWidth)
		}
	}
}

// 'p' must reach the text input when the input is focused; it used to be
// matched globally and stolen.
func TestPKeyReachesInput(t *testing.T) {
	m := testModel(120, 40)
	m.cursor = idx(t, "raw frame")
	m.execute(m.cursor)
	before := m.pelcoP
	got := press(m, runes("p"))
	if got.pelcoP != before {
		t.Error("'p' flipped the TX envelope while the input was focused")
	}
	if got.input.Value() != "p" {
		t.Errorf("input value = %q, want %q", got.input.Value(), "p")
	}
	// It still toggles from the menu pane.
	m2 := testModel(120, 40)
	if press(m2, runes("p")).pelcoP == m2.pelcoP {
		t.Error("'p' should toggle the TX envelope from the menu pane")
	}
}

// A pending destructive confirmation must own the keyboard. 'p' used to be
// handled first, leaving the command armed while erasing its warning.
func TestConfirmOwnsTheKeyboard(t *testing.T) {
	for _, k := range []string{"p", "n", "x"} {
		m := testModel(120, 40)
		m.cursor = idx(t, "40 defaults+selftest")
		m.execute(m.cursor)
		if m.confirmIdx < 0 {
			t.Fatal("expected the confirmation to be armed")
		}
		got := press(m, runes(k))
		if got.confirmIdx >= 0 {
			t.Errorf("%q left the destructive confirmation armed", k)
		}
		if got.pelcoP != m.pelcoP {
			t.Errorf("%q changed the TX envelope while a confirmation was pending", k)
		}
	}
}

// "preset call 125" is the byte-identical factory reset and must be gated.
func TestPresetCallDestructiveArmsConfirm(t *testing.T) {
	m := testModel(120, 40)
	m.cursor = idx(t, "preset call N")
	m.execute(m.cursor)
	m.input.SetValue("125")
	m.trySend(m.paramCmd)
	if m.confirmIdx < 0 {
		t.Fatal("preset call 125 must arm the y/n confirmation")
	}
	if !strings.Contains(m.status, "factory defaults") {
		t.Errorf("status should say why: %q", m.status)
	}

	// A harmless preset must not prompt.
	m2 := testModel(120, 40)
	m2.cursor = idx(t, "preset call N")
	m2.execute(m2.cursor)
	m2.input.SetValue("3")
	m2.trySend(m2.paramCmd)
	if m2.confirmIdx >= 0 {
		t.Error("preset call 3 must not prompt")
	}
}

// Tabbing away from the input abandons the pending parameter; it used to
// survive so a later Enter fired a forgotten tilt-set.
func TestPendingParamAbandonedOnTab(t *testing.T) {
	m := testModel(120, 40)
	m.cursor = idx(t, "42 tilt set")
	m.execute(m.cursor)
	m.input.SetValue("45")
	got := press(m, tea.KeyMsg{Type: tea.KeyTab})
	if got.paramCmd >= 0 {
		t.Errorf("pending %q survived tabbing away", Commands[got.paramCmd].Name)
	}
	if got.input.Value() != "" {
		t.Errorf("stale input %q survived", got.input.Value())
	}
}

// ctrl+l must actually clear the pane; layout() never touches viewport content,
// so a stale tilt reading used to stay on screen and could be misread.
func TestCtrlLClearsThePane(t *testing.T) {
	m := testModel(120, 40)
	f := Build(1, 0x00, 0x5B, 0x96, 0x47) // raw 38471
	m.appendLog(Decode(RxFrame{Frame: f, Wire: f[:]}, "RX", hypTiltCal), styleRX)
	if !strings.Contains(stripANSI(m.logView.View()), "38471") {
		t.Fatal("setup: reading should be on screen")
	}
	got := press(m, tea.KeyMsg{Type: tea.KeyCtrlL})
	if strings.Contains(stripANSI(got.logView.View()), "38471") {
		t.Error("ctrl+l left the previous reading rendered")
	}
}

// An arriving frame must not yank a scrolled-away operator back to the tail.
func TestScrollPositionHeld(t *testing.T) {
	m := testModel(120, 20)
	f := Build(1, 0x00, 0x5B, 0xD4, 0xD5)
	lines := Decode(RxFrame{Frame: f, Wire: f[:]}, "RX", hypTiltCal)
	for i := 0; i < 30; i++ {
		m.appendLog(lines, styleRX)
	}
	m.logView.GotoTop()
	m.appendLog(lines, styleRX)
	if m.logView.YOffset != 0 {
		t.Errorf("scroll position lost: YOffset = %d, want 0", m.logView.YOffset)
	}
	// Following the tail still works when already at the tail.
	m.logView.GotoBottom()
	m.appendLog(lines, styleRX)
	if !m.logView.AtBottom() {
		t.Error("should keep following the tail when already at the bottom")
	}
}

// Successive tilt readbacks get a delta line. It is pure observation: counts
// only by default (no elevation is asserted), degrees only under an explicit
// -tilt-cal hypothesis, and UNCHANGED called out — the signal that rules the
// word out as a position readback.
func TestTiltDeltaLine(t *testing.T) {
	first := Build(1, 0x00, 0x5B, 0x57, 0xB8)  // raw 22456
	second := Build(1, 0x00, 0x5B, 0x65, 0x9E) // raw 26014

	// Default: counts only, no degrees.
	m := testModel(120, 40)
	if got := m.tiltDelta(RxFrame{Frame: first, Wire: first[:]}); got != nil {
		t.Errorf("first reading has nothing to compare against, got %v", got)
	}
	got := m.tiltDelta(RxFrame{Frame: second, Wire: second[:]})
	if len(got) != 1 {
		t.Fatalf("expected one delta line, got %v", got)
	}
	if !strings.Contains(got[0], "+3558 counts") {
		t.Errorf("delta line = %q, want +3558 counts", got[0])
	}
	if strings.Contains(got[0], "°") {
		t.Errorf("no degrees may be asserted without -tilt-cal: %q", got[0])
	}
	if n := len([]rune(got[0])); n > maxLogCol {
		t.Errorf("delta line is %d cols, over the %d budget", n, maxLogCol)
	}

	// An unchanged word is called out explicitly.
	same := m.tiltDelta(RxFrame{Frame: second, Wire: second[:]})
	if len(same) != 1 || !strings.Contains(same[0], "UNCHANGED") {
		t.Errorf("an identical reading must report UNCHANGED, got %v", same)
	}

	// With a hypothesis the degrees appear, marked as hypothesis.
	mh := testModel(120, 40)
	mh.tiltCal = hypTiltCal
	mh.tiltDelta(RxFrame{Frame: first, Wire: first[:]})
	gotH := mh.tiltDelta(RxFrame{Frame: second, Wire: second[:]})
	if len(gotH) != 1 || !strings.Contains(gotH[0], "+10.00° hyp") {
		t.Errorf("with -tilt-cal the delta should show +10.00° hyp, got %v", gotH)
	}
	if n := len([]rune(gotH[0])); n > maxLogCol {
		t.Errorf("hypothesis delta line is %d cols, over the %d budget", n, maxLogCol)
	}

	// A pan response must not produce a tilt delta.
	pan := Build(1, 0x00, 0x59, 0x75, 0x2F)
	if got := m.tiltDelta(RxFrame{Frame: pan, Wire: pan[:]}); got != nil {
		t.Errorf("pan response produced a tilt delta: %v", got)
	}
}

// A read error must be reported persistently, not in a status line the next
// keypress overwrites, and a stale reader's error must not tear down a fresh one.
func TestReadErrorIsPersistentAndGenerationScoped(t *testing.T) {
	m := testModel(120, 40)
	out, _ := m.Update(serialErrMsg(readErr{gen: 0, err: errFake("input/output error")}))
	got := out.(model)
	if got.rxErr == "" {
		t.Fatal("read error should be latched")
	}
	if !strings.Contains(got.headerText, "RX DEAD") {
		t.Errorf("header should carry the failure: %q", got.headerText)
	}
	// A keypress must not clear it.
	after := press(got, runes("j"))
	if after.rxErr == "" || !strings.Contains(after.headerText, "RX DEAD") {
		t.Error("a keypress erased the RX-dead indication")
	}

	// An error from a reader generation we already replaced is ignored.
	fresh := testModel(120, 40)
	fresh.rxGen = 1
	out2, _ := fresh.Update(serialErrMsg(readErr{gen: 0, err: errFake("stale")}))
	if out2.(model).rxErr != "" {
		t.Error("a stale reader's error tore down the live reader")
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

// The idle gap must scale with baud and stay above a floor.
func TestIdleGapScalesWithBaud(t *testing.T) {
	slow := testModel(120, 40)
	fast := testModel(120, 40)
	fast.port = &SerialPort{name: "x", baud: 9600}
	if !(slow.idleGap() > fast.idleGap()) {
		t.Errorf("2400 gap %v should exceed 9600 gap %v", slow.idleGap(), fast.idleGap())
	}
	if fast.idleGap() < 20_000_000 {
		t.Errorf("gap %v below the 20 ms floor", fast.idleGap())
	}
}
