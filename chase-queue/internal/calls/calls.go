// Package calls compares callsigns the way spots, logs and the CQ finder
// need: one station under its portable forms.
package calls

import (
	"slices"
	"strings"
)

// SameStation reports whether two callsigns name one station: equal, or
// equal but for a portable prefix or suffix (EA8/DL1ABC, DL1ABC/P). Short
// suffixes (/P, /MM, /1) are ignored; the remaining parts of one must all be
// parts of the other, so VP2E/W1AW is W1AW but not VP2E/K1AB.
func SameStation(a, c string) bool {
	a, c = strings.ToUpper(strings.TrimSpace(a)), strings.ToUpper(strings.TrimSpace(c))
	if a == "" || c == "" {
		return false
	}
	if a == c {
		return true
	}
	pa, pc := callParts(a), callParts(c)
	return within(pa, pc) || within(pc, pa)
}

// callParts are the parts of a callsign of three or more characters.
func callParts(c string) []string {
	var out []string
	for p := range strings.SplitSeq(c, "/") {
		if len(p) >= 3 {
			out = append(out, p)
		}
	}
	return out
}

// within reports whether every one of parts, and at least one, is in of.
func within(parts, of []string) bool {
	for _, p := range parts {
		if !slices.Contains(of, p) {
			return false
		}
	}
	return len(parts) > 0
}
