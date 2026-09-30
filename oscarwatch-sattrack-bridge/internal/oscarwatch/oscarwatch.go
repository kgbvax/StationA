// Package oscarwatch decodes the OscarWatch "Satellite link" WebSocket frames
// (protocol version 1). OscarWatch — the satellite tracker on the shack PC —
// runs a small WebSocket server (default :7373) that pushes the focused
// satellite as a `satelliteStatus` JSON frame: a snapshot on connect, then an
// update whenever focus, mode, frequencies or tracking data change (throttled
// by its update interval). Logbook events (`qsoLogged`, `qsoUpdated`,
// `qsoDeleted`) share the socket; this bridge does not consume them.
//
// Pure decoding, no I/O. Unknown fields are ignored (the vendor contract says
// clients must be forward-compatible), and a frame whose version is not 1 is
// still decoded — the caller decides whether to warn.
package oscarwatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// ProtocolVersion is the Satellite-link protocol version this decoder targets.
const ProtocolVersion = 1

// Frame types on the Satellite-link socket.
const (
	TypeSatelliteStatus = "satelliteStatus"
	TypeQSOLogged       = "qsoLogged"
	TypeQSOUpdated      = "qsoUpdated"
	TypeQSODeleted      = "qsoDeleted"
)

// Kind classifies a frame for the read loop.
type Kind int

const (
	// KindOther is a frame type this decoder does not know (future protocol
	// additions). The caller ignores it.
	KindOther Kind = iota
	// KindStatus is a live tracking snapshot (`satelliteStatus`).
	KindStatus
	// KindQSO is a logbook event (`qsoLogged`/`qsoUpdated`/`qsoDeleted`),
	// not consumed in v1.
	KindQSO
)

// Classify maps a frame's `type` to its Kind.
func Classify(typ string) Kind {
	switch typ {
	case TypeSatelliteStatus:
		return KindStatus
	case TypeQSOLogged, TypeQSOUpdated, TypeQSODeleted:
		return KindQSO
	}
	return KindOther
}

// Header is the envelope every frame carries.
type Header struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
}

// PeekHeader decodes only the envelope, so the read loop can route a frame
// without decoding a body it will drop.
func PeekHeader(data []byte) (Header, error) {
	var h Header
	if err := json.Unmarshal(data, &h); err != nil {
		return Header{}, fmt.Errorf("oscarwatch: bad frame: %w", err)
	}
	if h.Type == "" {
		return Header{}, errors.New("oscarwatch: frame has no type")
	}
	return h, nil
}

// Status is a decoded `satelliteStatus` frame. Satellite is nil when no
// satellite is focused or — with OscarWatch's "only broadcast when above
// horizon" setting on — the focused one is below the horizon (the frame then
// carries only `inRange:false` and the `** NO SATELLITE **` legacy string).
type Status struct {
	Version         int          `json:"version"`
	TimestampUTC    string       `json:"timestampUtc"`
	InRange         bool         `json:"inRange"`
	Satellite       *Satellite   `json:"satellite"`
	Frequencies     *Frequencies `json:"frequencies"`
	Bands           *Bands       `json:"bands"`
	Tracking        *Tracking    `json:"tracking"`
	DopplerStrategy string       `json:"dopplerStrategy"`
}

// Satellite identifies the focused satellite.
type Satellite struct {
	// Name is the OscarWatch/LoTW name (same as ADIF SAT_NAME), e.g. "SO-50".
	Name string `json:"name"`
	// NoradID is documented as a string ("27607"). Kept a string: Alpha-5
	// catalogue numbers are not integers.
	NoradID FlexString `json:"noradId"`
	// ModeType is the selected transponder's label, e.g. "FM VOICE" — not a
	// canonical mode.
	ModeType string `json:"modeType"`
}

// Frequencies are the radio-corrected (Doppler-applied) frequencies OscarWatch
// drives CAT with, not the catalogue nominals.
type Frequencies struct {
	UplinkHz     int64  `json:"uplinkHz"`
	DownlinkHz   int64  `json:"downlinkHz"`
	UplinkMode   string `json:"uplinkMode"`
	DownlinkMode string `json:"downlinkMode"`
	IsBeaconOnly bool   `json:"isBeaconOnly"`
}

// Bands are ADIF band labels ("70cm", "2m").
type Bands struct {
	TX string `json:"tx"`
	RX string `json:"rx"`
}

// Tracking is the live look angle from the OscarWatch QTH. Pointer fields: a
// key missing from the frame must stay distinguishable from a real 0.
type Tracking struct {
	AzimuthDeg        *float64 `json:"azimuthDeg"`
	ElevationDeg      *float64 `json:"elevationDeg"`
	RangeKm           *float64 `json:"rangeKm"`
	RangeRateKmPerSec *float64 `json:"rangeRateKmPerSec"`
	IsSunlit          *bool    `json:"isSunlit"`
}

// DecodeStatus decodes a `satelliteStatus` frame body.
func DecodeStatus(data []byte) (Status, error) {
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return Status{}, fmt.Errorf("oscarwatch: bad satelliteStatus: %w", err)
	}
	return s, nil
}

// FlexString accepts a JSON string or number (and null → ""). The vendor
// documents noradId as a string; a number is tolerated so a producer-side
// type change does not drop the whole frame.
type FlexString string

// UnmarshalJSON implements json.Unmarshaler.
func (f *FlexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case bytes.Equal(b, []byte("null")):
		*f = ""
		return nil
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = FlexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("oscarwatch: noradId is neither string nor number: %s", b)
	}
	if i, err := n.Int64(); err == nil {
		*f = FlexString(strconv.FormatInt(i, 10))
		return nil
	}
	*f = FlexString(n.String())
	return nil
}
