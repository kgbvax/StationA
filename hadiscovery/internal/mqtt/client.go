// Package mqtt wires the hadiscovery engine to the station bus. It connects to the broker,
// registers a last-will on its own /status, publishes its own /meta (role "discovery"),
// subscribes to slot /meta announcements (the MetaFilter) and homeassistant/status, and
// hands each message to the engine. All rendering/discovery logic lives in package engine
// and ha; this layer is deliberately thin.
package mqtt

import (
	"context"
	"encoding/json"
	"log"

	paho "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"hadiscovery/internal/config"
	"hadiscovery/internal/engine"
)

// Client is the bus connection. It owns one paho.Client and feeds a *engine.Engine.
type Client struct {
	client paho.Client
	cfg    config.Config
	eng    *engine.Engine

	site    string
	station string
	slot    string

	// ctx governs this client's lifecycle: it is a child of the ctx passed to New,
	// so a SIGTERM cancels it, and Close cancels it directly to stop the worker
	// before disconnecting. jobs serializes engine work (OnMeta / OnHAStatus) on a
	// single goroutine that is NOT one of paho's dispatch goroutines. This is
	// required because paho delivers message handlers INLINE on its matchAndDispatch
	// goroutine when OrderMatters is true (the default — see paho Subscribe docs:
	// "callback must not block or call functions within this package that may block
	// (e.g. Publish) other than in a new go routine"). The engine's OnMeta publishes
	// synchronously (engine.Pub.Publish -> token.Wait()); running that inline stalls
	// dispatch after the first message: the handler blocks waiting for the outgoing
	// PUBACK while the read loop blocks pushing the next retained PUBLISH into the
	// now-full message channel the stalled dispatcher is no longer draining. Routing
	// the work here lets the handler return immediately, unblocking paho, while
	// keeping the engine single-threaded (its idempotency depends on sequential,
	// ordered OnMeta). The queue + worker live in shared/mqtt (Enqueue/RunJobs) so
	// the same fix is shared with every other stationa consumer.
	ctx    context.Context
	cancel context.CancelFunc
	jobs   chan func()
}

// New connects to the broker, registers the last-will, and subscribes to the meta filter
// and homeassistant/status. ctx governs the connect (a SIGTERM while the broker is
// unreachable interrupts it — paho's Connect().Wait() alone ignores ctx) and the worker
// lifecycle.
func New(ctx context.Context, cfg config.Config, eng *engine.Engine) (*Client, error) {
	c := &Client{
		cfg:     cfg,
		eng:     eng,
		site:    cfg.MQTT.Site,
		station: cfg.MQTT.Station,
		slot:    cfg.MQTT.Slot,
		jobs:    make(chan func(), 256),
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	go sharedmqtt.RunJobs(c.ctx, c.jobs)

	opts := paho.NewClientOptions().
		AddBroker(cfg.MQTT.Broker).
		// Client ID derives from the slot address (model §8).
		SetClientID(orDefault(cfg.MQTT.ClientID, c.site+"-"+c.station+"-"+c.slot)).
		SetAutoReconnect(true).
		SetCleanSession(false).
		SetWill(c.selfTopic("status"), "offline", 1, true)
	if cfg.MQTT.User != "" {
		opts.SetUsername(cfg.MQTT.User)
	}
	if cfg.MQTT.Password != "" {
		opts.SetPassword(cfg.MQTT.Password)
	}
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		log.Printf("[mqtt] connection lost: %v", err)
	})
	opts.SetOnConnectHandler(func(_ paho.Client) {
		log.Printf("[mqtt] connected broker=%s slot=%s", cfg.MQTT.Broker, c.selfBase())
		c.publishString(c.selfTopic("status"), "online", 1, true)
		c.publishMeta()
		c.subscribeAll()
	})

	c.client = paho.NewClient(opts)
	// Wire the engine's publisher to this client before Connect so retained /meta
	// delivered on OnConnect reaches the real broker, not the noop publisher.
	eng.SetPub(c)
	if err := sharedmqtt.Connect(c.ctx, c.client); err != nil {
		c.cancel()
		return nil, err
	}
	return c, nil
}

// Close stops the worker, publishes offline, and disconnects cleanly. The worker is
// stopped before the disconnect so no engine work (which publishes) races with shutdown.
func (c *Client) Close() {
	if c == nil {
		return
	}
	c.cancel()
	if c.client.IsConnectionOpen() {
		c.publishString(c.selfTopic("status"), "offline", 1, true)
		c.client.Disconnect(250)
	}
}

// Publish implements engine.Pub so the engine can emit discovery through this client.
func (c *Client) Publish(topic string, qos byte, retained bool, payload []byte) error {
	token := c.client.Publish(topic, qos, retained, payload)
	if token.Wait() && token.Error() != nil {
		return token.Error()
	}
	return nil
}

// --- subscriptions ----------------------------------------------------------

func (c *Client) subscribeAll() {
	c.subscribe(c.cfg.MQTT.MetaFilter, c.onMeta)
	// HA birth: when Home Assistant (re)starts it publishes "online" here; we re-publish
	// all discovery so its registry is repopulated.
	c.subscribe("homeassistant/status", c.onHAStatus)
}

func (c *Client) onMeta(_ paho.Client, msg paho.Message) {
	// Copy out of the paho message: it is only guaranteed valid for the duration of the
	// handler, but we run the engine work later, off this goroutine.
	topic := msg.Topic()
	payload := append([]byte(nil), msg.Payload()...)
	sharedmqtt.Enqueue(c.jobs, func() { c.eng.OnMeta(topic, payload) })
}

func (c *Client) onHAStatus(_ paho.Client, msg paho.Message) {
	payload := string(msg.Payload()) // string copy; outlives the handler
	sharedmqtt.Enqueue(c.jobs, func() { c.eng.OnHAStatus(payload) })
}

// --- own meta ---------------------------------------------------------------

// publishMeta announces this service on the bus. hadiscovery is a logic slot: role
// "discovery", link "none", no device. Its capabilities advertise the HA component kinds
// it can render, for diagnostics.
func (c *Client) publishMeta() {
	meta := map[string]any{
		"schema":   "1.0",
		"role":     "discovery",
		"link":     "none",
		"location": c.cfg.Location,
		"host":     c.cfg.Host,
		"capabilities": map[string]any{
			"renders": []string{"sensor", "binary_sensor", "number", "select", "button"},
			"filter":  c.cfg.MQTT.MetaFilter,
		},
	}
	c.publishJSON(c.selfTopic("meta"), meta, 1, true)
}

// --- publish helpers --------------------------------------------------------

func (c *Client) subscribe(topic string, handler paho.MessageHandler) {
	if token := c.client.Subscribe(topic, 1, handler); token.Wait() && token.Error() != nil {
		log.Printf("[mqtt] subscribe failed topic=%s err=%v", topic, token.Error())
		return
	}
	log.Printf("[mqtt] subscribed topic=%s", topic)
}

func (c *Client) publishJSON(topic string, v any, qos byte, retained bool) {
	b, _ := json.Marshal(v)
	if token := c.client.Publish(topic, qos, retained, b); token.Wait() && token.Error() != nil {
		log.Printf("[mqtt] publish failed topic=%s err=%v", topic, token.Error())
	}
}

func (c *Client) publishString(topic, payload string, qos byte, retained bool) {
	if token := c.client.Publish(topic, qos, retained, payload); token.Wait() && token.Error() != nil {
		log.Printf("[mqtt] publish failed topic=%s err=%v", topic, token.Error())
	}
}

// --- topic helpers ----------------------------------------------------------
// The address format lives in shared/schema; these wrap it for the suffix style this
// consumer uses (own meta/status).

func (c *Client) selfBase() string { return schema.SlotBase(c.site, c.station, c.slot) }
func (c *Client) selfTopic(suffix string) string {
	return schema.SiblingTopic(c.site, c.station, c.slot, suffix)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}