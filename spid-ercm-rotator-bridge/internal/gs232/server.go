// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gs232 implements a Yaesu GS-232B-compatible TCP server so legacy
// rotator-control software (PSTRotator, N1MM, …) can drive the sat-ops
// az/el mount directly — the wrc-rotator-bridge precedent (where this same
// server fronts the HF rotor), extended to elevation: this mount carries
// BOTH axes. It is an optional control path orthogonal to the MQTT
// three-plane contract: it consumes only the mount façade, and the resulting
// motion surfaces in /state exactly like bus-driven motion (R4).
//
// Supported subset of GS-232B:
//
//	C | C2          query → "+0aaa+0eee\r" (azimuth, elevation; an axis
//	                without a valid readback yet reports 000)
//	Waaa eee        goto azimuth aaa AND elevation eee (both axes)
//	Maaa            goto azimuth aaa only
//	S | SA | SE     stop (the façade halts both axes — the safe reading of
//	                any stop spelling for a legacy client)
//	anything else   "?>\r"
package gs232

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"

	"spid-ercm-rotator-bridge/internal/config"
	"spid-ercm-rotator-bridge/internal/mount"
)

// Mount is the slice of the mount façade the GS-232 server drives — the same
// pipeline /cmd and the other protocol servers feed (KTD12).
type Mount interface {
	Goto(mount.Target) []mount.Refusal
	Stop() []mount.AxisError
	Readback(ax mount.Axis) (float64, bool)
}

// Server listens for GS-232 clients and translates their commands into façade
// calls.
type Server struct {
	cfg config.GS232Config
	m   Mount
	log *slog.Logger
}

// New constructs a Server from its endpoint config and the mount façade.
func New(cfg config.GS232Config, m Mount, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{cfg: cfg, m: m, log: log}
}

// Run accepts connections until ctx is cancelled. The listen socket is closed
// when ctx is done.
func (s *Server) Run(ctx context.Context) error {
	addr := net.JoinHostPort(s.cfg.Bind, strconv.Itoa(s.cfg.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gs232 listen %s: %w", addr, err)
	}
	s.log.Info("gs232 TCP server up", "addr", addr)

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.log.Warn("gs232 accept error", "err", err)
			continue
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	s.log.Info("gs232 client connected", "remote", remote)

	reader := bufio.NewReader(conn)

	// Waaa eee (goto both) and Maaa (goto az). Capture the digit groups.
	wRegex := regexp.MustCompile(`^W(\d{1,3})\s+(\d{1,3})`)
	mRegex := regexp.MustCompile(`^M(\d{1,3})`)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		rawLine, err := readTerminatedLine(reader)
		if err != nil {
			if err != io.EOF {
				s.log.Warn("gs232 read error", "remote", remote, "err", err)
			}
			return
		}

		line := strings.TrimSpace(strings.ToUpper(rawLine))
		if len(line) == 0 {
			continue
		}

		switch {
		case line == "C" || line == "C2":
			az, azOK := s.m.Readback(mount.AZ)
			el, elOK := s.m.Readback(mount.EL)
			if !azOK {
				az = 0 // no readback yet: the placeholder stays 000, never a guess
			}
			if !elOK {
				el = 0
			}
			fmt.Fprintf(conn, "+0%03d+0%03d\r", int(az), int(el))

		case strings.HasPrefix(line, "W"):
			if m := wRegex.FindStringSubmatch(line); len(m) > 2 {
				az, _ := strconv.ParseFloat(m[1], 64)
				el, _ := strconv.ParseFloat(m[2], 64)
				s.log.Info("gs232 cmd", "remote", remote, "cmd", "goto", "az", az, "el", el)
				s.dispatch(mount.Target{AZ: az, EL: el, HasAZ: true, HasEL: true}, remote)
				fmt.Fprint(conn, "\r")
			} else {
				fmt.Fprint(conn, "?>\r")
			}

		case strings.HasPrefix(line, "M"):
			if m := mRegex.FindStringSubmatch(line); len(m) > 1 {
				az, _ := strconv.ParseFloat(m[1], 64)
				s.log.Info("gs232 cmd", "remote", remote, "cmd", "goto az", "az", az)
				s.dispatch(mount.Target{AZ: az, HasAZ: true}, remote)
				fmt.Fprint(conn, "\r")
			} else {
				fmt.Fprint(conn, "?>\r")
			}

		case line == "S" || line == "SA" || line == "SE":
			// All stop spellings halt BOTH axes: a legacy client's stop must
			// never leave the other axis running on a mount this small.
			s.log.Info("gs232 cmd", "remote", remote, "cmd", "stop")
			for _, ae := range s.m.Stop() {
				s.log.Warn("gs232 stop refused", "axis", ae.Axis, "err", ae.Err)
			}
			fmt.Fprint(conn, "\r")

		default:
			s.log.Info("gs232 unknown", "remote", remote, "line", line)
			fmt.Fprint(conn, "?>\r")
		}
	}
}

// dispatch feeds one goto to the façade and logs the per-axis refusals — the
// GS-232 wire has no error reply contract, so the log is the operator
// surface (same as the PstRotator path).
func (s *Server) dispatch(t mount.Target, remote string) {
	for _, r := range s.m.Goto(t) {
		s.log.Warn("gs232 goto refused", "remote", remote, "axis", r.Axis, "reason", r.Reason, "target", r.Target)
	}
}

// readTerminatedLine reads bytes until \r or \n is found (the GS-232
// terminators). It does NOT try to consume a \n following a \r: that would
// require a blocking Peek that deadlocks a client which sends "C\r" then
// waits for the reply. A leftover \n (from \r\n-terminated commands) is read
// on the next pass as an empty line, which the caller skips.
func readTerminatedLine(r *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		b, err := r.ReadByte()
		if err != nil {
			return sb.String(), err
		}
		if b == '\r' || b == '\n' {
			return sb.String(), nil
		}
		sb.WriteByte(b)
	}
}
