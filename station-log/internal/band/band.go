// Package band maps a frequency to its ADIF band name ("20m", "70cm"), the
// canonical band string station-log stores. Unlike the HF-only tables of the
// chase components it covers the VHF/UHF bands too, so a QSO on any band is
// kept. The band edges are the ADIF band table (IARU wide), not a national
// allocation.
package band

import "strings"

// Band is one amateur band.
type Band struct {
	Name string  // "20m"
	LoHz float64 // ADIF band edges
	HiHz float64
}

// Bands lists the bands a frequency can be mapped to, low to high.
var Bands = []Band{
	{"2190m", 135_700, 137_800},
	{"630m", 472_000, 479_000},
	{"560m", 501_000, 504_000},
	{"160m", 1_800_000, 2_000_000},
	{"80m", 3_500_000, 4_000_000},
	{"60m", 5_060_000, 5_450_000},
	{"40m", 7_000_000, 7_300_000},
	{"30m", 10_100_000, 10_150_000},
	{"20m", 14_000_000, 14_350_000},
	{"17m", 18_068_000, 18_168_000},
	{"15m", 21_000_000, 21_450_000},
	{"12m", 24_890_000, 24_990_000},
	{"10m", 28_000_000, 29_700_000},
	{"8m", 40_000_000, 45_000_000},
	{"6m", 50_000_000, 54_000_000},
	{"5m", 54_000_001, 69_900_000},
	{"4m", 70_000_000, 71_000_000},
	{"2m", 144_000_000, 148_000_000},
	{"1.25m", 222_000_000, 225_000_000},
	{"70cm", 420_000_000, 450_000_000},
	{"33cm", 902_000_000, 928_000_000},
	{"23cm", 1_240_000_000, 1_300_000_000},
	{"13cm", 2_300_000_000, 2_450_000_000},
	{"9cm", 3_300_000_000, 3_500_000_000},
	{"6cm", 5_650_000_000, 5_925_000_000},
	{"3cm", 10_000_000_000, 10_500_000_000},
}

// Of returns the band whose edges (included) contain hz.
func Of(hz float64) (Band, bool) {
	for _, b := range Bands {
		if hz >= b.LoHz && hz <= b.HiHz {
			return b, true
		}
	}
	return Band{}, false
}

// Normalize lower-cases and trims an ADIF BAND value.
func Normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Known reports whether name is a band of Bands or another ADIF band name
// (listed without edges, so only an explicit BAND field can name it).
func Known(name string) bool {
	for _, b := range Bands {
		if b.Name == name {
			return true
		}
	}
	switch name {
	case "1.25cm", "6mm", "4mm", "2.5mm", "2mm", "1mm":
		return true
	}
	return false
}
