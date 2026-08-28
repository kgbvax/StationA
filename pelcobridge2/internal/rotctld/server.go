// Package rotctld exposes the engine as a Hamlib rotctld TCP server, so any
// rotctl/gpredict client can drive the rotator — once the operator has armed
// it from the TUI (a set_pos while disarmed answers RPRT -9; no code path
// exists for a client to arm).
//
// Wire behaviour follows hamlib's rotctl_parse.c/netrotctl.c: get commands
// print values one per line, set commands answer "RPRT 0", errors answer
// "RPRT -<n>"; "\dump_state" speaks protocol v1 (version line, tag=value
// limits, "done") which gpredict's net-rotctl driver expects at open.
package rotctld

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"pelcobridge2/internal/control"
)

// Rot is the engine surface the server drives.
type Rot interface {
	Call(ctx context.Context, from control.Source, it control.Intent) control.Result
}

// Limits is the travel envelope reported in dump_state.
type Limits struct {
	Model int // rot model advertised to clients (901 = NET_ROTCTL)
	MinAz float64
	MaxAz float64
	MinEl float64
	MaxEl float64
}

// DefaultLimits is the head's travel: full azimuth circle, 0–90 elevation.
func DefaultLimits() Limits {
	return Limits{Model: 901, MinAz: 0, MaxAz: 360, MinEl: 0, MaxEl: 90}
}

// Server accepts any number of concurrent clients; all of them funnel through
// the engine's one-goroutine queue.
type Server struct {
	rot    Rot
	info   string
	limits Limits

	ln      atomic.Pointer[net.Listener]
	clients atomic.Int64
}

func New(rot Rot, info string, limits Limits) *Server {
	if limits.Model == 0 {
		limits.Model = 901
	}
	return &Server{rot: rot, info: info, limits: limits}
}

// Clients is the number of currently connected rotctl clients (for the /state
// snapshot and the TUI header).
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
		az, el, rprt := s.getPos()
		if rprt != 0 {
			return rprtLine(rprt), false
		}
		return extWrap(ext, fmt.Sprintf("%.2f\n%.2f\n", az, el)), false

	case "P", "set_pos":
		if len(fields) < 3 {
			return "RPRT -1\n", false
		}
		az, errAz := strconv.ParseFloat(strings.ReplaceAll(fields[1], ",", "."), 64)
		el, errEl := strconv.ParseFloat(strings.ReplaceAll(fields[2], ",", "."), 64)
		// ParseFloat accepts "nan"/"inf" without error; DegToWord would park
		// both at 0° — real motion from a garbage target. Refuse up front.
		if errAz != nil || errEl != nil ||
			math.IsNaN(az) || math.IsInf(az, 0) ||
			math.IsNaN(el) || math.IsInf(el, 0) {
			return "RPRT -1\n", false
		}
		if r := s.call(control.SetPanIntent{Deg: az}); r.Err != nil {
			return rprtLine(rprtForSet(r.Err)), false
		}
		if r := s.call(control.SetTiltIntent{Deg: el}); r.Err != nil {
			return rprtLine(rprtForSet(r.Err)), false
		}
		return "RPRT 0\n", false

	case "S", "stop":
		s.call(control.StopIntent{})
		return "RPRT 0\n", false

	case "_", "get_info":
		return extWrap(ext, s.info+"\n"), false

	case "dump_state":
		return extWrap(ext, s.dumpState()), false

	case "q", "Q":
		return "", true

	default:
		return "RPRT -4\n", false
	}
}

// call is one engine round-trip with the rotctld convention: a 2 s timeout.
func (s *Server) call(it control.Intent) control.Result {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.rot.Call(ctx, control.SrcRotctld, it)
}

func (s *Server) getPos() (az, el float64, rprt int) {
	r := s.call(control.QueryPanIntent{})
	if r.Err != nil {
		return 0, 0, -11 // no usable readback (moving counts: garbage while moving)
	}
	az = r.Deg
	r = s.call(control.QueryTiltIntent{})
	if r.Err != nil {
		return 0, 0, -11
	}
	return az, r.Deg, 0
}

// rprtForSet maps engine refusals onto hamlib rotator error codes: a disarmed
// set is RIG_ERJCTED (-9); timeouts are RIG_ERTIMEOUT (-6); anything else is
// treated as invalid (-1).
func rprtForSet(err error) int {
	switch {
	case errors.Is(err, control.ErrDisarmed):
		return -9
	case errors.Is(err, context.DeadlineExceeded):
		return -6
	default:
		return -1
	}
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
