package working

import "strings"

// CanonicalMode maps a logger mode string onto the canonical six values
// (cw, usb, lsb, am, fm, data) of docs/conventions/band-mode-reference.md.
// The digital family (RTTY, DIGI, FT8, ...) collapses to data; USB-D, LSB-D
// and FM-D keep their carrier (usb, lsb, fm); a plain SSB splits on the
// frequency (>= 10 MHz usb, below lsb). "" stays "" (the payload omits it).
// This is the console's vocabulary, not station-log's cw/phone/data mode class.
func CanonicalMode(raw string, freqHz int64) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
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
		return "data"
	}
}

// NormalizeBand accepts a logger-provided band label only when it already
// looks canonical ("20m"); DXLog's MHz form ("14.0") is rejected so callers
// derive the band from the frequency.
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
