// Package config loads the antenna-select reconciler's persistent configuration.
//
// Precedence (highest wins): explicit CLI flag > config-file value > built-in default.
// The config file is a single TOML document that also carries the MQTT password, so on
// the target machine it must be 0600. See docs/conventions/config-and-secrets.md.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// MQTT holds the broker settings. Password is a secret; the file that contains it must
// be 0600 on the target machine.
type MQTT struct {
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	Site     string `toml:"site"`
	Station  string `toml:"station"`
	Slot     string `toml:"slot"`
	User     string `toml:"user"`
	Password string `toml:"password"`
}

// BandPolicy maps antenna resources to the band names they serve (auto tier). Bands is
// resource-name -> band-name list; Fallback is the resource used for any band not listed
// (including 160m). Resource names must appear as values in the wiring map.
type BandPolicy struct {
	Bands    map[string][]string `toml:"bands"`
	Fallback string              `toml:"fallback"`
}

// BandFollow is the controller-map binding (integration model §4): it makes the antenna
// behind Resource track the radio frequency by emitting intents to the controller slot
// named by Slot. Resource must be a wiring-map resource name; Slot is a slot address
// segment (canonical role, e.g. "ant-ctrl"). An empty Resource disables band-follow.
// Which antenna follows the radio is site configuration, never code.
type BandFollow struct {
	Resource string `toml:"resource"`
	Slot     string `toml:"slot"`
}

// PAFollow is the PA band-follow binding (integration model §7.1 soft binding
// pa.set_band ← radio.band): it makes the reconciler push the radio's band to the PA
// slot's /cmd so the amp is pre-positioned before transmit (the ACOM auto-bands by
// sensing the RF drive, which trips the amp on the 1st TX on a new band). Unlike
// BandFollow, the PA is NOT a passive wiring-map resource — it is always in the RF path
// regardless of which antenna is selected — so this binding is NOT gated on a switch
// port / antenna selection. Enabled defaults false; the deploy seed turns it on. No TX
// guard: PA hot-switch protection is handled in hardware (model/ACOM design).
type PAFollow struct {
	Enabled bool   `toml:"enabled"`
	Slot    string `toml:"slot"` // target slot, default "pa" (canonical role)
}

// TunerFollow is the ATU in-line binding (integration model §7.1 soft binding
// tuner.set_inline ← band_policy, §10 residual): it makes the reconciler engage the
// ATU in-line when the selected antenna is the (non-resonant) Resource and the band
// is one of ATUBands, and bypass it otherwise. Like BandFollow, this IS gated on
// antenna selection — the ATU only matters when its resource is in circuit. Unlike
// PAFollow, the tuner /cmd is NOT retained (self-heal from the retained radio/state
// replay at reconnect). Enabled defaults false; the deploy seed turns it on.
type TunerFollow struct {
	Enabled  bool     `toml:"enabled"`
	Slot     string   `toml:"slot"`      // target slot, default "tuner" (canonical role)
	Resource string   `toml:"resource"`  // wiring-map resource the ATU serves (e.g. "fan-dipole")
	ATUBands []string `toml:"atu_bands"` // non-resonant bands needing the ATU in-line
}

// Idle is the walk-away safety timeout (integration model §10): after this long with no
// radio activity (a VFO/frequency change or a transmit), the reconciler grounds the
// antenna (target = off) to protect against lightning. Activity is inferred from
// radio/state (freq_hz change or tx == "tx"), not operator-set. The timeout is expressed
// in whole minutes (matching the codebase's integer-in-TOML convention; the reconciler
// converts to time.Duration).
type Idle struct {
	TimeoutMinutes int `toml:"timeout_minutes"`
}

// Config is the full runtime configuration for the reconciler.
type Config struct {
	// Location and Host are deployment facts published in /meta (integration model §3).
	Location string `toml:"location"`
	Host     string `toml:"host"`

	MQTT MQTT `toml:"mqtt"`
	// WiringMap maps switch ports (the switch's own port names — at Mühle "port1",
	// "port3", "port6", plus "off") to named passive resources. It is the single
	// editable place the antenna arrangement lives (integration model §4).
	WiringMap   map[string]string `toml:"wiring_map"`
	BandPolicy  BandPolicy        `toml:"band_policy"`
	BandFollow  BandFollow        `toml:"band_follow"`
	PAFollow    PAFollow          `toml:"pa_follow"`
	TunerFollow TunerFollow       `toml:"tuner_follow"`
	Idle        Idle              `toml:"idle"`
}

// Default returns the built-in defaults. Maps are left nil; a usable deployment must
// supply wiring_map and band_policy via the config file. Band-follow is disabled until
// the config names a resource; its slot defaults to the canonical ant-ctrl role. PA
// follow is disabled by default (opt-in); its slot defaults to the canonical pa role.
func Default() Config {
	return Config{
		MQTT: MQTT{
			Slot: "antenna-select",
		},
		BandFollow:  BandFollow{Slot: "ant-ctrl"},
		PAFollow:    PAFollow{Slot: "pa"},
		TunerFollow: TunerFollow{Slot: "tuner"},
		Idle:        Idle{TimeoutMinutes: 30},
	}
}

// Load reads the TOML file at path and overlays its values onto the built-in defaults.
// A missing file is returned as an error wrapping fs.ErrNotExist so the caller can
// distinguish "no file" from a malformed file.
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return Default(), fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

// ResourceToPort inverts the wiring map into resource-name -> port (e.g. "ultrabeam" ->
// "port3"). The "off" entry is excluded — it is a position, not a routable resource.
func (c Config) ResourceToPort() map[string]string {
	out := make(map[string]string, len(c.WiringMap))
	for port, resource := range c.WiringMap {
		if port == "off" {
			continue
		}
		out[resource] = port
	}
	return out
}

// Validate checks that the policy is internally consistent: every resource referenced by
// the band policy (and the fallback) is wired to a port, and no band maps to more than
// one switch port. Returns all problems joined.
func (c Config) Validate() error {
	if c.MQTT.Site == "" || c.MQTT.Station == "" {
		return fmt.Errorf("config: mqtt.site and mqtt.station are required")
	}
	if c.Location == "" || c.Host == "" {
		return fmt.Errorf("config: location and host are required (deployment facts published in /meta, model §3)")
	}
	r2p := c.ResourceToPort()
	var problems []string
	seen := map[string]bool{}
	for resource := range c.BandPolicy.Bands {
		if _, ok := r2p[resource]; !ok {
			seen[resource] = true
		}
	}
	if c.BandPolicy.Fallback != "" {
		if _, ok := r2p[c.BandPolicy.Fallback]; !ok {
			seen[c.BandPolicy.Fallback] = true
		}
	} else {
		problems = append(problems, "band_policy.fallback is required (unmatched bands would resolve to nothing)")
	}
	if len(seen) > 0 {
		missing := make([]string, 0, len(seen))
		for r := range seen {
			missing = append(missing, r)
		}
		sort.Strings(missing)
		problems = append(problems, fmt.Sprintf("band_policy references resources not in wiring_map: %s", strings.Join(missing, ", ")))
	}
	if overlaps := c.bandOverlaps(r2p); len(overlaps) > 0 {
		problems = append(problems, overlaps...)
	}
	if c.BandFollow.Resource != "" {
		if _, ok := r2p[c.BandFollow.Resource]; !ok {
			problems = append(problems, fmt.Sprintf("band_follow.resource %q is not in wiring_map", c.BandFollow.Resource))
		}
		if c.BandFollow.Slot == "" {
			problems = append(problems, "band_follow.slot is required when band_follow.resource is set")
		}
	}
	// PA follow is NOT gated on the wiring map: the PA is always in the RF path, not a
	// passive antenna resource. Only require a slot when the binding is enabled.
	if c.PAFollow.Enabled && c.PAFollow.Slot == "" {
		problems = append(problems, "pa_follow.slot is required when pa_follow.enabled is true")
	}
	// Tuner follow IS gated on a wiring-map resource (the ATU serves one antenna).
	if c.TunerFollow.Enabled {
		if c.TunerFollow.Slot == "" {
			problems = append(problems, "tuner_follow.slot is required when tuner_follow.enabled is true")
		}
		if c.TunerFollow.Resource == "" {
			problems = append(problems, "tuner_follow.resource is required when tuner_follow.enabled is true")
		} else if _, ok := r2p[c.TunerFollow.Resource]; !ok {
			problems = append(problems, fmt.Sprintf("tuner_follow.resource %q is not in wiring_map", c.TunerFollow.Resource))
		}
	}
	if c.Idle.TimeoutMinutes <= 0 {
		problems = append(problems, "idle.timeout_minutes must be positive")
	}
	if len(problems) > 0 {
		return fmt.Errorf("config: %s", strings.Join(problems, "; "))
	}
	return nil
}

// bandOverlaps checks whether any band is assigned to two resources that map to
// different switch ports. A band may be listed under multiple resources only when
// they are wired to the same port (harmless alias); conflicting wiring is reported
// so the config is deterministic and the antenna switch does not chatter.
func (c Config) bandOverlaps(r2p map[string]string) []string {
	if len(c.BandPolicy.Bands) == 0 {
		return nil
	}
	// band -> first port seen, plus set of conflicting ports.
	type conflict struct {
		firstPort string
		ports     map[string]bool
	}
	byBand := make(map[string]*conflict)
	var bands []string
	for resource, bandList := range c.BandPolicy.Bands {
		port, wired := r2p[resource]
		if !wired {
			continue // already reported as missing resource
		}
		for _, band := range bandList {
			if band == "" {
				continue
			}
			cc, ok := byBand[band]
			if !ok {
				cc = &conflict{firstPort: port, ports: map[string]bool{port: true}}
				byBand[band] = cc
				bands = append(bands, band)
				continue
			}
			cc.ports[port] = true
		}
	}
	sort.Strings(bands)
	var problems []string
	for _, band := range bands {
		cc := byBand[band]
		if len(cc.ports) <= 1 {
			continue
		}
		portList := make([]string, 0, len(cc.ports))
		for p := range cc.ports {
			portList = append(portList, p)
		}
		sort.Strings(portList)
		problems = append(problems, fmt.Sprintf("band %q maps to multiple switch ports: %s", band, strings.Join(portList, ", ")))
	}
	return problems
}
