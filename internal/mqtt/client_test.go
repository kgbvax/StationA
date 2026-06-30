package mqtt

import (
	"testing"
	"time"

	"ubctrl/internal/ub/service"
)

func TestBandOptionsHaveCenters(t *testing.T) {
	for _, b := range bandOptions {
		khz, ok := bandCenterKHz[b]
		if !ok {
			t.Errorf("band option %q has no center frequency", b)
		}
		if khz == 0 {
			t.Errorf("band %q center is zero", b)
		}
	}
	if got := bandCenterKHz["6m"]; got != 51000 {
		t.Errorf("6m center = %d, want 51000", got)
	}
	if got := bandCenterKHz["20m"]; got != 14175 {
		t.Errorf("20m center = %d, want 14175", got)
	}
}

func TestStateSnapshotIgnoresUpdatedAt(t *testing.T) {
	s1 := service.State{
		FrequencyKHz: 14000,
		BandName:     "20m",
		BandIndex:    4,
		ModeName:     "forward",
		MotorsMoving: false,
		MotorBits:    0,
		Offline:      false,
		LastError:    "",
		UpdatedAt:    time.Date(2026, 6, 7, 10, 0, 0, 0, time.UTC),
	}
	s2 := s1
	s2.UpdatedAt = s1.UpdatedAt.Add(5 * time.Second)

	snap1 := stateSnapshot(s1)
	snap2 := stateSnapshot(s2)

	if snap1 != snap2 {
		t.Fatalf("snapshots should be equal when only updated_at changes: %#v vs %#v", snap1, snap2)
	}
}

func TestShouldPublishStateDeduplicates(t *testing.T) {
	c := &Client{}
	base := publishedState{
		FrequencyKHz: 14000,
		BandName:     "20m",
		BandIndex:    4,
		ModeName:     "forward",
		MotorsMoving: false,
		MotorBits:    0,
		Offline:      false,
		LastError:    "",
	}

	if !c.shouldPublishState(base) {
		t.Fatal("first state should be published")
	}
	if c.shouldPublishState(base) {
		t.Fatal("identical state should not be published")
	}

	changed := base
	changed.ModeName = "reverse"
	if !c.shouldPublishState(changed) {
		t.Fatal("changed state should be published")
	}
	if c.shouldPublishState(changed) {
		t.Fatal("same changed state should not be published again")
	}
}

func TestAvailabilityPayload(t *testing.T) {
	online := service.State{Offline: false}
	offline := service.State{Offline: true}

	if got := availabilityPayload(online); got != "online" {
		t.Fatalf("online payload = %q, want %q", got, "online")
	}
	if got := availabilityPayload(offline); got != "offline" {
		t.Fatalf("offline payload = %q, want %q", got, "offline")
	}
}
