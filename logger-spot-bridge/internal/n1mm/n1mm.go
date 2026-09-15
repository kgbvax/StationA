// Package n1mm decodes the N1MM-family UDP broadcast XML that DXLog (and N1MM
// Logger+ itself) emit: https://n1mmwp.hamdocs.com/appendices/external-udp-broadcasts/.
// DXLog speaks the same datagram shape on its Options|Broadcast ports —
// verified against the DXLog wiki (dxlog.net/docs "Additional Information").
//
// Only the packets that carry an *operator-entered* callsign matter here:
//
//	lookupinfo — sent when the operator enters/selects a call (Space, Tab or
//	             call change in DXLog; call + spacebar in N1MM) BEFORE the QSO
//	             is logged. This is the "selected station" signal.
//
// contactinfo (QSO logged) and RadioInfo (radio state) are recognized and
// deliberately ignored: a logged QSO ends the chase, and radio context already
// reaches consumers via muehle/hf/radio. Spot packets are a future /event
// stream — not decoded yet (no consumer on the bus).
//
// Units: N1MM-family Freq/txfreq are 10 Hz units (DXLog example 352376 =
// 3523.76 kHz; N1MM's doc: "exported in units of 10 Hz"). Fields this package
// returns stay raw; converting to Hz / canonical band+mode is the caller's job
// (internal/canon), so the decoder tests stay table-driven over the wire data.
package n1mm

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// LookupInfo is the decoded lookupinfo datagram. Every field is optional in
// the wire format; emptiness is meaningful (an empty Call with reason
// CallChanged is the operator clearing the entry window).
type LookupInfo struct {
	// Call is the entered/selected callsign, uppercase, as the logger sent it.
	Call string
	// FreqTx10 / FreqRx10 are the TX/RX frequencies in 10 Hz units (the wire
	// unit), 0 when absent.
	FreqTx10, FreqRx10 int64
	// Band is the raw logger band label ("20m" N1MM-style, "14.0" MHz-style in
	// DXLog dialects) — NormalizeBand/BandFor decide what to trust.
	Band string
	// Mode is the raw logger mode string (CW, SSB, USB, RTTY, DIGI, …).
	Mode string
	// Grid is the locator the logger's lookup filled in, usually empty
	// (DXLog lookupinfo has no grid field at all; N1MM's is often blank).
	Grid string
	// Azimuth (degrees true) and DistanceKm as sent by DXLog, computed from
	// its CTY database relative to the logging PC's configured QTH. NaN when
	// absent (N1MM lookupinfo carries neither).
	Azimuth, DistanceKm float64
	// CountryPrefix / WPXPrefix as sent (e.g. "VK9", "VK9X" — operator
	// display + future DXCC resolution).
	CountryPrefix, WPXPrefix string
	// Reason is DXLog's trigger: "SpaceOrTab" | "CallChanged" ("" on N1MM).
	Reason string
	// Operator / MyCall identify the op seat when multiple loggers feed one
	// bridge; informational.
	Operator, MyCall string
	// Logger is the sending application (DXLog's `logger`, N1MM's `app`).
	Logger string
}

// RootKind classifies a datagram so the caller can route or ignore it.
type RootKind int

const (
	// KindOther is anything that is not one of the recognized roots (unknown
	// XML, garbage, an empty datagram).
	KindOther RootKind = iota
	KindLookupInfo
	KindContactInfo
	KindRadioInfo
	KindSpot
)

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

// Classify maps a root element name onto a RootKind (case-insensitive —
// N1MM's docs show lowercase contactinfo/lookupinfo/spot next to camel-case
// RadioInfo, and neither logger documents the casing as contract).
func Classify(root string) RootKind {
	switch strings.ToLower(strings.TrimSpace(root)) {
	case "lookupinfo":
		return KindLookupInfo
	case "contactinfo":
		return KindContactInfo
	case "radioinfo":
		return KindRadioInfo
	case "spot":
		return KindSpot
	default:
		return KindOther
	}
}

// wireLookupInfo is the superset struct over N1MM's contactinfo-shaped
// lookupinfo and DXLog's slimmer variant. encoding/xml fills what the
// datagram carries and leaves the rest zero — no error on unknown fields,
// which is what keeps one decoder working across logger dialects.
type wireLookupInfo struct {
	Call          string  `xml:"call"`
	FreqTx        string  `xml:"txfreq"`
	FreqRx        string  `xml:"rxfreq"`
	FreqTxAlt     string  `xml:"Freq"` // N1MM RadioInfo spelling; tolerated on lookupinfo
	Band          string  `xml:"band"`
	Mode          string  `xml:"mode"`
	Grid          string  `xml:"gridsquare"`
	Azimuth       float64 `xml:"azimuth"`
	DistanceKm    float64 `xml:"distance"`
	CountryPrefix string  `xml:"countryprefix"`
	WPXPrefix     string  `xml:"wpxprefix"`
	Reason        string  `xml:"reason"`
	Operator      string  `xml:"operator"`
	MyCall        string  `xml:"mycall"`
	Logger        string  `xml:"logger"`
	App           string  `xml:"app"`
}

// DecodeLookupInfo parses a lookupinfo datagram. It expects the caller has
// already classified the root (DecodeRoot does both); calling it on another
// root returns an error rather than guessing.
func DecodeLookupInfo(data []byte) (LookupInfo, error) {
	if got := RootName(data); !strings.EqualFold(got, "lookupinfo") {
		return LookupInfo{}, fmt.Errorf("not a lookupinfo datagram (root %q)", got)
	}
	var w wireLookupInfo
	if err := xml.Unmarshal(data, &w); err != nil {
		return LookupInfo{}, fmt.Errorf("decode lookupinfo: %w", err)
	}

	out := LookupInfo{
		Call:          strings.ToUpper(strings.TrimSpace(w.Call)),
		Band:          strings.TrimSpace(w.Band),
		Mode:          strings.TrimSpace(w.Mode),
		Grid:          strings.ToUpper(strings.TrimSpace(w.Grid)),
		Azimuth:       w.Azimuth,
		DistanceKm:    w.DistanceKm,
		CountryPrefix: strings.TrimSpace(w.CountryPrefix),
		WPXPrefix:     strings.TrimSpace(w.WPXPrefix),
		Reason:        strings.TrimSpace(w.Reason),
		Operator:      strings.TrimSpace(w.Operator),
		MyCall:        strings.ToUpper(strings.TrimSpace(w.MyCall)),
		Logger:        firstNonEmpty(w.Logger, w.App),
	}
	out.FreqTx10 = parseFreq10(w.FreqTx, w.FreqTxAlt)
	out.FreqRx10 = parseFreq10(w.FreqRx, "")
	return out, nil
}

// parseFreq10 parses a 10-Hz-unit frequency written either as an integer
// string or the N1MM `Freq`-spelled element. Returns 0 for empty/invalid —
// a garbage number must not surface as a bogus frequency. Strict: a partial
// numeric parse ("14.025.00" → 14) is rejected, not truncated.
func parseFreq10(vals ...string) int64 {
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return 0 // present but unparseable: treat as absent, not zero-Hz
		}
		return n
	}
	return 0
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
