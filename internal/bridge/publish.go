package bridge

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// Publisher is the MQTT surface the bridge needs. It's a small interface so
// the bridge logic can be unit-tested with a fake publisher.
type Publisher interface {
	// Publish sends a message. retained=true makes the broker store the last
	// value for late subscribers.
	Publish(topic string, retained bool, payload []byte) error
	// IsConnected reports broker connectivity.
	IsConnected() bool
}

// MemoPublisher is a Publisher that records every Publish call. Used in tests.
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

// WithClock sets the clock used for recorded At timestamps (for deterministic tests).
func (m *MemoPublisher) WithClock(now func() time.Time) *MemoPublisher {
	m.now = now
	return m
}

// Publish records the message.
func (m *MemoPublisher) Publish(topic string, retained bool, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, MemoMsg{Topic: topic, Retained: payload != nil && retained, Payload: payload, At: m.now()})
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

// Publish via paho. Retained documents (meta, state) go at QoS 1 per the station
// model §8. Non-retained traffic stays QoS 0.
func (p *PahoPublisher) Publish(topic string, retained bool, payload []byte) error {
	qos := byte(0)
	if retained {
		qos = 1
	}
	tok := p.Client.Publish(topic, qos, retained, payload)
	// Don't wait on the token (keeps the hot path concurrent); paho queues QoS 1
	// messages for delivery. Connection errors surface via IsConnected().
	_ = tok
	return nil
}

// IsConnected proxies to paho.
func (p *PahoPublisher) IsConnected() bool { return p.Client.IsConnected() }

// publishDiscovery serializes a discovery config to JSON and publishes it to
// the config topic (always retained). Retained for the legacy embedded path,
// which this bridge does not ship; kept here for symmetry with the sibling
// bridges in case the gated path is ever added.
func publishDiscovery(pub Publisher, topic string, cfg any) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal discovery: %w", err)
	}
	return pub.Publish(topic, true, b)
}
