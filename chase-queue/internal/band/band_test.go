package band

import "testing"

func TestOf(t *testing.T) {
	for hz, want := range map[float64]string{14_205_000: "20m", 7_000_000: "40m", 29_700_000: "10m",
		50_313_000: "6m", 144_300_000: "2m", 432_174_000: "70cm"} {
		if b, ok := Of(hz); !ok || b.Name != want {
			t.Errorf("Of(%v) = %v %v, want %s", hz, b.Name, ok, want)
		}
	}
	if _, ok := Of(10_120_000); ok {
		t.Error("30m is not an SSB band here")
	}
	if _, ok := Of(145_800_000 + 300_000); ok {
		t.Error("146.1 MHz is outside 2 m")
	}
	if !Known("17m") || !Known("6m") || !Known("70cm") || Known("30m") {
		t.Error("Known")
	}
}
