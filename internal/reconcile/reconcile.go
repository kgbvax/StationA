// Package reconcile implements the antenna-selection decision logic: the priority ladder
// (integration model §5), cold-switch sequencing (§6), and the controller-map band-follow
// binding (§4, §7.1). It is deliberately free of MQTT and I/O so it can be unit-tested in
// full; the mqtt package feeds it Inputs and acts on the Actions it returns.
package reconcile

import "antennaselect/internal/config"

// TX states as seen on radio/state.
const (
	TXReceive  = "rx"
	TXTransmit = "tx"
)

// Ladder sources (integration model §5), published on antenna-select/state.source.
const (
	SourceIdle     = "idle"
	SourceOperator = "operator"
	SourceAuto     = "auto"
)

// Derived operating modes.
const (
	ModeAuto   = "auto"
	ModeManual = "manual"
)

const (
	// requestAuto releases an operator hold.
	requestAuto = "auto"
	// PortOff is the switch's no-radiate / grounded position.
	PortOff = "off"
)

// Inputs is the latest known state the reconciler resolves over. Zero values mean
// "unknown"; RadioOnline gates whether radio-derived fields may be trusted (§10).
type Inputs struct {
	RadioOnline     bool
	RadioBand       string // canonical band name, e.g. "20m"
	RadioFreqHz     int64
	RadioTX         string // TXReceive | TXTransmit | "" (unknown)
	StationActivity string // "active" | "inactive" | "" (unknown -> treated as active)
	OperatorRequest string // "" | requestAuto | "off" | "p1".."p5"
	SwitchSelected  string // "off" | "p1".."p5" | "" (unknown)
	SwitchSettled   bool
}

// Decision is the resolved intent: which port the reconciler wants and why.
type Decision struct {
	Mode   string // ModeAuto | ModeManual (derived: manual iff an operator hold is active)
	Target string // "off" | "p1".."p5"; empty when unresolvable (hold last)
	Source string // SourceIdle | SourceOperator | SourceAuto
}

// Actions is what the reconciler wants the MQTT layer to emit after an input change.
type Actions struct {
	Decision Decision

	// SelectPort is non-empty when a switch select should be emitted now. It is withheld
	// (empty) when the target already matches, is unresolvable, or a port change is being
	// deferred because the radio is transmitting (see DeferredForTX).
	SelectPort string

	// DeferredForTX is true when a needed port change is being held back because the radio
	// is in TX (cold-switch discipline, §6). The MQTT layer should log it and re-resolve
	// when the radio returns to RX.
	DeferredForTX bool

	// FollowFreqHz is non-zero when the band-follow binding should push this frequency
	// to the configured controller slot (only while the followed antenna is selected).
	FollowFreqHz int64
}

// Reconciler holds config and the followed resource's port resolved from the wiring map.
// It is stateless with respect to inputs — every decision is a pure function of
// (config, Inputs) — which keeps it trivially testable.
type Reconciler struct {
	cfg            config.Config
	resourceToPort map[string]string
	followPort     string // port of band_follow.resource; empty = band-follow disabled
}

// New builds a Reconciler from validated config.
func New(cfg config.Config) *Reconciler {
	r2p := cfg.ResourceToPort()
	return &Reconciler{
		cfg:            cfg,
		resourceToPort: r2p,
		followPort:     r2p[cfg.BandFollow.Resource],
	}
}

// holdActive reports whether an operator hold is in effect. An empty request or the
// explicit "auto" release both mean no hold.
func holdActive(request string) bool {
	return request != "" && request != requestAuto
}

// Resolve applies the priority ladder. The returned bool is false when the auto tier
// cannot resolve a target (radio offline or band unknown and no higher tier asserting),
// in which case Target is empty and the caller should hold the last selection.
func (r *Reconciler) Resolve(in Inputs) Decision {
	mode := ModeAuto
	if holdActive(in.OperatorRequest) {
		mode = ModeManual
	}

	// Tier 1 — idle. Station inactive forces off and overrides everything, including an
	// operator hold (walk-away safety, §10). Unknown activity is treated as active.
	if in.StationActivity == "inactive" {
		return Decision{Mode: mode, Target: PortOff, Source: SourceIdle}
	}

	// Tier 2 — operator hold.
	if holdActive(in.OperatorRequest) {
		return Decision{Mode: mode, Target: in.OperatorRequest, Source: SourceOperator}
	}

	// Tier 3 — auto: band policy. Never trust radio state unless the radio is online (§10).
	if in.RadioOnline {
		if port, ok := r.portForBand(in.RadioBand); ok {
			return Decision{Mode: mode, Target: port, Source: SourceAuto}
		}
	}
	// Unresolvable: keep the last selection (empty target = no change).
	return Decision{Mode: mode, Target: "", Source: SourceAuto}
}

// portForBand maps a band name to a port via the band policy and wiring map. Unmatched
// bands (including 160m) use the configured fallback resource. Returns ok=false only if
// even the fallback cannot be resolved (which Validate() rules out at load time).
func (r *Reconciler) portForBand(band string) (string, bool) {
	if band != "" {
		for resource, bands := range r.cfg.BandPolicy.Bands {
			for _, b := range bands {
				if b == band {
					if port, ok := r.resourceToPort[resource]; ok {
						return port, true
					}
				}
			}
		}
	}
	if port, ok := r.resourceToPort[r.cfg.BandPolicy.Fallback]; ok {
		return port, true
	}
	return "", false
}

// Next resolves the decision and determines the concrete actions to emit, applying
// cold-switch sequencing and the band-follow binding.
func (r *Reconciler) Next(in Inputs) Actions {
	d := r.Resolve(in)
	act := Actions{Decision: d}

	// Cold-switch sequencing (§6): only move the port when the target is known and differs
	// from what the switch actually reports, and never while transmitting.
	if d.Target != "" && d.Target != in.SwitchSelected {
		if in.RadioTX == TXTransmit {
			act.DeferredForTX = true
		} else {
			act.SelectPort = d.Target
		}
	}

	// Band-follow (§4 controller map, §7.1): drive the followed antenna's controller to
	// the radio frequency while that antenna is the selected one. Gate on the resolved
	// target so we don't tune a beam that is not in circuit.
	if r.followPort != "" && d.Target == r.followPort && in.RadioOnline && in.RadioFreqHz > 0 {
		act.FollowFreqHz = in.RadioFreqHz
	}

	return act
}
