// SPDX-License-Identifier: AGPL-3.0-or-later

package steer

import "testing"

func f(v float64) *float64 { return &v }

func TestDecide(t *testing.T) {
	base := Inputs{Lobe: 30, BidirLobe: 45, MaxAz: 360}
	cases := []struct {
		name   string
		b, az  float64
		dir    string
		band   string
		maxAz  float64
		rotate *float64
		newDir string
	}{
		// forward / reverse — lobes ±30
		{name: "front lobe, forward: nothing", b: 100, az: 90, dir: Forward},
		{name: "front lobe edge 30", b: 120, az: 90, dir: Forward},
		{name: "front lobe, reversed: flip to forward", b: 80, az: 90, dir: Reverse, newDir: Forward},
		{name: "behind: flip to reverse, no rotation", b: 260, az: 90, dir: Forward, newDir: Reverse},
		{name: "behind edge 30", b: 300, az: 90, dir: Forward, newDir: Reverse},
		{name: "behind, already reversed: nothing", b: 275, az: 90, dir: Reverse},
		{name: "wrap: front lobe across north", b: 350, az: 10, dir: Forward},
		{name: "wrap: back lobe across north", b: 170, az: 355, dir: Forward, newDir: Reverse},
		{name: "outside, forward cheaper", b: 150, az: 90, dir: Forward, rotate: f(150)},
		{name: "outside, reverse cheaper", b: 330, az: 90, dir: Forward, rotate: f(150), newDir: Reverse},
		{name: "outside, reversed, forward cheaper: rotate + flip", b: 140, az: 90, dir: Reverse, rotate: f(140), newDir: Forward},
		{name: "stop-aware: az 10, b 300 → reverse to 120 (110) beats forward to 300 (290)",
			b: 300, az: 10, dir: Forward, rotate: f(120), newDir: Reverse},
		{name: "tie → forward (az 180, b 90: fwd 90 cost 90, rev 270 cost 90)",
			b: 90, az: 180, dir: Forward, rotate: f(90)},
		{name: "north target from high az uses 360", b: 0, az: 300, dir: Forward, rotate: f(360)},

		// 6m — forward only
		{name: "6m behind: rotate, never reverse", b: 270, az: 90, dir: Forward, band: "6m", rotate: f(270)},
		{name: "6m outside, would prefer reverse: forward", b: 330, az: 90, dir: Forward, band: "6m", rotate: f(330)},

		// bidirectional — ±45 both lobes, never switch
		{name: "bidir front 45", b: 135, az: 90, dir: Bidirectional},
		{name: "bidir back 45", b: 225, az: 90, dir: Bidirectional},
		{name: "bidir outside: rotate to nearer lobe, keep bidir", b: 160, az: 90, dir: Bidirectional, rotate: f(160)},
		{name: "bidir outside: back lobe nearer", b: 340, az: 90, dir: Bidirectional, rotate: f(160)},

		// overlap
		{name: "overlap used when allowed", b: 80, az: 400, dir: Forward, maxAz: 450, rotate: f(440)},
		{name: "overlap off: reverse to 260 (140) beats forward to 80 (320)", b: 80, az: 400, dir: Forward, rotate: f(260), newDir: Reverse},

		// unknown direction
		{name: "direction unknown: plain rotation", b: 260, az: 90, dir: "", rotate: f(260)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.Bearing, in.Az, in.Direction, in.Band = tc.b, tc.az, tc.dir, tc.band
			if tc.maxAz != 0 {
				in.MaxAz = tc.maxAz
			}
			d := Decide(in)
			switch {
			case tc.rotate == nil && d.RotateTo != nil:
				t.Errorf("rotate = %v, want none (%s)", *d.RotateTo, d.Reason)
			case tc.rotate != nil && d.RotateTo == nil:
				t.Errorf("rotate = none, want %v (%s)", *tc.rotate, d.Reason)
			case tc.rotate != nil && *d.RotateTo != *tc.rotate:
				t.Errorf("rotate = %v, want %v (%s)", *d.RotateTo, *tc.rotate, d.Reason)
			}
			if d.Direction != tc.newDir {
				t.Errorf("direction = %q, want %q (%s)", d.Direction, tc.newDir, d.Reason)
			}
			if d.Reason == "" {
				t.Error("empty reason")
			}
		})
	}
}

func TestEffectiveHeading(t *testing.T) {
	if h := EffectiveHeading(90, Forward); h != 90 {
		t.Errorf("forward = %v", h)
	}
	if h := EffectiveHeading(270, Reverse); h != 90 {
		t.Errorf("reverse = %v", h)
	}
	if h := EffectiveHeading(400, Bidirectional); h != 40 {
		t.Errorf("bidir overlap = %v", h)
	}
}
