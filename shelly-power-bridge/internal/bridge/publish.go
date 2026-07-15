package bridge

import (
	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// Publisher is the minimal MQTT surface the SlotBridge uses. The paho client
// adapter implements it; tests use MemoPublisher.
type Publisher interface {
	Publish(topic string, retained bool, payload []byte) error
	IsConnected() bool
}

// PahoPublisher adapts a paho client to Publisher. QoS 1 is used for retained
// messages (meta/state); the bridge only publishes retained topics, so QoS 1
// throughout.
type PahoPublisher struct {
	Client pahomqtt.Client
}

// Publish publishes the payload; retained selects the retain flag.
func (p *PahoPublisher) Publish(topic string, retained bool, payload []byte) error {
	tok := p.Client.Publish(topic, 1, retained, payload)
	tok.Wait() // ensure the publish is queued before returning
	return tok.Error()
}

// IsConnected reports the underlying paho client's connection state.
func (p *PahoPublisher) IsConnected() bool {
	if p.Client == nil {
		return false
	}
	return p.Client.IsConnected()
}
