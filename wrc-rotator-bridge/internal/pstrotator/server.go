// Package pstrotator implements a PSTRotator-compatible UDP listener so
// PSTRotator (YO3DMU) can drive the HF rotator directly. It is a parallel
// control path orthogonal to the MQTT three-plane contract: it drives the same
// device the bridge does, and the resulting motion still surfaces in /state.
//
// PSTRotator sends XML datagrams over UDP. Supported payloads:
//
//	<PST><AZIMUTH>180</AZIMUTH></PST>              rotate to azimuth
//	<PST><AZIMUTH>180</AZIMUTH><ELEVATION>45</ELEVATION></PST>  az only; el ignored
//	<PST><STOP>1</STOP></PST>                       halt motion
//	<PST><PARK>1</PARK></PST>                       logged and ignored
//	<PST>AZ?</PST>                                  position query; reply on port+1
//
// Elevation is ignored because the station rotator is azimuth-only.
package pstrotator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// Controller is the rotator surface the UDP server drives. It is a subset of
// rotor.Commander plus a current-azimuth reader, identical to gs232.Controller.
type Controller interface {
	SetAz(az float64) error
	Stop() error
	CurrentAz() float64
}

// Server listens for PSTRotator UDP datagrams and translates them into calls
// on a Controller.
type Server struct {
	bind string
	port int
	ctrl Controller
	log  *slog.Logger
}

// New constructs a Server. bind:port is the UDP listen address; ctrl is the
// rotator.
func New(bind string, port int, ctrl Controller, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{bind: bind, port: port, ctrl: ctrl, log: log}
}

// Run reads datagrams until ctx is cancelled. The UDP socket is closed when ctx
// is done so ReadFrom unblocks.
func (s *Server) Run(ctx context.Context) error {
	addr := net.JoinHostPort(s.bind, strconv.Itoa(s.port))
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("pstrotator listen %s: %w", addr, err)
	}
	s.log.Info("PSTRotator UDP listener", "addr", addr)

	// Close the socket when ctx is cancelled so ReadFrom unblocks.
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()

	buf := make([]byte, 1024)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.log.Warn("PSTRotator UDP read error", "err", err)
			continue
		}
		s.handle(pc, src, string(buf[:n]))
	}
}

// command patterns. PSTRotator uses plain XML; case-insensitive matching keeps us
// tolerant of firmware or user variants.
var (
	azimuthRe = regexp.MustCompile(`(?i)<AZIMUTH>\s*(-?\d+(?:\.\d+)?)\s*</AZIMUTH>`)
	stopRe    = regexp.MustCompile(`(?i)<STOP>\s*[^<]*\s*</STOP>`)
	parkRe    = regexp.MustCompile(`(?i)<PARK>\s*[^<]*\s*</PARK>`)
)

func (s *Server) handle(pc net.PacketConn, src net.Addr, msg string) {
	remote := src.String()

	// Position query: PSTRotator convention sends reply to source IP on port+1.
	if strings.Contains(strings.ToUpper(msg), "AZ?") {
		az := int(s.ctrl.CurrentAz())
		reply := fmt.Sprintf("<PST><AZIMUTH>%d</AZIMUTH></PST>", az)
		s.log.Debug("PSTRotator query", "remote", remote, "reply", reply)
		if err := s.replyTo(src, reply); err != nil {
			s.log.Warn("PSTRotator query reply failed", "remote", remote, "err", err)
		}
		return
	}

	// Stop takes precedence over any embedded azimuth value.
	if stopRe.MatchString(msg) {
		s.log.Info("PSTRotator cmd", "remote", remote, "cmd", "stop")
		if err := s.ctrl.Stop(); err != nil {
			s.log.Warn("PSTRotator stop failed", "err", err)
		}
		return
	}

	if parkRe.MatchString(msg) {
		s.log.Info("PSTRotator cmd", "remote", remote, "cmd", "park", "note", "ignored (no park support)")
		return
	}

	if m := azimuthRe.FindStringSubmatch(msg); len(m) > 1 {
		az, _ := strconv.ParseFloat(m[1], 64)
		s.log.Info("PSTRotator cmd", "remote", remote, "cmd", "set_az", "az", az)
		if err := s.ctrl.SetAz(az); err != nil {
			s.log.Warn("PSTRotator set_az failed", "err", err)
		}
		return
	}

	s.log.Info("PSTRotator unknown", "remote", remote, "msg", msg)
}

// replyTo sends a response datagram to the source host on port+1, following
// the PSTRotator protocol convention for position replies.
func (s *Server) replyTo(src net.Addr, reply string) error {
	udpAddr, ok := src.(*net.UDPAddr)
	if !ok {
		return fmt.Errorf("source is not UDP: %T", src)
	}
	replyAddr := &net.UDPAddr{
		IP:   udpAddr.IP,
		Port: s.port + 1,
		Zone: udpAddr.Zone,
	}
	conn, err := net.DialUDP("udp", nil, replyAddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(reply))
	return err
}
