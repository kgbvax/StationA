// Package adif parses ADIF records tolerantly: one datagram from Log4OM's
// ADIF_MESSAGE output, a file, or several records in a row. Tags match
// case-insensitively and by their length specifier; anything before <EOH>
// is a header and skipped.
package adif

import (
	"strconv"
	"strings"
)

// Record is one QSO: upper-case field name -> value.
type Record map[string]string

// Parse returns every record of the text, in order. Malformed tails are
// dropped.
func Parse(text string) []Record {
	if i := indexFold(text, "<eoh>"); i >= 0 {
		text = text[i+len("<eoh>"):]
	}
	var out []Record
	cur := Record{}
	for {
		lt := strings.IndexByte(text, '<')
		if lt < 0 {
			break
		}
		gt := strings.IndexByte(text[lt:], '>')
		if gt < 0 {
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
			break
		}
		cur[name] = text[:n]
		text = text[n:]
	}
	return out
}

func indexFold(s, sub string) int { return strings.Index(strings.ToLower(s), sub) }
