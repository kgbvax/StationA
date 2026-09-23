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
// The listener is flood-resistant, not authenticated — the no-auth posture is
// session-settled (KTD4). Transient accept errors are retried instead of
// crashing the whole compound bridge, sessions are capped at 16 and silently
// idle ones are reaped by a per-command read deadline (F6).
//
// The server owns NO cross-axis semantics (KTD12): it consumes the mount
// façade (internal/mount) for dispatch, stop and readback, and only maps its
// refusals onto the RPRT vocabulary.
package rotctld

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
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

	// Flood-resistance knobs (F6), defaulted in New and plain fields so
	// tests shrink them per instance — no package-level state to race with
	// a still-draining earlier server. listen is the seam the accept-retry
	// tests use to script Accept errors; production always gets net.Listen.
	listen           func(network, addr string) (net.Listener, error)
	maxClients       int
	acceptRetryPause time.Duration
	readIdleTimeout  time.Duration

	ln      atomic.Pointer[net.Listener]
	clients atomic.Int64
}

// The F6 defaults: a session flood can neither crash the bridge (transient
// accept errors retry) nor grow it unboundedly (16-session cap, idle reaped).
const (
	defaultMaxClients    = 16
	defaultAcceptPause   = 100 * time.Millisecond
	defaultReadIdleGrace = 60 * time.Second
)

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
	return &Server{
		facade:           f,
		info:             info,
		limits:           limits,
		log:              log,
		listen:           net.Listen,
		maxClients:       defaultMaxClients,
		acceptRetryPause: defaultAcceptPause,
		readIdleTimeout:  defaultReadIdleGrace,
	}
}

// Clients is the number of currently connected rotctl clients.
func (s *Server) Clients() int { return int(s.clients.Load()) }

// ListenAndServe serves until ctx is cancelled or the listener fails
// permanently. Transient accept errors are retried (F6): one fatal Accept
// would crash-loop the whole compound bridge — both MQTT slots, the UDP
// listener and the /cmd e-stop path all live in this one process.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := s.listen("tcp", addr)
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
			if transientAcceptError(err) {
				s.log.Warn("rotctld: transient accept error; retrying", "err", err)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(s.acceptRetryPause):
				}
				continue
			}
			return err
		}
		if s.clients.Load() >= int64(s.maxClients) {
			// F6: accept-then-close keeps the flood from wedging the loop;
			// existing sessions are never disturbed.
			s.log.Warn("rotctld: session cap reached; connection closed",
				"clients", s.maxClients)
			_ = conn.Close()
			continue
		}
		go s.serveConn(conn)
	}
}

// transientAcceptError classifies an Accept error as a temporary resource
// hiccup the accept loop should retry (F6): connections aborted mid-handshake
// (ECONNABORTED) and fd exhaustion (EMFILE/ENFILE), plus anything still
// carrying the net package's legacy Temporary verdict. Everything else — a
// closed or broken listener, a bad bind — is permanent and stays fatal.
func transientAcceptError(err error) bool {
	if errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE) {
		return true
	}
	var t interface{ Temporary() bool }
	return errors.As(err, &t) && t.Temporary()
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

	// Ingress parity with the gs232/pstrotator/MQTT paths: a connect and
	// every motion command log at Info. Before this, a rotctld-driven slew
	// left no journal trace at Info at all — the 2026-09-23 incident (a
	// client repositioning the mount invisibly) was diagnosed only by
	// packet sniffing. Polls (`p`) stay silent: gpredict polls at ~1 Hz and
	// would drown the journal.
	remote := conn.RemoteAddr().String()
	s.log.Info("rotctld: client connected", "remote", remote)

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1024), 4096)
	for {
		// F6: one read deadline per command line, extended on every read —
		// a silent client is reaped when its idle grace lapses (the reap
		// itself is expected and not logged).
		_ = conn.SetReadDeadline(time.Now().Add(s.readIdleTimeout))
		if !sc.Scan() {
			if err := sc.Err(); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
				s.log.Debug("rotctld: session read error",
					"remote", remote, "err", err)
			}
			return
		}
		s.logCmd(remote, sc.Text())
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

// logCmd surfaces the motion commands at Info, keyed to the client that sent
// them (the gs232 "cmd" log shape). Only the wire-visible actions matter:
// P/S move or halt hardware, everything else is protocol chatter.
func (s *Server) logCmd(remote, line string) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return
	}
	switch f[0] {
	case "P", "+P":
		if len(f) >= 3 {
			s.log.Info("rotctld: cmd", "remote", remote, "cmd", "goto", "az", f[1], "el", f[2])
		} else {
			s.log.Info("rotctld: cmd", "remote", remote, "cmd", "goto", "malformed", line)
		}
	case "S", "+S":
		s.log.Info("rotctld: cmd", "remote", remote, "cmd", "stop")
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

// callBounded runs f on a fresh goroutine bounded by callTimeout and reports
// whether the call completed in time (the single per-call timeout policy,
// F28 — the bound used to be triplicated across goto/stop/readback). On
// expiry the call is abandoned to its goroutine but never dropped (F13): a
// waiter goroutine drains the eventual result and hands it to onLate, which
// Warn-logs the late completion — a timed-out Goto can still admit motion
// after the client was already answered RPRT -6, and its refusals must
// surface instead of being silently discarded.
func callBounded[T any](f func() T, onLate func(v T)) (v T, ok bool) {
	done := make(chan T, 1)
	go func() { done <- f() }()
	select {
	case v = <-done:
		return v, true
	case <-time.After(callTimeout):
		if onLate != nil {
			go func() { onLate(<-done) }()
		} else {
			go func() { <-done }() // drain so the abandoned call's goroutine ends
		}
		return v, false
	}
}

// gotoAxes runs one façade Goto with the per-call bound. A timed-out goto
// answers RPRT -6 immediately, but the abandoned admission may still complete
// and admit the axes — the late completion and its refusals are Warn-logged
// by the waiter (F13), never silently discarded.
func (s *Server) gotoAxes(t mount.Target) (refs []mount.Refusal, timedOut bool) {
	refs, ok := callBounded(
		func() []mount.Refusal { return s.facade.Goto(t) },
		func(refs []mount.Refusal) {
			s.log.Warn("rotctld: timed-out goto completed late; motion may have been admitted after RPRT -6",
				"refused_axes", len(refs))
			for _, r := range refs {
				s.log.Warn("rotctld: late goto refusal",
					"axis", string(r.Axis), "reason", r.Reason.String(), "err", r.Error())
			}
		})
	return refs, !ok
}

// stopAxes runs the façade's atomic all-stop with the per-call bound. A
// timed-out stop still answers RPRT 0 (Handle Warns the expiry); the
// abandoned call's faults are Warn-logged by the waiter when it lands (F13).
func (s *Server) stopAxes() (errs []mount.AxisError, timedOut bool) {
	errs, ok := callBounded(
		func() []mount.AxisError { return s.facade.Stop() },
		func(errs []mount.AxisError) {
			s.log.Warn("rotctld: timed-out stop completed late")
			for _, ae := range errs {
				s.log.Warn("rotctld: late stop frame fault",
					"axis", string(ae.Axis), "err", ae.Err)
			}
		})
	return errs, !ok
}

// readbackResult lets Readback's two results ride the generic bound (F28).
type readbackResult struct {
	deg float64
	ok  bool
}

// readback reads one axis's cached position with the per-call bound; a
// timed-out read is no usable readback (the -11 path). A late readback is
// stale by the time it lands — nothing left to surface.
func (s *Server) readback(ax mount.Axis) (deg float64, ok bool) {
	r, _ := callBounded(
		func() readbackResult {
			deg, ok := s.facade.Readback(ax)
			return readbackResult{deg: deg, ok: ok}
		},
		nil)
	return r.deg, r.ok
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
