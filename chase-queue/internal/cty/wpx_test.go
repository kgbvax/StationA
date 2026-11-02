package cty

import "testing"

func TestWPX(t *testing.T) {
	cases := []struct {
		call string
		want string // "" = no prefix
	}{
		{"DL1ABC", "DL1"},
		{"dl1abc", "DL1"},
		{"DL1ABC/P", "DL1"},
		{"DL1ABC/QRP", "DL1"},
		{"DL1ABC-7", "DL1"},
		{"EA8/DL1ABC", "EA8"},
		{"DL1ABC/EA8", "EA8"},
		{"KH6/W1ABC", "KH6"},
		{"EA/DL1ABC", "EA0"},
		{"W1ABC/4", "W4"},
		{"RAEM", "RA0"},
		{"9A1A", "9A1"},
		{"HB9ABC", "HB9"},
		{"VK9XX/MM", ""},
		{"", ""},
	}
	for _, c := range cases {
		got, ok := WPX(c.call)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("WPX(%q) = %q, %v; want %q", c.call, got, ok, c.want)
		}
	}
}
