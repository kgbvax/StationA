// Package mqtt wires the hadiscovery engine to the station bus. It connects to the broker,
// registers a last-will on its own /status, publishes its own /meta (role "discovery"),
// subscribes to slot /meta announcements (the MetaFilter) and homeassistant/status, and
// hands each message to the engine. All rendering/discovery logic lives in package engine
// and ha; this layer is deliberately thin.
package mqtt

import (
	"encoding/json"
	"log"

	paho "github.com/eclipse/paho.mqtt.golang"

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

	// jobs serializes engine work (OnMeta / OnHAStatus) on a single goroutine that is
	// NOT one of paho's dispatch goroutines. This is required because paho delivers message
	// handlers INLINE on its matchAndDispatch goroutine when OrderMatters is true (the
	// default — see paho Subscribe docs: "callback must not block or call functions within
	// this package that may block (e.g. Publish) other than in a new go routine"). The
	// engine's OnMeta publishes synchronously (engine.Pub.Publish -> token.Wait()); running
	// that inline stalls dispatch after the first message: the handler blocks waiting for
	// the outgoing PUBACK while the read loop blocks pushing the next retained PUBLISH into
	// the now-full message channel the stalled dispatcher is no longer draining. Routing
	// the work here lets the handler return immediately, unblocking paho, while keeping the
	// engine single-threaded (its idempotency depends on sequential, ordered OnMeta).
	jobs chan func()
	done chan struct{}
}

// New connects to the broker, registers the last-will, and subscribes to the meta filter
// and homeassistant/status.
func New(cfg config.Config, eng *engine.Engine) (*Client, error) {
	c := &Client{
		cfg:     cfg,
		eng:     eng,
		site:    cfg.MQTT.Site,
		station: cfg.MQTT.Station,
		slot:    cfg.MQTT.Slot,
		jobs:    make(chan func(), 256),
		done:    make(chan struct{}),
	}
	go c.runJobs()

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
	if token := c.client.Connect(); token.Wait() && token.Error() != nil {
		return nil, token.Error()
	}
	return c, nil
}

// Close publishes offline and disconnects cleanly.
func (c *Client) Close() {
	if c == nil {
		return
	}
	// Stop the worker first so no engine work (which publishes) races with shutdown.
	close(c.done)
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
	c.enqueue(func() { c.eng.OnMeta(topic, payload) })
}

func (c *Client) onHAStatus(_ paho.Client, msg paho.Message) {
	payload := string(msg.Payload()) // string copy; outlives the handler
	c.enqueue(func() { c.eng.OnHAStatus(payload) })
}

// enqueue hands work to the worker without blocking paho's dispatch goroutine. It blocks
// only if the queue is full AND the service is not shutting down — impossible in practice
// (a handful of slots, drained by the worker as fast as it can publish); the `done` arm
// guarantees it never blocks during shutdown.
func (c *Client) enqueue(job func()) {
	select {
	case c.jobs <- job:
	case <-c.done:
	}
}

// runJobs is the single goroutine that owns the engine. It exits when Close closes `done`.
func (c *Client) runJobs() {
	for {
		select {
		case job, ok := <-c.jobs:
			if !ok {
				return
			}
			job()
		case <-c.done:
			return
		}
	}
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

func (c *Client) selfBase() string { return c.site + "/" + c.station + "/" + c.slot }
func (c *Client) selfTopic(suffix string) string {
	return c.selfBase() + "/" + suffix
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
