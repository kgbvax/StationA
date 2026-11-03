package band

import "testing"

func TestOf(t *testing.T) {
	for _, c := range []struct {
		hz   float64
		want string
	}{
		{14_074_000, "20m"}, {10_136_000, "30m"}, {1_840_000, "160m"}, {50_313_000, "6m"},
		{144_300_000, "2m"}, {432_200_000, "70cm"}, {7_000_000, "40m"}, {7_300_000, "40m"},
	} {
		b, ok := Of(c.hz)
		if !ok || b.Name != c.want {
			t.Errorf("Of(%v) = %v %v, want %s", c.hz, b.Name, ok, c.want)
		}
	}
	if _, ok := Of(9_000_000); ok {
		t.Error("9 MHz is in no band")
	}
}

func TestKnownNormalize(t *testing.T) {
	if !Known(Normalize(" 20M ")) || Known("21m") || !Known("2.5mm") {
		t.Error("Known/Normalize")
	}
}
