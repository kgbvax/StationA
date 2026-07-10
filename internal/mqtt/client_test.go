package mqtt

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"antennaselect/internal/config"
	"antennaselect/internal/reconcile"
)

// fakePaho embeds a nil paho.Client so only Publish is exercised; the test never calls any
// other paho method (no Connect/Subscribe/IsConnectionOpen on this path).
type fakePaho struct {
	paho.Client
	pub func(topic string, qos byte, retained bool, payload any) paho.Token
}

func (f fakePaho) Publish(topic string, qos byte, retained bool, payload any) paho.Token {
	return f.pub(topic, qos, retained, payload)
}

// blockingToken blocks Wait() until `release` is closed. It stands in for a slow broker:
// its Wait() blocks indefinitely until released, the exact condition that deadlocked paho's
// inline dispatch before the worker fix (update publishes synchronously; an inline
// onRadioState would block paho's matchAndDispatch goroutine waiting for the PUBACK the
// stalled read loop can never deliver).
type blockingToken struct{ release <-chan struct{} }

func (t blockingToken) Wait() bool { <-t.release; return true }
func (t blockingToken) WaitTimeout(d time.Duration) bool {
	select {
	case <-t.release:
		return true
	case <-time.After(d):
		return false
	}
}
func (t blockingToken) Done() <-chan struct{} { return make(chan struct{}) }
func (t blockingToken) Error() error          { return nil }

// fakeMessage is a minimal paho.Message for handler tests that never touch a real client.
type fakeMessage struct {
	topic   string
	payload []byte
}

func (m fakeMessage) Duplicate() bool   { return false }
func (m fakeMessage) Qos() byte         { return 0 }
func (m fakeMessage) Retained() bool    { return false }
func (m fakeMessage) Topic() string     { return m.topic }
func (m fakeMessage) MessageID() uint16 { return 0 }
func (m fakeMessage) Payload() []byte   { return m.payload }
func (m fakeMessage) Ack()              {}

// TestOnRadioStateDefersReconcile is the regression guard for the paho-handler deadlock.
// onRadioState runs on paho's matchAndDispatch goroutine (OrderMatters is the default) and
// must NOT run update() inline, because update() publishes synchronously and that blocks
// dispatch after the first message. Against the pre-fix code (`c.update(...)` called
// directly inside onRadioState) this test hangs at the first select and fails on timeout;
// with the worker it returns immediately and the deferred reconcile/publish later runs
// (and blocks) on the worker goroutine, not paho's.
func TestOnRadioStateDefersReconcile(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	var once sync.Once
	var calls int32
	fake := fakePaho{pub: func(string, byte, bool, any) paho.Token {
		atomic.AddInt32(&calls, 1)
		once.Do(func() { close(reached) })
		return blockingToken{release: release}
	}}

	// Minimal reconciler: update() reaches publishJSON on the first call regardless of the
	// decision (haveDecision is false), so the publish — and thus the block — is hit.
	rec := reconcile.New(config.Config{})
	c := &Client{
		client: fake,
		rec:    rec,
		site:   "s", station: "st", slot: "sl",
		jobs: make(chan func(), 256),
		done: make(chan struct{}),
	}
	go c.runJobs()

	msg := fakeMessage{
		topic:   "s/st/radio/state",
		payload: []byte(`{"band":"20m","freq_hz":14000000,"tx":"rx"}`),
	}

	// Phase 1 — onRadioState must return without waiting for the (blocked) publish.
	returned := make(chan struct{})
	go func() {
		c.onRadioState(nil, msg)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("onRadioState blocked: reconcile/publish ran inline on the caller (paho-dispatch) goroutine")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("publish happened during onRadioState (%d call(s)); work ran inline, not deferred", got)
	}

	// Phase 2 — the worker runs the deferred update and reaches the blocking publish.
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("worker never reached the deferred publish")
	}
	close(release) // let the publish complete
	close(c.done)  // tell the worker to exit
}
