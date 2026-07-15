// Package mqtt wires the powerseq sequencer to the station bus. It subscribes
// the /status of every slot the configured sequence references (plus the /state
// of every wait_state target) and the sequencer's own /cmd, feeds each
// observation to the sequencer, dispatches the operator one-button /cmd
// (start|stop, NOT retained), and publishes the sequencer's own
// /meta /state /status. The sequencer state machine lives in package seq; this
// layer is deliberately thin.
//
// Subscriptions are DATA-DRIVEN: the set is derived from the configured
// sequence (seq.Subscriptions()), so adding a wait or cmd target in TOML extends
// the subscriptions with no Go change.
//
// The paho message handlers run on paho's dispatch goroutine and only do a
// quick mutex update + (for /cmd) a non-blocking send to the sequencer's cmd
// channel — they never publish, so they never block (the stationa memory: a
// paho handler must not call a blocking Publish). The sequencer's runner
// goroutine does all the /cmd emission + /state publishing.
package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"

	"powerseq/internal/config"
	"powerseq/internal/seq"
)

// publishTimeout bounds a paho Publish's Wait so a broker outage surfaces as an
// error instead of stalling the runner on a never-completing QoS-1 token.
const publishTimeout = 10 * time.Second

// Publisher adapts a paho client to the seq.Publisher interface (QoS 1). Its
// Wait is bounded so a disconnect mid-sequence cannot wedge the runner. Client
// is guarded by a mutex: OnConnect writes it on paho's callback goroutine while
// Publish reads it on the sequencer's runner goroutine, so the two need a
// happens-before relation (a bare field would be a data race).
type Publisher struct {
	mu     sync.RWMutex
	Client pahomqtt.Client
}

// setClient swaps the paho client under the lock (called from OnConnect).
func (p *Publisher) setClient(c pahomqtt.Client) {
	p.mu.Lock()
	p.Client = c
	p.mu.Unlock()
}

func (p *Publisher) Publish(topic string, retained bool, payload []byte) error {
	p.mu.RLock()
	cl := p.Client
	p.mu.RUnlock()
	if cl == nil {
		return fmt.Errorf("publish %s: mqtt client not connected", topic)
	}
	tok := cl.Publish(topic, 1, retained, payload)
	if !tok.WaitTimeout(publishTimeout) {
		return fmt.Errorf("publish %s: timed out after %s", topic, publishTimeout)
	}
	return tok.Error()
}

// Client owns the paho connection and routes observations to the sequencer.
type Client struct {
	client pahomqtt.Client
	seq    *seq.Sequencer
	cfg    config.Config
	log    *slog.Logger

	mu        sync.Mutex
	connected bool
}

// New constructs the sequencer, connects to the broker (ctx-aware), and returns
// the wired Client. The caller starts the sequencer's runner with Run(ctx).
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*Client, *seq.Sequencer, error) {
	publisher := &Publisher{}
	s, err := seq.New(seq.Config{
		Site:            cfg.MQTT.Site,
		Station:         cfg.MQTT.Station,
		Slot:            cfg.MQTT.Slot,
		Location:        cfg.Location,
		Host:            cfg.Host,
		DiscoveryPrefix: cfg.MQTT.DiscoveryPrefix,
		Startup:         toSeqSteps(cfg.Startup),
		Shutdown:        toSeqSteps(cfg.Shutdown),
		NetworkDelay:    time.Duration(cfg.Timing.NetworkDelayS) * time.Second,
		StepTimeout:     time.Duration(cfg.Timing.StepTimeoutS) * time.Second,
		ShutdownStagger: time.Duration(cfg.Timing.ShutdownStaggerS) * time.Second,
		PollInterval:    time.Duration(cfg.Timing.PollIntervalMs) * time.Millisecond,
		DefaultHold:     time.Duration(cfg.Timing.DefaultHoldMs) * time.Millisecond,
	}, publisher, &slogAdapter{log})
	if err != nil {
		return nil, nil, fmt.Errorf("sequencer: %w", err)
	}

	c := &Client{seq: s, cfg: cfg, log: log}

	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTT.Broker)
	clientID := cfg.MQTT.ClientID
	if clientID == "" {
		clientID = cfg.MQTT.Site + "-" + cfg.MQTT.Station + "-" + cfg.MQTT.Slot
	}
	opts.SetClientID(clientID)
	if cfg.MQTT.User != "" {
		opts.SetUsername(cfg.MQTT.User)
	}
	if cfg.MQTT.Password != "" {
		opts.SetPassword(cfg.MQTT.Password)
	}
	opts.SetAutoReconnect(true)
	opts.SetCleanSession(false)
	opts.SetWill(s.SelfBase()+"/status", "offline", 1, true)

	opts.OnConnect = func(cl pahomqtt.Client) {
		log.Info("MQTT (re)connected", "broker", cfg.MQTT.Broker, "slot", s.SelfBase())
		cl.Publish(s.SelfBase()+"/status", 1, true, []byte("online"))
		publisher.setClient(cl)
		s.SetBrokerOnline(true)
		c.setConnected(true)
		// Publish the birth certificate on every (re)connect (fire-and-forget,
		// no Wait — OnConnect must not block).
		cl.Publish(s.SelfBase()+"/meta", 1, true, s.MetaPayload())
		// Ask the runner to re-publish the retained /state. We must NOT call a
		// blocking Publish from this paho callback (the stationa memory: a paho
		// handler that Publishes deadlocks); the runner owns all /state
		// publishing. After a broker wipe that drops retained messages this
		// restores an idle sequencer's /state instead of leaving it absent.
		s.RequestRepublish()
		c.subscribeAll(cl)
	}
	opts.OnConnectionLost = func(_ pahomqtt.Client, err error) {
		log.Warn("MQTT connection lost", "err", err)
		s.SetBrokerOnline(false)
		c.setConnected(false)
	}

	client := pahomqtt.NewClient(opts)
	if err := sharedmqtt.Connect(ctx, client); err != nil {
		return nil, nil, fmt.Errorf("mqtt connect: %w", err)
	}
	// OnConnect may fire just before or after the connect token resolves; set
	// the publisher + broker-online explicitly so the runner (started next) can
	// never observe a nil client / offline state at boot.
	publisher.setClient(client)
	s.SetBrokerOnline(true)
	c.client = client
	return c, s, nil
}

// Close disconnects cleanly (publishes offline first if connected).
func (c *Client) Close() {
	if c == nil || c.client == nil {
		return
	}
	if c.client.IsConnectionOpen() {
		c.client.Publish(c.seq.SelfBase()+"/status", 1, true, []byte("offline"))
		c.client.Disconnect(250)
	}
}

func (c *Client) setConnected(v bool) {
	c.mu.Lock()
	c.connected = v
	c.mu.Unlock()
}

// ---------------------------------------------------------------------------
// subscriptions
// ---------------------------------------------------------------------------

func (c *Client) subscribeAll(cl pahomqtt.Client) {
	statusSlots, stateSlots := c.seq.Subscriptions()
	// Liveness + control observability: /status of every slot the sequence
	// references (model §7.1 — subscribe is for observability, not only waits).
	for _, slot := range statusSlots {
		topic := slot + "/status"
		c.subscribe(cl, topic, func(_ pahomqtt.Client, m pahomqtt.Message) {
			online := strings.EqualFold(strings.TrimSpace(string(m.Payload())), "online")
			c.seq.SetStatus(slot, online)
		})
	}
	// wait_state confirmation: <slot>/state (whole JSON snapshot).
	for _, slot := range stateSlots {
		topic := slot + "/state"
		c.subscribe(cl, topic, func(_ pahomqtt.Client, m pahomqtt.Message) {
			c.seq.SetState(slot, m.Payload())
		})
	}
	// Operator one-button command. NOT retained (one-shot). Subscribed at QoS 0
	// ON PURPOSE: with a persistent session (CleanSession=false) a QoS-1
	// subscription would let the broker QUEUE a /cmd published while powerseq is
	// offline and replay it on reconnect — re-energizing the station, the exact
	// case the "own /cmd NOT retained" rule guards against. QoS 0 means the
	// broker never queues it; a /cmd issued while we are down is simply lost
	// (correct for a one-shot operator command). /status + /state keep QoS 1 +
	// the persistent session so their last retained value replays on reconnect.
	c.subscribeQoS(cl, c.seq.CmdTopic(), 0, func(_ pahomqtt.Client, m pahomqtt.Message) {
		var cmd struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(m.Payload(), &cmd); err != nil {
			c.log.Warn("bad /cmd", "err", err)
			return
		}
		switch cmd.Action {
		case "start":
			c.seq.Start()
		case "stop":
			c.seq.Stop()
		default:
			c.log.Warn("unknown /cmd action", "action", cmd.Action)
		}
	})
}

func (c *Client) subscribe(cl pahomqtt.Client, topic string, h pahomqtt.MessageHandler) {
	c.subscribeQoS(cl, topic, 1, h)
}

func (c *Client) subscribeQoS(cl pahomqtt.Client, topic string, qos byte, h pahomqtt.MessageHandler) {
	if tok := cl.Subscribe(topic, qos, h); tok.Wait() && tok.Error() != nil {
		c.log.Warn("subscribe failed", "topic", topic, "err", tok.Error())
		return
	}
	c.log.Debug("subscribed", "topic", topic, "qos", qos)
}

// toSeqSteps converts the config (TOML) step list to the seq (site-relative)
// step list. Field names match 1:1; resolution to absolute addresses happens
// in seq.New.
func toSeqSteps(cs []config.Step) []seq.Step {
	out := make([]seq.Step, len(cs))
	for i, st := range cs {
		out[i] = seq.Step{
			Name:      st.Name,
			Kind:      st.Kind,
			Slot:      st.Slot,
			Action:    st.Action,
			Value:     st.Value,
			Retain:    st.Retain,
			Slots:     st.Slots,
			State:     st.State,
			Field:     st.Field,
			HoldMs:    st.HoldMs,
			TimeoutS:  st.TimeoutS,
			DurationS: st.DurationS,
			Duration:  st.Duration,
		}
	}
	return out
}

// slogAdapter adapts *slog.Logger to the seq.Logger interface.
type slogAdapter struct{ l *slog.Logger }

func (s *slogAdapter) Infof(f string, a ...any)  { s.l.Info(fmt.Sprintf(f, a...)) }
func (s *slogAdapter) Warnf(f string, a ...any)  { s.l.Warn(fmt.Sprintf(f, a...)) }
func (s *slogAdapter) Debugf(f string, a ...any) { s.l.Debug(fmt.Sprintf(f, a...)) }

// NewLogger builds the slog logger from a level string (shared with main).
func NewLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}
