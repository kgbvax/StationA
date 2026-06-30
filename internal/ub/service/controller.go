package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"ubctrl/internal/ub/protocol"
	"ubctrl/internal/ub/transport"
)

type Controller struct {
	dev transport.Client

	mu    sync.RWMutex
	state State
}

func NewController(dev transport.Client) *Controller {
	return &Controller{dev: dev}
}

func (c *Controller) State() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

func (c *Controller) setState(state State) {
	c.mu.Lock()
	c.state = state
	c.mu.Unlock()
}

func (c *Controller) Refresh(ctx context.Context) error {
	pkt, err := c.dev.Exchange(ctx, protocol.CmdStatusQuery, nil, 2*time.Second)
	if err != nil {
		c.setOffline(err)
		return err
	}
	if pkt.Com != protocol.ReplyOK {
		return fmt.Errorf("status query reply com=%d", pkt.Com)
	}
	gs, err := protocol.ParseGeneralStatus(pkt.Data)
	if err != nil {
		return err
	}
	c.setState(State{
		FrequencyKHz: gs.FrequencyKHz,
		BandIndex:    gs.BandIndex,
		BandName:     bandName(gs.FrequencyKHz, gs.BandIndex),
		ModeName:     protocol.ModeName(gs.Orientation & 0x0F),
		MotorsMoving: gs.MotorBits != 0,
		MotorBits:    gs.MotorBits,
		UpdatedAt:    time.Now(),
	})
	return nil
}

func (c *Controller) PollMotorStatus(ctx context.Context) error {
	pkt, err := c.dev.Exchange(ctx, protocol.CmdMovingStatus, nil, 2*time.Second)
	if err != nil {
		c.setOffline(err)
		return err
	}
	if pkt.Com != protocol.ReplyOK {
		return fmt.Errorf("moving status reply com=%d", pkt.Com)
	}
	ms, err := protocol.ParseMovingStatus(pkt.Data)
	if err != nil {
		return err
	}
	state := c.State()
	state.MotorsMoving = ms.TotalDistanceMM != 0
	state.UpdatedAt = time.Now()
	state.Offline = false
	state.LastError = ""
	c.setState(state)
	return nil
}

func (c *Controller) Retract(ctx context.Context) error {
	pkt, err := c.dev.Exchange(ctx, protocol.CmdRetract, nil, 5*time.Second)
	if err != nil {
		c.setOffline(err)
		return err
	}
	if pkt.Com != protocol.ReplyOK {
		return fmt.Errorf("retract reply com=%d", pkt.Com)
	}
	return c.Refresh(ctx)
}

func (c *Controller) SetFrequency(ctx context.Context, frequencyKHz uint16, mode string) error {
	modeByte, err := protocol.ParseMode(mode)
	if err != nil {
		return err
	}
	data := []byte{byte(frequencyKHz & 0xFF), byte(frequencyKHz >> 8), modeByte}
	pkt, err := c.dev.Exchange(ctx, protocol.CmdChangeFrequency, data, 5*time.Second)
	if err != nil {
		c.setOffline(err)
		return err
	}
	if pkt.Com != protocol.ReplyOK {
		if pkt.Com == protocol.ReplyBadParams {
			return fmt.Errorf("controller rejected params")
		}
		return fmt.Errorf("frequency reply com=%d", pkt.Com)
	}
	return c.Refresh(ctx)
}

func (c *Controller) SetMode(ctx context.Context, mode string) error {
	state := c.State()
	if state.FrequencyKHz == 0 {
		if err := c.Refresh(ctx); err != nil {
			return err
		}
		state = c.State()
		if state.FrequencyKHz == 0 {
			return fmt.Errorf("current frequency unknown")
		}
	}
	return c.SetFrequency(ctx, state.FrequencyKHz, mode)
}

func (c *Controller) setOffline(err error) {
	state := c.State()
	state.Offline = true
	state.LastError = err.Error()
	state.UpdatedAt = time.Now()
	c.setState(state)
}

func bandName(freqKHz uint16, bandIndex byte) string {
	switch {
	case freqKHz >= 1800 && freqKHz < 2000:
		return "160m"
	case freqKHz >= 3500 && freqKHz < 4000:
		return "80m"
	case freqKHz >= 7000 && freqKHz < 7300:
		return "40m"
	case freqKHz >= 10100 && freqKHz < 10150:
		return "30m"
	case freqKHz >= 14000 && freqKHz < 14350:
		return "20m"
	case freqKHz >= 18068 && freqKHz < 18168:
		return "17m"
	case freqKHz >= 21000 && freqKHz < 21450:
		return "15m"
	case freqKHz >= 24890 && freqKHz < 24990:
		return "12m"
	case freqKHz >= 28000 && freqKHz < 29700:
		return "10m"
	case freqKHz >= 50000 && freqKHz < 54000:
		return "6m"
	default:
		return fmt.Sprintf("band-%d", bandIndex)
	}
}
