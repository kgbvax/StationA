package rotor

import (
	"encoding/json"
	"testing"
)

func TestIsMoving(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{"", false},
		{"stopped", false},
		{"idle", false},
		{"rotating", true},
		{"Rotating", true},
		{"moving", true},
		{"ROTATING-CCW", true},
	}
	for _, c := range cases {
		if got := IsMoving(c.state); got != c.want {
			t.Errorf("IsMoving(%q) = %v, want %v", c.state, got, c.want)
		}
	}
}

func TestFromStatus(t *testing.T) {
	s := RotorStatus{State: "rotating", Az: 123.5, TDeg: 180, FMsg: ""}
	st := FromStatus(s, true)
	if st.Az != 123.5 {
		t.Errorf("az = %v", st.Az)
	}
	if !st.Moving {
		t.Error("moving should be true for state=rotating")
	}
	if st.TargetAz != 180 {
		t.Errorf("target_az = %v, want 180", st.TargetAz)
	}
	if !st.DeviceOnline {
		t.Error("device_online should be true")
	}
}

func TestFromStatusOmitsZeroTarget(t *testing.T) {
	s := RotorStatus{State: "stopped", Az: 90}
	st := FromStatus(s, true)
	if st.TargetAz != 0 {
		t.Errorf("target_az = %v, want 0 (omitted)", st.TargetAz)
	}
	if st.Moving {
		t.Error("moving should be false for state=stopped")
	}
}

func TestRotorCommandMarshal(t *testing.T) {
	// Numeric command → {"az":180}.
	b, _ := json.Marshal(RotorCommand{Az: 180})
	if string(b) != `{"az":180}` {
		t.Errorf("numeric cmd = %s, want {\"az\":180}", b)
	}
	// String command → {"az":"stop"}.
	b, _ = json.Marshal(RotorCommand{Az: "stop"})
	if string(b) != `{"az":"stop"}` {
		t.Errorf("stop cmd = %s, want {\"az\":\"stop\"}", b)
	}
}
