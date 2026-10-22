// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mqtt wires the engine to the station bus. Every input — paho
// message, poll result, tick — is Enqueued onto one jobs channel and run on
// a single worker, so the engine is single-threaded and no paho handler ever
// publishes. This layer is deliberately thin.
package mqtt

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"antmode/internal/config"
	"antmode/internal/engine"
)

// publishTimeout bounds a Publish Wait so a broker outage cannot wedge the
// jobs worker.
const publishTimeout = 10 * time.Second

// tickInterval drives engine.Tick (startup hold, timeouts, retries).
const tickInterval = time.Second

// publisher adapts paho to engine.Publisher. The client is set after
// construction and on every reconnect, so it sits behind a lock.
type publisher struct {
	mu sync.RWMutex
	cl paho.Client
}

func (p *publisher) set(c paho.Client) { p.mu.Lock(); p.cl = c; p.mu.Unlock() }

// Publish fails fast while the link is down instead of letting paho queue
// the message: a queued retained set_power replayed after a reconnect could
// override what powerseq decided meanwhile.
func (p *publisher) Publish(topic string, qos byte, retained bool, payload []byte) error {
	p.mu.RLock()
	cl := p.cl
	p.mu.RUnlock()
	if cl == nil || !cl.IsConnectionOpen() {
		return fmt.Errorf("publish %s: mqtt link down", topic)
	}
	tok := cl.Publish(topic, qos, retained, payload)
	if !tok.WaitTimeout(publishTimeout) {
		return fmt.Errorf("publish %s: timed out after %s", topic, publishTimeout)
	}
	return tok.Error()
}

// Client owns the paho connection and the jobs worker.
type Client struct {
	client paho.Client
	pub    *publisher
	eng    *engine.Engine
	cfg    config.Config
	log    *slog.Logger
	jobs   chan func()
	// adopt: no state file was loaded — subscribe the own retained /state
	// so the engine can take over a lock that is still on the broker.
	adopt bool

	tickQueued atomic.Bool
}

// New builds the engine (over loaded, nil when there is no state file) and
// starts the jobs worker and the tick. It does not touch the network.
func New(ctx context.Context, cfg config.Config, saver engine.Saver, loaded *engine.Persisted, log *slog.Logger) *Client {
	pub := &publisher{}
	c := &Client{
		pub: pub, eng: engine.New(cfg, pub, saver, loaded, log, nil), cfg: cfg, log: log,
		jobs: make(chan func(), 64), adopt: loaded == nil,
	}
	go sharedmqtt.RunJobs(ctx, c.jobs)
	go func() {
		t := time.NewTicker(tickInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Coalesced: at most one Tick waits in the queue, so a
				// worker stalled on publishes cannot fill it with ticks.
				if c.tickQueued.CompareAndSwap(false, true) {
					sharedmqtt.Enqueue(c.jobs, func() {
						c.tickQueued.Store(false)
						c.eng.Tick()
					})
				}
			}
		}
	}()
	return c
}

// Do runs f on the jobs worker with the engine (poll results).
func (c *Client) Do(f func(e *engine.Engine)) {
	sharedmqtt.Enqueue(c.jobs, func() { f(c.eng) })
}

// Connect connects to the broker (ctx-aware). With ConnectRetry it blocks
// until the broker accepts; a Warn every 30 s makes that visible.
func (c *Client) Connect(ctx context.Context) error {
	log, m := c.log, c.cfg.MQTT
	client := paho.NewClient(c.buildOptions())
	c.client = client
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				log.Warn("still connecting to the MQTT broker", "broker", m.Broker)
			}
		}
	}()
	log.Info("connecting to the MQTT broker", "broker", m.Broker)
	if err := sharedmqtt.Connect(ctx, client); err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	c.pub.set(client)
	return nil
}

// buildOptions is the paho configuration (separate so a test pins it).
func (c *Client) buildOptions() *paho.ClientOptions {
	log, pub, eng, m := c.log, c.pub, c.eng, c.cfg.MQTT
	opts := paho.NewClientOptions()
	opts.AddBroker(m.Broker)
	clientID := m.ClientID
	if clientID == "" {
		clientID = m.Site + "-" + m.Station + "-" + m.Slot
	}
	opts.SetClientID(clientID)
	if m.User != "" {
		opts.SetUsername(m.User)
	}
	if m.Password != "" {
		opts.SetPassword(m.Password)
	}
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	// Clean session: every input is a retained snapshot, re-delivered on
	// each subscribe; a persistent session would queue QoS-1 /state before
	// OnConnect subscribes and poison itself (beamsteer, live 2026-10-01).
	// Own /cmd is QoS 0 so the broker never queues a one-shot for us.
	opts.SetCleanSession(true)
	statusTopic := schema.StatusTopic(m.Site, m.Station, m.Slot)
	opts.SetWill(statusTopic, "offline", 1, true)

	opts.OnConnect = func(cl paho.Client) {
		log.Info("MQTT (re)connected", "broker", m.Broker)
		cl.Publish(statusTopic, 1, true, []byte("online")) // fire-and-forget: OnConnect must not block on it
		pub.set(cl)
		// Queued ahead of every retained replay the subscriptions bring in.
		sharedmqtt.Enqueue(c.jobs, func() { eng.SetConnected(true) })
		c.subscribeAll(cl)
		sharedmqtt.Enqueue(c.jobs, eng.Republish)
	}
	opts.OnConnectionLost = func(_ paho.Client, err error) {
		log.Warn("MQTT connection lost", "err", err)
		sharedmqtt.Enqueue(c.jobs, func() { eng.SetConnected(false) })
	}
	return opts
}

// Close publishes offline and disconnects.
func (c *Client) Close() {
	if c == nil || c.client == nil {
		return
	}
	if c.client.IsConnectionOpen() {
		m := c.cfg.MQTT
		c.client.Publish(schema.StatusTopic(m.Site, m.Station, m.Slot), 1, true, []byte("offline")).WaitTimeout(time.Second)
		c.client.Disconnect(250)
	}
}

// on returns a paho handler that hands a copy of the payload to f on the
// jobs worker. Handlers never publish.
func (c *Client) on(f func([]byte)) paho.MessageHandler {
	return func(_ paho.Client, msg paho.Message) {
		p := append([]byte(nil), msg.Payload()...)
		sharedmqtt.Enqueue(c.jobs, func() { f(p) })
	}
}

// Topics lists every subscription in order (own /state first when adopting,
// then own /cmd, then the siblings), so a test can pin it.
func (c *Client) Topics() []Sub {
	m, s, e := c.cfg.MQTT, c.cfg.Slots, c.eng
	sib := func(rel, suffix string) string {
		st, sl := config.SplitSlot(rel)
		return schema.SiblingTopic(m.Site, st, sl, suffix)
	}
	var subs []Sub
	if c.adopt {
		subs = append(subs, Sub{schema.StateTopic(m.Site, m.Station, m.Slot), 1, e.OwnState})
	}
	subs = append(subs,
		Sub{schema.CmdTopic(m.Site, m.Station, m.Slot), 0, e.Cmd},
		Sub{sib(s.PowerSeq, "status"), 1, e.PowerSeqStatus},
		Sub{sib(s.PowerSeq, "state"), 1, e.PowerSeqState},
		Sub{sib(s.Rotator, "status"), 1, e.RotatorStatus},
		Sub{sib(s.Rotator, "state"), 1, e.RotatorState},
		Sub{sib(s.AntCtrl, "status"), 1, e.AntCtrlStatus},
		Sub{sib(s.AntCtrl, "state"), 1, e.AntCtrlState},
		Sub{sib(s.AntSwitch, "status"), 1, e.AntSwitchStatus},
		Sub{sib(s.AntSwitch, "state"), 1, e.AntSwitchState},
		Sub{sib(s.Radio, "status"), 1, e.RadioStatus},
		Sub{sib(s.Radio, "state"), 1, e.RadioState},
		Sub{sib(s.AntennaPower, "status"), 1, e.PowerStatus},
		Sub{sib(s.AntennaPower, "state"), 1, e.PowerState},
	)
	return subs
}

// Sub is one subscription.
type Sub struct {
	Topic string
	QoS   byte
	F     func([]byte)
}

func (c *Client) subscribeAll(cl paho.Client) {
	for _, s := range c.Topics() {
		// Bounded Wait: a stalled SUBACK must not park OnConnect silently.
		if tok := cl.Subscribe(s.Topic, s.QoS, c.on(s.F)); !tok.WaitTimeout(publishTimeout) || tok.Error() != nil {
			c.log.Warn("subscribe failed", "topic", s.Topic, "err", tok.Error())
			continue
		}
		c.log.Debug("subscribed", "topic", s.Topic, "qos", s.QoS)
	}
}
