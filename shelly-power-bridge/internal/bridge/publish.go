package bridge

import (
	"fmt"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// publishTimeout bounds a paho token Wait (powerseq pattern). A broker that
// accepts the TCP connection but stalls PUBACKs must not park the slot's
// single jobs worker — /state freezes, the heartbeat stops flipping
// device_online, and set_power commands (incl. powerseq shutdown steps) are
// never applied for that slot (review S1b).
const publishTimeout = 10 * time.Second

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

// Publish publishes the payload; retained selects the retain flag. The Wait
// is bounded so a stalled broker surfaces as an error instead of wedging the
// slot worker.
func (p *PahoPublisher) Publish(topic string, retained bool, payload []byte) error {
	tok := p.Client.Publish(topic, 1, retained, payload)
	if !tok.WaitTimeout(publishTimeout) {
		return fmt.Errorf("publish %s: timed out after %s", topic, publishTimeout)
	}
	return tok.Error()
}

// IsConnected reports the underlying paho client's connection state.
func (p *PahoPublisher) IsConnected() bool {
	if p.Client == nil {
		return false
	}
	return p.Client.IsConnected()
}
