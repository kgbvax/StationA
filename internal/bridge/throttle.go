// Package bridge wires the FlexRadio protocol layer to MQTT and Home
// Assistant discovery. It owns the meter registry, applies per-meter
// throttling and duplicate removal, generates HA discovery payloads, and
// publishes state to the broker.
package bridge

import (
	"math"
	"sync"
	"time"

	"flex2mqtt/internal/flexradio"
)

// Gate implements per-meter publish throttling plus duplicate removal.
//
// A value passes the gate iff (a) at least the meter's min interval has
// elapsed since its last publish, AND (b) the value, rounded to its unit
// deadband, differs from the last published (rounded) value.
//
// This deterministically bounds broker load regardless of the 10-20 fps
// stream rate, while still surfacing any change that crosses the deadband
// as soon as the interval window opens.
type Gate struct {
	mu    sync.Mutex
	rates map[flexradio.MeterGroup]time.Duration
	last  map[string]gateEntry
	now   func() time.Time
}

type gateEntry struct {
	lastPublish time.Time
	lastValue   float64 // rounded to deadband at publish time
	hasValue    bool
}

// NewGate returns a Gate with the given per-group min intervals.
func NewGate(rates map[flexradio.MeterGroup]time.Duration) *Gate {
	return &Gate{
		rates: rates,
		last:  make(map[string]gateEntry),
		now:   time.Now,
	}
}

// SetNow injects the clock (for tests).
func (g *Gate) SetNow(f func() time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.now = f
}

// Allow reports whether key should be published now for the given value.
// It records the publish on a true return. key scopes the gate (e.g. the
// MQTT topic); group selects the min interval; unit selects the deadband.
func (g *Gate) Allow(key string, group flexradio.MeterGroup, unit string, value float64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	minInterval := g.rates[group]
	if minInterval <= 0 {
		minInterval = time.Second
	}
	deadband := flexradio.Deadband(unit)
	rounded := roundTo(value, deadband)

	e := g.last[key]
	now := g.now()
	if e.hasValue && rounded == e.lastValue {
		// Same value (within deadband): never republish. This is the
		// duplicate-removal path and wins over the interval check.
		return false
	}
	if e.hasValue && now.Sub(e.lastPublish) < minInterval {
		// Changed value but too soon since last publish: skip this round.
		// The change will be picked up when the window opens (the next
		// packet whose value differs will pass).
		return false
	}
	g.last[key] = gateEntry{
		lastPublish: now,
		lastValue:   rounded,
		hasValue:    true,
	}
	return true
}

// Reset clears the gate's memory (call on reconnect to force republish).
func (g *Gate) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k := range g.last {
		delete(g.last, k)
	}
}

// roundTo rounds v to the nearest multiple of step (0.1, 0.01, ...).
// Uses math.Round on the scaled value to get nearest, half-away-from-zero.
func roundTo(v, step float64) float64 {
	if step <= 0 {
		return v
	}
	return math.Round(v/step) * step
}
