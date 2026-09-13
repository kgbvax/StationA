// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pstrotator implements the PstRotator native UDP listener (plan U7):
// the datagram grammar pinned in the plan's Appendix C (KTD11) served on the
// configured port (default 12041), with elevation honored — unlike the
// wrc-rotator-bridge precedent, whose HF rotator is azimuth-only.
//
// Grammar and precedence (parse-then-apply, the wrc server.go shape):
//
//		<PST><AZIMUTH>200</AZIMUTH><ELEVATION>45</ELEVATION></PST>  goto, per axis
//		<PST><STOP>1</STOP>…</PST>                                  stop wins
//		<PST><PARK>1</PARK></PST>                                   park both axes
//		<PST>AZ?</PST> / <PST>EL?</PST>                              position query
//
//	  - Dispatch is by axis (R8): a datagram dispatches only the axes whose
//	    tags are PRESENT — an azimuth-only datagram never constructs an el
//	    intent (AE2).
//	  - STOP halts both axes and beats AZIMUTH/ELEVATION/PARK tags in the same
//	    datagram (the manual's batched-command example; wrc stop precedence).
//	    STOP also beats PARK: a sender batching both gets a pure halt, which is
//	    the safe reading of an ambiguous datagram.
//	  - PARK is one atomic façade call (mount.Park): its stop phase cancels
//	    queues, then both configured park targets dispatch through the normal
//	    paths — the façade guarantees no self-suppression (KTD8/KTD11).
//	  - AZ?/EL? reply to the source IP at listen-port+1 with the pinned manual
//	    strings "AZ:xxx.x\r" / "EL:yy.y\r" (one decimal, trailing CR — KTD11
//	    defaults to the manual shape; the bench pass reconciles it against the
//	    shack's installed PstRotator version).
//	  - With no valid cached readback a query gets NO reply, and a Warn log:
//	    KTD9 forbids fabricating a position, and the grammar documents no
//	    error reply for queries. This choice is flagged for the bench
//	    reconciliation pass.
//	  - Motion datagrams get no reply at all (Appendix C: documented replies
//	    exist only for the ? queries and OK-style commands).
//	  - Refusals (R9 liveness, R10 limits) are LOGGED at Warn with the
//	    axis/reason/target attrs — the PstRotator path has no reply contract
//	    for refusals, so the log is the operator surface.
//	  - The parser is tag-tolerant: batched commands, case-insensitive tags,
//	    stray spaces inside and around tags, unknown tags ignored.
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

	"spid-ercm-rotator-bridge/internal/config"
	"spid-ercm-rotator-bridge/internal/mount"
)

// Mount is the slice of the mount façade (plan U4, KTD12) this server
// consumes. The server translates datagrams into façade calls only — every
// cross-axis semantic (stop epoch, refusal aggregation, park dispatch,
// deadband) lives in the façade and is never re-implemented here.
type Mount interface {
	Goto(mount.Target) []mount.Refusal
	Stop() []mount.AxisError
	Park() []mount.Refusal
	Readback(ax mount.Axis) (float64, bool)
}

// The real façade satisfies Mount as-is — no adapter layer (KTD12).
var _ Mount = (*mount.Mount)(nil)

// Server is the PstRotator native UDP listener.
type Server struct {
	cfg config.PstRotatorConfig
	m   Mount
	log *slog.Logger
}

// New constructs the listener. cfg carries the bind address and listen port
// (query replies go to the source IP at listen-port+1); m is the mount
// façade; log is a child of the component root logger (the constant
// component attr comes from the caller, per docs/conventions/logging.md).
func New(cfg config.PstRotatorConfig, m Mount, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{cfg: cfg, m: m, log: log}
}

// Run serves datagrams until ctx is done, then returns. Listen failure is the
// returned error so main can exit non-zero and let systemd restart the unit.
func (s *Server) Run(ctx context.Context) error {
	addr := net.JoinHostPort(s.cfg.Bind, strconv.Itoa(s.cfg.Port))
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("pstrotator listen %s: %w", addr, err)
	}
	s.log.Info("pstrotator UDP listener up", "addr", addr)

	// Close the socket on ctx done so ReadFrom unblocks.
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
			s.log.Warn("pstrotator UDP read error", "err", err)
			continue
		}
		s.handle(pc, src, string(buf[:n]))
	}
}

// The grammar (Appendix C). Tag-tolerant: case-insensitive, stray spaces
// tolerated inside and around every tag, unknown tags simply do not match.
var (
	azimuthRe   = regexp.MustCompile(`(?i)<\s*AZIMUTH\s*>\s*([-+]?\d+(?:\.\d+)?)\s*<\s*/\s*AZIMUTH\s*>`)
	elevationRe = regexp.MustCompile(`(?i)<\s*ELEVATION\s*>\s*([-+]?\d+(?:\.\d+)?)\s*<\s*/\s*ELEVATION\s*>`)
	stopRe      = regexp.MustCompile(`(?i)<\s*STOP\s*>[^<]*<\s*/\s*STOP\s*>`)
	parkRe      = regexp.MustCompile(`(?i)<\s*PARK\s*>[^<]*<\s*/\s*PARK\s*>`)
	// The query wire text is "AZ?" / "EL?" (Appendix C) — tolerated both
	// bare inside <PST>…</PST> and in a <AZ?> tag (the manual is read both
	// ways in the field; nothing else in the grammar contains "AZ?"/"EL?").
	azQueryRe = regexp.MustCompile(`(?i)<?\s*AZ\s*\?\s*>?`)
	elQueryRe = regexp.MustCompile(`(?i)<?\s*EL\s*\?\s*>?`)
)

// datagram is one parsed UDP message: which commands the wire carries, and
// which axes a motion command carries (R8's structural-omission selector —
// a tag that is absent sets no flag and no value).
type datagram struct {
	azQuery, elQuery bool
	stop, park       bool
	az, el           float64
	hasAZ, hasEL     bool
	known            bool // any recognized tag matched
}

// parseDatagram extracts every command the message carries in one pass — the
// precedence between them is applied afterwards, in handle.
func parseDatagram(msg string) datagram {
	var d datagram
	if m := azimuthRe.FindStringSubmatch(msg); len(m) > 1 {
		if az, err := strconv.ParseFloat(m[1], 64); err == nil {
			d.az, d.hasAZ, d.known = az, true, true
		}
	}
	if m := elevationRe.FindStringSubmatch(msg); len(m) > 1 {
		if el, err := strconv.ParseFloat(m[1], 64); err == nil {
			d.el, d.hasEL, d.known = el, true, true
		}
	}
	if stopRe.MatchString(msg) {
		d.stop, d.known = true, true
	}
	if parkRe.MatchString(msg) {
		d.park, d.known = true, true
	}
	if azQueryRe.MatchString(msg) {
		d.azQuery, d.known = true, true
	}
	if elQueryRe.MatchString(msg) {
		d.elQuery, d.known = true, true
	}
	return d
}

// handle applies one datagram's commands in the fixed precedence order:
// query (reply, nothing else) → STOP → PARK → motion (present axes only).
func (s *Server) handle(pc net.PacketConn, src net.Addr, msg string) {
	remote := src.String()
	d := parseDatagram(msg)
	if !d.known {
		s.log.Info("pstrotator unknown datagram ignored", "remote", remote, "msg", strings.TrimSpace(msg))
		return
	}

	// Queries short-circuit the datagram (wrc precedent): each present query
	// is answered, and no motion tags in the same datagram are applied.
	if d.azQuery || d.elQuery {
		if d.azQuery {
			s.answerQuery(src, remote, mount.AZ)
		}
		if d.elQuery {
			s.answerQuery(src, remote, mount.EL)
		}
		return
	}

	// STOP beats motion and park tags in the same datagram: a halt is the
	// safe reading of an ambiguous batch (the manual's own example batches
	// STOP with AZIMUTH).
	if d.stop {
		s.log.Info("pstrotator cmd", "remote", remote, "cmd", "stop",
			"precedes_motion", d.hasAZ || d.hasEL || d.park)
		for _, ae := range s.m.Stop() {
			s.log.Warn("pstrotator stop fault", "remote", remote,
				"axis", string(ae.Axis), "err", ae.Err)
		}
		return
	}

	// PARK is one atomic façade intent: stop phase, then both park targets
	// through the normal dispatch paths (KTD11).
	if d.park {
		s.log.Info("pstrotator cmd", "remote", remote, "cmd", "park")
		s.logRefusals(remote, "park", s.m.Park())
		return
	}

	// Motion: dispatch the axes PRESENT on the wire only (R8) — an omitted
	// tag leaves that axis wherever it is.
	if d.hasAZ || d.hasEL {
		s.log.Info("pstrotator cmd", "remote", remote, "cmd", "goto",
			"az", d.az, "el", d.el, "has_az", d.hasAZ, "has_el", d.hasEL)
		s.logRefusals(remote, "goto", s.m.Goto(mount.Target{
			AZ:    d.az,
			EL:    d.el,
			HasAZ: d.hasAZ,
			HasEL: d.hasEL,
		}))
	}
}

// answerQuery replies to AZ?/EL? with the axis's cached readback, pinned to
// the manual shape (KTD11): "AZ:xxx.x\r" / "EL:yy.y\r", one decimal, sent to
// the source IP at listen-port+1.
func (s *Server) answerQuery(src net.Addr, remote string, ax mount.Axis) {
	deg, valid := s.m.Readback(ax)
	if !valid {
		// KTD9 no-fabrication on the query path: no valid cached readback
		// means NO reply — the grammar documents no error reply for queries,
		// and inventing a position is worse than silence. Bench
		// reconciliation item: confirm the shack's PstRotator tolerates a
		// missing reply pre-first-readback.
		s.log.Warn("pstrotator query without a valid readback; no reply sent",
			"remote", remote, "axis", string(ax))
		return
	}
	reply := fmt.Sprintf("AZ:%.1f\r", deg)
	if ax == mount.EL {
		reply = fmt.Sprintf("EL:%.1f\r", deg)
	}
	s.log.Debug("pstrotator query reply", "remote", remote, "axis", string(ax), "reply", reply)
	if err := s.replyTo(src, reply); err != nil {
		s.log.Warn("pstrotator query reply failed", "remote", remote, "err", err)
	}
}

// logRefusals surfaces R9 (liveness) and R10 (limit) refusals on the
// PstRotator path: UDP motion datagrams carry no reply contract, so the log
// is the operator's only signal that a command was refused.
func (s *Server) logRefusals(remote, cmd string, refs []mount.Refusal) {
	for _, r := range refs {
		s.log.Warn("pstrotator motion refused", "remote", remote, "cmd", cmd,
			"axis", string(r.Axis), "reason", r.Reason.String(),
			"target", r.Target, "detail", r.Detail)
	}
}

// replyTo sends one datagram to the source host on listen-port+1, the
// PstRotator reply-addressing convention (Appendix C).
func (s *Server) replyTo(src net.Addr, reply string) error {
	udpAddr, ok := src.(*net.UDPAddr)
	if !ok {
		return fmt.Errorf("source is not UDP: %T", src)
	}
	replyAddr := &net.UDPAddr{
		IP:   udpAddr.IP,
		Port: s.cfg.Port + 1,
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
