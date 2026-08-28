package mqtt

import (
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// Publisher is the MQTT surface the slot needs; small so the slot logic is
// unit-testable with the MemoPublisher.
type Publisher interface {
	// Publish sends a message; retained=true makes the broker keep it for late
	// subscribers.
	Publish(topic string, retained bool, payload []byte) error
	// IsConnected reports broker connectivity.
	IsConnected() bool
}

// PahoPublisher adapts a paho client to Publisher. Retained documents (meta,
// state) go at QoS 1 per the station model; non-retained traffic stays QoS 0.
// Publish never waits on the token — paho queues QoS 1 messages.
type PahoPublisher struct {
	Client pahomqtt.Client
}

func (p *PahoPublisher) Publish(topic string, retained bool, payload []byte) error {
	qos := byte(0)
	if retained {
		qos = 1
	}
	_ = p.Client.Publish(topic, qos, retained, payload)
	return nil
}

func (p *PahoPublisher) IsConnected() bool { return p.Client.IsConnected() }

// MemoMsg is one recorded publish (tests).
type MemoMsg struct {
	Topic    string
	Retained bool
	Payload  []byte
	At       time.Time
}

// MemoPublisher records every Publish call.
type MemoPublisher struct {
	mu   sync.Mutex
	msgs []MemoMsg
}

func NewMemoPublisher() *MemoPublisher { return &MemoPublisher{} }

func (m *MemoPublisher) Publish(topic string, retained bool, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, MemoMsg{Topic: topic, Retained: payload != nil && retained, Payload: payload, At: time.Now()})
	return nil
}

func (m *MemoPublisher) IsConnected() bool { return true }

// Messages returns a copy of the recorded messages.
func (m *MemoPublisher) Messages() []MemoMsg {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MemoMsg, len(m.msgs))
	copy(out, m.msgs)
	return out
}

// Last returns the last message published to topic (nil if none).
func (m *MemoPublisher) Last(topic string) *MemoMsg {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.msgs) - 1; i >= 0; i-- {
		if m.msgs[i].Topic == topic {
			mm := m.msgs[i]
			return &mm
		}
	}
	return nil
}

// Reset clears the recording.
func (m *MemoPublisher) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = nil
}
