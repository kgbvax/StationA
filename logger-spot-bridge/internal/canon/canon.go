// Package canon maps logger vocabulary onto the station's canonical band and
// mode labels (docs/conventions/band-mode-reference.md). The loggers speak
// N1MM-family dialects: frequency in 10 Hz units, band as "20m" (N1MM) or MHz
// ("14.0", DXLog contactinfo), mode as CW/SSB/USB/LSB/RTTY/DIGI/…
package canon

import "strings"

// BandTable is the canonical band edge table (Hz, both edges inclusive) from
// docs/conventions/band-mode-reference.md (DL / IARU Region 1 allocations).
// Ordered low → high; BandFor returns the first match.
type BandTable struct {
	Band      string
	Low, High int64
}

var bands = []BandTable{
	{"160m", 1_800_000, 2_000_000},
	{"80m", 3_500_000, 4_000_000},
	{"60m", 5_351_500, 5_366_500},
	{"40m", 7_000_000, 7_300_000},
	{"30m", 10_100_000, 10_150_000},
	{"20m", 14_000_000, 14_350_000},
	{"17m", 18_068_000, 18_168_000},
	{"15m", 21_000_000, 21_450_000},
	{"12m", 24_890_000, 24_990_000},
	{"10m", 28_000_000, 29_700_000},
	{"6m", 50_000_000, 54_000_000},
}

// hfGeneralLow/High bound the "gen" general-coverage fallback (1.8–30 MHz).
const (
	hfGeneralLow  = 1_800_000
	hfGeneralHigh = 30_000_000
)

// BandFor resolves a frequency (Hz) onto the canonical band label: the
// allocation containing it, `gen` for the HF general-coverage gap, or ""
// outside (the reference's `unknown` case; the field is omitempty in the
// state payload so consumers just see no band).
func BandFor(freqHz int64) string {
	if freqHz <= 0 {
		return ""
	}
	for _, b := range bands {
		if freqHz >= b.Low && freqHz <= b.High {
			return b.Band
		}
	}
	if freqHz >= hfGeneralLow && freqHz <= hfGeneralHigh {
		return "gen"
	}
	return ""
}

// CanonicalMode maps a logger mode string onto the canonical six values
// (cw, usb, lsb, am, fm, data). The digital family (RTTY, DIGI, FT8, …)
// collapses to `data` per the reference; a plain "SSB" splits on the
// frequency (≥ 10 MHz → usb, below → lsb, the conventional sideband split).
// "" (unrecognized/empty) stays "" — the payload omits it.
func CanonicalMode(raw string, freqHz int64) string {
	m := strings.ToUpper(strings.TrimSpace(raw))
	switch m {
	case "CW":
		return "cw"
	case "USB", "USB-D", "USB_D":
		return "usb"
	case "LSB", "LSB-D", "LSB_D":
		return "lsb"
	case "AM":
		return "am"
	case "FM", "FM-D", "FM_D":
		return "fm"
	case "SSB":
		if freqHz >= 10_000_000 {
			return "usb"
		}
		return "lsb"
	case "":
		return ""
	default:
		// RTTY, DIGI, DIGITAL, FT8, FT4, PKT, … — the data family.
		return "data"
	}
}

// NormalizeBand accepts a logger-provided band label only when it already
// looks canonical ("20m") — DXLog's MHz form ("14.0") is rejected so callers
// fall back to deriving the band from the frequency. Uppercases the letter.
func NormalizeBand(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if len(s) < 2 || !strings.HasSuffix(s, "m") {
		return ""
	}
	for _, c := range s[:len(s)-1] {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return s
}
