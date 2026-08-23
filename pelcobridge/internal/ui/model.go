// Package ui implements the Bubble Tea TUI: a thin view/controller over the
// headless engine. It renders the engine's published snapshot and translates
// keystrokes into engine commands. Local keyboard motion is hold-to-move —
// motion lasts only while a key is held (see armRelease) — so the TUI never
// drives the unit without an active key press.
package ui

import (
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"pelcots/internal/config"
	"pelcots/internal/control"
	"pelcots/internal/engine"
)

const (
	uiInterval = 120 * time.Millisecond  // snapshot refresh cadence
	staleAfter = 1500 * time.Millisecond // no readback within this → "stale"
	logHeight  = 14                      // rows reserved for the TX/RX log panel

	// holdRelease is how long a motion lasts after the last key event before it
	// is auto-stopped. Terminals deliver no key-up events, so a held key is
	// detected via the OS key-repeat stream: each repeat re-arms the timer;
	// when repeats stop (key released), the timer fires and stops the motion.
	holdRelease = 300 * time.Millisecond
)

// focus values for the editable fields; focusNone means jog/hotkey mode.
// Toggle fields (focusGS232On, focusRotctldOn) hold a boolean rather than text
// and are flipped with space/enter/left/right — see isToggle / handleToggleKey.
const (
	focusNone = -1

	focusAddr         = iota - 1 // 0  unit address
	focusPan                     // 1  target azimuth
	focusTilt                    // 2  target elevation
	focusEndpoint                // 3  link endpoint
	focusGS232                   // 4  gs232 port
	focusGS232On                 // 5  gs232 on/off toggle
	focusRotctld                 // 6  rotctld port
	focusRotctldOn               // 7  rotctld on/off toggle
	focusPstRotator              // 8  pstrotator port
	focusPstRotatorOn            // 9  pstrotator on/off toggle
	focusWrapLimit               // 10 cable-wrap limit
	fieldCount        = 11
)

// isToggle reports whether a focus index is a boolean toggle field (flipped in
// place) rather than a text-entry field.
func isToggle(focus int) bool {
	return focus == focusGS232On || focus == focusRotctldOn || focus == focusPstRotatorOn
}

type tickMsg time.Time

// releaseMsg fires holdRelease after a motion key press; if seq still matches
// the latest press, the key was released → stop the motion.
type releaseMsg struct{ seq uint64 }

// Model is the Bubble Tea application state.
type Model struct {
	eng  *engine.Engine
	snap engine.State

	inputs []textinput.Model
	focus  int

	// connection being edited (mirrors the active transport's endpoint fields)
	transport  string
	serialPort string
	tcpAddr    string
	baud       int

	turbo   bool
	heldSeq uint64

	width, height int
	logPath       string
	logLevel      config.LogLevel // preserved across TUI quit-saves (Config() restores it)
}

// New builds the model around a started engine and the initial config.
func New(eng *engine.Engine, cfg config.Config, logPath string) Model {
	mk := func(val string, w int) textinput.Model {
		ti := textinput.New()
		ti.SetValue(val)
		ti.CharLimit = 24
		ti.Width = w
		ti.Prompt = ""
		return ti
	}
	endpoint := cfg.Serial.Port
	if cfg.Transport == config.TransportTCP {
		endpoint = cfg.TCP.Address
	}
	m := Model{
		eng:        eng,
		inputs:     make([]textinput.Model, fieldCount),
		focus:      focusNone,
		transport:  cfg.Transport,
		serialPort: cfg.Serial.Port,
		tcpAddr:    cfg.TCP.Address,
		baud:       cfg.Serial.Baud,
		logPath:    logPath,
	}
	m.inputs[focusAddr] = mk(strconv.Itoa(int(cfg.Addr)), 4)
	m.inputs[focusPan] = mk("0", 7)
	m.inputs[focusTilt] = mk("0", 7)
	m.inputs[focusEndpoint] = mk(endpoint, 24)
	m.inputs[focusGS232] = mk(strconv.Itoa(cfg.Control.GS232.Port), 6)
	m.inputs[focusRotctld] = mk(strconv.Itoa(cfg.Control.Rotctld.Port), 6)
	m.inputs[focusPstRotator] = mk(strconv.Itoa(cfg.Control.PstRotator.Port), 6)
	m.inputs[focusWrapLimit] = mk(strconv.FormatFloat(cfg.Wrap.Limit, 'f', -1, 64), 6)
	m.snap = eng.Snapshot()
	m.logLevel = cfg.LogLevel
	return m
}

// Init starts the snapshot refresh tick.
func (m Model) Init() tea.Cmd { return tickCmd() }

func tickCmd() tea.Cmd {
	return tea.Tick(uiInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Update is the Bubble Tea event handler.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		m.snap = m.eng.Snapshot()
		return m, tickCmd()
	case releaseMsg:
		if msg.seq == m.heldSeq {
			m.eng.StopMotion()
		}
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m.quit()
	case "tab":
		m.cycleFocus(1)
		return m, nil
	case "shift+tab":
		m.cycleFocus(-1)
		return m, nil
	case "esc":
		m.blur()
		return m, nil
	}

	if m.focus != focusNone {
		if isToggle(m.focus) {
			m.handleToggleKey(msg)
			return m, nil
		}
		if msg.String() == "enter" {
			return m, m.commitFocused()
		}
		var cmd tea.Cmd
		m.inputs[m.focus], cmd = m.inputs[m.focus].Update(msg)
		return m, cmd
	}

	// Jog / hotkey mode. Motion keys must be held: each press (re)issues the
	// move and arms a release timer.
	var cmd tea.Cmd
	switch msg.String() {
	case "q":
		return m.quit()
	case " ":
		m.eng.StopMotion()
	case "g", "enter":
		cmd = m.doGoto()
	case "h":
		m.applyAddr()
		m.eng.Home()
		cmd = m.armRelease()
	case "t":
		m.turbo = !m.turbo
	case "m":
		m.toggleTransport()
	case "r":
		m.doReconnect()
	case "y":
		m.eng.SetServer(control.GS232, !m.snap.GS232On, m.portField(focusGS232, m.snap.GS232Port))
	case "o":
		m.eng.SetServer(control.Rotctld, !m.snap.RotctldOn, m.portField(focusRotctld, m.snap.RotctldPort))
	case "p":
		m.eng.SetServer(control.PstRotator, !m.snap.PstRotatorOn, m.portField(focusPstRotator, m.snap.PstRotatorPort))
	case "w":
		m.eng.SetWrap(!m.snap.WrapEnabled, m.limitField())
	case "z":
		m.eng.ZeroWrap()
	case "up", "k":
		cmd = m.keyJog(0, 1)
	case "down", "j":
		cmd = m.keyJog(0, -1)
	case "left":
		cmd = m.keyJog(-1, 0)
	case "right":
		cmd = m.keyJog(1, 0)
	}
	return m, cmd
}

// armRelease bumps the held-key sequence and returns a command that fires a
// releaseMsg after holdRelease. A later press invalidates earlier timers.
func (m *Model) armRelease() tea.Cmd {
	m.heldSeq++
	seq := m.heldSeq
	return tea.Tick(holdRelease, func(time.Time) tea.Msg { return releaseMsg{seq} })
}

func (m *Model) keyJog(pan, tilt int) tea.Cmd {
	m.eng.Jog(pan, tilt, m.turbo)
	return m.armRelease()
}

func (m *Model) doGoto() tea.Cmd {
	m.applyAddr()
	az, el, ok := m.targetFromFields()
	if !ok {
		// Invalid pan/tilt text: revert to the current readback (like the addr
		// and port fields do) instead of silently keeping un-parseable text.
		m.revertTargets()
		return nil
	}
	m.eng.Goto(az, el)
	return m.armRelease()
}

// revertTargets resets the pan/tilt input fields to the latest readback (or 0
// when no readback has arrived), so a failed goto leaves the fields usable.
func (m *Model) revertTargets() {
	s := m.eng.Snapshot()
	pan, tilt := "0", "0"
	if s.HavePan {
		pan = strconv.FormatFloat(s.CurPan, 'f', -1, 64)
	}
	if s.HaveTilt {
		tilt = strconv.FormatFloat(s.CurTilt, 'f', -1, 64)
	}
	m.inputs[focusPan].SetValue(pan)
	m.inputs[focusTilt].SetValue(tilt)
}

// handleToggleKey flips the focused on/off field. The unit stays put; only the
// inbound-control server is started or stopped, reusing its edited port.
func (m *Model) handleToggleKey(msg tea.KeyMsg) {
	switch msg.String() {
	case " ", "enter", "left", "right":
		switch m.focus {
		case focusGS232On:
			m.eng.SetServer(control.GS232, !m.snap.GS232On, m.portField(focusGS232, m.snap.GS232Port))
		case focusRotctldOn:
			m.eng.SetServer(control.Rotctld, !m.snap.RotctldOn, m.portField(focusRotctld, m.snap.RotctldPort))
		case focusPstRotatorOn:
			m.eng.SetServer(control.PstRotator, !m.snap.PstRotatorOn, m.portField(focusPstRotator, m.snap.PstRotatorPort))
		}
	}
}

// commitFocused applies the focused field's value and returns any follow-up cmd.
func (m *Model) commitFocused() tea.Cmd {
	switch m.focus {
	case focusAddr:
		// Committing the address field only applies the address; it must NOT
		// issue a goto (the pan/tilt fields are unrelated to the unit address).
		m.applyAddr()
	case focusPan, focusTilt:
		cmd := m.doGoto()
		m.blur()
		return cmd
	case focusEndpoint:
		m.doReconnect()
	case focusGS232:
		m.eng.SetServer(control.GS232, m.snap.GS232On, m.portField(focusGS232, m.snap.GS232Port))
	case focusRotctld:
		m.eng.SetServer(control.Rotctld, m.snap.RotctldOn, m.portField(focusRotctld, m.snap.RotctldPort))
	case focusPstRotator:
		m.eng.SetServer(control.PstRotator, m.snap.PstRotatorOn, m.portField(focusPstRotator, m.snap.PstRotatorPort))
	case focusWrapLimit:
		m.eng.SetWrap(m.snap.WrapEnabled, m.limitField())
	}
	m.blur()
	return nil
}

func (m *Model) applyAddr() {
	if v, err := strconv.ParseUint(strings.TrimSpace(m.inputs[focusAddr].Value()), 10, 8); err == nil && v >= 1 {
		m.eng.SetAddr(byte(v))
	} else {
		m.inputs[focusAddr].SetValue(strconv.Itoa(int(m.snap.Addr)))
	}
}

func (m *Model) targetFromFields() (az, el float64, ok bool) {
	pan, e1 := strconv.ParseFloat(strings.TrimSpace(m.inputs[focusPan].Value()), 64)
	tilt, e2 := strconv.ParseFloat(strings.TrimSpace(m.inputs[focusTilt].Value()), 64)
	if e1 != nil || e2 != nil {
		return 0, 0, false
	}
	return pan, tilt, true
}

func (m *Model) portField(idx, fallback int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(m.inputs[idx].Value())); err == nil && v > 0 && v < 65536 {
		return v
	}
	m.inputs[idx].SetValue(strconv.Itoa(fallback))
	return fallback
}

func (m *Model) limitField() float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(m.inputs[focusWrapLimit].Value()), 64); err == nil && v > 0 {
		return v
	}
	m.inputs[focusWrapLimit].SetValue(strconv.FormatFloat(m.snap.WrapLimit, 'f', -1, 64))
	return m.snap.WrapLimit
}

func (m *Model) toggleTransport() {
	if m.transport == config.TransportTCP {
		m.transport = config.TransportSerial
		m.inputs[focusEndpoint].SetValue(m.serialPort)
	} else {
		m.transport = config.TransportTCP
		m.inputs[focusEndpoint].SetValue(m.tcpAddr)
	}
}

func (m *Model) doReconnect() {
	ep := strings.TrimSpace(m.inputs[focusEndpoint].Value())
	if m.transport == config.TransportTCP {
		m.tcpAddr = ep
	} else {
		m.serialPort = ep
	}
	m.eng.Reconnect(engine.ConnSpec{
		Transport:  m.transport,
		SerialPort: m.serialPort,
		Baud:       m.baud,
		TCPAddr:    m.tcpAddr,
	})
}

func (m *Model) cycleFocus(delta int) {
	next := m.focus + delta
	switch {
	case next < focusNone: // only wrap when past focusNone, so -1 stays focusNone
		next = fieldCount - 1
	case next >= fieldCount:
		next = focusNone
	}
	m.blur()
	m.focus = next
	if m.focus != focusNone && !isToggle(m.focus) {
		m.inputs[m.focus].Focus()
	}
}

func (m *Model) blur() {
	for i := range m.inputs {
		m.inputs[i].Blur()
	}
	m.focus = focusNone
}

func (m Model) quit() (tea.Model, tea.Cmd) {
	m.eng.StopMotion()
	return m, tea.Quit
}

// Config builds the persisted configuration from current engine + UI state.
func (m Model) Config() config.Config {
	s := m.eng.Snapshot()
	c := config.Default()
	c.Transport = m.transport
	c.Serial.Port = m.serialPort
	c.Serial.Baud = m.baud
	c.TCP.Address = m.tcpAddr
	c.Addr = s.Addr
	c.Log = m.logPath
	c.LogLevel = m.logLevel
	c.Control.Bind = s.Bind
	c.Control.GS232 = config.ServerConfig{Enabled: s.GS232On, Port: s.GS232Port}
	c.Control.Rotctld = config.ServerConfig{Enabled: s.RotctldOn, Port: s.RotctldPort}
	c.Control.PstRotator = config.ServerConfig{Enabled: s.PstRotatorOn, Port: s.PstRotatorPort}
	c.Wrap = config.WrapConfig{Enabled: s.WrapEnabled, Limit: s.WrapLimit, Accumulated: s.Wrap}
	return c
}
