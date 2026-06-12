package protocol

import (
	"errors"
	"fmt"
)

const (
	CmdStatusQuery      byte = 1
	CmdRetract          byte = 2
	CmdChangeFrequency  byte = 3
	CmdMovingStatus     byte = 10
	CmdModifyElementLen byte = 12
)

const (
	ReplyOK             byte = 0
	ReplyError          byte = 20
	ReplyBadParams      byte = 30
	ReplyInvalidCommand byte = 40
	ReplyDebug          byte = 64
)

const (
	ModeNormal byte = 0
	Mode180    byte = 1
	ModeBidir  byte = 2
)

var (
	ErrUnsupportedWrite = errors.New("unsupported write command")
)

type GeneralStatus struct {
	FirmwareMinor byte
	FirmwareMajor byte
	Operation     byte

	FrequencyKHz uint16
	BandIndex    byte
	Orientation  byte
	Flags1       byte
	Flags2       byte
	MotorBits    byte
	MinFreqMHz   byte
	MaxFreqMHz   byte
}

func ParseGeneralStatus(data []byte) (GeneralStatus, error) {
	if len(data) < 12 {
		return GeneralStatus{}, fmt.Errorf("status payload too short: have=%d want>=12", len(data))
	}
	return GeneralStatus{
		FirmwareMinor: data[0],
		FirmwareMajor: data[1],
		Operation:     data[2],
		FrequencyKHz:  uint16(data[3]) | (uint16(data[4]) << 8),
		BandIndex:     data[5],
		Orientation:   data[6] & 0x0F,
		Flags1:        data[7],
		Flags2:        data[8],
		MotorBits:     data[9],
		MinFreqMHz:    data[10],
		MaxFreqMHz:    data[11],
	}, nil
}

type MovingStatus struct {
	TotalDistanceMM uint16
	ProgressUnits   uint16
}

func ParseMovingStatus(data []byte) (MovingStatus, error) {
	if len(data) < 4 {
		return MovingStatus{}, fmt.Errorf("moving payload too short: have=%d want>=4", len(data))
	}
	return MovingStatus{
		TotalDistanceMM: uint16(data[0]) | (uint16(data[1]) << 8),
		ProgressUnits:   uint16(data[2]) | (uint16(data[3]) << 8),
	}, nil
}

func ParseMode(mode string) (byte, error) {
	switch mode {
	case "", "forward", "normal":
		return ModeNormal, nil
	case "reverse", "180":
		return Mode180, nil
	case "bidirectional", "bidir":
		return ModeBidir, nil
	default:
		return 0, fmt.Errorf("invalid mode %q (expected forward|reverse|bidirectional)", mode)
	}
}

func ModeName(mode byte) string {
	switch mode {
	case ModeNormal:
		return "forward"
	case Mode180:
		return "reverse"
	case ModeBidir:
		return "bidirectional"
	default:
		return "unknown"
	}
}
