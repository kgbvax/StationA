package mqtt

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// TestEnqueueNonBlocking verifies Enqueue never blocks when the channel is full:
// it drops the job instead. This is the property that keeps paho handlers off the
// blocking publish path.
func TestEnqueueNonBlocking(t *testing.T) {
	jobs := make(chan func(), 1)
	// Fill the buffer.
	jobs <- func() {}

	done := make(chan struct{})
	go func() {
		Enqueue(jobs, func() {}) // would block on a send; must drop instead
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Enqueue blocked on a full channel instead of dropping")
	}
}

// TestRunJobsRunsClosures verifies RunJobs runs queued closures until ctx done.
func TestRunJobsRunsClosures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	jobs := make(chan func(), 4)

	var ran atomic.Int32
	for i := 0; i < 3; i++ {
		Enqueue(jobs, func() { ran.Add(1) })
	}

	done := make(chan struct{})
	go func() { RunJobs(ctx, jobs); close(done) }()

	if !waitCnt(&ran, 3, time.Second) {
		t.Fatalf("ran %d closures, want 3", ran.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunJobs did not return after ctx cancel")
	}
}

func waitCnt(c *atomic.Int32, want int32, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c.Load() >= want {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return c.Load() >= want
}

// fakeToken is an already-complete paho token.
type fakeToken struct{ err error }

func (fakeToken) Wait() bool                     { return true }
func (fakeToken) WaitTimeout(time.Duration) bool { return true }
func (fakeToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (t fakeToken) Error() error                 { return t.err }

// fakeClient records the order of Publish and Disconnect calls. The embedded
// nil interface panics if the code under test calls anything else.
type fakeClient struct {
	pahomqtt.Client
	open   bool
	events []string
	topic  string
	qos    byte
	retain bool
	body   string
	quiesc uint
}

func (f *fakeClient) IsConnectionOpen() bool { return f.open }
func (f *fakeClient) Publish(topic string, qos byte, retained bool, payload interface{}) pahomqtt.Token {
	f.events = append(f.events, "publish")
	f.topic, f.qos, f.retain, f.body = topic, qos, retained, string(payload.([]byte))
	return fakeToken{}
}
func (f *fakeClient) Disconnect(q uint) { f.events = append(f.events, "disconnect"); f.quiesc = q }

// TestDisconnectOfflinePublishesBeforeDisconnect: a clean DISCONNECT suppresses
// the LWT, so the retained "offline" must go out first, retained at QoS 1.
func TestDisconnectOfflinePublishesBeforeDisconnect(t *testing.T) {
	f := &fakeClient{open: true}
	DisconnectOffline(f, "muehle/hf/radio/status", 500)
	if got := strings.Join(f.events, ","); got != "publish,disconnect" {
		t.Fatalf("events = %q, want publish,disconnect", got)
	}
	if f.topic != "muehle/hf/radio/status" || f.body != "offline" || f.qos != 1 || !f.retain {
		t.Fatalf("publish = %q %q qos=%d retained=%v", f.topic, f.body, f.qos, f.retain)
	}
	if f.quiesc != 500 {
		t.Fatalf("quiesce = %d, want 500", f.quiesc)
	}
}

// TestDisconnectOfflineSkipsPublishWhenDown: with no open connection a publish
// would only block; the broker owns the will, so just disconnect.
func TestDisconnectOfflineSkipsPublishWhenDown(t *testing.T) {
	f := &fakeClient{open: false}
	DisconnectOffline(f, "t/status", 250)
	if got := strings.Join(f.events, ","); got != "disconnect" {
		t.Fatalf("events = %q, want disconnect only", got)
	}
}

func TestDisconnectOfflineNilClient(t *testing.T) {
	DisconnectOffline(nil, "t/status", 250) // must not panic
}
