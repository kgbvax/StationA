// Package ui is pelcobridge2's Bubble Tea TUI: header, position pane, wire-log
// viewport, and the prompt state machine (arm azimuth entry, self-test
// confirmation).
//
// Hold-to-move: Bubble Tea has no key-release events, so each jog keypress arms
// a ONE-SHOT tea.Tick(jog_hold); terminal auto-repeat refreshes the deadline and
// a tick that fires without a fresh keypress sends one stop. That one-shot is a
// safety net, not polling, and is the recorded deviation from "no timers".
//
// Prompt discipline (ptest rule): a prompt owns the keyboard until answered —
// no motion key can fire mid-prompt. The e-stop (space/esc) and ctrl chords cut
// through everything.
package ui

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"pelcobridge2/internal/control"
	"pelcobridge2/internal/pelco"
)

// promptKind is which prompt currently owns the keyboard, if any.
type promptKind int

const (
	promptNone     promptKind = iota
	promptArm                 // enter the true azimuth the head is pointing at
	promptSelfTest            // two-stage: y/n, then type RIPCABLES
)

// jogHoldMsg fires when the hold-to-move window expires; the sequence number
// discards ticks overtaken by a fresher keypress.
type jogHoldMsg int

// queryMsg carries one query round-trip's outcome for the status line.
type queryMsg struct {
	axis string // "azimuth" | "elevation"
	deg  float64
	err  error
}

// armMsg carries the arm flow's outcome.
type armMsg struct{ err error }

// Options wires the TUI to the engine and the environment.
type Options struct {
	ReqCh    chan<- control.Request
	EvCh     <-chan control.Event
	PortName string
	Baud     int
	Addr     byte
	JogHold  time.Duration     // hold-to-move window
	Prefill  float64           // state.toml last offset, offered as the arm default
	OnArm    func(deg float64) // called after a successful arm (persist prefill)
	MQTTOn   func() bool       // MQTT broker link, for the header
	Clients  func() int        // rotctld client count, for the header
}

type model struct {
	opts Options
	snap *control.Snapshot

	logView viewport.Model
	log     []string

	input     textinput.Model
	prompt    promptKind
	selfStage int // self-test confirm stage: 0 = y/n, 1 = type RIPCABLES
	help      bool

	holdSeq int
	status  string

	width, height int
}

// New builds the TUI model.
func New(opts Options) model {
	in := textinput.New()
	in.Prompt = "> "
	return model{
		opts:   opts,
		input:  in,
		status: "arrows/hjkl: jog (works disarmed) · A: arm (enables rotctl) · a/e: query · ? : all keys · SPACE/ESC = E-STOP",
	}
}

// Init subscribes to the engine's event stream.
func (m model) Init() tea.Cmd {
	return m.waitEvent()
}

// Run starts the TUI on the alt screen.
func Run(m model) error {
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

// --- outgoing requests ----------------------------------------------------------

func (m *model) submit(it control.Intent) {
	_ = control.Submit(m.opts.ReqCh, control.SrcTUI, it)
}

// submitStop arms the e-stop delivery as a Cmd. Unlike Submit, control.Call
// blocks until the engine has actually DEQUEUED the stop — a saturated queue
// or a stalled engine can never silently drop an e-stop while the status line
// claims it was sent.
func (m *model) submitStop(reason string) tea.Cmd {
	it := control.StopIntent{}
	reqCh := m.opts.ReqCh
	m.status = "E-STOP sent"
	if reason != "" {
		m.status += " (" + reason + ")"
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = control.Call(ctx, reqCh, control.SrcTUI, it)
		return nil
	}
}

// --- update ---------------------------------------------------------------------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case control.Event:
		if msg.Snap != nil {
			m.snap = msg.Snap
			m.layout()
		}
		if msg.Log != "" {
			m.appendLog(msg.Log)
		}
		return m, m.waitEvent()

	case jogHoldMsg:
		if int(msg) == m.holdSeq {
			// No jog keypress since this tick was armed: the key was
			// released (or auto-repeat is suppressed) → all-stop.
			m.layout()
			return m, m.submitStop("hold expired")
		}
		return m, nil

	case queryMsg:
		if msg.err != nil {
			m.status = "query " + msg.axis + ": " + msg.err.Error()
		} else {
			m.status = fmt.Sprintf("%s = %.2f°", msg.axis, msg.deg)
		}
		m.layout()
		return m, nil

	case armMsg:
		m.prompt = promptNone
		m.cancelPrompt()
		if msg.err != nil {
			m.status = "arm refused: " + msg.err.Error()
		} else {
			m.status = "ARMED — rotctld may now command motion"
		}
		m.layout()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) waitEvent() tea.Cmd {
	ch := m.opts.EvCh
	return func() tea.Msg { return <-ch }
}

// handleKey is the keymap. Prompt rules: a prompt owns the keyboard until
// answered; space/esc (e-stop) and ctrl chords cut through everything.
func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Global, every state: e-stop and program chords.
	switch key {
	case " ", "esc":
		m.cancelPrompt()
		return m, m.submitStop("")
	case "ctrl+c", "ctrl+q":
		// best-effort all-stop; the engine repeats on ctx end
		return m, tea.Batch(m.submitStop("quitting"), tea.Quit)
	case "ctrl+l":
		m.log = nil
		m.logView.SetContent("")
		m.layout()
		return m, nil
	case "ctrl+r":
		m.submit(control.ReopenIntent{})
		m.status = "port reopen requested"
		return m, nil
	}

	if key == "?" {
		m.help = !m.help
		m.layout()
		return m, nil
	}

	if m.prompt != promptNone {
		return m.handlePromptKey(msg)
	}

	// TUI manual motion (jog, goto 0) is intentionally NOT gated by arming:
	// the operator needs it to position the head BEFORE arming. Arming gates
	// the rotctld path only.

	switch key {
	case "up", "k":
		m.status = "jog up — release to stop"
		return m.jog(control.DirUp)
	case "down", "j":
		m.status = "jog down — release to stop"
		return m.jog(control.DirDown)
	case "left", "h":
		m.status = "jog left — release to stop"
		return m.jog(control.DirLeft)
	case "right", "l":
		m.status = "jog right — release to stop"
		return m.jog(control.DirRight)
	case "a":
		return m, queryCmd(m.opts.ReqCh, false)
	case "e":
		return m, queryCmd(m.opts.ReqCh, true)
	case "A":
		if m.snap != nil && m.snap.Armed {
			m.status = "already armed (d disarms)"
			return m, nil
		}
		m.prompt = promptArm
		m.input.Prompt = "true az ° > "
		m.input.SetValue(fmt.Sprintf("%.1f", m.opts.Prefill))
		m.input.Focus()
		m.status = "ARM: enter the TRUE azimuth the head points at right now (enter confirms, esc cancels)"
		return m, textinput.Blink
	case "0":
		m.submit(control.GotoPhysZeroIntent{})
		m.status = "goto PHYSICAL zero (offset not applied)"
		return m, nil
	case "s":
		if m.snap != nil && m.snap.Armed {
			m.status = "self-test is disarmed-only — disarm first"
			return m, nil
		}
		m.prompt = promptSelfTest
		m.selfStage = 0
		m.status = "SELF-TEST re-homes the head — KEEP CABLES CLEAR.  Send? y/n"
		return m, nil
	case "d":
		m.submit(control.DisarmIntent{})
		m.status = "disarm sent"
		return m, nil
	case "+", "=":
		return m.bumpSpeed(1)
	case "-", "_":
		return m.bumpSpeed(-1)
	case "tab":
		m.logView.HalfPageDown()
		return m, nil
	case "shift+tab":
		m.logView.HalfPageUp()
		return m, nil
	}
	return m, nil
}

// handlePromptKey runs the arm and self-test prompt state machines.
func (m model) handlePromptKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch m.prompt {
	case promptArm:
		switch key {
		case "enter":
			return m, armCmd(m.opts.ReqCh, m.input.Value(), m.opts.OnArm)
		default:
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}

	case promptSelfTest:
		switch {
		case m.selfStage == 0:
			if key == "y" || key == "Y" {
				m.selfStage = 1
				m.input.Prompt = "type RIPCABLES > "
				m.input.Reset()
				m.input.Focus()
				m.status = "This fires preset call 125: the head re-homes and can RIP CABLES."
				return m, textinput.Blink
			}
			m.cancelPrompt()
			m.status = "self-test cancelled"
			m.layout()
			return m, nil
		default: // stage 1: type-the-word confirmation
			if key == "enter" {
				if strings.TrimSpace(m.input.Value()) == "RIPCABLES" {
					m.cancelPrompt()
					m.submit(control.SelfTestIntent{})
					m.status = "SELF-TEST sent — stand clear"
				} else {
					m.cancelPrompt()
					m.status = "self-test cancelled (confirmation text mismatch)"
				}
				m.layout()
				return m, nil
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

func (m *model) cancelPrompt() {
	m.prompt = promptNone
	m.selfStage = 0
	m.input.Reset()
	m.input.Blur()
}

// jog submits the jog intent and arms the one-shot hold timer. Terminal
// auto-repeat re-arms with a fresh sequence; a tick firing with the CURRENT
// sequence number means no keypress landed since it was armed → stop.
// The incremented sequence flows back into the model on purpose: if it were
// lost, no tick would ever match and releasing the key would never stop the
// head (this exact bug shipped once).
func (m model) jog(dir control.Dir) (model, tea.Cmd) {
	m.holdSeq++
	seq := m.holdSeq
	it := control.JogIntent{Dir: dir}
	reqCh := m.opts.ReqCh
	return m, tea.Batch(
		func() tea.Msg { _ = control.Submit(reqCh, control.SrcTUI, it); return nil },
		tea.Tick(m.opts.JogHold, func(time.Time) tea.Msg { return jogHoldMsg(seq) }),
	)
}

// queryCmd runs one readback round-trip off the update loop.
func queryCmd(reqCh chan<- control.Request, tilt bool) tea.Cmd {
	axis, it := "az", control.Intent(control.QueryPanIntent{})
	if tilt {
		axis, it = "el", control.Intent(control.QueryTiltIntent{})
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		r := control.Call(ctx, reqCh, control.SrcTUI, it)
		return queryMsg{axis: axis, deg: r.Deg, err: r.Err}
	}
}

// armCmd runs the arm flow: fresh pan readback first (the engine refuses a
// stale one), then Arm with the entered TRUE azimuth.
func armCmd(reqCh chan<- control.Request, raw string, onArm func(float64)) tea.Cmd {
	return func() tea.Msg {
		deg, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		// ParseFloat("nan") succeeds with NaN — both NaN and the range must
		// be checked, or "nan" arms the rotator with a NaN offset.
		if err != nil || math.IsNaN(deg) || deg < 0 || deg > 360 {
			return armMsg{err: fmt.Errorf("azimuth %q is not a degree number in 0..360", raw)}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if r := control.Call(ctx, reqCh, control.SrcTUI, control.QueryPanIntent{}); r.Err != nil {
			return armMsg{err: fmt.Errorf("readback: %w", r.Err)}
		}
		r := control.Call(ctx, reqCh, control.SrcTUI, control.ArmIntent{TrueAz: deg})
		if r.Err == nil && onArm != nil {
			onArm(deg)
		}
		return armMsg{err: r.Err}
	}
}

// bumpSpeed adjusts the jog speed one step inside 0x00..0x3F.
func (m model) bumpSpeed(delta int) (model, tea.Cmd) {
	cur := pelco.DefaultJogSpeed
	if m.snap != nil && m.snap.JogSpeed != 0 {
		cur = int(m.snap.JogSpeed)
	}
	next := int(cur) + delta
	if next < 0 {
		next = 0
	}
	if next > int(pelco.MaxSpeed) {
		next = int(pelco.MaxSpeed)
	}
	m.submit(control.JogSpeedIntent{Speed: byte(next)})
	m.status = fmt.Sprintf("jog speed 0x%02X", next)
	return m, nil
}
