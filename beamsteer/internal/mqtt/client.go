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
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// Publish fails fast while the link is down instead of letting paho queue
// the message: a set_az stored during an outage would be replayed on
// reconnect (in map order, after the operator's STOP) and move the mast to a
// stale target; the worker would also block 10 s per publish meanwhile.
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

	// tickQueued coalesces the 1 s tick: at most one Tick waits in the jobs
	// queue, so a worker stalled on publishes cannot fill the queue with
	// ticks and make it drop real jobs (Gotos, retained replays).
	tickQueued atomic.Bool
}

// tickInterval drives engine.Tick (in-flight command expiry, held-flip retry).
const tickInterval = time.Second

// New builds the engine and starts the jobs worker and the tick. It does not
// touch the network, so main can bind the UDP listener before the broker
// connect (which may block for a long time).
func New(ctx context.Context, cfg config.Config, log *slog.Logger) *Client {
	pub := &publisher{}
	c := &Client{pub: pub, eng: engine.New(cfg, pub, log, nil), cfg: cfg, log: log, jobs: make(chan func(), 64)}
	go sharedmqtt.RunJobs(ctx, c.jobs)
	go func() {
		t := time.NewTicker(tickInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
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

// Connect connects to the broker (ctx-aware). With ConnectRetry it blocks
// until the broker accepts; a Warn every 30 s makes that visible instead of
// a silent hang.
func (c *Client) Connect(ctx context.Context) error {
	log, m := c.log, c.cfg.MQTT
	opts := c.buildOptions()
	client := paho.NewClient(opts)
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

// buildOptions is the paho configuration (separate so a test pins the
// clean-session fix).
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
	// Clean session, deliberately. With a persistent session the broker
	// queues QoS-1 /state while beamsteer is down and delivers it right
	// after CONNACK — before OnConnect has subscribed, so paho has no route,
	// never PUBACKs, and after 20 such messages mosquitto's in-flight window
	// is full: no rotator/ant-ctrl/radio data ever again, across restarts
	// (live 2026-10-01). Everything beamsteer needs is retained, so a clean
	// session loses nothing — and connecting clean also discards the old
	// poisoned session on the broker.
	opts.SetCleanSession(true)
	statusTopic := schema.StatusTopic(m.Site, m.Station, m.Slot)
	opts.SetWill(statusTopic, "offline", 1, true)

	opts.OnConnect = func(cl paho.Client) {
		log.Info("MQTT (re)connected", "broker", m.Broker)
		cl.Publish(statusTopic, 1, true, []byte("online")) // fire-and-forget: OnConnect must not block
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

	// Own /cmd FIRST, so the retained enable/disable is queued ahead of the
	// sibling replays: otherwise their liveness changes publish /state with
	// the default enabled:false and AUTO flickers off on every start. QoS 0
	// with a clean session: nothing is ever queued for us.
	c.subscribe(cl, schema.CmdTopic(m.Site, m.Station, m.Slot), 0, func(_ paho.Client, msg paho.Message) {
		p, retained := append([]byte(nil), msg.Payload()...), msg.Retained()
		sharedmqtt.Enqueue(c.jobs, func() { c.onCmd(p, retained) })
	})

	c.subscribe(cl, sib(s.RotatorSlot, "status"), 1, c.on(c.eng.RotatorStatus))
	c.subscribe(cl, sib(s.RotatorSlot, "state"), 1, c.on(c.eng.RotatorState))
	c.subscribe(cl, sib(s.AntCtrlSlot, "status"), 1, c.on(c.eng.AntStatus))
	c.subscribe(cl, sib(s.AntCtrlSlot, "state"), 1, c.on(c.eng.AntState))
	c.subscribe(cl, sib(s.RadioSlot, "status"), 1, c.on(c.eng.RadioStatus))
	c.subscribe(cl, sib(s.RadioSlot, "state"), 1, c.on(c.eng.RadioState))
}

// onCmd handles the own /cmd. retained is the broker's replay flag: the
// enable/disable steady state is meant to replay, an aim never is (a retained
// aim would re-steer the mast on every connect).
func (c *Client) onCmd(p []byte, retained bool) {
	if len(strings.TrimSpace(string(p))) == 0 {
		return // retained cmd cleared
	}
	var cmd struct {
		Action string          `json:"action"`
		Value  json.RawMessage `json:"value"`
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
	case "aim":
		// The console's tap-to-aim with AUTO on: the same decision as a
		// logger request. Published unretained; the value is a bearing.
		if retained {
			c.log.Warn("retained aim ignored (aim must be published unretained)")
			return
		}
		b, ok := parseBearing(cmd.Value)
		if !ok {
			c.log.Warn("bad aim value", "value", string(cmd.Value))
			return
		}
		c.eng.Goto(b)
	default:
		c.log.Warn("unknown /cmd action", "action", cmd.Action)
	}
}

// parseBearing accepts a JSON number or numeric string in [-360, 720] (a
// bearing; Goto normalises it).
func parseBearing(raw json.RawMessage) (float64, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, false
	}
	var b float64
	switch x := v.(type) {
	case float64:
		b = x
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false
		}
		b = f
	default:
		return 0, false
	}
	if math.IsNaN(b) || math.IsInf(b, 0) || b < -360 || b > 720 {
		return 0, false
	}
	return b, true
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
