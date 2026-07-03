// Package flexradio implements the FlexRadio 6000-series network protocol:
// UDP discovery, the SmartSDR TCP/IP API (port 4992) and the VITA-49 meter
// stream (UDP). Nothing in this package talks to MQTT or Home Assistant.
package flexradio

import (
	"errors"
	"math"
	"strings"
)

// MeterSource identifies which part of the radio a meter comes from.
//
// Flex assigns meter indices per-session and the numeric index is NOT a
// stable contract — the same logical meter can move around between
// sessions and even during a session (e.g. when slices are created or
// band changes happen). Meters must be matched by (Source, Name).
type MeterSource string

const (
	SourceSlice MeterSource = "SLC"  // per-slice receiver meter
	SourceTX    MeterSource = "TX"   // transmit chain meter
	SourceRadio MeterSource = "RAD"  // radio-wide hardware meter
	SourceCodec MeterSource = "COD-" // codec / microphone input
	SourceAmp   MeterSource = "AMP"  // optional PGXL/TGXL amplifier
)

// MeterGroup buckets meters by publish-rate policy. See config.RatesConfig.
type MeterGroup string

const (
	GroupTX    MeterGroup = "tx"    // TX RF meters (power, SWR)
	GroupAudio MeterGroup = "audio" // TX audio processing meters
	GroupRX    MeterGroup = "rx"    // per-slice receiver meters
	GroupHW    MeterGroup = "hw"    // slow radio hardware telemetry
)

// MeterDef is the static, declarative definition of a meter we care about.
// It is matched against the radio's runtime meter list by (Source, Name).
type MeterDef struct {
	// Source is the meter source prefix (SLC, TX, RAD, COD-, AMP).
	Source MeterSource
	// Name is the meter name within that source (LEVEL, FWDPWR, ...).
	Name string
	// Group controls the publish rate and deadband bucket.
	Group MeterGroup
	// Unit is the engineering unit the raw value converts to. One of:
	// dBm, dBFS, dB, SWR, Volts, Amps, degC, degF, Watts.
	Unit string
	// PublishUnit is the unit the value is published in (may differ, e.g.
	// FWDPWR arrives in dBm but is published in Watts).
	PublishUnit string
	// ObjectID is the stable HA object id for this meter (sans slice index).
	ObjectID string
	// Label is the human label.
	Label string
}

// wantedMeters lists the meters flexbridge publishes, per the user's
// selections. TX RF + TX audio + mic input levels, RX S-meter + broadband,
// and slow hardware telemetry. PACURRENT is deliberately excluded: it is
// flagged unreliable/erroneous on the 8000-series (incl. the 8400).
//
// Source/name strings MUST match what "meter N" status replies return;
// they come straight from FlexLib's Meter.cs.
var wantedMeters = []MeterDef{
	// --- TX RF meters (2 Hz) ---
	{SourceTX, "FWDPWR", GroupTX, "dBm", "W", "tx_fwd_power", "Forward RF Power"},
	{SourceTX, "REFPWR", GroupTX, "dBm", "W", "tx_ref_power", "Reflected RF Power"},
	{SourceTX, "SWR", GroupTX, "SWR", "SWR", "tx_swr", "TX SWR"},

	// --- TX audio processing (2 Hz) ---
	{SourceTX, "COMPPEAK", GroupAudio, "dBFS", "dB", "tx_compression", "Speech Compression"},
	{SourceTX, "ALC", GroupAudio, "dBFS", "dB", "tx_alc", "TX ALC"},

	// --- Mic input levels (2 Hz) ---
	{SourceCodec, "MIC", GroupAudio, "dBFS", "dB", "mic_level", "Microphone Level"},
	{SourceCodec, "MICPEAK", GroupAudio, "dBFS", "dB", "mic_peak", "Microphone Peak"},

	// --- RX per-slice (1 Hz) ---
	{SourceSlice, "LEVEL", GroupRX, "dBm", "dBm", "s_meter", "S-Meter"},
	{SourceSlice, "24kHz", GroupRX, "dBFS", "dBFS", "broadband", "Broadband Level (24kHz)"},

	// --- Radio hardware (0.1 Hz; radio polls internally, slow) ---
	{SourceRadio, "PATEMP", GroupHW, "degC", "°C", "pa_temp", "PA Temperature"},
	{SourceRadio, "+13.8A", GroupHW, "Volts", "V", "supply_voltage_a", "Supply Voltage (pre-fuse)"},
	{SourceRadio, "+13.8B", GroupHW, "Volts", "V", "supply_voltage_b", "Supply Voltage (post-fuse)"},
}

// MeterRegistry tracks the runtime meter index -> definition map. Indices
// are session-specific and must be rebuilt whenever the radio sends new
// "meter N" status lines.
type MeterRegistry struct {
	byIndex map[uint16]runtimeMeter
	byKey   map[meterKey][]runtimeMeter // (source,name) -> meters, supports per-slice
}

type meterKey struct {
	source MeterSource
	name   string
}

// runtimeMeter pairs a runtime meter index with its definition and an
// optional source-internal index (the slice index for SLC meters).
type runtimeMeter struct {
	def       MeterDef
	index     uint16
	sourceNum int // slice index for SLC; unstable
}

// NewMeterRegistry returns an empty registry.
func NewMeterRegistry() *MeterRegistry {
	return &MeterRegistry{
		byIndex: make(map[uint16]runtimeMeter),
		byKey:   make(map[meterKey][]runtimeMeter),
	}
}

// LookupDef returns the static MeterDef for a (source,name) if flexbridge
// publishes it. ok is false for meters we don't care about.
func LookupDef(source MeterSource, name string) (MeterDef, bool) {
	for _, d := range wantedMeters {
		if d.Source == source && d.Name == name {
			return d, true
		}
	}
	return MeterDef{}, false
}

// WantedMeterKeys returns the set of (source,name) keys we publish, for
// logging / diagnostics.
func WantedMeterKeys() []meterKey {
	seen := make(map[meterKey]bool)
	var out []meterKey
	for _, d := range wantedMeters {
		k := meterKey{d.Source, d.Name}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// WantedMeterDefs returns a copy of the static wanted-meter definition list.
// Exposed so the bridge can generate Home Assistant discovery for every
// meter we publish.
func WantedMeterDefs() []MeterDef {
	out := make([]MeterDef, len(wantedMeters))
	copy(out, wantedMeters)
	return out
}

// Register adds a runtime meter to the registry. source and name come from
// the radio's "meter N" status reply; def comes from LookupDef. Meters we
// don't publish are silently ignored (Register returns false).
func (r *MeterRegistry) Register(index uint16, source string, sourceNum int, name string) bool {
	def, ok := LookupDef(MeterSource(source), name)
	if !ok {
		return false
	}
	rm := runtimeMeter{def: def, index: index, sourceNum: sourceNum}
	r.byIndex[index] = rm
	k := meterKey{MeterSource(source), name}
	r.byKey[k] = append(r.byKey[k], rm)
	return true
}

// Reset clears the registry (call before rebuilding from a fresh meter list).
func (r *MeterRegistry) Reset() {
	for k := range r.byIndex {
		delete(r.byIndex, k)
	}
	for k := range r.byKey {
		delete(r.byKey, k)
	}
}

// Count returns the number of currently-registered wanted meters.
func (r *MeterRegistry) Count() int { return len(r.byIndex) }

// LookupIndex returns the runtime meter for a runtime index, if any.
func (r *MeterRegistry) LookupIndex(idx uint16) (runtimeMeter, bool) {
	rm, ok := r.byIndex[idx]
	return rm, ok
}

// Definition exposes the runtime meter for callers that need def fields.
func (rm runtimeMeter) Definition() MeterDef { return rm.def }

// SourceNum returns the source-internal index (slice index for SLC meters).
func (rm runtimeMeter) SourceNum() int { return rm.sourceNum }

// ErrUnknownMeter is returned by conversion helpers when no def is known.
var ErrUnknownMeter = errors.New("flexradio: unknown meter")

// ConvertRaw converts a raw int16 meter reading to engineering units using
// the FlexLib divisors (sourced from FlexLib's Meter.cs via AetherSDR):
//
//	dBm, dB, dBFS, SWR: raw / 128.0
//	Volts, Amps:        raw / 256.0   (firmware >= 1.11; older fw used /1024)
//	degC, degF:         raw / 64.0
//	default / Watts:    raw            (unscaled)
//
// Then applies the unit-specific publish transform (e.g. dBm -> Watts for
// forward/reflected power). unit is the *source* unit; def.PublishUnit is
// the target.
func ConvertRaw(unit string, raw int16, def MeterDef) (float64, string) {
	v := convertSource(unit, raw)
	return convertToPublish(v, def.Unit, def.PublishUnit), def.PublishUnit
}

// convertSource applies the per-unit divisor to a raw reading.
func convertSource(unit string, raw int16) float64 {
	switch unit {
	case "dBm", "dB", "dBFS", "SWR":
		return float64(raw) / 128.0
	case "Volts", "Amps":
		return float64(raw) / 256.0
	case "degC", "degF":
		return float64(raw) / 64.0
	default:
		return float64(raw) // Watts, Percent, anything unknown
	}
}

// convertToPublish transforms a source-unit value into the published unit.
// Currently the only non-identity transform is dBm -> Watts (forward and
// reflected RF power), which is friendlier to operators and HA device
// classes.
func convertToPublish(v float64, fromUnit, toUnit string) float64 {
	if fromUnit == toUnit {
		return v
	}
	if fromUnit == "dBm" && toUnit == "W" {
		// dBm -> dBW then dBW -> W:  W = 10^((dBm-30)/10)
		// Negative or extremely low dBm -> 0 W (don't publish 1e-30 dust).
		if v <= -60 { // < 1 nW: effectively zero for RF power meters
			return 0
		}
		return math.Pow(10, (v-30)/10)
	}
	// Unknown transform: return the source value unchanged.
	return v
}

// Deadband returns the rounding precision used for duplicate detection,
// per unit. Values whose rounded form matches the last published rounded
// value are suppressed (subject also to the per-group min interval).
func Deadband(unit string) float64 {
	switch unit {
	case "dBm", "dB", "dBFS":
		return 0.1
	case "SWR":
		return 0.01
	case "Volts", "Amps":
		return 0.01
	case "degC", "degF":
		return 0.1
	case "W", "Watts":
		return 0.1
	default:
		return 0.1
	}
}

// MeterKeyStr returns a stable, human-readable identifier for a (source,name)
// pair, used in log messages.
func MeterKeyStr(source, name string) string {
	return strings.ToLower(string(source)) + "/" + strings.ToLower(name)
}
