package control

import (
	"fmt"
	"strconv"
	"strings"
)

// rotctld decodes one line of the Hamlib net-rotator protocol and returns the
// reply (already newline-terminated) plus whether to close the connection.
//
// Protocol summary (non-extended): "get" commands print their values, one per
// line; "set" commands reply "RPRT 0" on success; any error is "RPRT -<n>".
// Long-form names may be prefixed with a backslash (e.g. "\get_pos").
func (s *Server) rotctld(line string) (reply string, closeConn bool) {
	fields := strings.Fields(strings.TrimRight(line, "\r\n"))
	if len(fields) == 0 {
		return "", false
	}
	cmd := strings.TrimPrefix(fields[0], "\\")

	switch cmd {
	case "p", "get_pos":
		az, el, ok := s.pos.Get()
		if !ok {
			return "RPRT -1\n", false // no readback yet
		}
		return fmt.Sprintf("%.6f\n%.6f\n", az, el), false

	case "P", "set_pos":
		if len(fields) < 3 {
			return "RPRT -1\n", false
		}
		az, e1 := strconv.ParseFloat(fields[1], 64)
		el, e2 := strconv.ParseFloat(fields[2], 64)
		if e1 != nil || e2 != nil {
			return "RPRT -1\n", false
		}
		s.submit(Command{Kind: KindSetPos, Az: az, El: el})
		return "RPRT 0\n", false

	case "S", "stop":
		s.submit(Command{Kind: KindStop})
		return "RPRT 0\n", false

	case "_", "get_info":
		return "pelcots\n", false

	case "q", "Q":
		return "", true

	default:
		return "RPRT -1\n", false
	}
}
