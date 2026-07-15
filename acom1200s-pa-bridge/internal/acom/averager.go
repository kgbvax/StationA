package acom

import (
	"sync"
	"time"
)

// PowerSample holds a single measurement and its timestamp.
type PowerSample struct {
	Value     uint16
	Timestamp time.Time
}

// PowerAverager is a time-based moving average for the forward-power meter,
// which is noisy frame-to-frame. Samples older than the window are dropped.
type PowerAverager struct {
	windowDuration time.Duration
	samples        []PowerSample
	mu             sync.Mutex
}

// NewPowerAverager returns an averager with the given window in milliseconds
// (clamped to >= 1ms).
func NewPowerAverager(durationMs int) *PowerAverager {
	if durationMs < 1 {
		durationMs = 1
	}
	return &PowerAverager{
		windowDuration: time.Duration(durationMs) * time.Millisecond,
		samples:        make([]PowerSample, 0, 50),
	}
}

// Add appends a sample and returns the windowed average.
func (pa *PowerAverager) Add(value uint16) uint16 {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	now := time.Now()
	pa.samples = append(pa.samples, PowerSample{Value: value, Timestamp: now})

	cutoff := now.Add(-pa.windowDuration)
	validStartIndex := 0
	for i, s := range pa.samples {
		if s.Timestamp.After(cutoff) {
			validStartIndex = i
			break
		}
	}
	if validStartIndex > 0 {
		pa.samples = pa.samples[validStartIndex:]
	}

	if len(pa.samples) == 0 {
		return 0
	}
	var sum uint32
	for _, s := range pa.samples {
		sum += uint32(s.Value)
	}
	return uint16(sum / uint32(len(pa.samples)))
}
