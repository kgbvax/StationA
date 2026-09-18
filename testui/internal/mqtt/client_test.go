package mqtt

import (
	"errors"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// ----- fakes -------------------------------------------------------------------

// fakePaho embeds a nil paho.Client and overrides exactly what Publish touches
// (IsConnectionOpen + Publish); no Connect/Subscribe on this path.
type fakePaho struct {
	paho.Client
	open    bool
	pubbed  int
	pubFunc func(topic string) paho.Token
}

func (f *fakePaho) IsConnectionOpen() bool { return f.open }
func (f *fakePaho) Publish(topic string, _ byte, _ bool, _ interface{}) paho.Token {
	f.pubbed++
	if f.pubFunc != nil {
		return f.pubFunc(topic)
	}
	return instantToken{}
}

// instantToken completes immediately — a fast broker.
type instantToken struct{ err error }

func (t instantToken) Wait() bool                     { return true }
func (t instantToken) WaitTimeout(time.Duration) bool { return t.err == nil }
func (t instantToken) Done() <-chan struct{}          { return make(chan struct{}) }
func (t instantToken) Error() error                   { return t.err }

// blockingToken never completes — a broker that accepted the TCP conn but
// never acks (the wedge the bounded wait defends against).
type blockingToken struct{ release <-chan struct{} }

func (t blockingToken) Wait() bool                     { <-t.release; return true }
func (t blockingToken) WaitTimeout(time.Duration) bool { <-t.release; return true }
func (t blockingToken) Done() <-chan struct{}          { return make(chan struct{}) }
func (t blockingToken) Error() error                   { return nil }

// ----- tests -------------------------------------------------------------------

// While paho is mid-reconnect the publish must be DROPPED before it reaches
// paho: queuing would stash it in paho's outbound store and replay it (DUP=1)
// after the reconnect — stale operator intent executing late (review T4).
func TestPublishDroppedWhileReconnecting(t *testing.T) {
	fp := &fakePaho{open: false}
	c := &Client{client: fp, done: make(chan struct{})}
	err := c.Publish("muehle/hf/radio/cmd", 1, false, []byte("{}"))
	if !errors.Is(err, ErrDisconnected) {
		t.Fatalf("err = %v, want ErrDisconnected", err)
	}
	if fp.pubbed != 0 {
		t.Errorf("publish reached paho %d time(s) while disconnected; want dropped before paho", fp.pubbed)
	}
}

// A bare Client (no paho client wired) is "not connected", not a panic.
func TestPublishNilClientIsDisconnected(t *testing.T) {
	c := &Client{done: make(chan struct{})}
	if err := c.Publish("muehle/x/cmd", 1, false, nil); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("err = %v, want ErrDisconnected", err)
	}
}

func TestPublishWhenConnected(t *testing.T) {
	fp := &fakePaho{open: true}
	c := &Client{client: fp, done: make(chan struct{})}
	if err := c.Publish("muehle/hf/radio/cmd", 1, false, []byte("{}")); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if fp.pubbed != 1 {
		t.Errorf("pubbed = %d, want 1", fp.pubbed)
	}
}

// A publish that races into a just-dropped connection resolves within
// publishWait instead of hanging the HTTP handler for the whole outage.
func TestPublishBoundedWait(t *testing.T) {
	old := publishWait
	publishWait = 30 * time.Millisecond
	defer func() { publishWait = old }()

	never := make(chan struct{})
	fp := &fakePaho{open: true, pubFunc: func(string) paho.Token { return blockingToken{release: never} }}
	c := &Client{client: fp, done: make(chan struct{})}

	start := time.Now()
	err := c.Publish("muehle/x/state", 1, true, []byte("{}"))
	if err == nil || errors.Is(err, ErrDisconnected) || errors.Is(err, ErrShuttingDown) {
		t.Fatalf("err = %v, want an unconfirmed-timeout error", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("publish wait took %s; not bounded", el)
	}
}
