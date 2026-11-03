package contest

import (
	"bufio"
	"io"
	"strings"
)

var suffixes = map[string]bool{"P": true, "M": true, "MM": true, "QRP": true, "A": true}

// NormalizeCall upper-cases a callsign, drops an SSID and the suffixes the
// CQ finder's callsign vote drops (/P /M /MM /QRP /A) and keeps prefix forms
// (EA8/DL1ABC), so a logged DL1ABC/P and a heard DL1ABC are the same call.
func NormalizeCall(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if i := strings.LastIndexByte(c, '-'); i > 0 {
		if rest := c[i+1:]; rest == "#" || (len(rest) <= 2 && isDigits(rest)) {
			c = c[:i]
		}
	}
	parts := strings.Split(c, "/")
	for len(parts) > 1 && suffixes[parts[len(parts)-1]] {
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, "/")
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Prefill maps a callsign to the received-exchange value DXLog's prefill
// database holds for it (the column DB_FILE_COL_RCVD names).
type Prefill struct {
	byCall map[string]string
}

// ParsePrefill reads a DXLog prefill database in its plain text format
// (callsign,info1,info2,...), the older callsign=info form, or an N1MM-style
// CSV, and keeps column col (1-based among the info columns; 0 means 1).
// Lines starting with # or ! are comments. A binary Win-Test .DTB file is not
// read: it yields an empty prefill.
func ParsePrefill(r io.Reader, col int) (*Prefill, error) {
	if col < 1 {
		col = 1
	}
	p := &Prefill{byCall: map[string]string{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" || line[0] == '#' || line[0] == '!' {
			continue
		}
		var call, val string
		if f := strings.Split(line, ","); len(f) > col {
			call, val = f[0], f[col]
		} else if k, v, ok := strings.Cut(line, "="); ok && col == 1 && !strings.Contains(line, ",") {
			call, val = k, v
		} else {
			continue
		}
		call, val = NormalizeCall(strings.Trim(call, `" `)), strings.ToUpper(strings.Trim(val, `" `))
		if call != "" && val != "" {
			p.byCall[call] = val
		}
	}
	return p, sc.Err()
}

// Len is the number of callsigns with a value.
func (p *Prefill) Len() int {
	if p == nil {
		return 0
	}
	return len(p.byCall)
}

// Lookup returns the stored value of a callsign.
func (p *Prefill) Lookup(call string) (string, bool) {
	if p == nil {
		return "", false
	}
	v, ok := p.byCall[NormalizeCall(call)]
	return v, ok
}
