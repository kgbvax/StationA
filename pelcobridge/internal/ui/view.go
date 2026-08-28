package ui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"pelcots/internal/config"
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("63")).Padding(0, 1)
	labelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	valueStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)
	focusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("213")).Bold(true)
	liveStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	staleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	hintStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
	logStyle   = lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(lipgloss.Color("238")).PaddingLeft(1).Foreground(lipgloss.Color("250"))
)

// View renders the whole TUI from the latest engine snapshot.
func (m Model) View() string {
	s := m.snap
	field := func(i int) string {
		v := m.inputs[i].View()
		if m.focus == i {
			return focusStyle.Render("[") + v + focusStyle.Render("]")
		}
		return " " + v + " "
	}
	// toggle renders a boolean on/off field, bracketed when focused.
	toggle := func(i int) string {
		on := false
		if i == focusRotctldOn {
			on = s.RotctldOn
		}
		v := hintStyle.Render("off")
		if on {
			v = liveStyle.Render("on")
		}
		if m.focus == i {
			return focusStyle.Render("[") + v + focusStyle.Render("]")
		}
		return " " + v + " "
	}

	connState := staleStyle.Render("disconnected")
	switch {
	case s.Connected:
		connState = liveStyle.Render("connected")
	case s.Reconnecting:
		connState = warnStyle.Render("reconnecting…")
	}
	transport := "serial"
	switch s.Transport {
	case config.TransportTCP:
		transport = "tcp"
	case config.TransportSim:
		transport = "sim"
	}

	azLine := m.readbackLine("Current  azimuth  ", s.HavePan, s.CurPanRaw, s.LastPan)
	if s.AzOffset != 0 {
		azLine += "  " + hintStyle.Render(fmt.Sprintf("zeroed @ %.2f°", s.AzOffset))
	}

	header := strings.Join([]string{
		titleStyle.Render("pelcots") + "  " + hintStyle.Render("Pelco-D PTZ diagnostic & rotator network controller"),
		fmt.Sprintf("%s %s %s   %s %s   %s%s   %s %s   %s %s   %s",
			labelStyle.Render("link"), valueStyle.Render(transport), field(focusEndpoint),
			labelStyle.Render("baud"), valueStyle.Render(fmt.Sprintf("%d 8N1", s.Baud)),
			labelStyle.Render("addr"), field(focusAddr),
			labelStyle.Render("proto"), valueStyle.Render(strings.ToUpper(s.Protocol)),
			labelStyle.Render("rx"), valueStyle.Render(fmt.Sprintf("%d B", s.BytesIn)),
			connState),
		"",
		fmt.Sprintf("%s  azimuth%s°  elevation%s°  %s",
			labelStyle.Render("Target "), field(focusPan), field(focusTilt),
			hintStyle.Render("(type, then Enter to go)")),
		azLine,
		m.readbackLine("         elevation", s.HaveTilt, s.CurTiltRaw, s.LastTilt),
		m.jogLine(),
		m.wrapLine(field),
		m.serversLine(field, toggle),
		"",
		hintStyle.Render(s.Status),
	}, "\n")

	footer := hintStyle.Render("tab field · on/off field: space toggles · g/enter go · h home · arrows/kj tap-step · t turbo · ") +
		warnStyle.Render("⏹ esc/space STOP")
	footer += "\n" + hintStyle.Render("m transport · r reconnect · o rotctld · w wrap · z zero-wrap · a zero-az · q quit")
	if m.logPath != "" {
		footer += "\n" + hintStyle.Render("trace → "+m.logPath)
	}

	return lipgloss.JoinVertical(lipgloss.Left, header, m.logView(), footer)
}

// jogLine shows the current motion state and the turbo toggle.
func (m Model) jogLine() string {
	s := m.snap
	turbo := hintStyle.Render("turbo off")
	if m.turbo {
		turbo = staleStyle.Render("TURBO on")
	}
	switch {
	case s.Unwrapping:
		return labelStyle.Render("Motion  ") + warnStyle.Render("unwinding cable (esc/space to abort)") + "   " + turbo
	case s.Gotoing:
		return labelStyle.Render("Motion  ") + liveStyle.Render("seeking target (esc/space to abort)") + "   " + turbo
	case s.Jogging:
		dir := ""
		switch {
		case s.JogPan > 0:
			dir += "→ "
		case s.JogPan < 0:
			dir += "← "
		}
		switch {
		case s.JogTilt > 0:
			dir += "↑"
		case s.JogTilt < 0:
			dir += "↓"
		}
		return labelStyle.Render("Motion  ") + liveStyle.Render(fmt.Sprintf("stepping %s (auto-stop / esc·space)", dir)) + "   " + turbo
	default:
		return labelStyle.Render("Motion  ") + hintStyle.Render("idle") + "   " + turbo
	}
}

// wrapLine shows the cable-wind accumulator, protection state, and the editable
// wind limit (±limit°).
func (m Model) wrapLine(field func(int) string) string {
	s := m.snap
	limit := labelStyle.Render("limit ±") + field(focusWrapLimit) + labelStyle.Render("°")
	if !s.WrapEnabled {
		return labelStyle.Render("Wrap    ") + hintStyle.Render(fmt.Sprintf("off  (wind %+.0f°)", s.Wrap)) + "   " + limit
	}
	st := liveStyle
	switch frac := math.Abs(s.Wrap) / s.WrapLimit; {
	case frac >= 1.0:
		st = staleStyle
	case frac >= 0.85:
		st = warnStyle
	}
	return labelStyle.Render("Wrap    ") + st.Render(fmt.Sprintf("wind %+.0f° / ±%.0f°", s.Wrap, s.WrapLimit)) + "   " + limit
}

// serversLine shows the inbound-control server state with its editable port
// and tab-able on/off toggle.
func (m Model) serversLine(field, toggle func(int) string) string {
	s := m.snap
	return fmt.Sprintf("%s %s%s%s   %s %s",
		labelStyle.Render("Servers "),
		labelStyle.Render("rotctld"), field(focusRotctld), toggle(focusRotctldOn),
		labelStyle.Render("bind"), valueStyle.Render(s.Bind))
}

func (m Model) readbackLine(label string, have bool, raw uint16, last time.Time) string {
	l := labelStyle.Render(label)
	if !have {
		return fmt.Sprintf("%s  %s", l, staleStyle.Render("— no readback —"))
	}
	state := liveStyle.Render("live")
	if age := time.Since(last); age > staleAfter {
		state = staleStyle.Render(fmt.Sprintf("stale %.1fs", age.Seconds()))
	}
	// Print the raw readback bytes in hex — do not interpret the data word as
	// degrees. The tilt decode is calibrated per-device and may be wrong, so the
	// diagnostic TUI shows the verbatim wire bytes (D1 D2 big-endian) and the
	// 16-bit word, never a converted angle.
	return fmt.Sprintf("%s  %s  %s",
		l,
		valueStyle.Render(fmt.Sprintf("%02X %02X  (0x%04X)", byte(raw>>8), byte(raw), raw)),
		state)
}

func (m Model) logView() string {
	lines := m.snap.Log
	if len(lines) > logHeight {
		lines = lines[len(lines)-logHeight:]
	}
	if len(lines) == 0 {
		lines = []string{hintStyle.Render("waiting for serial traffic…")}
	}
	body := strings.Join(lines, "\n")
	return logStyle.Height(logHeight).Render(body)
}
