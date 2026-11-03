package mode

import "testing"

func TestSideband(t *testing.T) {
	for _, c := range []struct {
		hz   float64
		want string
	}{
		{1_850_000, LSB}, {3_700_000, LSB}, {5_360_000, USB}, {7_100_000, LSB}, {9_999_000, LSB},
		{10_100_000, USB}, {14_250_000, USB}, {28_500_000, USB}, {0, ""},
	} {
		if got := Sideband(c.hz); got != c.want {
			t.Errorf("Sideband(%.0f) = %q, want %q", c.hz, got, c.want)
		}
	}
}

func TestFromFreq(t *testing.T) {
	for _, c := range []struct {
		name string
		hz   float64
		want string
	}{
		{"160 cw", 1_825_000, CW}, {"160 digital", 1_839_000, Data}, {"160 ft8", 1_840_500, Data}, {"160 phone", 1_900_000, LSB},
		{"80 cw", 3_520_000, CW}, {"80 digital", 3_590_000, Data}, {"80 ft8", 3_573_000, Data}, {"80 phone", 3_750_000, LSB},
		{"60 cw", 5_352_000, CW}, {"60 ft8", 5_357_000, Data}, {"60 phone usb", 5_362_000, USB},
		{"40 cw", 7_020_000, CW}, {"40 digital", 7_055_000, Data}, {"40 ft8", 7_074_500, Data}, {"40 ft4", 7_047_500, Data}, {"40 phone", 7_150_000, LSB},
		{"30 cw", 10_110_000, CW}, {"30 ft8", 10_137_000, Data}, {"30 digital", 10_145_000, Data}, {"30 above plan", 10_150_100, ""},
		{"20 cw", 14_030_000, CW}, {"20 digital", 14_085_000, Data}, {"20 ft8", 14_074_000, Data}, {"20 beacons", 14_100_000, ""}, {"20 phone", 14_250_000, USB},
		{"17 cw", 18_080_000, CW}, {"17 ft8", 18_100_000, Data}, {"17 digital", 18_106_000, Data}, {"17 phone", 18_140_000, USB},
		{"15 cw", 21_030_000, CW}, {"15 ft8", 21_074_000, Data}, {"15 ft4", 21_140_000, Data}, {"15 mixed", 21_125_000, ""}, {"15 phone", 21_300_000, USB},
		{"12 cw", 24_900_000, CW}, {"12 ft8", 24_915_000, Data}, {"12 digital", 24_925_000, Data}, {"12 phone", 24_950_000, USB},
		{"10 cw", 28_030_000, CW}, {"10 ft8", 28_074_000, Data}, {"10 digital", 28_120_000, Data}, {"10 beacons", 28_200_000, ""}, {"10 phone", 28_500_000, USB}, {"10 fm segments", 29_600_000, FM}, {"10 fm simplex", 29_150_000, FM}, {"10 low phone", 28_500_000, USB},
		{"out of band", 12_000_000, ""}, {"zero", 0, ""},
	} {
		if got := FromFreq(c.hz); got != c.want {
			t.Errorf("%s: FromFreq(%.0f) = %q, want %q", c.name, c.hz, got, c.want)
		}
	}
}

func TestDerive(t *testing.T) {
	for _, c := range []struct {
		name    string
		hz      float64
		comment string
		want    string
	}{
		{"cw keyword in phone segment", 14_250_000, "CW up 1", CW},
		{"ft8 keyword", 14_250_000, "ft8 -12dB", Data},
		{"rtty keyword", 7_150_000, "RTTY contest", Data},
		{"ssb keyword on 40", 7_020_000, "ssb", LSB},
		{"ssb keyword on 20", 14_020_000, "SSB", USB},
		{"ssb keyword on 60", 5_360_000, "ssb", USB},
		{"literal usb on 40", 7_150_000, "usb split", USB},
		{"literal am", 7_150_000, "AM net", AM},
		{"literal fm", 29_600_000, "FM 88.5", FM},
		{"english am on a cw frequency", 14_025_000, "I AM QRV", CW},
		{"time am on a cw frequency", 14_025_000, "QRT 11 AM", CW},
		{"english am on phone", 14_250_000, "I AM here", USB},
		{"time am on phone", 14_250_000, "back 10 AM", USB},
		{"am in a cw segment is not am", 14_025_000, "AM", CW},
		{"first keyword wins", 14_250_000, "CW then FT8", CW},
		{"no keyword uses plan", 14_030_000, "tnx 5/9", CW},
		{"word is not a substring", 14_250_000, "FT80X", USB},
		{"unclear", 14_105_000, "cq", ""},
		{"no frequency", 0, "", ""},
	} {
		if got := Derive(c.hz, c.comment); got != c.want {
			t.Errorf("%s: Derive(%.0f, %q) = %q, want %q", c.name, c.hz, c.comment, got, c.want)
		}
	}
}
