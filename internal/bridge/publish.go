package bridge

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"flex2mqtt/internal/ha"
)

// Publisher is the MQTT surface the bridge needs. It's a small interface so
// the bridge logic can be unit-tested with a fake publisher.
type Publisher interface {
	// Publish sends a message. retained=true makes the broker store the
	// last value for late subscribers.
	Publish(topic string, retained bool, payload []byte) error
	// IsConnected reports broker connectivity.
	IsConnected() bool
}

// MemoPublisher is a Publisher that records every Publish call. Used in
// tests to assert what the bridge emitted.
type MemoPublisher struct {
	mu   sync.Mutex
	msgs []MemoMsg
	now  func() time.Time
}

// MemoMsg is one recorded publish.
type MemoMsg struct {
	Topic    string
	Retained bool
	Payload  []byte
	At       time.Time
}

// NewMemoPublisher returns a recording publisher.
func NewMemoPublisher() *MemoPublisher {
	return &MemoPublisher{now: time.Now}
}

// Publish records the message.
func (m *MemoPublisher) Publish(topic string, retained bool, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, MemoMsg{Topic: topic, Retained: retained, Payload: payload, At: m.now()})
	return nil
}

// IsConnected always returns true for the fake.
func (m *MemoPublisher) IsConnected() bool { return true }

// Messages returns a copy of recorded messages.
func (m *MemoPublisher) Messages() []MemoMsg {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MemoMsg, len(m.msgs))
	copy(out, m.msgs)
	return out
}

// Reset clears recorded messages.
func (m *MemoPublisher) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = nil
}

// PahoPublisher adapts paho.mqtt.golang's Client to the Publisher interface.
type PahoPublisher struct {
	Client pahomqtt.Client
}

// Publish via paho at QoS 0 (telemetry is high-frequency and stateless).
func (p *PahoPublisher) Publish(topic string, retained bool, payload []byte) error {
	tok := p.Client.Publish(topic, 0, retained, payload)
	// QoS 0 is fire-and-forget in paho; we don't wait on the token to avoid
	// serializing the hot meter path. Connection errors surface via
	// IsConnected() and OnConnectionLost.
	_ = tok
	return nil
}

// IsConnected proxies to paho.
func (p *PahoPublisher) IsConnected() bool { return p.Client.IsConnected() }

// publishDiscovery serializes a ha.DiscoveryConfig to JSON and publishes it
// to the config topic (always retained, so HA picks it up on its restart).
func publishDiscovery(pub Publisher, topic string, cfg ha.DiscoveryConfig) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal discovery: %w", err)
	}
	return pub.Publish(topic, true, b)
}
