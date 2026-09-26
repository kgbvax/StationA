// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pstrotator is the shared PstRotator native UDP listener: the
// tag-tolerant datagram grammar and the UDP read loop, behind a small Handler
// interface. It is the grammar spid-ercm-rotator-bridge pinned (plan Appendix
// C, KTD11); beamsteer is the first consumer of this shared copy. Moving
// spid/wrc onto it is later de-boilerplating work.
//
// Grammar and precedence (parse-then-apply):
//
//		<PST><AZIMUTH>200</AZIMUTH><ELEVATION>45</ELEVATION></PST>  goto, per axis
//		<PST><STOP>1</STOP>…</PST>                                  stop wins
//		<PST><PARK>1</PARK></PST>                                   park
//		<PST>AZ?</PST> / <PST>EL?</PST>                              position query
//
//	  - Queries short-circuit the datagram: each present query is answered, and
//	    no motion tags in the same datagram are applied.
//	  - STOP beats PARK and AZIMUTH/ELEVATION in the same datagram (the
//	    manual's batched example; a halt is the safe reading).
//	  - PARK beats motion.
//	  - Motion carries only the axes PRESENT on the wire (HasAZ/HasEL).
//	  - Queries are answered to the source IP at listen-port+1. With no valid
//	    readback there is NO reply (never fabricate a position).
//	  - The parser is tag-tolerant: batched commands, case-insensitive tags,
//	    stray spaces inside and around tags, unknown tags ignored.
//
// Handler methods run on the single UDP read loop. They must not block — a
// wedged handler would starve every datagram behind it, including STOP.
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

// Axis names a rotator axis in a query.
type Axis string

const (
	AZ Axis = "az"
	EL Axis = "el"
)

// Datagram is one parsed UDP message: which commands the wire carries, and
// which axes a motion command carries (an absent tag sets no flag and no
// value).
type Datagram struct {
	AZQuery, ELQuery bool
	Stop, Park       bool
	AZ, EL           float64
	HasAZ, HasEL     bool
	Known            bool // any recognized tag matched
}

// Tag-tolerant grammar: case-insensitive, stray spaces tolerated inside and
// around every tag, unknown tags simply do not match.
var (
	azimuthRe   = regexp.MustCompile(`(?i)<\s*AZIMUTH\s*>\s*([-+]?\d+(?:\.\d+)?)\s*<\s*/\s*AZIMUTH\s*>`)
	elevationRe = regexp.MustCompile(`(?i)<\s*ELEVATION\s*>\s*([-+]?\d+(?:\.\d+)?)\s*<\s*/\s*ELEVATION\s*>`)
	stopRe      = regexp.MustCompile(`(?i)<\s*STOP\s*>[^<]*<\s*/\s*STOP\s*>`)
	parkRe      = regexp.MustCompile(`(?i)<\s*PARK\s*>[^<]*<\s*/\s*PARK\s*>`)
	// "AZ?" / "EL?" is tolerated both bare inside <PST>…</PST> and as a
	// <AZ?> tag; nothing else in the grammar contains "AZ?"/"EL?".
	azQueryRe = regexp.MustCompile(`(?i)<?\s*AZ\s*\?\s*>?`)
	elQueryRe = regexp.MustCompile(`(?i)<?\s*EL\s*\?\s*>?`)
)

// Parse extracts every command the message carries in one pass. Precedence
// between them is applied by the Server, not here.
func Parse(msg string) Datagram {
	var d Datagram
	if m := azimuthRe.FindStringSubmatch(msg); len(m) > 1 {
		if az, err := strconv.ParseFloat(m[1], 64); err == nil {
			d.AZ, d.HasAZ, d.Known = az, true, true
		}
	}
	if m := elevationRe.FindStringSubmatch(msg); len(m) > 1 {
		if el, err := strconv.ParseFloat(m[1], 64); err == nil {
			d.EL, d.HasEL, d.Known = el, true, true
		}
	}
	if stopRe.MatchString(msg) {
		d.Stop, d.Known = true, true
	}
	if parkRe.MatchString(msg) {
		d.Park, d.Known = true, true
	}
	if azQueryRe.MatchString(msg) {
		d.AZQuery, d.Known = true, true
	}
	if elQueryRe.MatchString(msg) {
		d.ELQuery, d.Known = true, true
	}
	return d
}

// Handler is what a Server drives. Every method must return promptly.
type Handler interface {
	// Goto receives a motion datagram; only the axes with HasAZ/HasEL set
	// are present on the wire.
	Goto(d Datagram)
	Stop()
	Park()
	// Readback returns the position to report for a query; ok=false means
	// no valid position — the query gets no reply.
	Readback(ax Axis) (deg float64, ok bool)
}

// ReplyFormat selects the query reply shape.
type ReplyFormat string

const (
	// ReplyManual is the PstRotator manual shape: "AZ:xxx.x\r" / "EL:yy.y\r".
	ReplyManual ReplyFormat = "manual"
	// ReplyXML is the shape wrc-rotator-bridge has always answered with:
	// "<PST><AZIMUTH>nnn</AZIMUTH></PST>" (integer degrees).
	ReplyXML ReplyFormat = "xml"
)

// Server is the PstRotator UDP listener.
type Server struct {
	Bind  string
	Port  int
	H     Handler
	Reply ReplyFormat // empty = ReplyManual
	Log   *slog.Logger

	listenPort int // the bound port (differs from Port when Port is 0)
}

func (s *Server) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return s.Log
}

// Run serves datagrams until ctx is done, then returns nil. A listen failure
// is returned so main can exit non-zero and let systemd restart the unit.
func (s *Server) Run(ctx context.Context) error {
	pc, err := s.Listen()
	if err != nil {
		return err
	}
	return s.Serve(ctx, pc)
}

// Listen binds the UDP socket. Split from Serve so tests can bind port 0 and
// read the chosen address.
func (s *Server) Listen() (net.PacketConn, error) {
	addr := net.JoinHostPort(s.Bind, strconv.Itoa(s.Port))
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("pstrotator listen %s: %w", addr, err)
	}
	s.log().Info("pstrotator UDP listener up", "addr", pc.LocalAddr().String())
	return pc, nil
}

// Serve reads datagrams from pc until ctx is done. It closes pc on return.
func (s *Server) Serve(ctx context.Context, pc net.PacketConn) error {
	s.listenPort = s.Port
	if u, ok := pc.LocalAddr().(*net.UDPAddr); ok {
		s.listenPort = u.Port
	}
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()
	buf := make([]byte, 2048)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.log().Warn("pstrotator UDP read error", "err", err)
			continue
		}
		s.handle(src, string(buf[:n]))
	}
}

// handle applies one datagram in the fixed precedence order:
// query → STOP → PARK → motion.
func (s *Server) handle(src net.Addr, msg string) {
	remote := src.String()
	d := Parse(msg)
	if !d.Known {
		s.log().Info("pstrotator unknown datagram ignored", "remote", remote, "msg", strings.TrimSpace(msg))
		return
	}
	if d.AZQuery || d.ELQuery {
		if d.AZQuery {
			s.answerQuery(src, remote, AZ)
		}
		if d.ELQuery {
			s.answerQuery(src, remote, EL)
		}
		return
	}
	if d.Stop {
		s.log().Info("pstrotator cmd", "remote", remote, "cmd", "stop")
		s.H.Stop()
		return
	}
	if d.Park {
		s.log().Info("pstrotator cmd", "remote", remote, "cmd", "park")
		s.H.Park()
		return
	}
	if d.HasAZ || d.HasEL {
		s.log().Info("pstrotator cmd", "remote", remote, "cmd", "goto",
			"az", d.AZ, "el", d.EL, "has_az", d.HasAZ, "has_el", d.HasEL)
		s.H.Goto(d)
	}
}

// FormatReply renders a query reply in the given format.
func FormatReply(f ReplyFormat, ax Axis, deg float64) string {
	if f == ReplyXML {
		tag := "AZIMUTH"
		if ax == EL {
			tag = "ELEVATION"
		}
		return fmt.Sprintf("<PST><%s>%d</%s></PST>", tag, int(deg+0.5), tag)
	}
	if ax == EL {
		return fmt.Sprintf("EL:%.1f\r", deg)
	}
	return fmt.Sprintf("AZ:%.1f\r", deg)
}

func (s *Server) answerQuery(src net.Addr, remote string, ax Axis) {
	deg, ok := s.H.Readback(ax)
	if !ok {
		s.log().Warn("pstrotator query without a valid readback; no reply sent",
			"remote", remote, "axis", string(ax))
		return
	}
	reply := FormatReply(s.Reply, ax, deg)
	s.log().Debug("pstrotator query reply", "remote", remote, "axis", string(ax), "reply", reply)
	if err := s.replyTo(src, reply); err != nil {
		s.log().Warn("pstrotator query reply failed", "remote", remote, "err", err)
	}
}

// replyTo sends one datagram to the source host at listen-port+1, the
// PstRotator reply-addressing convention.
func (s *Server) replyTo(src net.Addr, reply string) error {
	u, ok := src.(*net.UDPAddr)
	if !ok {
		return fmt.Errorf("source is not UDP: %T", src)
	}
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: u.IP, Port: s.listenPort + 1, Zone: u.Zone})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(reply))
	return err
}
