// Package gs232 implements a Yaesu GS-232B-compatible TCP server so legacy
// rotator-control software (PSTRotator, N1MM, rotctld clients, …) can drive the
// HF rotator directly. It is an optional control path orthogonal to the MQTT
// three-plane contract: it talks to the same device the bridge does, and the
// resulting motion still surfaces in /state on the bus.
//
// Supported subset of GS-232B:
//
//	C | C2          query → "+0aaa+0000\r" (aaa = current azimuth, eee = 0000)
//	Mxxx            move to azimuth xxx
//	Wxxx yyy        set azimuth xxx (elevation yyy ignored — azimuth-only)
//	S               stop
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
)

// Controller is the rotator surface the GS-232 server drives. It is a subset of
// rotor.Commander plus a current-azimuth reader.
type Controller interface {
	SetAz(az float64) error
	Stop() error
	CurrentAz() float64
}

// Server listens for GS-232 clients and translates their commands into calls on
// a Controller.
type Server struct {
	bind string
	port int
	ctrl Controller
	log  *slog.Logger
}

// New constructs a Server. bind:port is the listen address; ctrl is the rotator.
func New(bind string, port int, ctrl Controller, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{bind: bind, port: port, ctrl: ctrl, log: log}
}

// Run accepts connections until ctx is cancelled. The listen socket is closed
// when ctx is done.
func (s *Server) Run(ctx context.Context) error {
	addr := net.JoinHostPort(s.bind, strconv.Itoa(s.port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gs232 listen %s: %w", addr, err)
	}
	s.log.Info("GS-232B server listening", "addr", addr)

	// Close the listener when ctx is cancelled so Accept unblocks.
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
			s.log.Warn("GS-232 accept error", "err", err)
			continue
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	s.log.Info("GS-232 client connected", "remote", remote)

	reader := bufio.NewReader(conn)

	// Mxxx (Move) and Wxxx yyy (Set). Capture the azimuth digits.
	moveRegex := regexp.MustCompile(`^M(\d{1,3})`)
	setRegex := regexp.MustCompile(`^W(\d{1,3})\s+\d+`)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		rawLine, err := readTerminatedLine(reader)
		if err != nil {
			if err != io.EOF {
				s.log.Warn("GS-232 read error", "remote", remote, "err", err)
			}
			return
		}

		line := strings.TrimSpace(strings.ToUpper(rawLine))
		if len(line) == 0 {
			continue
		}

		switch {
		case line == "C" || line == "C2":
			az := int(s.ctrl.CurrentAz())
			// GS-232B standard format: +0aaa+0eee (azimuth, elevation).
			fmt.Fprintf(conn, "+0%03d+0000\r", az)

		case line == "S":
			s.log.Info("GS-232 cmd", "remote", remote, "cmd", "STOP")
			if err := s.ctrl.Stop(); err != nil {
				s.log.Warn("GS-232 stop failed", "err", err)
			}
			fmt.Fprint(conn, "\r")

		case strings.HasPrefix(line, "M"):
			if m := moveRegex.FindStringSubmatch(line); len(m) > 1 {
				az, _ := strconv.ParseFloat(m[1], 64)
				s.log.Info("GS-232 cmd", "remote", remote, "cmd", "move", "az", az)
				if err := s.ctrl.SetAz(az); err != nil {
					s.log.Warn("GS-232 move failed", "err", err)
				}
				fmt.Fprint(conn, "\r")
			}

		case strings.HasPrefix(line, "W"):
			if m := setRegex.FindStringSubmatch(line); len(m) > 1 {
				az, _ := strconv.ParseFloat(m[1], 64)
				s.log.Info("GS-232 cmd", "remote", remote, "cmd", "set", "az", az)
				if err := s.ctrl.SetAz(az); err != nil {
					s.log.Warn("GS-232 set failed", "err", err)
				}
				fmt.Fprint(conn, "\r")
			}

		default:
			s.log.Info("GS-232 unknown", "remote", remote, "line", line)
			fmt.Fprint(conn, "?>\r")
		}
	}
}

// readTerminatedLine reads bytes until \r or \n is found (the GS-232
// terminators). It does NOT try to consume a \n following a \r: that would
// require a blocking Peek that deadlocks a client which sends "C\r" then waits
// for the reply. A leftover \n (from a \r\n-terminated command) is simply read
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
