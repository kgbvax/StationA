package calls

import "testing"

func TestSameStation(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"DL1ABC", "dl1abc", true},
		{"EA8/DL1ABC", "DL1ABC", true},
		{"DL1ABC/P", "DL1ABC", true},
		{"VP2E/W1AW", "W1AW", true},
		{"VP2E/W1AW", "VP2E/K1AB", false},
		{"DL1ABC", "", false},
	}
	for _, c := range cases {
		if got := SameStation(c.a, c.b); got != c.want {
			t.Errorf("SameStation(%q,%q) = %v", c.a, c.b, got)
		}
	}
}
