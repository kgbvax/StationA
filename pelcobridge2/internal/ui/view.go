package ui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

var (
	styleBar    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("62"))
	styleArmed  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("52"))
	styleGood   = lipgloss.NewStyle().Foreground(lipgloss.Color("120"))
	styleBad    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("248"))
	styleLabel  = lipgloss.NewStyle().Foreground(lipgloss.Color("117"))
	styleNumber = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
)

// helpLines is the ? overlay: every key with what it really does. Manual
// motion comes first — it is the primary use and works while disarmed.
var helpLines = []string{
	"keys",
	"  ←→↑↓ / hjkl     jog (HOLD the key; auto-repeat keeps it moving, release stops) — works DISARMED",
	"  0               goto PHYSICAL zero (offset not applied) — works disarmed",
	"  a / e           query azimuth / elevation",
	"  A               ARM: enter the TRUE azimuth the head points at now — enables rotctl",
	"  d               disarm (rotctl motion locked again)",
	"  + / -           jog speed ±1",
	"  s               self-test — DANGER re-homes head, can rip cables; disarmed only, y/n confirm",
	"  c               self-check OFF — station default, no confirm",
	"  C               self-check ON — maintenance: head re-homes itself UNPROMPTED; disarmed only, y/n",
	"  SPACE / ESC     E-STOP: all-stop now, cancels prompts — always works",
	"  tab / shift+tab scroll wire log",
	"  ctrl+r          reopen serial port      ctrl+l  clear log",
	"  ctrl+c / ctrl+q quit (all-stop sent first)",
	"  ?               toggle this help",
}

// layout re-sizes the log viewport to the space below the fixed panes. The
// fixed chrome is 7 rows: header, blank, 3 position lines, blank, prompt,
// status — plus the help overlay when open. Everything else is log.
func (m *model) layout() {
	chrome := 7
	if m.help {
		chrome += len(helpLines) + 2
	}
	h := m.height - chrome
	if h < 3 {
		h = 3
	}
	w := m.width
	if w < 20 {
		w = 20
	}
	m.logView.Width = w
	m.logView.Height = h
	m.logView.SetContent(strings.Join(m.log, "\n"))
}

// appendLog appends one wire-log line, keeping the viewport pinned to the tail
// only when the operator is already reading the tail (ptest discipline).
func (m *model) appendLog(line string) {
	atBottom := m.logView.AtBottom()
	m.log = append(m.log, line)
	if len(m.log) > 2000 {
		m.log = append([]string(nil), m.log[len(m.log)-1000:]...)
	}
	m.logView.SetContent(strings.Join(m.log, "\n"))
	if atBottom {
		m.logView.GotoBottom()
	}
}

func fmtDeg(v float64) string {
	if math.IsNaN(v) {
		return "  ---.--"
	}
	return fmt.Sprintf("%7.2f", v)
}

func fmtAge(d time.Duration, valid bool) string {
	if !valid {
		return styleDim.Render("no fix")
	}
	return styleDim.Render(fmt.Sprintf("%.1fs", d.Seconds()))
}

// View renders header · position pane · log viewport · prompt · status.
func (m model) View() string {
	var b strings.Builder

	// Header bar: identity, armed state, links.
	armed := styleDim.Render("DISARMED")
	if m.snap != nil && m.snap.Armed {
		armed = styleArmed.Render("● ARMED")
	}
	mqttTxt := "mqtt: --"
	if m.opts.MQTTOn != nil {
		if m.opts.MQTTOn() {
			mqttTxt = styleGood.Render("mqtt: on")
		} else {
			mqttTxt = styleDim.Render("mqtt: off")
		}
	}
	linkTxt := styleDim.Render("head: ---")
	clients := "-"
	if m.opts.Clients != nil {
		clients = fmt.Sprint(m.opts.Clients())
	}
	if m.snap != nil {
		if m.snap.DeviceOnline {
			linkTxt = styleGood.Render("head: online")
		} else {
			linkTxt = styleBad.Render("head: offline")
		}
	}
	speed := "--"
	if m.snap != nil {
		speed = fmt.Sprintf("0x%02X", m.snap.JogSpeed)
	}
	bar := fmt.Sprintf(" pelcobridge2 · %s · %d 8N1 · addr %d · jog %s · rotctl:%s · %s · %s · %s ",
		displayPort(m.opts.PortName), m.opts.Baud, m.opts.Addr, speed, clients, mqttTxt, linkTxt, armed)
	b.WriteString(styleBar.Width(m.width).Render(bar))
	b.WriteString("\n\n")

	// Position pane.
	b.WriteString(m.positionPane())
	b.WriteString("\n\n")

	// Wire log.
	b.WriteString(m.logView.View())
	b.WriteString("\n")

	// Help overlay (toggle with ?) — the full keymap, since the status line
	// only carries the highlights.
	if m.help {
		b.WriteString("\n")
		b.WriteString(strings.Join(helpLines, "\n"))
		b.WriteString("\n")
	}

	// Prompt line (input visible only while a prompt is open).
	if m.prompt != promptNone {
		b.WriteString(m.input.View())
		b.WriteString("\n")
	}

	// Status line.
	st := m.status
	if st != "" {
		b.WriteString(st)
	}
	return b.String()
}

func (m model) positionPane() string {
	if m.snap == nil {
		return styleDim.Render("waiting for engine…")
	}
	s := m.snap

	trueAz, trueEl := fmtDeg(s.Az), fmtDeg(s.El)
	physAz, physEl := fmtDeg(s.PhysAz), fmtDeg(s.PhysEl)
	off := fmt.Sprintf("%+.2f", s.Offset)
	target := "---"
	if !math.IsNaN(s.TargetAz) || !math.IsNaN(s.TargetEl) {
		target = fmt.Sprintf("%s / %s", fmtDeg(s.TargetAz), fmtDeg(s.TargetEl))
	}

	setStat := styleDim.Render("idle")
	switch {
	case s.SetStatus == "setting":
		setStat = styleLabel.Render("setting")
	case s.SetStatus == "converged":
		setStat = styleGood.Render("converged")
	case s.SetStatus == "failed":
		setStat = styleBad.Render("FAILED")
	}
	moving := ""
	if s.Moving {
		moving = " · " + styleLabel.Render("MOVING")
	}
	// The periodic self-check is a standing hazard while on: the head
	// re-homes itself unprompted. "on" must be loud; "unknown" (no RX proof
	// of the claim yet, or the link died) stays honest, never optimistic.
	switch s.SelfCheck {
	case "on":
		moving += " · " + styleBad.Render("SELF-CHECK ON")
	case "off":
		moving += " · " + styleDim.Render("self-check off")
	default: // "unknown" — claim not proven by a frame from the head
		moving += " · " + styleDim.Render("self-check ?")
	}
	if s.Error != "" {
		moving += " · " + styleBad.Render(s.Error)
	}

	line1 := fmt.Sprintf("%s %s (%s)   %s %s (%s)   readback %s",
		styleLabel.Render("TRUE AZ"), styleNumber.Render(trueAz), fmtAge(s.PanAge, s.ReadbackValid),
		styleLabel.Render("EL"), styleNumber.Render(trueEl), fmtAge(s.TiltAge, s.ReadbackValid),
		onOff(s.ReadbackValid))
	line2 := fmt.Sprintf("%s %s   %s %s   %s %s",
		styleLabel.Render("PHYS AZ"), styleNumber.Render(physAz),
		styleLabel.Render("PHYS EL"), styleNumber.Render(physEl),
		styleLabel.Render("offset"), styleNumber.Render(off))
	line3 := fmt.Sprintf("%s %s   %s%s",
		styleLabel.Render("TARGET"), styleNumber.Render(target), setStat, moving)
	return line1 + "\n" + line2 + "\n" + line3
}

func onOff(b bool) string {
	if b {
		return styleGood.Render("ok")
	}
	return styleDim.Render("down")
}

// displayPort shortens long by-id paths for the header bar.
func displayPort(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
