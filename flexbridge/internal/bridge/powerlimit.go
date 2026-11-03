package bridge

import (
	"context"
	"strconv"
	"strings"
	"time"

	"flexbridge/internal/flexradio"
)

// Power ceiling (set_rf_power_limit).
//
// The bridge holds an RF power CEILING set over /cmd by the tx-guard logic slot
// (muehle/hf/tx-guard caps the FLEX while the HF PA is live and armed). The
// ceiling is enforced here, bridge-locally, against the radio's `transmit`
// status stream: an operator slider drag, a band change (band persistence
// restores a per-band rfpower) or a TX profile load is clamped within one
// status frame, with no MQTT round trip. This is a carve-out from the
// read-only boundary, not a setpoint: the bridge only ever LOWERS rfpower /
// tunepower, or sets the radio's own max_power_level ceiling. It never raises
// output and never restores a remembered power when the ceiling lifts.
//
// Mechanisms (config power_limit.mechanism):
//
//	clamp   lower rfpower/tunepower whenever the radio reports them above the
//	        ceiling (default; works on any firmware that accepts transmit set)
//	native  drive the radio-side max_power_level (the radio enforces it, and
//	        it persists in the radio if the bridge dies). With native the
//	        bridge OWNS max_power_level: the ceiling while one is set, 100 once
//	        it is lifted.
//	both    native + clamp
//
// Lifetime: the ceiling lives for the bridge process. It survives radio
// reconnects (Reset keeps it) but not a bridge restart — tx-guard re-sends it
// whenever this bridge's /status goes online and re-asserts it periodically.
// Until the first set_rf_power_limit arrives the ceiling is UNKNOWN and the
// enforcer does nothing at all (in particular it does not lift a persisted
// native ceiling), so a restart without tx-guard fails closed.

// Power-limit mechanisms (config power_limit.mechanism).
const (
	MechanismClamp  = "clamp"
	MechanismNative = "native"
	MechanismBoth   = "both"
)

const (
	// powerLimitTick is the enforcer's re-check period. Kicks (transmit status,
	// new ceiling, reconnect) run it immediately; the tick retries a command
	// the radio has not yet confirmed.
	powerLimitTick = 5 * time.Second
	// powerLimitResend is the minimum spacing between two identical commands,
	// so a burst of status frames cannot turn into a command storm.
	powerLimitResend = time.Second
	// powerLimitWarnAfter is how many consecutive unconfirmed attempts of the
	// same command trigger a Warn (the radio is not accepting the ceiling).
	powerLimitWarnAfter = 3
	// maxPowerLevelFull is max_power_level with no ceiling.
	maxPowerLevelFull = 100
)

// optInt is an optional int with value semantics (radioState is copied as a
// snapshot and published outside the lock, so no pointers into it).
type optInt struct {
	v  int
	ok bool
}

func some(v int) optInt { return optInt{v: v, ok: true} }

func (o optInt) ptr() *int {
	if !o.ok {
		return nil
	}
	v := o.v
	return &v
}

// txKnob names the transmit setting a txCmd writes.
type txKnob int

const (
	knobRF txKnob = iota
	knobTune
	knobMax
)

func (k txKnob) String() string {
	switch k {
	case knobRF:
		return "rfpower"
	case knobTune:
		return "tunepower"
	default:
		return "max_power_level"
	}
}

// txCmd is one `transmit set <knob>=<level>` the enforcer sends.
type txCmd struct {
	knob  txKnob
	level int
}

// txObs is the radio-reported transmit power state.
type txObs struct {
	rf, tune, max optInt
}

// planLimit returns the commands that bring the observed transmit state into
// line with the ceiling. It is pure. limitKnown=false (no set_rf_power_limit
// received yet) plans nothing. clamp only ever lowers a KNOWN value — sending
// the ceiling to an unknown value could raise it. native owns max_power_level:
// the ceiling while one is set (sent even when unknown: a ceiling never raises
// output), back to 100 once lifted (only when known to be below 100).
func planLimit(limitKnown bool, limit optInt, obs txObs, mech string) []txCmd {
	if !limitKnown {
		return nil
	}
	native := mech == MechanismNative || mech == MechanismBoth
	clamp := mech != MechanismNative // clamp, both, and any unrecognized value
	var out []txCmd
	if native {
		switch {
		case limit.ok && (!obs.max.ok || obs.max.v != limit.v):
			out = append(out, txCmd{knobMax, limit.v})
		case !limit.ok && obs.max.ok && obs.max.v < maxPowerLevelFull:
			out = append(out, txCmd{knobMax, maxPowerLevelFull})
		}
	}
	if clamp && limit.ok {
		if obs.rf.ok && obs.rf.v > limit.v {
			out = append(out, txCmd{knobRF, limit.v})
		}
		if obs.tune.ok && obs.tune.v > limit.v {
			out = append(out, txCmd{knobTune, limit.v})
		}
	}
	return out
}

// setPowerLimit applies a set_rf_power_limit value: "1".."100" (watts) sets
// the ceiling, "off" lifts it. It works with the radio offline — the ceiling
// is held and enforced once the radio is back. /state is republished every
// time so a re-assert from tx-guard always finds /state current.
func (b *Bridge) setPowerLimit(value string) {
	v := strings.ToLower(strings.TrimSpace(value))
	var next optInt
	if v != "off" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			b.log.Warnf("cmd set_rf_power_limit: bad value %q (want 1..100 W or \"off\")", value)
			return
		}
		next = some(n)
	}
	b.mu.Lock()
	prev, prevKnown := b.state.rfPowerLimit, b.limitKnown
	b.state.rfPowerLimit = next
	b.limitKnown = true
	snap := b.state
	b.mu.Unlock()
	if !prevKnown || prev != next {
		b.log.Infof("cmd set_rf_power_limit: %s -> %s", fmtLimit(prevKnown, prev), fmtLimit(true, next))
	} else {
		b.log.Debugf("cmd set_rf_power_limit: %s (unchanged)", fmtLimit(true, next))
	}
	b.publishStateSnapshot(snap)
	b.kickLimit()
}

func fmtLimit(known bool, l optInt) string {
	switch {
	case !known:
		return "unknown"
	case !l.ok:
		return "off"
	default:
		return strconv.Itoa(l.v) + " W"
	}
}

// handleTransmit merges a "transmit" status frame's power fields into state.
// The radio splits transmit state over several frames, so only the fields a
// frame carries are applied. Frames with topic args (e.g. per-band TX
// settings) are not the live transmit state and are ignored.
func (b *Bridge) handleTransmit(f flexradio.Frame) {
	if strings.TrimSpace(f.TopicArgs) != "" {
		b.log.Debugf("transmit frame with topic args ignored: %s", f.RawBody)
		return
	}
	ts := flexradio.ParseTransmit(fieldsString(f))
	b.mu.Lock()
	changed := false
	if ts.HasRF && b.state.rfPower != some(ts.RFPower) {
		b.state.rfPower = some(ts.RFPower)
		changed = true
	}
	if ts.HasTune && b.state.tunePower != some(ts.TunePower) {
		b.state.tunePower = some(ts.TunePower)
		changed = true
	}
	if ts.HasMax && b.state.maxPowerLevel != some(ts.MaxPowerLevel) {
		b.state.maxPowerLevel = some(ts.MaxPowerLevel)
		changed = true
	}
	snap := b.state
	b.mu.Unlock()
	if changed {
		b.publishStateSnapshot(snap)
		b.kickLimit()
	}
}

// kickLimit wakes the enforcer without blocking (the channel holds one
// pending kick; further kicks coalesce into it).
func (b *Bridge) kickLimit() {
	select {
	case b.limitKick <- struct{}{}:
	default:
	}
}

// RunPowerLimit is the ceiling enforcer. Run it once for the process
// lifetime (`go b.RunPowerLimit(ctx)` next to StateHeartbeat). It runs on its
// own goroutine so neither the radio read loop nor the /cmd worker blocks on
// a TCP write; commands go out through the current Commander, outside b.mu.
func (b *Bridge) RunPowerLimit(ctx context.Context) {
	t := time.NewTicker(powerLimitTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.limitKick:
		case <-t.C:
		}
		b.enforceLimit(time.Now())
	}
}

// enforceLimit plans and sends the commands for the current snapshot. The
// limSent/limTries bookkeeping is owned by the enforcer goroutine.
func (b *Bridge) enforceLimit(now time.Time) {
	b.mu.RLock()
	cmd := b.commander
	known := b.limitKnown
	limit := b.state.rfPowerLimit
	obs := txObs{rf: b.state.rfPower, tune: b.state.tunePower, max: b.state.maxPowerLevel}
	b.mu.RUnlock()
	if cmd == nil {
		return
	}
	plan := planLimit(known, limit, obs, b.cfg.PowerLimitMechanism)

	// Forget tries for commands no longer needed: the radio confirmed them.
	pending := make(map[txCmd]bool, len(plan))
	for _, c := range plan {
		pending[c] = true
	}
	for c := range b.limTries {
		if !pending[c] {
			delete(b.limTries, c)
			delete(b.limSent, c)
		}
	}

	for _, c := range plan {
		if last, ok := b.limSent[c]; ok && now.Sub(last) < powerLimitResend {
			continue
		}
		b.limSent[c] = now
		b.limTries[c]++
		n := b.limTries[c]
		switch {
		case n == 1:
			b.log.Infof("rf power limit %s: transmit set %s=%d (radio rfpower=%s tunepower=%s max_power_level=%s)",
				fmtLimit(known, limit), c.knob, c.level, fmtOpt(obs.rf), fmtOpt(obs.tune), fmtOpt(obs.max))
		case n == powerLimitWarnAfter:
			b.log.Warnf("rf power limit %s: radio has not confirmed %s=%d after %d attempts", fmtLimit(known, limit), c.knob, c.level, n)
		default:
			b.log.Debugf("rf power limit: resend %s=%d (attempt %d)", c.knob, c.level, n)
		}
		var err error
		switch c.knob {
		case knobRF:
			err = cmd.SetRFPower(c.level)
		case knobTune:
			err = cmd.SetTunePower(c.level)
		case knobMax:
			err = cmd.SetMaxPowerLevel(c.level)
		}
		if err != nil {
			b.log.Warnf("rf power limit: transmit set %s=%d: %v", c.knob, c.level, err)
		}
	}
}

func fmtOpt(o optInt) string {
	if !o.ok {
		return "?"
	}
	return strconv.Itoa(o.v)
}
