package transport

import (
	"context"
	"sync"
	"time"

	"ubctrl/internal/ub/protocol"
)

type mockClient struct {
	mu    sync.Mutex
	state mockState
}

type mockState struct {
	frequencyKHz   uint16
	bandIndex      byte
	orientation    byte
	motorBits      byte
	retractStartAt time.Time
}

const retractDurationMs = 2000 // simulate 2-second retraction

func NewMock() Client {
	return &mockClient{state: mockState{frequencyKHz: 14000, bandIndex: 4, orientation: protocol.ModeNormal, motorBits: 0}}
}

func (d *mockClient) Close() error { return nil }

func (d *mockClient) Exchange(_ context.Context, com byte, data []byte, _ time.Duration) (protocol.Packet, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch com {
	case protocol.CmdStatusQuery:
		payload := []byte{1, 0, 0, byte(d.state.frequencyKHz & 0xFF), byte(d.state.frequencyKHz >> 8), d.state.bandIndex, d.state.orientation, 0, 0, d.state.motorBits, 7, 30}
		return protocol.Packet{Seq: 0, Com: protocol.ReplyOK, Data: payload}, nil
	case protocol.CmdMovingStatus:
		var total uint16
		if d.state.motorBits != 0 && !d.state.retractStartAt.IsZero() {
			elapsed := time.Since(d.state.retractStartAt)
			if elapsed < time.Duration(retractDurationMs)*time.Millisecond {
				// still retracting
				remaining := retractDurationMs - int(elapsed.Milliseconds())
				total = uint16(remaining)
			} else {
				// retraction complete, clear motor state
				d.state.motorBits = 0
				d.state.retractStartAt = time.Time{}
				total = 0
			}
		}
		payload := []byte{byte(total & 0xFF), byte(total >> 8), 0, 0}
		return protocol.Packet{Seq: 0, Com: protocol.ReplyOK, Data: payload}, nil
	case protocol.CmdRetract:
		d.state.motorBits = 1
		d.state.retractStartAt = time.Now()
		d.state.frequencyKHz = 14000
		return protocol.Packet{Seq: 0, Com: protocol.ReplyOK}, nil
	case protocol.CmdChangeFrequency:
		if len(data) < 3 {
			return protocol.Packet{Seq: 0, Com: protocol.ReplyBadParams}, nil
		}
		d.state.frequencyKHz = uint16(data[0]) | (uint16(data[1]) << 8)
		d.state.orientation = data[2]
		return protocol.Packet{Seq: 0, Com: protocol.ReplyOK}, nil
	default:
		return protocol.Packet{Seq: 0, Com: protocol.ReplyInvalidCommand}, nil
	}
}
