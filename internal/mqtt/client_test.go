package mqtt

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hadiscovery/internal/engine"
)

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

// blockingPub blocks every Publish on its release channel, and signals `reached` the first
// time it is entered. It stands in for a slow broker: its Publish blocks indefinitely until
// the test releases it, which is the exact condition that deadlocked paho's inline dispatch
// before the worker fix (the engine publishes synchronously; an inline onMeta would block
// paho's matchAndDispatch goroutine waiting for the PUBACK that the stalled read loop can
// never deliver).
type blockingPub struct {
	release chan struct{}
	reached chan struct{}
	once    sync.Once
	calls   int32
}

func (b *blockingPub) Publish(string, byte, bool, []byte) error {
	atomic.AddInt32(&b.calls, 1)
	b.once.Do(func() { close(b.reached) })
	<-b.release
	return nil
}

// TestOnMetaDefersEngineWork is the regression guard for the live-bus deadlock. onMeta runs
// on paho's matchAndDispatch goroutine (OrderMatters is the default) and must NOT run the
// engine inline, because the engine publishes synchronously and that blocks dispatch after
// the first message. Against the pre-fix code (`c.eng.OnMeta(...)` called directly inside
// onMeta) this test hangs at the first select and fails on timeout; with the worker it
// returns immediately and the deferred publish later runs on the worker goroutine.
func TestOnMetaDefersEngineWork(t *testing.T) {
	pub := &blockingPub{release: make(chan struct{}), reached: make(chan struct{})}
	eng := engine.NewEngine("homeassistant")
	eng.SetPub(pub)

	c := &Client{
		eng:  eng,
		site: "s", station: "st", slot: "sl",
		jobs: make(chan func(), 256),
		done: make(chan struct{}),
	}

	meta := []byte(`{"schema":"1.0","role":"x","expose":{"fields":[{"key":"f","name":"F","type":"string"}]}}`)
	msg := fakeMessage{topic: "s/st/sl/meta", payload: meta}

	// Phase 1 — onMeta must return without waiting for the (blocked) engine publish.
	returned := make(chan struct{})
	go func() {
		c.onMeta(nil, msg)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("onMeta blocked: engine work ran inline on the caller (paho-dispatch) goroutine")
	}
	if got := atomic.LoadInt32(&pub.calls); got != 0 {
		t.Fatalf("engine published during onMeta (%d call(s)); work ran inline, not deferred", got)
	}

	// Phase 2 — the worker drains the queue and runs the deferred engine publish.
	workerDone := make(chan struct{})
	go func() {
		c.runJobs()
		close(workerDone)
	}()
	select {
	case <-pub.reached: // worker reached the blocking publish
	case <-time.After(time.Second):
		t.Fatal("worker never reached the deferred engine publish")
	}
	close(pub.release) // let the publish complete
	close(c.done)      // tell the worker to exit
	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit after done")
	}
	if got := atomic.LoadInt32(&pub.calls); got < 1 {
		t.Fatalf("deferred engine work never ran on the worker (publishes=%d)", got)
	}
}

// TestOnHAStatusDefersEngineWork mirrors the above for the homeassistant/status handler:
// it too must not run the engine inline. (OnHAStatus only publishes when slots are known,
// so this asserts the weaker but still-deadlock-relevant property that the handler returns
// promptly and never blocks the caller even with a blocking publisher wired up.)
func TestOnHAStatusDefersEngineWork(t *testing.T) {
	pub := &blockingPub{release: make(chan struct{}), reached: make(chan struct{})}
	eng := engine.NewEngine("homeassistant")
	eng.SetPub(pub)

	c := &Client{
		eng:  eng,
		site: "s", station: "st", slot: "sl",
		jobs: make(chan func(), 256),
		done: make(chan struct{}),
	}

	returned := make(chan struct{})
	go func() {
		c.onHAStatus(nil, fakeMessage{topic: "homeassistant/status", payload: []byte("online")})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("onHAStatus blocked the caller goroutine")
	}
	close(c.done)
}
