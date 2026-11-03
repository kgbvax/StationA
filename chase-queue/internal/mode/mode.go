// Package mode derives the canonical mode (docs/conventions/band-mode-reference.md:
// cw, usb, lsb, am, fm, data) of a heard or spotted station for its frequency
// and, for a cluster spot, its comment. "" means unknown.
package mode

import (
	"strings"
	"unicode"
)

// Canonical mode names.
const (
	CW   = "cw"
	USB  = "usb"
	LSB  = "lsb"
	AM   = "am"
	FM   = "fm"
	Data = "data"
)

// Sideband is the phone sideband by the amateur rule: LSB below 10 MHz
// except on 60 m, USB elsewhere. 0 (or less) Hz is unknown.
func Sideband(hz float64) string {
	switch {
	case hz <= 0:
		return ""
	case hz >= 5_300_000 && hz <= 5_410_000: // 60 m
		return USB
	case hz < 10_000_000:
		return LSB
	}
	return USB
}

// kind is what a band-plan segment is for.
type kind int

const (
	cw kind = iota
	digital
	phone
	fm
)

// segment is a stretch of the IARU Region 1 HF band plan, in Hz, edges
// included. Stretches that are mixed, beacon-only or satellite are left out:
// a frequency outside every segment has no mode.
type segment struct {
	lo, hi float64
	kind   kind
}

// segments is the IARU Region 1 HF band plan (the 500 Hz/2.7 kHz bandwidth
// recommendations, simplified to the three kinds a spot can be told by),
// low to high. 60 m follows the DL allocation (5351.5-5366.5 kHz).
var segments = []segment{
	// 160 m
	{1_810_000, 1_838_000, cw},
	{1_838_000, 1_843_000, digital},
	{1_843_000, 2_000_000, phone},
	// 80 m
	{3_500_000, 3_570_000, cw},
	{3_570_000, 3_600_000, digital},
	{3_600_000, 3_800_000, phone},
	// 60 m
	{5_351_500, 5_354_000, cw},
	{5_354_000, 5_366_500, phone},
	// 40 m
	{7_000_000, 7_040_000, cw},
	{7_040_000, 7_060_000, digital},
	{7_060_000, 7_200_000, phone},
	// 30 m (no phone)
	{10_100_000, 10_130_000, cw},
	{10_130_000, 10_150_000, digital},
	// 20 m (14.099-14.112 beacons and mixed: unknown)
	{14_000_000, 14_070_000, cw},
	{14_070_000, 14_099_000, digital},
	{14_112_000, 14_350_000, phone},
	// 17 m (18.109-18.111 beacons)
	{18_068_000, 18_095_000, cw},
	{18_095_000, 18_109_000, digital},
	{18_111_000, 18_168_000, phone},
	// 15 m (21.110-21.151 mixed and beacons: unknown)
	{21_000_000, 21_070_000, cw},
	{21_070_000, 21_110_000, digital},
	{21_151_000, 21_450_000, phone},
	// 12 m (24.931 upward is phone)
	{24_890_000, 24_915_000, cw},
	{24_915_000, 24_931_000, digital},
	{24_931_000, 24_990_000, phone},
	// 10 m (28.190-28.225 beacons, 29.2-29.51 digital/satellite: unknown; FM from 29.1)
	{28_000_000, 28_070_000, cw},
	{28_070_000, 28_190_000, digital},
	{28_225_000, 29_100_000, phone},
	{29_100_000, 29_200_000, fm}, // FM simplex channels
	{29_510_000, 29_700_000, fm}, // repeater channels and the 29.600 FM calling frequency
}

// digiWindow is the half width around a standard FT8/FT4 dial frequency in
// which a station is a digital one whatever the band plan says.
const digiWindow = 3_000

// digiDials are the standard FT8 and FT4 dial frequencies in Hz.
var digiDials = []float64{
	// FT8
	1_840_000, 3_573_000, 5_357_000, 7_074_000, 10_136_000, 14_074_000, 18_100_000, 21_074_000, 24_915_000, 28_074_000,
	// FT4
	3_575_000, 7_047_500, 10_140_000, 14_080_000, 18_104_000, 21_140_000, 24_919_000, 28_180_000,
}

// FromFreq is the mode the band plan gives a frequency: "" when unclear.
func FromFreq(hz float64) string {
	if hz <= 0 {
		return ""
	}
	for _, d := range digiDials {
		if hz >= d-digiWindow && hz <= d+digiWindow {
			return Data
		}
	}
	for _, s := range segments {
		if hz >= s.lo && hz <= s.hi {
			switch s.kind {
			case cw:
				return CW
			case digital:
				return Data
			case fm:
				return FM
			}
			return Sideband(hz)
		}
	}
	return ""
}

// keywords maps a comment word to the mode it names; "" stands for SSB, which
// takes the sideband rule.
var keywords = map[string]string{
	"CW":  CW,
	"FT8": Data, "FT4": Data, "RTTY": Data, "PSK": Data, "PSK31": Data, "PSK63": Data,
	"JT65": Data, "JT9": Data, "JS8": Data, "JS8CALL": Data, "DIGI": Data, "DATA": Data,
	"MFSK": Data, "OLIVIA": Data, "FSK": Data, "FSK441": Data, "Q65": Data, "MSK144": Data,
	"SSB": "sideband",
	"USB": USB, "LSB": LSB, "AM": AM, "FM": FM,
}

// modeAllowed tells the plain English "am" ("I am QRV", "QRT 11 AM") from the
// AM mode: it must not follow "I" or a number, and the band plan must leave
// room for a phone mode at the frequency.
func modeAllowed(words []string, i int, hz float64) bool {
	if i > 0 {
		prev := words[i-1]
		if prev == "I" || isNumber(prev) {
			return false
		}
	}
	switch FromFreq(hz) {
	case USB, LSB, FM, "":
		return true
	}
	return false
}

func isNumber(w string) bool {
	for _, r := range w {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return w != ""
}

// FromComment is the mode a spot comment names (the first keyword wins), ""
// when it names none.
func FromComment(comment string, hz float64) string {
	words := strings.FieldsFunc(strings.ToUpper(comment), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for i, w := range words {
		m, ok := keywords[w]
		if !ok {
			continue
		}
		if (m == AM || m == FM) && !modeAllowed(words, i, hz) {
			continue
		}
		if m == "sideband" {
			return Sideband(hz)
		}
		return m
	}
	return ""
}

// Derive is the mode of a cluster spot: the comment's keyword first, else the
// band plan at the spot frequency, else "".
func Derive(hz float64, comment string) string {
	if m := FromComment(comment, hz); m != "" {
		return m
	}
	return FromFreq(hz)
}
