package civ

// Transceive broadcasts: the frames the radio pushes when the operating
// state changes while CI-V Transceive is ON (radio-side prerequisite, deploy
// gate 3). Frequency (00) and mode (01) transceive fire for MAIN and SUB
// changes alike — the wire does NOT say which band moved — and PTT status
// (1C 00) is reported the same way. Attributing a 00/01 frame to MAIN or
// SUB is the session layer's reconciliation job (poll 07 D2 00), not the
// codec's; the derived band name below is the canonical band OF THE
// FREQUENCY, not the VFO it belongs to.

import (
	"errors"
	"fmt"
)

// TransceiveKind selects which payload a Transceive carries.
type TransceiveKind uint8

const (
	TransceiveFreq TransceiveKind = iota // 00 — frequency changed
	TransceiveMode                       // 01 — operating mode changed
	TransceivePTT                        // 1C 00 — TX status changed
)

func (k TransceiveKind) String() string {
	switch k {
	case TransceiveFreq:
		return "freq"
	case TransceiveMode:
		return "mode"
	case TransceivePTT:
		return "ptt"
	default:
		return "transceive(?)"
	}
}

// Transceive is one parsed state-change broadcast. Fields the kind does not
// carry stay zero. ModeOK=false marks a mode byte the bus never publishes
// raw (RTTY/DV/DD, R7): the consumer omits the mode field, never
// substituting a lookalike. BandOK=false marks a frequency outside the
// canonical band table.
type Transceive struct {
	Kind   TransceiveKind
	FreqHz uint32 // TransceiveFreq
	Band   string // canonical band of FreqHz ("" when BandOK=false)
	BandOK bool
	Mode   string // canonical mode ("" when ModeOK=false)
	ModeOK bool
	Filter byte // mode transceive: 0 when the radio omitted the filter byte
	PTTOn  bool // TransceivePTT: true = TX (keyed)
}

// ErrNotTransceive: the reply is not a transceive-capable frame.
var errNotTransceive = errors.New("frame is not a transceive broadcast")

// ParseTransceive converts a KindData reply carrying command 00, 01 or
// 1C 00 into a Transceive event. The same frame shapes serve as solicited
// read replies and as broadcasts, so a poll answer can ride the same path.
func ParseTransceive(r Reply) (Transceive, error) {
	if r.Kind != KindData {
		return Transceive{}, fmt.Errorf("%w: reply kind %d", errNotTransceive, r.Kind)
	}
	switch r.Cmd {
	case CmdFreqTransceive:
		hz, err := r.FreqHz()
		if err != nil {
			return Transceive{}, err
		}
		t := Transceive{Kind: TransceiveFreq, FreqHz: hz}
		t.Band, t.BandOK = BandForFreq(hz)
		return t, nil
	case CmdModeTransceive:
		mode, filter, ok, err := r.Mode()
		if err != nil {
			return Transceive{}, err
		}
		return Transceive{Kind: TransceiveMode, Mode: mode, ModeOK: ok, Filter: filter}, nil
	case CmdStatus:
		on, err := r.OnOff()
		if err != nil {
			return Transceive{}, err
		}
		return Transceive{Kind: TransceivePTT, PTTOn: on}, nil
	default:
		return Transceive{}, fmt.Errorf("%w: command %02x", errNotTransceive, r.Cmd)
	}
}
