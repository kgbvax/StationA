// SPDX-License-Identifier: AGPL-3.0-or-later

// Package rotctld exposes the satellite mount as a Hamlib rotctld TCP server
// on :4534, so any rotctl/gpredict client can drive both axes (plan U6).
//
// The wire dialect is a byte-copy of pelcobridge2's field-proven server
// (plan KTD10; pelcobridge2/internal/rotctld) — get commands print values one
// per line, set commands answer "RPRT 0", errors answer "RPRT -<n>";
// "\dump_state" speaks protocol v1 (version line, tag=value limits, "done")
// which gpredict's net-rotctl driver expects at open. server_test.go pins the
// replies byte-exactly — do not change them without re-verifying against
// hamlib's rotctl_parse.c / netrotctl.c.
//
// One deliberate divergence from pelcobridge2 (KTD9, grafted): this station
// has no arming gate, so -9 is not "disarmed" but the per-axis LIVENESS
// refusal — motion toward an axis whose device link is down answers
// RPRT -9 while the other axis still proceeds. A P command answers ONE RPRT
// for both axes (limit → -1, liveness → -9); the documented deterministic
// precedence for a mixed refusal is liveness-over-limit, pinned by test.
// Before the first successful readback a position query answers RPRT -11,
// never a fabricated position.
//
// The server owns NO cross-axis semantics (KTD12): it consumes the mount
// façade (internal/mount) for dispatch, stop and readback, and only maps its
// refusals onto the RPRT vocabulary.
package rotctld

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"spid-ercm-rotator-bridge/internal/config"
	"spid-ercm-rotator-bridge/internal/mount"
)

// Facade is the mount dispatch surface the server drives (KTD12: protocol
// servers consume the façade; cross-axis semantics live there, never here).
// *mount.Mount satisfies it as-is.
type Facade interface {
	// Goto admits one mount-level position intent and returns the refusals,
	// one per refused axis (empty = fully admitted).
	Goto(mount.Target) []mount.Refusal
	// Stop is the atomic all-stop on both axes; per-axis faults are returned
	// for the caller's reply mapping.
	Stop() []mount.AxisError
	// Readback returns the axis's cached position and its validity.
	Readback(mount.Axis) (float64, bool)
}

// Compile-time: the mount façade satisfies Facade without an adapter.
var _ Facade = (*mount.Mount)(nil)

// Limits is the travel envelope reported in dump_state — the same configured
// envelope the façade enforces for refusals (R10), so clients pre-validate
// against real limits.
type Limits struct {
	Model int // rot model advertised to clients (901 = NET_ROTCTL)
	MinAz float64
	MaxAz float64
	MinEl float64
	MaxEl float64
}

// LimitsFromControl maps the configured per-axis travel envelopes onto the
// dump_state envelope. This is the constructor the wiring (main/U5) calls
// with cfg.Control.
func LimitsFromControl(ctl config.ControlConfig) Limits {
	return Limits{
		Model: 901,
		MinAz: ctl.AZ.Min,
		MaxAz: ctl.AZ.Max,
		MinEl: ctl.EL.Min,
		MaxEl: ctl.EL.Max,
	}
}

// Server accepts any number of concurrent one-command-per-line TCP sessions;
// all of them funnel into the façade's per-axis dispatch workers.
type Server struct {
	facade Facade
	info   string
	limits Limits
	log    *slog.Logger

	ln      atomic.Pointer[net.Listener]
	clients atomic.Int64
}

// New builds the server over the mount façade. A zero Model defaults to 901
// (NET_ROTCTL, pelcobridge2 parity); a nil log silences refusal/fault
// logging.
func New(f Facade, info string, limits Limits, log *slog.Logger) *Server {
	if limits.Model == 0 {
		limits.Model = 901
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{facade: f, info: info, limits: limits, log: log}
}

// Clients is the number of currently connected rotctl clients.
func (s *Server) Clients() int { return int(s.clients.Load()) }

// ListenAndServe serves until ctx is cancelled or the listener fails.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln.Store(&ln)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go s.serveConn(conn)
	}
}

// Addr is the bound address (useful when listening on port 0 in tests).
func (s *Server) Addr() net.Addr {
	if p := s.ln.Load(); p != nil {
		return (*p).Addr()
	}
	return nil
}

func (s *Server) serveConn(conn net.Conn) {
	s.clients.Add(1)
	defer s.clients.Add(-1)
	defer conn.Close()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1024), 4096)
	for sc.Scan() {
		reply, closeConn := s.Handle(sc.Text())
		if reply != "" {
			if _, err := conn.Write([]byte(reply)); err != nil {
				return
			}
		}
		if closeConn {
			return
		}
	}
}

// Handle decodes one line of the net-rotator protocol and returns the reply
// (already newline-terminated; empty for nothing to send) plus whether to
// close the connection.
func (s *Server) Handle(line string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
		return "", false // hamlib: comments and empty lines say nothing
	}

	ext := false
	cmd := fields[0]
	if strings.HasPrefix(cmd, "+") { // extended responses
		ext = true
		cmd = cmd[1:]
	}
	cmd = strings.TrimPrefix(cmd, "\\")

	switch cmd {
	case "p", "get_pos":
		az, azOK := s.readback(mount.AZ)
		el, elOK := s.readback(mount.EL)
		// KTD9: pre-readback (or a timed-out read) answers RPRT -11 on
		// either axis — never a fabricated position.
		if !azOK || !elOK {
			return rprtLine(-11), false
		}
		return extWrap(ext, fmt.Sprintf("%.2f\n%.2f\n", az, el)), false

	case "P", "set_pos":
		if len(fields) < 3 {
			return rprtLine(-1), false
		}
		az, errAz := strconv.ParseFloat(strings.ReplaceAll(fields[1], ",", "."), 64)
		el, errEl := strconv.ParseFloat(strings.ReplaceAll(fields[2], ",", "."), 64)
		// ParseFloat accepts "nan"/"inf" without error; a non-finite target
		// is motion toward garbage. Refuse up front (KTD10).
		if errAz != nil || errEl != nil ||
			math.IsNaN(az) || math.IsInf(az, 0) ||
			math.IsNaN(el) || math.IsInf(el, 0) {
			return rprtLine(-1), false
		}
		// ONE façade Goto carrying both axes (R8: rotctld P always carries
		// both): one dispatch per axis, one RPRT for the command.
		refs, timedOut := s.gotoAxes(mount.Target{
			AZ:    az,
			EL:    el,
			HasAZ: true,
			HasEL: true,
		})
		if timedOut {
			return rprtLine(-6), false
		}
		if len(refs) > 0 {
			return rprtLine(s.rprtForRefusals(refs)), false
		}
		return rprtLine(0), false

	case "S", "stop":
		errs, timedOut := s.stopAxes()
		for _, ae := range errs {
			s.log.Warn("rotctld: stop frame fault", "axis", string(ae.Axis), "err", ae.Err)
		}
		if timedOut {
			s.log.Warn("rotctld: stop timed out")
		}
		// pelcobridge2 parity (KTD10): S always answers RPRT 0 — the dialect
		// defines no error code for a faulted stop frame, and the faults are
		// logged above.
		return rprtLine(0), false

	case "_", "get_info":
		return extWrap(ext, s.info+"\n"), false

	case "dump_state":
		return extWrap(ext, s.dumpState()), false

	case "q", "Q":
		return "", true

	default:
		return rprtLine(-4), false
	}
}

// callTimeout is the per-call round-trip bound (KTD10: 2 s, the pelcobridge2
// convention). The façade's admission paths are quick; the bound guards the
// reply path against a wedged serial section (a stop frame queued behind an
// in-flight write), so a client never hangs on a silent station.
const callTimeout = 2 * time.Second

// gotoAxes runs one façade Goto with the per-call bound.
func (s *Server) gotoAxes(t mount.Target) (refs []mount.Refusal, timedOut bool) {
	done := make(chan []mount.Refusal, 1)
	go func() { done <- s.facade.Goto(t) }()
	select {
	case refs = <-done:
	case <-time.After(callTimeout):
		timedOut = true
	}
	return refs, timedOut
}

// stopAxes runs the façade's atomic all-stop with the per-call bound.
func (s *Server) stopAxes() (errs []mount.AxisError, timedOut bool) {
	done := make(chan []mount.AxisError, 1)
	go func() { done <- s.facade.Stop() }()
	select {
	case errs = <-done:
	case <-time.After(callTimeout):
		timedOut = true
	}
	return errs, timedOut
}

// readback reads one axis's cached position with the per-call bound; a
// timed-out read is no usable readback (the -11 path).
func (s *Server) readback(ax mount.Axis) (deg float64, ok bool) {
	done := make(chan struct {
		deg float64
		ok  bool
	}, 1)
	go func() {
		deg, ok := s.facade.Readback(ax)
		done <- struct {
			deg float64
			ok  bool
		}{deg, ok}
	}()
	select {
	case r := <-done:
		return r.deg, r.ok
	case <-time.After(callTimeout):
		return 0, false
	}
}

// rprtForRefusals maps the façade's refusals onto the single RPRT a P command
// answers (KTD9), logging each refusal at Warn (a refused motion is station
// news the journalctl warning filter must see). Deterministic precedence for
// a mixed refusal, pinned by test: any liveness refusal wins (-9) over limit
// refusals (-1) — a dead axis is the station fault the operator must learn
// about, and a partially refused command already reads as failure to the
// client either way.
func (s *Server) rprtForRefusals(refs []mount.Refusal) int {
	for _, r := range refs {
		s.log.Warn("rotctld: target refused",
			"axis", string(r.Axis), "reason", r.Reason.String(), "err", r.Error())
	}
	for _, r := range refs {
		if r.Reason == mount.RefusalLiveness {
			return -9
		}
	}
	return -1
}

func rprtLine(n int) string { return fmt.Sprintf("RPRT %d\n", n) }

// extWrap prefixes a successful get-style reply with the extended-response
// RPRT 0 marker hamlib's "+" mode expects.
func extWrap(ext bool, body string) string {
	if ext {
		return rprtLine(0) + body
	}
	return body
}

func (s *Server) dumpState() string {
	var b strings.Builder
	fmt.Fprintf(&b, "1\n") // ROTCTLD_PROT_VER
	fmt.Fprintf(&b, "rot_model=%d\n", s.limits.Model)
	fmt.Fprintf(&b, "min_az=%f\n", s.limits.MinAz)
	fmt.Fprintf(&b, "max_az=%f\n", s.limits.MaxAz)
	fmt.Fprintf(&b, "min_el=%f\n", s.limits.MinEl)
	fmt.Fprintf(&b, "max_el=%f\n", s.limits.MaxEl)
	fmt.Fprintf(&b, "south_zero=0\n")
	fmt.Fprintf(&b, "rot_type=AzEl\n")
	fmt.Fprintf(&b, "done\n")
	return b.String()
}
