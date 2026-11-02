// Package band maps a frequency to its HF amateur band name ("20m"), the key
// every history source and the CQ finder agree on.
package band

// Band is one HF amateur band.
type Band struct {
	Name string  // "80m"
	LoHz float64 // DL allocation edges
	HiHz float64
}

// Bands lists the HF bands with SSB phone activity, low to high.
var Bands = []Band{
	{"160m", 1_810_000, 2_000_000},
	{"80m", 3_500_000, 3_800_000},
	{"60m", 5_351_500, 5_366_500},
	{"40m", 7_000_000, 7_200_000},
	{"20m", 14_000_000, 14_350_000},
	{"17m", 18_068_000, 18_168_000},
	{"15m", 21_000_000, 21_450_000},
	{"12m", 24_890_000, 24_990_000},
	{"10m", 28_000_000, 29_700_000},
}

// Of returns the band whose allocation (edges included) contains hz.
func Of(hz float64) (Band, bool) {
	for _, b := range Bands {
		if hz >= b.LoHz && hz <= b.HiHz {
			return b, true
		}
	}
	return Band{}, false
}

// Known reports whether name is one of Bands.
func Known(name string) bool {
	for _, b := range Bands {
		if b.Name == name {
			return true
		}
	}
	return false
}
