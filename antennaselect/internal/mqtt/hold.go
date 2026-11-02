package mqtt

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"

	"antennaselect/internal/reconcile"
)

// maxHoldTTL caps a station hold from the moment it was received, whatever
// hold_until it claims: a publisher with a wrong clock (or a bug) cannot keep
// the antenna ungrounded for longer than this without refreshing the hold.
const maxHoldTTL = 10 * time.Minute

// stationHold is the last seen state of one [idle].hold_slots slot: a consumer
// that uses the station without touching the VFO or transmitting (bmrec's HF
// lab records from a panadapter for hours). A live hold is presence, like an
// operator hold: it keeps the station active, so the walk-away grounding and,
// through activity/last_activity, powerseq's standby and idle shutdown wait.
//
// The hold must stay alive on its own: it is live only while the slot's
// /status is online (the publisher's last will clears it when it dies) AND
// its /state says station_hold=true AND now is before hold_until (the
// publisher refreshes it every heartbeat; a hung publisher stops doing so)
// AND within maxHoldTTL of receipt. Anything else, including a malformed
// /state, is no hold: the safe default is the walk-away grounding.
type stationHold struct {
	online   bool
	held     bool
	until    time.Time
	received time.Time
	holder   string
	reason   string
}

func (h *stationHold) live(now time.Time) bool {
	if h == nil || !h.online || !h.held {
		return false
	}
	end := h.received.Add(maxHoldTTL)
	if h.until.Before(end) {
		end = h.until
	}
	return now.Before(end)
}

// holdPayload is the hold slot's /state (bmrec: muehle/hf/recorder/state).
type holdPayload struct {
	StationHold bool   `json:"station_hold"`
	HoldUntil   string `json:"hold_until"`
	Holder      string `json:"holder"`
	Reason      string `json:"reason"`
}

// parseHold reads a hold /state. An empty payload (cleared retained topic) is
// no hold. A hold without a valid hold_until is an error: a hold that cannot
// expire is not trusted.
func parseHold(b []byte) (holdPayload, time.Time, error) {
	var p holdPayload
	if len(strings.TrimSpace(string(b))) == 0 {
		return p, time.Time{}, nil
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return holdPayload{}, time.Time{}, err
	}
	if !p.StationHold {
		return p, time.Time{}, nil
	}
	until, err := time.Parse(time.RFC3339Nano, p.HoldUntil)
	if err != nil {
		return holdPayload{}, time.Time{}, errors.New("station_hold without a valid hold_until")
	}
	return p, until, nil
}

func (c *Client) subscribeHolds() {
	for _, slot := range c.cfg.Idle.HoldSlots {
		c.subscribe(c.siblingTopic(slot, "status"), c.onHoldStatus(slot))
		c.subscribe(c.siblingTopic(slot, "state"), c.onHoldState(slot))
	}
}

func (c *Client) onHoldStatus(slot string) paho.MessageHandler {
	return func(_ paho.Client, msg paho.Message) {
		online := strings.EqualFold(strings.TrimSpace(string(msg.Payload())), "online")
		sharedmqtt.Enqueue(c.jobs, func() {
			c.update(func(in *reconcile.Inputs) {
				c.hold(slot).online = online
				c.applyHolds(in, time.Now())
			})
		})
	}
}

func (c *Client) onHoldState(slot string) paho.MessageHandler {
	return func(_ paho.Client, msg paho.Message) {
		p, until, err := parseHold(msg.Payload())
		if err != nil {
			// Released, not kept: the walk-away grounding is the safe default.
			slog.Error("[mqtt] bad station hold state; treating as released", "slot", slot, "err", err)
		}
		now := time.Now()
		sharedmqtt.Enqueue(c.jobs, func() {
			c.update(func(in *reconcile.Inputs) {
				h := c.hold(slot)
				h.held, h.until, h.received = p.StationHold, until, now
				h.holder, h.reason = p.Holder, p.Reason
				c.applyHolds(in, time.Now())
			})
		})
	}
}

// hold returns the slot's record (caller holds c.mu via update).
func (c *Client) hold(slot string) *stationHold {
	if c.holds == nil {
		c.holds = map[string]*stationHold{}
	}
	h := c.holds[slot]
	if h == nil {
		h = &stationHold{}
		c.holds[slot] = h
	}
	return h
}

// applyHolds marks the station active while any hold is live and logs when
// the station becomes held or free (caller holds c.mu via update). It runs on
// every hold message and on every idle tick, so a live hold keeps resetting
// the idle clock: the walk-away timeout starts when the last hold ends or
// expires, not when it began.
func (c *Client) applyHolds(in *reconcile.Inputs, now time.Time) {
	var by *stationHold
	var bySlot string
	for _, slot := range c.cfg.Idle.HoldSlots {
		if h := c.holds[slot]; h.live(now) {
			by, bySlot = h, slot
			break
		}
	}
	if by != nil {
		c.markActive(in)
		if !c.held {
			slog.Info("[reconcile] station held: idle grounding suspended", "slot", bySlot, "holder", by.holder, "reason", by.reason)
		}
	} else if c.held {
		slog.Info("[reconcile] station hold ended: idle timeout runs from now", "timeout_minutes", c.cfg.Idle.TimeoutMinutes)
	}
	c.held = by != nil
}
