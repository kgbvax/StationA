// SPDX-License-Identifier: AGPL-3.0-or-later

package heading

import (
	"math"
	"testing"
)

func TestStorm(t *testing.T) {
	cases := []struct {
		name string
		in   Inputs
		want int
	}{
		{"west wind, boom at 180 → 180", Inputs{WindDeg: 270, WindOK: true, Az: 170, AzOK: true, MaxAz: 360}, 180},
		{"west wind, boom near 0 → 0", Inputs{WindDeg: 270, WindOK: true, Az: 20, AzOK: true, MaxAz: 360}, 0},
		{"west wind, boom near 350 → 360 (same heading as 0)", Inputs{WindDeg: 270, WindOK: true, Az: 350, AzOK: true, MaxAz: 360}, 360},
		{"265.0° (live 2650) → 175 near 175", Inputs{WindDeg: 265, WindOK: true, Az: 175, AzOK: true, MaxAz: 360}, 175},
		{"265.0° → 355 near 300", Inputs{WindDeg: 265, WindOK: true, Az: 300, AzOK: true, MaxAz: 360}, 355},
		{"north wind → 90 or 270, nearer 100", Inputs{WindDeg: 0, WindOK: true, Az: 100, AzOK: true, MaxAz: 360}, 90},
		{"north wind → 270 near 260", Inputs{WindDeg: 0, WindOK: true, Az: 260, AzOK: true, MaxAz: 360}, 270},
		{"wind 359.6 rounds to 0 → 90 near 0", Inputs{WindDeg: 359.6, WindOK: true, Az: 0, AzOK: true, MaxAz: 360}, 90},
		{"wind 360 == 0", Inputs{WindDeg: 360, WindOK: true, Az: 280, AzOK: true, MaxAz: 360}, 270},
		{"negative wind folds", Inputs{WindDeg: -90, WindOK: true, Az: 10, AzOK: true, MaxAz: 360}, 0},
		{"wrap: wind 200 → 110/290, az 10 → 110", Inputs{WindDeg: 200, WindOK: true, Az: 10, AzOK: true, MaxAz: 360}, 110},
		{"overlap 450: az 420 → 470? no: 110+360 > 450 → 290", Inputs{WindDeg: 200, WindOK: true, Az: 420, AzOK: true, MaxAz: 450}, 290},
		{"overlap 450: wind 300 → 30/210/390, az 420 → 390", Inputs{WindDeg: 300, WindOK: true, Az: 420, AzOK: true, MaxAz: 450}, 390},
		{"max 360 never offers 390", Inputs{WindDeg: 300, WindOK: true, Az: 360, AzOK: true, MaxAz: 360}, 210},
		{"tie picks lower", Inputs{WindDeg: 0, WindOK: true, Az: 180, AzOK: true, MaxAz: 360}, 90},
		{"az unknown → lowest", Inputs{WindDeg: 270, WindOK: true, MaxAz: 360}, 0},
		{"stale wind → fallback", Inputs{WindDeg: 270, WindOK: false, Az: 100, AzOK: true, MaxAz: 360, FallbackAz: 222}, 222},
		{"NaN wind → fallback", Inputs{WindDeg: math.NaN(), WindOK: true, MaxAz: 360, FallbackAz: 45}, 45},
		{"max_az below 360 clamps to 360", Inputs{WindDeg: 270, WindOK: true, Az: 355, AzOK: true, MaxAz: 0}, 360},
	}
	for _, c := range cases {
		if got := Storm(c.in); got != c.want {
			t.Errorf("%s: Storm = %d, want %d", c.name, got, c.want)
		}
	}
}

// The park is always perpendicular to the wind and within [0, MaxAz].
func TestStormPerpendicularProperty(t *testing.T) {
	for wind := 0; wind < 360; wind += 7 {
		for az := 0; az <= 450; az += 13 {
			for _, maxAz := range []int{360, 450} {
				got := Storm(Inputs{WindDeg: float64(wind), WindOK: true, Az: float64(az), AzOK: true, MaxAz: maxAz})
				if got < 0 || got > maxAz {
					t.Fatalf("wind %d az %d max %d: %d out of range", wind, az, maxAz, got)
				}
				if d := Norm(got - wind); d != 90 && d != 270 {
					t.Fatalf("wind %d az %d: park %d not perpendicular (Δ %d)", wind, az, got, d)
				}
			}
		}
	}
}

func TestNorm(t *testing.T) {
	for in, want := range map[int]int{0: 0, 359: 359, 360: 0, 450: 90, -1: 359, -360: 0, 725: 5} {
		if got := Norm(in); got != want {
			t.Errorf("Norm(%d) = %d, want %d", in, got, want)
		}
	}
}
