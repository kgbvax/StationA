// Package log4om decodes Log4OM's outbound UDP datagrams.
//
// Log4OM v2's software-integration "connections" screen offers three outbound
// service types (advanced guide, "Outbound services description"): ADIF
// MESSAGE (QSO logged), PSTROTATOR, and CALLSIGN — "The Call signs entered
// into the input field of the main Log4OM user interface, keyer interface or
// contest interface are broadcasted as UDP messages using this outbound
// service type." That CALLSIGN datagram is the selected-station signal this
// bridge exists for.
//
// The packet layout is NOT documented in either Log4OM guide (the advanced
// guide only spells out the RemoteControl request/response XML and the
// N1MM-shaped RadioInfo status message). This decoder is therefore TOLERANT:
// it accepts any XML root and pulls the callsign out of the common element
// names, rather than pinning a shape nobody has published. The first live
// packet capture should let this collapse into a strict struct — the bridge
// logs the root element name of every unmatched datagram at debug for
// exactly that purpose.
package log4om

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"

	"logger-spot-bridge/internal/canon"
)

// normalizeFreq decides the unit of a frequency value. The N1MM-compatible
// RadioInfo spelling carries 10 Hz units while Log4OM's own messages carry
// whole Hz, and the tolerance-based decoder cannot know which dialect sent
// the number. The test is deterministic: a value inside a known ham band is
// Hz; a value outside every band whose ×10 lands inside one is a 10 Hz-unit
// count. Everything else is taken verbatim (the band field stays empty and
// the console shows no band).
func normalizeFreq(n int64) int64 {
	if canon.BandFor(n) != "" {
		return n
	}
	if canon.BandFor(n*10) != "" {
		return n * 10
	}
	return n
}

// Callsign is a decoded CALLSIGN datagram. Frequency is in Hz (Log4OM's
// own outbound messages, where present, carry whole Hz — the N1MM-shaped
// RadioInfo is the one that uses 10 Hz units; the decoder accepts the
// N1MM spelling at its documented unit and scales it).
type Callsign struct {
	Call     string
	FreqHz   int64
	Band     string
	Mode     string
	Locator  string
	Operator string
	// Raw is the original datagram (for debug logging of unmatched shapes).
	Raw string
}

// elementNames worth probing, in probe order. Case-insensitive: Log4OM's
// remote-control XML is PascalCase while N1MM's is lowercase, and an
// undocumented format gets both spellings tried before it is trusted.
var callElements = []string{"callsign", "call", "dxcall", "station"}
var freqElements = []string{"frequency", "freq", "txfreq", "rxfreq"}
var bandElements = []string{"band"}
var modeElements = []string{"mode"}
var locatorElements = []string{"locator", "gridsquare", "gridsquare6"}
var operatorElements = []string{"operator", "opcall"}

// RootName returns the datagram's root element name ("" when unparseable).
func RootName(data []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local
		}
	}
}

// DecodeCallsign extracts the operator-entered callsign (and whatever
// location context the datagram carries) from a Log4OM outbound CALLSIGN
// datagram. Returns an error when the datagram has no XML or carries no
// callsign-like element — the caller logs those at debug and moves on.
//
// The datagram is collected in ONE pass into a name→text map (first
// occurrence wins, names lowercased), then probed by the element-name lists
// below. A sequential probe would consume the token stream field by field
// and silently lose everything after the first match. DecodeElement flattens
// nested subtrees into their text content — fine for the flat datagrams a
// callsign broadcast is expected to be.
func DecodeCallsign(data []byte) (Callsign, error) {
	rootName := RootName(data)
	if rootName == "" {
		return Callsign{}, fmt.Errorf("not XML")
	}

	dec := xml.NewDecoder(bytes.NewReader(data))
	fields := map[string]string{}
	first := true
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Callsign{}, fmt.Errorf("decode: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if first {
			first = false // the root element itself — decoding it would eat the whole document
			continue
		}
		var text string
		if err := dec.DecodeElement(&text, &se); err != nil {
			continue
		}
		name := strings.ToLower(se.Name.Local)
		if _, seen := fields[name]; !seen {
			fields[name] = strings.TrimSpace(text)
		}
	}

	lookup := func(names []string) string {
		for _, n := range names {
			if v, ok := fields[n]; ok && v != "" {
				return v
			}
		}
		return ""
	}

	out := Callsign{
		Call:     strings.ToUpper(lookup(callElements)),
		Band:     lookup(bandElements),
		Mode:     lookup(modeElements),
		Locator:  strings.ToUpper(lookup(locatorElements)),
		Operator: lookup(operatorElements),
		Raw:      string(data),
	}
	if freq := lookup(freqElements); freq != "" {
		if n, err := strconv.ParseInt(freq, 10, 64); err == nil && n > 0 {
			out.FreqHz = normalizeFreq(n)
		}
	}

	if out.Call == "" {
		return Callsign{}, fmt.Errorf("no callsign element in <%s> datagram", rootName)
	}
	return out, nil
}
