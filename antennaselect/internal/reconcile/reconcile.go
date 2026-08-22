// Package reconcile implements the antenna-selection decision logic: the priority ladder
// (integration model §5), cold-switch sequencing (§6), and the controller-map band-follow
// binding (§4, §7.1). It is deliberately free of MQTT and I/O so it can be unit-tested in
// full; the mqtt package feeds it Inputs and acts on the Actions it returns.
package reconcile

import (
	"sort"

	"antennaselect/internal/config"
)

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
// "unknown"; RadioOnline gates whether radio-derived fields may be trusted (§10). It is
// the AND of bridge liveness (radio/status LWT) and radio-link liveness
// (radio/state.device_online) — see the mqtt layer, which computes it.
type Inputs struct {
	RadioOnline     bool
	RadioBand       string // canonical band name, e.g. "20m"
	RadioFreqHz     int64
	RadioTX         string // TXReceive | TXTransmit | "" (unknown)
	StationActivity string // "active" | "inactive" | "" (unknown -> treated as active)
	OperatorRequest string // "" | requestAuto | "off" | any switch port (e.g. "port1", "port3", "port6")
	SwitchSelected  string // "off" | any switch port | "" (unknown)
	SwitchSettled   bool
}

// Decision is the resolved intent: which port the reconciler wants and why.
type Decision struct {
	Mode   string // ModeAuto | ModeManual (derived: manual iff an operator hold is active)
	Target string // "off" | any switch port; empty when unresolvable (hold last)
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

	// SetBand is non-empty when the PA band-follow binding should push this band label to
	// the configured PA slot's /cmd (model §7.1 soft binding pa.set_band ← radio.band).
	// The PA is always in the RF path, so (unlike FollowFreqHz) this is NOT gated on
	// antenna selection — only on radio online + a known band. No TX gate: PA hot-switch
	// protection is hardware (model/ACOM design).
	SetBand string

	// SetInline is non-nil when the tuner-follow binding should push an in-line/bypass
	// intent to the configured tuner slot's /cmd (model §7.1 soft binding
	// tuner.set_inline ← band_policy, §10 residual). nil = no emit; non-nil = emit that
	// bool (true = ATU in line, false = bypass). Engages the ATU when the selected
	// resource is the tuner's resource AND the band is non-resonant; bypasses otherwise.
	// Gated on radio online + a known band (§10), like SetBand.
	SetInline *bool
}

// Reconciler holds config and the followed resource's port resolved from the wiring map.
// It is stateless with respect to inputs — every decision is a pure function of
// (config, Inputs) — which keeps it trivially testable.
type Reconciler struct {
	cfg             config.Config
	resourceToPort  map[string]string
	policyResources []string // band_policy resource names in stable sorted order
	followPort      string   // port of band_follow.resource; empty = band-follow disabled
	tunerPort       string   // port of tuner_follow.resource; empty = tuner-follow disabled
}

// New builds a Reconciler from validated config.
func New(cfg config.Config) *Reconciler {
	r2p := cfg.ResourceToPort()
	resources := make([]string, 0, len(cfg.BandPolicy.Bands))
	for r := range cfg.BandPolicy.Bands {
		resources = append(resources, r)
	}
	sort.Strings(resources)
	return &Reconciler{
		cfg:             cfg,
		resourceToPort:  r2p,
		policyResources: resources,
		followPort:      r2p[cfg.BandFollow.Resource],
		tunerPort:       r2p[cfg.TunerFollow.Resource],
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
	// An empty band is a transient "no slice reported yet" / reconnect-Reset state from
	// flexbridge, not a tuning intent: resolving it to the fallback would chatter the
	// antenna to the fallback resource and back on every reconnect cycle. Hold the last
	// selection instead — only a known-but-unmatched band (160m, gen, …) reaches the
	// fallback via portForBand.
	if in.RadioOnline && in.RadioBand != "" {
		if port, ok := r.portForBand(in.RadioBand); ok {
			return Decision{Mode: mode, Target: port, Source: SourceAuto}
		}
	}
	// Unresolvable: keep the last selection (empty target = no change).
	return Decision{Mode: mode, Target: "", Source: SourceAuto}
}

// portForBand maps a band name to a port via the band policy and wiring map. Unmatched
// bands (including 160m and the out-of-band "gen" marker) use the configured fallback
// resource. Returns ok=false only if even the fallback cannot be resolved (which
// Validate() rules out at load time).
//
// An empty band never reaches here from Resolve, which holds the last selection for that
// transient state instead of falling through to the fallback; the `band != ""` guard below
// is defense-in-depth should portForBand be called from elsewhere.
//
// The scan uses policyResources (sorted at construction) instead of ranging over the
// band_policy map so the same config always resolves to the same port, even if a band
// is listed under multiple resources. Validate() rejects overlapping bands that map to
// different ports; harmless aliases mapping to the same port resolve deterministically.
func (r *Reconciler) portForBand(band string) (string, bool) {
	if band != "" {
		for _, resource := range r.policyResources {
			for _, b := range r.cfg.BandPolicy.Bands[resource] {
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

	// PA band-follow (§7.1 soft binding pa.set_band ← radio.band): pre-position the amp to
	// the radio's band so the ACOM (which auto-bands by RF sense) doesn't trip on the 1st
	// TX on a new band. The PA is always in the RF path, so this is NOT gated on antenna
	// selection — only on radio online (§10: don't trust radio state otherwise) + a known
	// band. No TX guard: hot-switch protection is hardware.
	if r.cfg.PAFollow.Enabled && in.RadioOnline && in.RadioBand != "" {
		act.SetBand = in.RadioBand
	}

	// Tuner follow (§7.1 soft binding tuner.set_inline ← band_policy, §10 residual):
	// engage the ATU in-line when the selected resource is its resource AND the band is
	// non-resonant; bypass it otherwise (so leaving a non-resonant band drops the ATU out
	// of line). Gated on radio online + a known band (§10). The ATU engages only while the
	// tuner's resource is the resolved target — cold-switch sequencing above already
	// withholds a port change during TX, so the ATU is not re-keyed mid-TX.
	if r.cfg.TunerFollow.Enabled && r.tunerPort != "" && in.RadioOnline && in.RadioBand != "" {
		desired := d.Target == r.tunerPort && contains(r.cfg.TunerFollow.ATUBands, in.RadioBand)
		act.SetInline = &desired
	}

	return act
}

// contains reports whether s lists v. A nil/empty slice matches nothing.
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
