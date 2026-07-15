package acom

import "encoding/binary"

// Observation is one decoded telemetry frame: the raw, protocol-level values.
// The bridge canonicalizes these (mode/keyed/fault) and publishes the /state
// snapshot. ForwardPower is already window-averaged.
type Observation struct {
	ForwardPower   uint16  // averaged, watts
	ReflectedPower uint16  // watts
	InputPower     float64 // watts
	SWR            float64 // ratio
	Temperature    float64 // degrees Celsius
	FreqKHz        uint16  // kHz (NOT published to /state; band is the PA's band)
	BandIndex      int     // amp band index 1..10 (0 = unknown)
	BandName       string  // canonical band label
	ModeRaw        string  // raw firmware mode (also published as pa_state)
	ErrByte        byte    // fault byte
	ErrMsg         string  // human-readable fault message
}

// ParseTelemetry decodes a 72-byte telemetry frame into an Observation. ok is
// false when the frame is not exactly 72 bytes (the parser is lenient: short
// frames are dropped, not fatal). fwdAvg may be nil (no averaging).
func ParseTelemetry(data []byte, fwdAvg *PowerAverager) (Observation, bool) {
	if len(data) != 72 {
		return Observation{}, false
	}

	rawFwdPwr := binary.LittleEndian.Uint16(data[22:24])
	refPwr := binary.LittleEndian.Uint16(data[24:26])
	inPwrRaw := binary.LittleEndian.Uint16(data[20:22])
	inPwr := float64(inPwrRaw) / 10.0
	swrRaw := binary.LittleEndian.Uint16(data[26:28])
	swr := float64(swrRaw) / 100.0

	tempK := binary.LittleEndian.Uint16(data[16:18])
	tempC := 0.0
	if tempK > 0 {
		tempC = float64(tempK) - 273.15
	}

	freqKHz := binary.LittleEndian.Uint16(data[48:50])
	bandByte := data[69] & 0x0F
	modeByte := data[3]
	errByte := data[66]

	// decodeBand maps only 1..10 to labels; anything else (0, 11..15) is "UNK".
	// Keep BandIndex in lockstep with that contract: out-of-range bytes become
	// 0 (unknown) so SetBand's current-band guard rejects navigation from a
	// garbage index instead of walking the amp from an invalid position.
	bandIndex := int(bandByte)
	if bandIndex < 1 || bandIndex > 10 {
		bandIndex = 0
	}

	var avgFwd uint16 = rawFwdPwr
	if fwdAvg != nil {
		avgFwd = fwdAvg.Add(rawFwdPwr)
	}

	return Observation{
		ForwardPower:   avgFwd,
		ReflectedPower: refPwr,
		InputPower:     inPwr,
		SWR:            swr,
		Temperature:    tempC,
		FreqKHz:        freqKHz,
		BandIndex:      bandIndex,
		BandName:       decodeBand(bandByte),
		ModeRaw:        decodeMode(modeByte),
		ErrByte:        errByte,
		ErrMsg:         decodeError(errByte),
	}, true
}
