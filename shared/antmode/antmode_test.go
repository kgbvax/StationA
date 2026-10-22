// SPDX-License-Identifier: AGPL-3.0-or-later

package antmode

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	az := 135
	cases := []struct {
		name    string
		in      string
		want    State
		locked  bool
		wantErr bool
	}{
		{name: "empty is normal", in: "", want: NormalState},
		{name: "whitespace is normal", in: " \n", want: NormalState},
		{name: "normal drops stray lock", in: `{"ant_mode":"normal","lock":{"seq":3,"ground":true}}`, want: NormalState},
		{name: "storm without lock is zero lock", in: `{"ant_mode":"storm"}`, want: State{Mode: Storm, Lock: &Lock{}}, locked: true},
		{name: "full storm", in: `{"ant_mode":"storm","lock":{"seq":7,"ground":true,"retract":true,"park_az":135},"power_hold":true,"wind":{"dir_deg":225}}`,
			want: State{Mode: Storm, Lock: &Lock{Seq: 7, Ground: true, Retract: true, ParkAz: &az}, PowerHold: true}, locked: true},
		{name: "maintenance nudge", in: `{"ant_mode":"maintenance","lock":{"seq":1,"nudge":true}}`,
			want: State{Mode: Maintenance, Lock: &Lock{Seq: 1, Nudge: true}}, locked: true},
		{name: "bad json", in: `{"ant_mode":`, wantErr: true},
		{name: "unknown mode", in: `{"ant_mode":"hurricane"}`, wantErr: true},
		{name: "missing mode", in: `{"lock":{}}`, wantErr: true},
		{name: "park out of range", in: `{"ant_mode":"storm","lock":{"park_az":451}}`, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse([]byte(c.in))
			if c.wantErr {
				if !errors.Is(err, ErrMalformed) {
					t.Fatalf("err = %v, want ErrMalformed", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Mode != c.want.Mode || got.PowerHold != c.want.PowerHold || (got.Lock == nil) != (c.want.Lock == nil) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			if got.Lock != nil {
				g, w := got.L(), c.want.L()
				if g.Seq != w.Seq || g.Ground != w.Ground || g.Retract != w.Retract || g.Nudge != w.Nudge ||
					(g.ParkAz == nil) != (w.ParkAz == nil) || (g.ParkAz != nil && *g.ParkAz != *w.ParkAz) {
					t.Fatalf("lock %+v, want %+v", g, w)
				}
			}
			if got.Locked() != c.locked {
				t.Fatalf("Locked = %v, want %v", got.Locked(), c.locked)
			}
		})
	}
}

func TestLabel(t *testing.T) {
	if NormalState.Label() != "" {
		t.Fatal("normal label not empty")
	}
	if (State{Mode: Storm}).Label() != "storm" {
		t.Fatal("storm label")
	}
	if (State{}).Locked() {
		t.Fatal("zero State must not be locked")
	}
}
