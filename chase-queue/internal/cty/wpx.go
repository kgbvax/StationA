package cty

import "strings"

// WPX returns the CQ WPX prefix of a callsign: the letters and digits up to
// and including the last digit of the call, with these rules. A portable
// prefix in front or behind (EA8/DL1ABC, DL1ABC/EA8) replaces the call's
// own, and gets a 0 when it has no digit (EA/DL1ABC is EA0). A single digit
// suffix replaces the call's digit (W1ABC/4 is W4). /P /M /QRP /A /LH /B and
// an SSID are ignored. A call without a digit gets its first two letters
// and a 0 (RAEM is RA0). /MM and /AM have no prefix.
func WPX(call string) (string, bool) {
	s := strings.ToUpper(strings.TrimSpace(call))
	if loc := ssid.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	if s == "" {
		return "", false
	}
	parts := strings.Split(s, "/")
	kept := parts[:1]
	digit := ""
	for _, p := range parts[1:] {
		switch {
		case p == "MM" || p == "AM":
			return "", false
		case modifiers[p]:
		case len(p) == 1 && p[0] >= '0' && p[0] <= '9':
			digit = p
		default:
			kept = append(kept, p)
		}
	}
	base, portable := kept[0], ""
	if len(kept) > 1 {
		// the shorter part is the prefix, as in Lookup
		a, b := kept[0], kept[1]
		if len(b) < len(a) {
			base, portable = a, b
		} else {
			base, portable = b, a
		}
	}
	if portable != "" {
		if !strings.ContainsAny(portable, "0123456789") {
			return portable + "0", true
		}
		return portable, true
	}
	last := strings.LastIndexAny(base, "0123456789")
	if last < 0 {
		if len(base) < 2 {
			return "", false
		}
		return base[:2] + "0", true
	}
	// the prefix ends at the last digit of the first run of letters-then-digits
	i := last
	for i > 0 && base[i-1] >= '0' && base[i-1] <= '9' {
		i--
	}
	pre := base[:last+1]
	if digit != "" {
		pre = base[:i] + digit
	}
	return pre, true
}
