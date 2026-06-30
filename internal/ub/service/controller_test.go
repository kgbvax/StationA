package service

import "testing"

func TestBandName(t *testing.T) {
	cases := []struct {
		freq uint16
		want string
	}{
		{14175, "20m"},
		{18118, "17m"},
		{21225, "15m"},
		{24940, "12m"},
		{28850, "10m"},
		{51000, "6m"},
		{50000, "6m"},
		{53999, "6m"},
		{14350, "band-0"}, // upper edge is exclusive
	}
	for _, c := range cases {
		if got := bandName(c.freq, 0); got != c.want {
			t.Errorf("bandName(%d) = %q, want %q", c.freq, got, c.want)
		}
	}
}
