package control

import (
	"fmt"
	"strconv"
	"strings"
)

// gs232 decodes one Yaesu GS-232A command and returns the reply (CR-terminated
// for queries; empty for motion commands, which the protocol does not ack).
//
// Supported: C (azimuth), B (elevation), C2 (both); W aaa eee / M aaa (absolute
// move); S/A/E (stop); R/L (pan CW/CCW) and U/D (tilt up/down) continuous jog.
// Azimuth↔pan, elevation↔tilt. A/E map to a full stop (Pelco-D has no per-axis
// stop). Position queries are withheld (empty reply) until the first readback
// arrives, since GS-232 has no "unknown position" reply and AZ=000 would be
// indistinguishable from a real reading.
func (s *Server) gs232(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	up := strings.ToUpper(line)

	switch {
	case up == "C2":
		az, el, ok := s.pos.Get()
		if !ok {
			return ""
		}
		return fmt.Sprintf("AZ=%03.0f EL=%03.0f\r", az, el)
	case up[0] == 'C':
		az, _, ok := s.pos.Get()
		if !ok {
			return ""
		}
		return fmt.Sprintf("AZ=%03.0f\r", az)
	case up[0] == 'B':
		_, el, ok := s.pos.Get()
		if !ok {
			return ""
		}
		return fmt.Sprintf("EL=%03.0f\r", el)

	case up[0] == 'W':
		if n := numbers(line[1:]); len(n) >= 2 {
			s.submit(Command{Kind: KindSetPos, Az: n[0], El: n[1]})
		}
		return ""
	case up[0] == 'M':
		if n := numbers(line[1:]); len(n) >= 1 {
			_, el, _ := s.pos.Get() // azimuth-only move: keep current elevation (0 if unknown)
			s.submit(Command{Kind: KindSetPos, Az: n[0], El: el})
		}
		return ""

	case up[0] == 'S', up[0] == 'A', up[0] == 'E':
		s.submit(Command{Kind: KindStop})
		return ""

	case up[0] == 'R':
		s.submit(Command{Kind: KindJog, Pan: 1})
		return ""
	case up[0] == 'L':
		s.submit(Command{Kind: KindJog, Pan: -1})
		return ""
	case up[0] == 'U':
		s.submit(Command{Kind: KindJog, Tilt: 1})
		return ""
	case up[0] == 'D':
		s.submit(Command{Kind: KindJog, Tilt: -1})
		return ""
	}
	return ""
}

// numbers extracts integer runs from s as floats. It also splits a single
// contiguous 6-digit run into two 3-digit groups, supporting the spaceless
// GS-232 "Waaaeee" form alongside the spaced "W aaa eee" form.
func numbers(s string) []float64 {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r < '0' || r > '9' })
	if len(fields) == 1 && len(fields[0]) == 6 {
		fields = []string{fields[0][:3], fields[0][3:]}
	}
	var out []float64
	for _, f := range fields {
		if v, err := strconv.ParseFloat(f, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}
