package winpower

import (
	"testing"
	"time"
)

func TestIdleFromTicks(t *testing.T) {
	cases := []struct {
		name string
		now  uint64
		last uint32
		want time.Duration
	}{
		{"simple", 100_000, 40_000, 60 * time.Second},
		{"zero", 5_000, 5_000, 0},
		// now's low 32 bits wrapped past 0; last input was just before the wrap.
		{"wrap", 1<<32 + 1_000, 1<<32 - 2_000, 3 * time.Second},
		// 64-bit tick count far past one wrap; only the low 32 bits matter.
		{"high bits", 5<<32 + 90_000, 30_000, 60 * time.Second},
	}
	for _, c := range cases {
		if got := idleFromTicks(c.now, c.last); got != c.want {
			t.Errorf("%s: idleFromTicks(%d, %d) = %v, want %v", c.name, c.now, c.last, got, c.want)
		}
	}
}
