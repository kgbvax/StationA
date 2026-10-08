// Package rigctl is the minimal hamlib rigctld client for the park retune.
//
// The bridge never opens the radio's serial port for control (the serial
// port is reserved for other software). The only control path is a rigctld
// on the host that owns a dedicated CI-V port (scmino), and this client can
// set three things: satellite mode off, the mode, and the frequency (and read
// the last two back). There is deliberately no PTT, no power and no raw-CI-V
// send in this package — the receive-only posture holds.
package rigctl

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// Modes maps the station's canonical mode names (band-mode-reference.md) to
// the hamlib names the park retune may select. Anything else is refused
// before a connection is opened.
var Modes = map[string]string{
	"usb": "USB", "lsb": "LSB", "cw": "CW", "am": "AM", "fm": "FM",
}

// passbandHz is the filter width set with the mode. Passband 0 means "keep
// the current filter": a radio coming from CW stayed on its 500 Hz filter in
// FM (live 2026-10-08), so each mode names its own.
var passbandHz = map[string]int{
	"usb": 2400, "lsb": 2400, "cw": 500, "am": 6000, "fm": 12000,
}

// Frequency sanity bound: the IC-9700 covers 144 MHz up to 1.3 GHz.
const (
	MinFreqHz = 100_000_000
	MaxFreqHz = 1_400_000_000
)

// Validate checks a park target without touching the network.
func Validate(freqHz int64, mode string) error {
	if freqHz < MinFreqHz || freqHz > MaxFreqHz {
		return fmt.Errorf("freq_hz %d outside %d..%d", freqHz, int64(MinFreqHz), int64(MaxFreqHz))
	}
	if _, ok := Modes[mode]; !ok {
		return fmt.Errorf("mode %q not one of usb lsb cw am fm", mode)
	}
	return nil
}

// Client talks to one rigctld. A fresh TCP connection per Tune: the retune is
// a rare one-shot, and holding the connection open would pin rigctld's
// single-client serial access.
type Client struct {
	Addr    string
	Timeout time.Duration // whole Tune call (default 5 s)
}

// Tune parks the radio: satellite mode off first (a radio left in sat mode
// keeps its split main/sub receivers and its own mode, and leaving sat mode
// restores a different main frequency), then the frequency, then the
// (canonical) mode, then both are read back and must match. The mode must
// follow the frequency: the IC-9700 stores a mode per band and restores it
// on a band change, so a mode set before the frequency is overwritten.
func (c *Client) Tune(ctx context.Context, freqHz int64, mode string) error {
	if err := Validate(freqHz, mode); err != nil {
		return err
	}
	to := c.Timeout
	if to <= 0 {
		to = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return fmt.Errorf("rigctld %s: %w", c.Addr, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	r := bufio.NewReader(conn)

	talk := func(cmd string) ([]string, error) {
		if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
			return nil, fmt.Errorf("rigctld write %q: %w", cmd, err)
		}
		n := 1
		switch cmd {
		case "m":
			n = 2 // mode, passband
		}
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			line, err := r.ReadString('\n')
			if err != nil {
				return nil, fmt.Errorf("rigctld read after %q: %w", cmd, err)
			}
			out = append(out, strings.TrimSpace(line))
		}
		return out, nil
	}
	set := func(cmd string) error {
		out, err := talk(cmd)
		if err != nil {
			return err
		}
		if out[0] != "RPRT 0" {
			return fmt.Errorf("rigctld refused %q: %s", cmd, out[0])
		}
		return nil
	}

	for _, cmd := range []string{
		"U SATMODE 0",
		fmt.Sprintf("F %d", freqHz),
		fmt.Sprintf("M %s %d", Modes[mode], passbandHz[mode]),
	} {
		if err := set(cmd); err != nil {
			return err
		}
	}

	// Read back: the radio, not rigctld's RPRT 0, is the proof.
	if out, err := talk("f"); err != nil {
		return err
	} else if out[0] != fmt.Sprint(freqHz) {
		return fmt.Errorf("radio reads %s Hz after retune, want %d", out[0], freqHz)
	}
	if out, err := talk("m"); err != nil {
		return err
	} else if out[0] != Modes[mode] {
		return fmt.Errorf("radio reads mode %s after retune, want %s", out[0], Modes[mode])
	}
	return nil
}
