package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type focusArea int

const (
	focusMenu focusArea = iota
	focusInput
	focusLog
)

type serialDataMsg []byte
type serialErrMsg readErr

// rxIdleMsg fires after a receive gap. It carries the RX sequence number that
// was current when it was scheduled, so a tick that has been overtaken by new
// bytes is ignored.
type rxIdleMsg int

type model struct {
	port    *SerialPort
	addr    byte
	tiltCal TiltCal
	rxCh    chan []byte
	errCh   chan readErr

	cursor  int
	focus   focusArea
	input   textinput.Model
	logView viewport.Model
	log     []string

	assembler  Assembler
	paramCmd   int  // menu index awaiting a parameter, -1 = none
	confirmIdx int  // menu index awaiting y/n, -1 = none
	pelcoP     bool // TX framing: false = Pelco-D (default), true = Pelco-P; RX is always adaptive
	status     string
	headerText string
	width      int
	height     int

	rxGen    int     // reader generation; a stale reader's error is ignored
	rxErr    string  // non-empty ⇒ RX is dead until ctrl+r; shown persistently
	rxSeq    int     // bumped on every received chunk, to age out idle ticks
	lastTilt *uint16 // previous 0x5B readback word, for the delta line
}

const (
	menuWidth    = 26
	minMenuWidth = 14
	minLogWidth  = 30
)

var (
	styleSel = lipgloss.NewStyle().Bold(true).Reverse(true)
	styleDim = lipgloss.NewStyle().Faint(true)
	styleTX  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))  // cyan
	styleRX  = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // green
	styleBad = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // red
	styleBar = lipgloss.NewStyle().Background(lipgloss.Color("236"))
)

func newModel(sp *SerialPort, addr byte, pelcoP bool, cal TiltCal) model {
	in := textinput.New()
	in.Prompt = "> "
	m := model{
		port:       sp,
		addr:       addr,
		tiltCal:    cal,
		pelcoP:     pelcoP,
		rxCh:       make(chan []byte, 64),
		errCh:      make(chan readErr, 4),
		input:      in,
		paramCmd:   -1,
		confirmIdx: -1,
		status:     "tab: pane · enter: send · ctrl+r: reopen port · ctrl+l: clear · ctrl+c: quit",
	}
	m.logView = viewport.New(60, 20)
	m.logView.SetContent("")
	go sp.ReadLoop(m.rxGen, m.rxCh, m.errCh)
	return m
}

func waitData(ch chan []byte) tea.Cmd {
	return func() tea.Msg { return serialDataMsg(<-ch) }
}

func waitErr(ch chan readErr) tea.Cmd {
	return func() tea.Msg { return serialErrMsg(<-ch) }
}

// idleGap is ~1.5 frame times at the current baud: long enough that a frame
// split across reads is never mistaken for a truncated one, short enough that a
// truncated reply is flushed well before the next reply arrives.
func (m model) idleGap() time.Duration {
	baud := m.port.Baud()
	if baud <= 0 {
		baud = 2400
	}
	d := time.Duration(float64(frameLen*10) / float64(baud) * 1.5 * float64(time.Second))
	if d < 20*time.Millisecond {
		d = 20 * time.Millisecond
	}
	return d
}

func (m model) idleTick() tea.Cmd {
	seq := m.rxSeq
	return tea.Tick(m.idleGap(), func(time.Time) tea.Msg { return rxIdleMsg(seq) })
}

func (m model) Init() tea.Cmd {
	return tea.Batch(waitData(m.rxCh), waitErr(m.errCh))
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case serialDataMsg:
		m.rxSeq++
		m.logEvents(m.assembler.Feed(msg))
		m.layout()
		cmds := []tea.Cmd{waitData(m.rxCh)}
		if m.assembler.Pending() {
			cmds = append(cmds, m.idleTick())
		}
		return m, tea.Batch(cmds...)

	case rxIdleMsg:
		if int(msg) != m.rxSeq {
			return m, nil // new bytes arrived; a later tick owns the flush
		}
		m.logEvents(m.assembler.FlushIdle())
		m.layout()
		return m, nil

	case serialErrMsg:
		if msg.gen != m.rxGen {
			return m, waitErr(m.errCh) // a reader we already replaced
		}
		m.rxErr = msg.err.Error()
		m.status = "RX stopped — ctrl+r to reopen the port"
		m.layout()
		return m, waitErr(m.errCh)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Only control-key chords are global: they cannot be typed as parameter
	// text. 'p' used to be matched here, so it was stolen from the input line
	// and, worse, flipped the TX envelope while a destructive y/n confirmation
	// was armed — replacing the warning text while leaving the command armed.
	switch msg.String() {
	case "ctrl+c", "ctrl+q":
		return m, tea.Quit
	case "ctrl+l":
		m.log = nil
		// layout() does not touch the viewport content, so without this the
		// pane kept rendering the cleared entries until the next frame — an
		// old tilt reading could be read as the current position.
		m.logView.SetContent("")
		m.logView.GotoTop()
		m.layout()
		return m, nil
	case "ctrl+r":
		return m, m.reopen()
	}

	// A pending y/n confirmation owns the keyboard until it is answered.
	if m.confirmIdx >= 0 {
		idx := m.confirmIdx
		m.confirmIdx = -1
		if strings.ToLower(msg.String()) == "y" {
			m.send(idx)
			m.clearParam()
		} else {
			m.status = "cancelled"
			m.clearParam()
		}
		return m, nil
	}

	switch m.focus {
	case focusMenu:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(Commands)-1 {
				m.cursor++
			}
		case "enter":
			m.execute(m.cursor)
		case "home", "g":
			m.cursor = 0
		case "end", "G":
			m.cursor = len(Commands) - 1
		case "p":
			m.togglePelcoP()
		case "tab", "shift+tab":
			m.focus = focusInput
		}
		return m, nil

	case focusInput:
		switch msg.String() {
		case "esc":
			m.clearParam()
			m.status = "cancelled"
			return m, nil
		case "enter":
			if m.paramCmd >= 0 {
				m.trySend(m.paramCmd)
			}
			return m, nil
		case "tab", "shift+tab":
			// Leaving the input abandons any pending parameter. It used to
			// survive, so tabbing back later and pressing Enter fired a
			// forgotten tilt-set with stale input and moved the rotor.
			if m.paramCmd >= 0 {
				m.status = "pending " + Commands[m.paramCmd].Name + " abandoned"
			}
			m.clearParam()
			m.focus = focusLog
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	default: // focusLog
		switch msg.String() {
		case "up", "k":
			m.logView.LineUp(1)
		case "down", "j":
			m.logView.LineDown(1)
		case "pgup":
			m.logView.HalfPageUp()
		case "pgdown":
			m.logView.HalfPageDown()
		case "home", "g":
			m.logView.GotoTop()
		case "end", "G":
			m.logView.GotoBottom()
		case "p":
			m.togglePelcoP()
		case "tab", "shift+tab":
			m.focus = focusMenu
		default:
			var cmd tea.Cmd
			m.logView, cmd = m.logView.Update(msg)
			return m, cmd
		}
		return m, nil
	}
}

func (m *model) togglePelcoP() {
	m.pelcoP = !m.pelcoP
	mode := "Pelco-D"
	if m.pelcoP {
		mode = "Pelco-P"
	}
	m.status = "TX mode: " + mode + " (RX is adaptive)"
	m.layout()
}

// reopen restarts the serial link after a read error (a USB-serial adapter that
// dropped and re-enumerated). A new reader generation makes the dead reader's
// error harmless if it is still in flight.
func (m *model) reopen() tea.Cmd {
	if err := m.port.Reopen(); err != nil {
		m.rxErr = "reopen failed: " + err.Error()
		m.status = m.rxErr
		m.layout()
		return nil
	}
	m.rxGen++
	m.rxErr = ""
	m.status = "port reopened"
	m.assembler = Assembler{}
	go m.port.ReadLoop(m.rxGen, m.rxCh, m.errCh)
	m.layout()
	// No waitData here: exactly one is always outstanding on rxCh (Init arms
	// it, every serialDataMsg re-arms it, and a read error leaves it blocked),
	// and the new reader writes to that same channel. Arming a second one
	// would leave a duplicate waiter racing on rxCh for the rest of the run.
	return nil
}

// logEvents renders assembler output in wire order. Noise and frames used to be
// returned as separate slices and logged noise-first, so the transcript
// contradicted the wire for exactly the corrupted traffic being investigated.
func (m *model) logEvents(events []Event) {
	for _, e := range events {
		if e.IsNoise() {
			label := "?? unframed"
			if e.Partial {
				label = "?? partial frame (gap)"
			}
			m.appendLog([]string{fmt.Sprintf("%s: % X", label, []byte(e.Noise))}, styleBad)
			continue
		}
		lines := Decode(e.Frame, "RX", m.tiltCal)
		lines = append(lines, m.tiltDelta(e.Frame)...)
		m.appendLog(lines, styleRX)
	}
}

// tiltDelta reports the change since the previous tilt readback. This is pure
// observation with no model attached, and it is the main tool for working out
// what the 0x5B word actually tracks: UNCHANGED across a move rules the word
// out as a position readback, and a delta that does not scale with the move
// rules out any linear elevation map.
func (m *model) tiltDelta(rf RxFrame) []string {
	if rf.Frame[3] != 0x5B {
		return nil
	}
	w := rf.Word()
	var out []string
	if m.lastTilt != nil {
		switch d := int(w) - int(*m.lastTilt); {
		case d == 0:
			out = append(out, "   Δ prev tilt: UNCHANGED")
		case m.tiltCal.Valid():
			out = append(out, fmt.Sprintf("   Δ prev tilt: %+d counts (%+.2f° hyp)",
				d, float64(d)/m.tiltCal.Scale()))
		default:
			out = append(out, fmt.Sprintf("   Δ prev tilt: %+d counts", d))
		}
	}
	m.lastTilt = &w
	return out
}

// execute activates the selected menu entry: sends immediately for
// parameterless commands, else parks in the input line.
func (m *model) execute(idx int) {
	c := Commands[idx]
	if c.Param != ParamNone {
		m.paramCmd = idx
		m.focus = focusInput
		m.input.Placeholder = paramPrompt(c)
		m.input.Focus()
		m.status = c.Desc
		return
	}
	m.trySend(idx)
}

// trySend arms a y/n confirmation if the resolved frame is destructive,
// otherwise sends. The check is on the built frame, not just the menu entry:
// every "NN call" command in the doc sheet is really preset-call NN, so
// "preset call 125" emits the byte-identical frame to the confirm-gated
// "40 defaults+selftest" and used to go out unconfirmed.
func (m *model) trySend(idx int) {
	c := Commands[idx]
	input := ""
	if c.Param != ParamNone {
		input = strings.TrimSpace(m.input.Value())
	}
	if reason := DangerousReason(c, input); reason != "" {
		m.confirmIdx = idx
		m.status = fmt.Sprintf("%s %s — send? y/n", c.Name, reason)
		return
	}
	m.send(idx)
	m.clearParam()
}

func (m *model) clearParam() {
	m.paramCmd = -1
	m.input.Reset()
	m.input.Blur()
	if m.focus == focusInput {
		m.focus = focusMenu
	}
}

func paramPrompt(c Command) string {
	switch c.Param {
	case ParamDegrees:
		return "degrees, e.g. 300 or 79.99 (decimal point, not comma)"
	case ParamPreset:
		return "preset number 0..255"
	case ParamSpeeds:
		return "pan tilt hex 00..3F, e.g. 20 20 (only this axis is used)"
	case ParamHex:
		return "7 hex bytes (D) or 8 (P), sent as typed, e.g. FF 01 00 53 00 00 54"
	}
	return ""
}

func (m *model) send(idx int) {
	c := Commands[idx]
	input := ""
	if c.Param != ParamNone {
		input = strings.TrimSpace(m.input.Value())
	}
	wire, f, err := BuildWire(m.addr, c, input, m.pelcoP)
	if err != nil {
		m.status = err.Error()
		return
	}
	if err := m.port.Write(wire); err != nil {
		m.status = "write failed: " + err.Error()
		return
	}
	rf := RxFrame{Frame: f, Wire: wire, P: m.pelcoP && c.Param != ParamHex}
	if c.Param == ParamHex {
		rf.P = wire[0] == pelcoSTX && len(wire) == frameLenP // raw entry: label by what was typed
	}
	m.appendLog(Decode(rf, "TX", m.tiltCal), styleTX)
	m.layout()
	m.status = c.Name + " sent"
}

// appendLog timestamps the first line and styles the whole entry.
func (m *model) appendLog(lines []string, style lipgloss.Style) {
	stamped := make([]string, len(lines))
	for i, l := range lines {
		if i == 0 {
			stamped[i] = style.Render(time.Now().Format("15:04:05.000 ") + l)
		} else {
			stamped[i] = styleDim.Render(l)
		}
	}
	if len(m.log) > 0 {
		m.log = append(m.log, "")
	}
	m.log = append(m.log, stamped...)
	if len(m.log) > 4000 {
		m.log = m.log[len(m.log)-3000:]
	}
	// Only follow the tail if the operator was already at the tail. GotoBottom
	// used to be unconditional, so an arriving frame yanked the scroll position
	// back — and this head streams readback continuously while the motor runs,
	// which is exactly when earlier readings need comparing.
	atBottom := m.logView.AtBottom()
	m.logView.SetContent(strings.Join(m.log, "\n"))
	if atBottom {
		m.logView.GotoBottom()
	}
}

func (m *model) layout() {
	mw := m.menuW()
	m.logView.Width = m.width - mw - 2
	if m.logView.Width < minLogWidth {
		m.logView.Width = minLogWidth
	}
	m.logView.Height = m.height - 6 // header + input + status + borders
	if m.logView.Height < 3 {
		m.logView.Height = 3
	}
	txMode := "D"
	if m.pelcoP {
		txMode = "P"
	}
	h := fmt.Sprintf("ptest · %s · %d 8N1 · addr %d · TX Pelco-%s",
		m.port.Name(), m.port.Baud(), m.addr, txMode)
	if m.rxErr != "" {
		h = "RX DEAD (ctrl+r): " + m.rxErr
	}
	m.headerText = h
}

// menuW shrinks the command pane on a narrow terminal so the log — where the
// position word and checksum live — keeps its columns.
func (m model) menuW() int {
	if m.width > 0 && m.width-menuWidth-2 < minLogWidth {
		if w := m.width - minLogWidth - 2; w >= minMenuWidth {
			return w
		}
		return minMenuWidth
	}
	return menuWidth
}

// menuWindow is the slice of Commands that fits the menu pane, following the
// cursor. The whole 32-entry list used to be rendered unconditionally, making
// the view 36 rows tall: on an 80x24 terminal the header and the first entries
// — the tilt and pan query/set commands — were pushed off the top.
func (m model) menuWindow() (int, int) {
	h := m.logView.Height
	if h >= len(Commands) {
		return 0, len(Commands)
	}
	start := m.cursor - h/2
	if start < 0 {
		start = 0
	}
	if start+h > len(Commands) {
		start = len(Commands) - h
	}
	return start, start + h
}

func (m model) View() string {
	if m.width == 0 {
		return "starting…"
	}

	// Menu pane.
	mw := m.menuW()
	start, end := m.menuWindow()
	var b strings.Builder
	for i := start; i < end; i++ {
		label := Commands[i].Name
		switch {
		case i == start && start > 0:
			label = "↑ " + label
		case i == end-1 && end < len(Commands):
			label = "↓ " + label
		}
		if len(label) > mw {
			label = label[:mw]
		}
		if i == m.cursor {
			b.WriteString(styleSel.Render(label))
		} else {
			b.WriteString(label)
		}
		b.WriteString("\n")
	}
	menu := lipgloss.NewStyle().
		Width(mw).
		Height(m.logView.Height).
		MaxHeight(m.logView.Height+1).
		Border(lipgloss.NormalBorder(), false, true, false, false).
		Render(b.String())

	body := lipgloss.JoinHorizontal(lipgloss.Top, menu, " "+m.logView.View())

	// Input + status line.
	prompt := ""
	if m.paramCmd >= 0 {
		prompt = styleTX.Render(Commands[m.paramCmd].Name + " · ")
	}
	if m.confirmIdx >= 0 {
		prompt = styleBad.Render("confirm") + " "
	}
	inputLine := prompt + m.input.View()

	status := m.status
	if m.confirmIdx < 0 && m.paramCmd < 0 {
		status = styleDim.Render(Commands[m.cursor].Desc) + "  ·  " + m.status
	}

	header := styleBar.Render(m.headerText)
	if m.rxErr != "" {
		header = styleBad.Render(m.headerText)
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, body, inputLine, status)
}

// run starts the TUI.
func run(sp *SerialPort, addr byte, pelcoP bool, cal TiltCal) error {
	_, err := tea.NewProgram(newModel(sp, addr, pelcoP, cal), tea.WithAltScreen()).Run()
	return err
}
