// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mqtt wires the engine to the station bus and to the PstRotator UDP
// listener. Every input — paho message or UDP datagram — is Enqueued onto one
// jobs channel and run on a single worker, so the engine is single-threaded
// and no paho handler ever publishes. This layer is deliberately thin.
package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	"codeberg.org/kgbvax/stationa/shared/pstrotator"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"beamsteer/internal/config"
	"beamsteer/internal/engine"
)

// publishTimeout bounds a Publish Wait so a broker outage cannot wedge the
// jobs worker.
const publishTimeout = 10 * time.Second

// publisher adapts paho to engine.Publisher. The client is set after
// construction and on every reconnect, so it sits behind a lock.
type publisher struct {
	mu sync.RWMutex
	cl paho.Client
}

func (p *publisher) set(c paho.Client) { p.mu.Lock(); p.cl = c; p.mu.Unlock() }

func (p *publisher) Publish(topic string, retained bool, payload []byte) error {
	p.mu.RLock()
	cl := p.cl
	p.mu.RUnlock()
	if cl == nil {
		return fmt.Errorf("publish %s: mqtt client not connected", topic)
	}
	tok := cl.Publish(topic, 1, retained, payload)
	if !tok.WaitTimeout(publishTimeout) {
		return fmt.Errorf("publish %s: timed out after %s", topic, publishTimeout)
	}
	return tok.Error()
}

// Client owns the paho connection and the jobs worker.
type Client struct {
	client paho.Client
	eng    *engine.Engine
	cfg    config.Config
	log    *slog.Logger
	jobs   chan func()
}

// New connects to the broker (ctx-aware) and starts the jobs worker.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*Client, error) {
	pub := &publisher{}
	eng := engine.New(cfg, pub, log, nil)
	c := &Client{eng: eng, cfg: cfg, log: log, jobs: make(chan func(), 64)}
	go sharedmqtt.RunJobs(ctx, c.jobs)

	m := cfg.MQTT
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
	opts.SetCleanSession(false)
	statusTopic := schema.StatusTopic(m.Site, m.Station, m.Slot)
	opts.SetWill(statusTopic, "offline", 1, true)

	opts.OnConnect = func(cl paho.Client) {
		log.Info("MQTT (re)connected", "broker", m.Broker)
		cl.Publish(statusTopic, 1, true, []byte("online")) // fire-and-forget: OnConnect must not block
		pub.set(cl)
		c.subscribeAll(cl)
		sharedmqtt.Enqueue(c.jobs, eng.Republish)
	}
	opts.OnConnectionLost = func(_ paho.Client, err error) {
		log.Warn("MQTT connection lost", "err", err)
	}

	client := paho.NewClient(opts)
	if err := sharedmqtt.Connect(ctx, client); err != nil {
		return nil, fmt.Errorf("mqtt connect: %w", err)
	}
	pub.set(client)
	c.client = client
	return c, nil
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

// on returns a paho handler that hands the payload to f on the jobs worker.
func (c *Client) on(f func([]byte)) paho.MessageHandler {
	return func(_ paho.Client, msg paho.Message) {
		p := append([]byte(nil), msg.Payload()...)
		sharedmqtt.Enqueue(c.jobs, func() { f(p) })
	}
}

func (c *Client) subscribeAll(cl paho.Client) {
	m, s := c.cfg.MQTT, c.cfg.Steer
	sib := func(slot, suffix string) string { return schema.SiblingTopic(m.Site, m.Station, slot, suffix) }

	c.subscribe(cl, sib(s.RotatorSlot, "status"), 1, c.on(c.eng.RotatorStatus))
	c.subscribe(cl, sib(s.RotatorSlot, "state"), 1, c.on(c.eng.RotatorState))
	c.subscribe(cl, sib(s.AntCtrlSlot, "status"), 1, c.on(c.eng.AntStatus))
	c.subscribe(cl, sib(s.AntCtrlSlot, "state"), 1, c.on(c.eng.AntState))
	c.subscribe(cl, sib(s.RadioSlot, "status"), 1, c.on(c.eng.RadioStatus))
	c.subscribe(cl, sib(s.RadioSlot, "state"), 1, c.on(c.eng.RadioState))

	// Own /cmd. Retained by the console: the enable/disable steady state
	// survives restarts. QoS 0 so a persistent session never queues old
	// commands on top of the retained one.
	c.subscribe(cl, schema.CmdTopic(m.Site, m.Station, m.Slot), 0, c.on(c.onCmd))
}

func (c *Client) onCmd(p []byte) {
	if len(strings.TrimSpace(string(p))) == 0 {
		return // retained cmd cleared
	}
	var cmd struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(p, &cmd); err != nil {
		c.log.Warn("bad /cmd", "err", err)
		return
	}
	switch cmd.Action {
	case "enable":
		c.eng.SetEnabled(true)
	case "disable":
		c.eng.SetEnabled(false)
	default:
		c.log.Warn("unknown /cmd action", "action", cmd.Action)
	}
}

func (c *Client) subscribe(cl paho.Client, topic string, qos byte, h paho.MessageHandler) {
	// Bounded Wait: a stalled SUBACK must not park OnConnect silently.
	if tok := cl.Subscribe(topic, qos, h); !tok.WaitTimeout(publishTimeout) || tok.Error() != nil {
		c.log.Warn("subscribe failed", "topic", topic, "err", tok.Error())
		return
	}
	c.log.Debug("subscribed", "topic", topic, "qos", qos)
}

// PstRotatorHandler adapts the engine to the UDP listener. Motion and stop
// are Enqueued (the UDP loop never blocks); the query reads the engine's
// locked heading directly.
func (c *Client) PstRotatorHandler() pstrotator.Handler { return udpHandler{c} }

type udpHandler struct{ c *Client }

func (h udpHandler) Goto(d pstrotator.Datagram) {
	if !d.HasAZ {
		return // elevation is meaningless for the HF rotator
	}
	sharedmqtt.Enqueue(h.c.jobs, func() { h.c.eng.Goto(d.AZ) })
}

func (h udpHandler) Stop() { sharedmqtt.Enqueue(h.c.jobs, h.c.eng.Stop) }

func (h udpHandler) Park() { h.c.log.Info("pstrotator PARK ignored") }

func (h udpHandler) Readback(ax pstrotator.Axis) (float64, bool) {
	if ax != pstrotator.AZ {
		return 0, false
	}
	return h.c.eng.Readback()
}
