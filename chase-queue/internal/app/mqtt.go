package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"

	"chase-queue/internal/bus"
	"chase-queue/internal/engine"
)

// connect opens the MQTT link: LWT on /status, birth sequence on every
// (re)connect, subscriptions to bmrec's candidates and to /cmd. Handlers only
// enqueue; the jobs worker applies the work.
func (s *Service) connect(ctx context.Context) error {
	o := pahomqtt.NewClientOptions()
	o.AddBroker(s.cfg.MQTT.Broker)
	id := s.cfg.MQTT.ClientID
	if id == "" {
		id = s.cfg.MQTT.Site + "-" + s.cfg.Slot.Station + "-" + s.cfg.Slot.Slot
	}
	o.SetClientID(id)
	if s.cfg.MQTT.User != "" {
		o.SetUsername(s.cfg.MQTT.User)
		o.SetPassword(s.cfg.MQTT.Password)
	}
	o.SetAutoReconnect(true)
	o.SetCleanSession(true) // retained candidates replay on subscribe; /cmd is QoS 0 so nothing queues offline
	o.SetWill(s.topics.status, "offline", 1, true)
	o.OnConnect = func(c pahomqtt.Client) {
		s.log.Info("MQTT (re)connected", "broker", s.cfg.MQTT.Broker)
		c.Publish(s.topics.status, 1, true, []byte("online"))
		if meta, err := s.metaPayload(); err == nil {
			c.Publish(s.topics.meta, 1, true, meta)
		}
		c.Subscribe(s.topics.finder, 1, func(_ pahomqtt.Client, m pahomqtt.Message) {
			s.candMu.Lock()
			s.candRaw = append(s.candRaw[:0], m.Payload()...)
			s.candMu.Unlock()
			select {
			case s.candSig <- struct{}{}:
			default:
			}
		})
		c.Subscribe(s.topics.cmd, 0, func(_ pahomqtt.Client, m pahomqtt.Message) {
			p := append([]byte(nil), m.Payload()...)
			sharedmqtt.Enqueue(s.jobs, func() { s.onCmd(p) })
		})
		if s.view != nil {
			// events are not retained and the session is clean: whatever
			// happened while we were away comes with a fresh load
			c.Subscribe(s.topics.logEvent, 1, func(_ pahomqtt.Client, m pahomqtt.Message) { s.view.Apply(m.Payload()) })
			s.view.Reload()
		}
		s.forcePub.Store(true)
		s.poke() // republish the retained state after a broker restart
	}
	o.OnConnectionLost = func(_ pahomqtt.Client, err error) { s.log.Warn("MQTT connection lost", "err", err) }
	c := pahomqtt.NewClient(o)
	s.clientMu.Lock()
	s.client = c
	s.clientMu.Unlock()
	if err := sharedmqtt.Connect(ctx, c); err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	return nil
}

func (s *Service) getClient() pahomqtt.Client {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	return s.client
}

func (s *Service) shutdownBus() {
	if c := s.getClient(); c != nil && c.IsConnected() {
		t := c.Publish(s.topics.status, 1, true, []byte("offline"))
		t.WaitTimeout(2 * time.Second)
		c.Disconnect(500)
	}
}

// candidateWorker applies the newest candidate state. bmrec republishes
// often; only the latest matters, so a slow /cmd or contest switch never
// makes states queue up or /cmd get dropped.
func (s *Service) candidateWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.candSig:
			s.candMu.Lock()
			p := append([]byte(nil), s.candRaw...)
			s.candMu.Unlock()
			s.onCandidates(p)
		}
	}
}

// publishState writes the retained /state with a bounded wait.
func (s *Service) publishState(b []byte) bool {
	c := s.getClient()
	if c == nil || !c.IsConnected() {
		return false
	}
	t := c.Publish(s.topics.state, 1, true, b)
	if !t.WaitTimeout(10 * time.Second) {
		return false
	}
	if err := t.Error(); err != nil {
		s.log.Warn("state publish failed", "err", err)
		return false
	}
	return true
}

func (s *Service) metaPayload() ([]byte, error) {
	history := "local" // Log4OM listener + the local log
	if s.view != nil {
		history = "station-log"
	}
	s.awardMu.Lock()
	awards := slices.Clone(s.awardCodes)
	s.awardMu.Unlock()
	return json.Marshal(map[string]any{
		"schema": "1.0",
		"role":   "chase-queue",
		"host":   s.cfg.Host,
		"device": map[string]any{"name": "Chase queue"},
		"capabilities": map[string]any{
			"activities": []string{bus.ActivityDX, bus.ActivityContest},
			"actions":    []string{bus.CmdSkip, bus.CmdHandled, bus.CmdUndo, bus.CmdSetMode, bus.CmdSwitchInstance, bus.CmdDeleteInstance},
			"input":      s.topics.finder,
			"history":    history,
			"awards":     awards,
		},
	})
}

// onCandidates takes bmrec's retained candidates. Jobs worker only.
func (s *Service) onCandidates(p []byte) {
	var st bus.CQFinderState
	if err := json.Unmarshal(p, &st); err != nil {
		s.log.Warn("bad candidate state", "err", err)
		return
	}
	if s.eng.SetCandidates(st) {
		s.mu.Lock()
		s.finderSeen = s.now()
		s.mu.Unlock()
		s.poke()
	}
}

// onCmd applies one /cmd. Jobs worker only. An action on a row that left the
// list is logged and ignored, so two windows cannot act twice.
func (s *Service) onCmd(p []byte) {
	var c struct{ Action, Value string }
	if err := json.Unmarshal(p, &c); err != nil {
		s.log.Warn("bad command", "err", err)
		return
	}
	var err error
	switch c.Action {
	case bus.CmdSkip, bus.CmdHandled:
		var t bus.Target
		if err = json.Unmarshal([]byte(c.Value), &t); err == nil {
			if c.Action == bus.CmdSkip {
				err = s.eng.Skip(t)
			} else {
				if t.How == "" {
					t.How = "qsy"
				}
				err = s.eng.Handled(t)
			}
		}
	case bus.CmdUndo:
		err = s.eng.Undo()
	case bus.CmdSetMode:
		err = s.SwitchDX()
	case bus.CmdSwitchInstance:
		var r bus.SwitchReq
		if err = json.Unmarshal([]byte(c.Value), &r); err == nil {
			err = s.SwitchContest(r)
		}
	case bus.CmdDeleteInstance:
		var id int64
		if id, err = strconv.ParseInt(c.Value, 10, 64); err == nil {
			err = s.DeleteInstance(id)
		}
	default:
		err = fmt.Errorf("unknown action %q", c.Action)
	}
	if err != nil {
		level := s.log.Warn
		if err == engine.ErrNotListed || err == engine.ErrNothingToUndo {
			level = s.log.Info
		}
		level("command refused", "action", c.Action, "err", err)
	}
	s.poke()
}
