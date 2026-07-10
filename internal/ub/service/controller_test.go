package service

import (
	"context"
	"testing"

	"ultrabridge/internal/ub/transport"
)

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

// TestSetFrequencyDeadband verifies the RCU-06 is only told to retune when the commanded
// frequency differs from the current setting by at least freqDeadbandKHz. The mock records
// the last commanded frequency, so a suppressed command leaves it unchanged.
func TestSetFrequencyDeadband(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name     string
		delta    uint16 // change from the mock's current frequency
		wantSent bool   // whether the change-frequency command should reach the device
	}{
		{"within deadband by 1 kHz", 1, false},
		{"within deadband by 24 kHz", 24, false},
		{"exactly 25 kHz (at least)", 25, true},
		{"well beyond deadband", 500, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := NewController(transport.NewMock())
			if err := ctrl.Refresh(ctx); err != nil {
				t.Fatalf("initial refresh: %v", err)
			}
			cur := ctrl.State().FrequencyKHz
			target := cur + tc.delta

			if err := ctrl.SetFrequency(ctx, target, ctrl.State().ModeName); err != nil {
				t.Fatalf("SetFrequency: %v", err)
			}
			got := ctrl.State().FrequencyKHz // sendFrequency refreshes; a no-op leaves the old value
			if tc.wantSent {
				if got != target {
					t.Errorf("frequency = %d kHz, want %d (command should have been sent)", got, target)
				}
			} else {
				if got != cur {
					t.Errorf("frequency = %d kHz, want %d (command should have been suppressed by deadband)", got, cur)
				}
			}
		})
	}
}

// TestSetFrequencyUnknownCurrentSkipsDeadband verifies that when the current frequency is
// unknown (0 — before the first successful refresh), the deadband is not applied and the
// command always goes through even if it would otherwise fall inside the band.
func TestSetFrequencyUnknownCurrentSkipsDeadband(t *testing.T) {
	ctx := context.Background()
	ctrl := NewController(transport.NewMock()) // state.FrequencyKHz == 0, never refreshed

	// The mock defaults to 14000 kHz; asking for 14001 is within the deadband, but since
	// the controller does not yet know its frequency, the command must be sent.
	if err := ctrl.SetFrequency(ctx, 14001, "forward"); err != nil {
		t.Fatalf("SetFrequency: %v", err)
	}
	if got := ctrl.State().FrequencyKHz; got != 14001 {
		t.Errorf("frequency = %d kHz, want 14001 (deadband must be skipped when current is unknown)", got)
	}
}

// TestSetModeBypassesDeadband verifies a direction change at the current frequency still
// reaches the controller — the deadband suppresses retunes, not direction flips.
func TestSetModeBypassesDeadband(t *testing.T) {
	ctx := context.Background()
	ctrl := NewController(transport.NewMock())
	if err := ctrl.Refresh(ctx); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	cur := ctrl.State().FrequencyKHz
	if cur == 0 {
		t.Fatal("precondition: current frequency should be known after refresh")
	}

	if err := ctrl.SetMode(ctx, "reverse"); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	state := ctrl.State()
	if state.ModeName != "reverse" {
		t.Errorf("mode = %q, want reverse (direction change must not be suppressed by deadband)", state.ModeName)
	}
	if state.FrequencyKHz != cur {
		t.Errorf("frequency = %d kHz, want %d (direction change must keep the frequency)", state.FrequencyKHz, cur)
	}
}
