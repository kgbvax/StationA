// Package mqtt connects testui to the station bus as a passive consumer and proxies
// browser publish requests back onto it.
//
// It subscribes to <site>/# at QoS 1 with a persistent session (CleanSession=false) so
// retained /meta, /state, /status replay on every reconnect. Each inbound message is
// handed to a Store (the in-memory slot tree) and fanned out to SSE subscribers.
//
// Publish requests from the HTTP layer (/api/publish, /api/clear) are executed here too,
// but — critically — never inline in a paho message handler. paho delivers handlers
// inline on its matchAndDispatch goroutine when OrderMatters is true (the default);
// calling Publish().Wait() from inside a handler stalls dispatch after the first message
// (the documented live bug that hit hadiscovery and antennaselect). See
// hadiscovery/internal/mqtt/client.go for the canonical write-up.
//
// Two separate execution paths keep a stuck publish from freezing the live stream:
//   - inbound store updates run on a single jobs worker (serial ordering, off paho's
//     dispatch goroutine);
//   - publishes run on their own goroutine, so a slow broker / reconnect backoff on a
//     publish never blocks store updates or SSE fan-out.
package mqtt

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// ErrShuttingDown is returned by Publish when the client is closing, so the HTTP layer
// can report 503 instead of a false success.
var ErrShuttingDown = errors.New("mqtt client shutting down")

// Message is one inbound MQTT publication handed to the store.
type Message struct {
	Topic    string
	Payload  []byte
	Retained bool
	QoS      byte
	TS       time.Time
}

// Store receives inbound messages. The slot tree implements this.
type Store interface {
	Update(m Message)
}

// Client is the paho wrapper.
type Client struct {
	client paho.Client
	store  Store

	// jobs serializes inbound store updates (keeps ordering simple and keeps the work
	// off paho's dispatch goroutine). Publishes do NOT use this worker.
	jobs chan func()
	done chan struct{}

	mu       sync.Mutex
	knownSub bool
}

// New connects to the broker and subscribes to <site>/#. It blocks until the initial
// Connect succeeds (matching the ecosystem convention; auto-reconnect handles later
// drops). The store receives every inbound message.
func New(broker, clientID, site, user, password string, store Store) (*Client, error) {
	c := &Client{
		store: store,
		jobs:  make(chan func(), 1024),
		done:  make(chan struct{}),
	}
	go c.runJobs()

	topic := site + "/#"

	opts := paho.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetCleanSession(false).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			slog.Warn("[mqtt] connection lost", "err", err)
		}).
		SetOnConnectHandler(func(_ paho.Client) {
			slog.Info("[mqtt] connected", "broker", broker, "sub", topic)
			c.subscribe(topic)
		})

	if user != "" {
		opts.SetUsername(user)
	}
	if password != "" {
		opts.SetPassword(password)
	}

	// No Last Will: this is a passive consumer with no slot of its own. It must not
	// publish an online/offline onto the station bus.

	c.client = paho.NewClient(opts)
	if token := c.client.Connect(); token.Wait() && token.Error() != nil {
		return nil, token.Error()
	}
	return c, nil
}

// subscribe (re)subscribes to the wildcard topic. Called from OnConnect so a
// reconnect replays the retained burst.
func (c *Client) subscribe(topic string) {
	handler := func(_ paho.Client, msg paho.Message) {
		// Copy the payload: paho's buffer is only valid for the handler's lifetime,
		// but the store update runs later, off this goroutine, on the worker.
		payload := append([]byte(nil), msg.Payload()...)
		m := Message{
			Topic:    msg.Topic(),
			Payload:  payload,
			Retained: msg.Retained(),
			QoS:      msg.Qos(),
			TS:       time.Now().UTC(),
		}
		c.enqueue(func() { c.store.Update(m) })
	}
	if token := c.client.Subscribe(topic, 1, handler); token.Wait() && token.Error() != nil {
		slog.Error("[mqtt] subscribe failed", "topic", topic, "err", token.Error())
		return
	}
	c.mu.Lock()
	c.knownSub = true
	c.mu.Unlock()
}

// Publish publishes a payload. Retained=false for /cmd (the model §8 safety rule is
// enforced by the HTTP handler before this is called; this layer is a thin proxy).
//
// It runs on its OWN goroutine, not the store-update worker, so a slow broker or
// reconnect backoff on one publish cannot block inbound store updates or SSE fan-out
// (head-of-line blocking). It blocks the caller until the publish has been attempted
// so the HTTP handler can report the error synchronously, and returns ErrShuttingDown
// if the client closes first (so the handler can answer 503, not a false 200).
func (c *Client) Publish(topic string, qos byte, retained bool, payload []byte) error {
	// Fast-path the shutdown case so a Publish issued after Close returns ErrShuttingDown
	// (-> 503) deterministically, instead of racing the spawned goroutine's "not
	// connected" error (-> 502). The later select still handles a close that happens
	// between this check and the goroutine completing.
	select {
	case <-c.done:
		return ErrShuttingDown
	default:
	}

	res := make(chan error, 1)
	go func() {
		tok := c.client.Publish(topic, qos, retained, payload)
		tok.Wait()
		if tok.Error() != nil {
			slog.Error("[mqtt] publish failed", "topic", topic, "err", tok.Error())
		}
		res <- tok.Error()
	}()
	select {
	case err := <-res:
		return err
	case <-c.done:
		return ErrShuttingDown
	}
}

// Close stops the worker and disconnects. Disconnect first (quiesce 250ms) so in-flight
// publishes resolve and paho stops dispatching, then close done to stop the store
// worker. paho v1.5.1's Disconnect handles a mid-reconnect client cleanly, so it is
// safe to call unconditionally.
func (c *Client) Close() {
	if c == nil {
		return
	}
	if c.client != nil {
		c.client.Disconnect(250)
	}
	close(c.done)
}

// enqueue hands an inbound store update to the worker. It blocks on the buffered jobs
// channel under sustained backpressure (e.g. a large retained-replay burst while a
// snapshot read holds the tree lock); the generous buffer plus the fast, non-publishing
// worker makes this practically unreachable, and blocking is preferred over dropping
// messages for a state mirror. It yields immediately once the worker drains or the
// client is closed.
func (c *Client) enqueue(job func()) {
	select {
	case c.jobs <- job:
	case <-c.done:
	}
}

// runJobs is the single goroutine owning inbound store updates. Publishes are not
// routed through it (see Publish), so a stuck publish cannot freeze the stream.
func (c *Client) runJobs() {
	for {
		select {
		case job, ok := <-c.jobs:
			if !ok {
				return
			}
			job()
		case <-c.done:
			return
		}
	}
}