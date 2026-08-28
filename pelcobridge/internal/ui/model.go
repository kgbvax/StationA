// Package ui implements the Bubble Tea TUI: a thin view/controller over the
// headless engine. It renders the engine's published snapshot and translates
// keystrokes into engine commands. Local keyboard motion is tap-step: each
// arrow tap drives the unit for one short step then auto-stops, so the TUI
// never leaves the unit moving without an explicit action. Goto/home run to
// completion. Esc or space is the emergency stop.
package ui

import (
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"pelcots/internal/config"
	"pelcots/internal/engine"
)

const (
	uiInterval = 120 * time.Millisecond  // snapshot refresh cadence
	staleAfter = 1500 * time.Millisecond // no readback within this → "stale"
	logHeight  = 14                      // rows reserved for the TX/RX log panel

	// stepDuration is how long a single tap-step drives the unit before it
	// auto-stops. Terminals deliver no key-up event, so the earlier hold-to-move
	// model (detect release via the OS key-repeat stream) was unreliable.
	// Tap-step replaces it: each arrow tap = one discrete step; OS key-repeat
	// events arriving within stepDuration are suppressed, so holding a key
	// produces one step, not a runaway slew. Tap again for the next step.
	stepDuration = 500 * time.Millisecond
)

// focus values for the editable fields; focusNone means jog/hotkey mode.
// The toggle field (focusRotctldOn) holds a boolean rather than text and is
// flipped with space/enter/left/right — see isToggle / handleToggleKey.
const (
	focusNone = -1

	focusAddr      = iota - 1 // 0  unit address
	focusPan                  // 1  target azimuth
	focusTilt                 // 2  target elevation
	focusEndpoint             // 3  link endpoint
	focusRotctld              // 4  rotctld port
	focusRotctldOn            // 5  rotctld on/off toggle
	focusWrapLimit            // 6  cable-wrap limit
	focusTrueAz               // 7  true azimuth (calibration: what the antenna actually points at)
	fieldCount     = 8
)

// isToggle reports whether a focus index is a boolean toggle field (flipped in
// place) rather than a text-entry field.
func isToggle(focus int) bool {
	return focus == focusRotctldOn
}

type tickMsg time.Time

// stepMsg fires stepDuration after a tap-step begins; the handler stops the
// motion, ending the step. It is the tap-step replacement for hold-to-move.
type stepMsg struct{}

// confirmKind is a pending y/n question the TUI asks before a motion or
// arming action that must not fire on a stray keypress: the calibration
// goto-0 (drives the unit to its mechanical zero) and the arm (unlocks
// network motion). While a confirm is active all other keys are swallowed.
type confirmKind string

const (
	confirmNone  confirmKind = ""
	confirmGoto0 confirmKind = "goto0"
	confirmArm   confirmKind = "arm"
)

// Model is the Bubble Tea application state.
type Model struct {
	eng  *engine.Engine
	snap engine.State

	inputs []textinput.Model
	focus  int

	// confirm holds a pending y/n question (calibration goto-0, arm). While
	// set, all other keys are swallowed until the user answers.
	confirm confirmKind

	// connection being edited (mirrors the active transport's endpoint fields)
	transport  string
	serialPort string
	tcpAddr    string
	baud       int

	turbo bool
	// stepUntil is the time until which a tap-step is in progress; OS key-repeat
	// events arriving before it are suppressed so a held key yields one step.
	stepUntil time.Time

	width, height int
	logPath       string
	logLevel      config.LogLevel // preserved across TUI quit-saves (Config() restores it)
	base          config.Config   // loaded config; Config() starts from it so non-editable fields (self_check, sim) survive quit-saves
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
		base:       cfg,
	}
	m.inputs[focusAddr] = mk(strconv.Itoa(int(cfg.Addr)), 4)
	m.inputs[focusPan] = mk("0", 7)
	m.inputs[focusTilt] = mk("0", 7)
	m.inputs[focusEndpoint] = mk(endpoint, 24)
	m.inputs[focusRotctld] = mk(strconv.Itoa(cfg.Control.Rotctld.Port), 6)
	m.inputs[focusWrapLimit] = mk(strconv.FormatFloat(cfg.Wrap.Limit, 'f', -1, 64), 6)
	m.inputs[focusTrueAz] = mk("0", 7)
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
	case stepMsg:
		// A tap-step's stepDuration has elapsed: halt the unit, ending the step.
		m.stopAll()
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
		// Esc is the always-available emergency stop: halt all motion, cancel
		// any pending confirm, and drop out of any focused field. Stopping is
		// harmless when idle.
		m.confirm = confirmNone
		m.stopAll()
		m.blur()
		return m, nil
	}

	// A pending confirm swallows every other key: y (or enter) executes the
	// guarded action, anything else cancels.
	if m.confirm != confirmNone {
		switch msg.String() {
		case "y", "Y":
			m.doConfirm()
		default:
			m.confirm = confirmNone
		}
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

	// Jog / hotkey mode. Arrow keys are tap-step (one discrete step per tap);
	// goto/home run to completion. Space or Esc is the emergency stop.
	var cmd tea.Cmd
	switch msg.String() {
	case "q":
		return m.quit()
	case " ":
		m.stopAll()
	case "g", "enter":
		cmd = m.doGoto()
	case "h":
		m.applyAddr()
		m.eng.Home()
	case "t":
		m.turbo = !m.turbo
	case "m":
		m.toggleTransport()
	case "r":
		m.doReconnect()
	case "o":
		m.eng.SetRotctld(!m.snap.RotctldOn, m.portField(focusRotctld, m.snap.RotctldPort))
	case "w":
		m.eng.SetWrap(!m.snap.WrapEnabled, m.limitField())
	case "z":
		m.eng.ZeroWrap()
	case "a":
		m.eng.ZeroAzimuth()
	case "c":
		// Calibration goto-0: asks before driving the unit to its mechanical
		// zero (the cable-safe reference the arm workflow re-zeros the wind at).
		m.confirm = confirmGoto0
	case "A":
		// Arm: asks before unlocking inbound network motion.
		m.confirm = confirmArm
	case "up", "k":
		cmd = m.keyStep(0, 1)
	case "down", "j":
		cmd = m.keyStep(0, -1)
	case "left":
		cmd = m.keyStep(-1, 0)
	case "right":
		cmd = m.keyStep(1, 0)
	}
	return m, cmd
}

// stopAll is the emergency stop: halt the unit and clear the tap-step window so
// the next step is responsive. Idempotent and safe to call when idle.
func (m *Model) stopAll() {
	m.stepUntil = time.Time{}
	m.eng.StopMotion()
}

// doConfirm executes the guarded action the user just confirmed with y.
func (m *Model) doConfirm() {
	k := m.confirm
	m.confirm = confirmNone
	switch k {
	case confirmGoto0:
		m.applyAddr()
		m.eng.GotoZero()
	case confirmArm:
		m.eng.Arm()
	}
}

// keyStep drives the unit for one tap-step in the given pan/tilt direction.
// OS key-repeat events arriving while a step is in progress (before stepUntil)
// are suppressed, so holding a key produces a single step, not continuous
// motion. A stepMsg is armed to auto-stop the unit after stepDuration.
func (m *Model) keyStep(pan, tilt int) tea.Cmd {
	now := time.Now()
	if now.Before(m.stepUntil) {
		return nil // a step is in progress; ignore the key-repeat
	}
	m.stepUntil = now.Add(stepDuration)
	m.eng.Jog(pan, tilt, m.turbo)
	return tea.Tick(stepDuration, func(time.Time) tea.Msg { return stepMsg{} })
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
	return nil
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
		if m.focus == focusRotctldOn {
			m.eng.SetRotctld(!m.snap.RotctldOn, m.portField(focusRotctld, m.snap.RotctldPort))
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
	case focusRotctld:
		m.eng.SetRotctld(m.snap.RotctldOn, m.portField(focusRotctld, m.snap.RotctldPort))
	case focusWrapLimit:
		m.eng.SetWrap(m.snap.WrapEnabled, m.limitField())
	case focusTrueAz:
		// Offset entry: what the antenna ACTUALLY points at right now. The
		// engine sets azOffset = curPan − true so incoming azimuths map onto
		// the physical frame. Invalid text reverts to the current readback.
		if v, err := strconv.ParseFloat(strings.TrimSpace(m.inputs[focusTrueAz].Value()), 64); err == nil {
			m.eng.SetAzimuthTrue(v)
		} else {
			s := m.eng.Snapshot()
			fallback := 0.0
			if s.HavePan {
				fallback = s.CurPan
			}
			m.inputs[focusTrueAz].SetValue(strconv.FormatFloat(fallback, 'f', -1, 64))
		}
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
// It starts from the loaded config (not Default()) so fields the TUI does not
// edit — self_check, sim, log_level — survive a quit-save unchanged.
func (m Model) Config() config.Config {
	s := m.eng.Snapshot()
	c := m.base
	c.Transport = m.transport
	c.Serial.Port = m.serialPort
	c.Serial.Baud = m.baud
	c.TCP.Address = m.tcpAddr
	c.Addr = s.Addr
	c.AzOffset = s.AzOffset
	c.Log = m.logPath
	c.LogLevel = m.logLevel
	c.Control.Bind = s.Bind
	c.Control.Rotctld = config.ServerConfig{Enabled: s.RotctldOn, Port: s.RotctldPort}
	c.Wrap = config.WrapConfig{Enabled: s.WrapEnabled, Limit: s.WrapLimit, Accumulated: s.Wrap}
	return c
}
