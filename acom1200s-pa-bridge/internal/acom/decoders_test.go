package acom

import "testing"

func TestDecodeMode(t *testing.T) {
	cases := []struct {
		byte byte
		want string
	}{
		{0x10, "RESET"},
		{0x20, "INIT"},
		{0x30, "DEBUG"},
		{0x40, "SERVICE"},
		{0x50, "STANDBY"},
		{0x60, "OPR/RX"},
		{0x70, "OPR/TX"},
		{0x80, "ATAC"},
		{0x90, "MENU"},
		{0xA0, "OFF"},
		{0x00, "UNKNOWN"},
		{0xB0, "UNKNOWN"},
	}
	for _, c := range cases {
		if got := decodeMode(c.byte); got != c.want {
			t.Errorf("decodeMode(0x%02X) = %q, want %q", c.byte, got, c.want)
		}
	}
}

func TestCanonicalMode(t *testing.T) {
	cases := map[string]string{
		"OPR/RX":   "operate",
		"OPR/TX":   "operate",
		"STANDBY":  "standby",
		"OFF":      "standby",
		"ATAC":     "standby",
		"RESET":    "standby",
		"INIT":     "standby",
		"DEBUG":    "standby",
		"SERVICE":  "standby",
		"MENU":     "standby",
		"UNKNOWN":  "standby",
		"whatever": "standby",
	}
	for raw, want := range cases {
		if got := CanonicalMode(raw); got != want {
			t.Errorf("CanonicalMode(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestCanonicalKeyed(t *testing.T) {
	cases := map[string]string{
		"OPR/TX":   "tx",
		"OPR/RX":   "rx",
		"STANDBY":  "inhibited",
		"OFF":      "inhibited",
		"ATAC":     "inhibited",
		"UNKNOWN":  "inhibited",
		"whatever": "inhibited",
	}
	for raw, want := range cases {
		if got := CanonicalKeyed(raw); got != want {
			t.Errorf("CanonicalKeyed(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestCanonicalFault(t *testing.T) {
	cases := []struct {
		byte byte
		want string
	}{
		{0xFF, "none"},
		{0x1C, "temp"},      // PAM1 TEMP TOO HIGH
		{0x1E, "temp"},      // PAM1 EXCESSIVE TEMP
		{0xAE, "temp"},      // ATU TEMP TOO HIGH
		{0x0D, "swr"},       // PA LOAD SWR TOO HIGH
		{0xAC, "swr"},       // ANTENNA SWR TOO HIGH
		{0x04, "reflected"}, // REFLECTED POWER WARNING
		{0xA9, "reflected"}, // ANT REFL PWR TOO HIGH (HARD)
		{0x00, "other"},     // HOT SWITCHING ATTEMPT
		{0x70, "other"},     // CAT ERROR
		{0x14, "other"},     // unmapped long tail
	}
	for _, c := range cases {
		if got := CanonicalFault(c.byte, decodeError(c.byte)); got != c.want {
			t.Errorf("CanonicalFault(0x%02X) = %q, want %q", c.byte, got, c.want)
		}
	}
}

func TestDecodeErrorNone(t *testing.T) {
	if got := decodeError(0xFF); got != "NONE" {
		t.Errorf("decodeError(0xFF) = %q, want NONE", got)
	}
	// A non-mapped byte still yields a formatted string, not empty.
	if got := decodeError(0xFE); got == "" {
		t.Error("decodeError(0xFE) should return a non-empty message for unmapped codes")
	}
}

func TestBandNameToIndex(t *testing.T) {
	cases := []struct {
		in    string
		want  int
		valid bool
	}{
		{"160m", 1, true}, {"160", 1, true},
		{"80m", 2, true}, {"80", 2, true},
		{"40m", 3, true}, {"40", 3, true},
		{"30m", 4, true},
		{"20m", 5, true},
		{"17m", 6, true},
		{"15m", 7, true},
		{"12m", 8, true},
		{"10m", 9, true},
		{"6m", 10, true},
		{"60m", 0, false}, // no 60m on the ACOM 1200S
		{"", 0, false},
		{"garbage", 0, false},
		{" 20M ", 5, true}, // case/space insensitive
	}
	for _, c := range cases {
		got, ok := BandNameToIndex(c.in)
		if ok != c.valid || got != c.want {
			t.Errorf("BandNameToIndex(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.valid)
		}
	}
}

func TestDecodeBand(t *testing.T) {
	cases := []struct {
		byte byte
		want string
	}{
		{0x01, "160m"}, {0x02, "80m"}, {0x03, "40m"}, {0x04, "30m"},
		{0x05, "20m"}, {0x06, "17m"}, {0x07, "15m"}, {0x08, "12m"},
		{0x09, "10m"}, {0x0A, "6m"},
		{0x00, "UNK"}, {0x0B, "UNK"}, {0xFF, "UNK"},
	}
	for _, c := range cases {
		if got := decodeBand(c.byte); got != c.want {
			t.Errorf("decodeBand(0x%02X) = %q, want %q", c.byte, got, c.want)
		}
	}
}

func TestBandOptionsHasNo60m(t *testing.T) {
	for _, b := range BandOptions {
		if b == "60m" {
			t.Fatal("BandOptions must not contain 60m (the ACOM 1200S has no 60m band)")
		}
	}
	if len(BandOptions) != 10 {
		t.Errorf("BandOptions len = %d, want 10", len(BandOptions))
	}
}
