// Package adif parses ADIF records tolerantly: one datagram from Log4OM's
// ADIF_MESSAGE output, a file, or several records in a row. Tags match
// case-insensitively and by their length specifier; anything before <EOH>
// is a header and skipped.
package adif

import (
	"sort"
	"strconv"
	"strings"
)

// Record is one QSO: upper-case field name -> value.
type Record map[string]string

// Parse returns every record of the text, in order. Malformed tails are
// dropped.
func Parse(text string) []Record {
	out, _ := ParseChecked(text)
	return out
}

// ParseChecked is Parse that also reports whether anything was dropped: a
// tag that is cut off or has a bad length, or fields after the last <EOR>.
// A caller that deletes on the strength of the parse (replace_source)
// refuses such a text.
func ParseChecked(text string) (out []Record, truncated bool) {
	if i := indexFold(text, "<eoh>"); i >= 0 {
		text = text[i+len("<eoh>"):]
	}
	cur := Record{}
	for {
		lt := strings.IndexByte(text, '<')
		if lt < 0 {
			break
		}
		gt := strings.IndexByte(text[lt:], '>')
		if gt < 0 {
			truncated = true
			break
		}
		tag := text[lt+1 : lt+gt]
		text = text[lt+gt+1:]
		parts := strings.Split(tag, ":")
		name := strings.ToUpper(strings.TrimSpace(parts[0]))
		if name == "EOR" {
			if len(cur) > 0 {
				out = append(out, cur)
			}
			cur = Record{}
			continue
		}
		if len(parts) < 2 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || n < 0 || n > len(text) {
			truncated = true
			break
		}
		cur[name] = text[:n]
		text = text[n:]
	}
	if len(cur) > 0 {
		truncated = true
	}
	return out, truncated
}

// indexFold finds sub (lower case, ASCII) in s ignoring ASCII case, on the
// original bytes: lower-casing a copy shifts offsets for invalid UTF-8.
func indexFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i] == '<' && strings.EqualFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

// Format writes one record as ADIF text ending in <EOR>, fields sorted by
// name, lengths in bytes (what Parse reads back). Empty values are left out.
func Format(r Record) string {
	names := make([]string, 0, len(r))
	for k, v := range r {
		if v != "" {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		v := r[k]
		b.WriteString("<" + k + ":" + strconv.Itoa(len(v)) + ">" + v + " ")
	}
	b.WriteString("<EOR>\n")
	return b.String()
}
