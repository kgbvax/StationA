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
	}

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
	if c != nil && c.client.IsConnectionOpen() {
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
	c.eng.OnMeta(msg.Topic(), msg.Payload())
}

func (c *Client) onHAStatus(_ paho.Client, msg paho.Message) {
	c.eng.OnHAStatus(string(msg.Payload()))
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
