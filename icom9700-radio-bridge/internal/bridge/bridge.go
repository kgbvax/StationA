// Package bridge is the four-plane MQTT surface for the muehle/uhf/radio
// slot (plan U5): the station integration model's planes over ONE paho
// connection, lifted from the spid-ercm mqttslot template (the newest gate
// set) and adapted to the on-demand radio session:
//
//	/meta    retained birth certificate (role `radio`, capabilities, the
//	         READ-ONLY expose — no PTT/arm widgets in HA, v1)
//	/state   retained hybrid snapshot (R5): top-level active-TX fields +
//	         main/sub detail + session/armed/meters, per-state payload rules
//	         (R6), poll-tick cadence with dedup + freshness heartbeat (KTD14
//	         / pol-ctrl precedent), meters dedup'd to <= 1 Hz (KTD-8)
//	/status  retained online|offline LWT, self-published offline on clean exit
//	/cmd     one-shot actions (KTD6): QoS-0 subscription, ts gate, 4 KB size
//	         gate, clear-after-execute-or-reject with the echo guard, clipped
//	         rejections
//
// PTT is safety-gated HERE (R10: armed ∧ live at send time); the max-TX
// watchdog and the remaining loss-of-plane rules land with U6's safety core.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"icom9700-radio-bridge/internal/radio"
)

// Options wires the slot. Every address and identity field comes from the
// bridge config — no site, station or location constant lives here (§8.1
// item 6).
type Options struct {
	Broker   string
	ClientID string
	User     string
	Password string

	Site     string // "muehle"
	Station  string // "uhf"
	Slot     string // "radio"
	Location string
	Host     string // the radio host, surfaced in /meta

	// DeviceModel is the configured /meta device model until the radio's
	// own identity read lands (which folds in as device.firmware's
	// companion — the identity never clobbers a previously published /meta,
	// it only enriches it).
	DeviceModel string

	// Manager is the radio session manager (U4). The bridge owns no CI-V
	// path of its own — everything goes through Demand/SetHold.
	Manager *radio.Manager

	// PollInterval is the /state snapshot tick (config radio.poll_interval);
	// meters ride the same tick, dedup'd to <= 1 Hz into the retained
	// snapshot (KTD-8).
	PollInterval time.Duration

	// TXWatchdog is the max-TX bound (config session.tx_watchdog, KTD5):
	// a keyed PTT is unkeyed by force after this long. Zero disables.
	TXWatchdog time.Duration

	Logger *slog.Logger
}

// Bridge is the slot's MQTT surface.
type Bridge struct {
	opts Options
	mgr  *radio.Manager
	log  *slog.Logger

	cli mqClient

	ctx    context.Context
	cancel context.CancelFunc
	jobs   chan func()

	statusTopic string
	metaTopic   string
	stateTopic  string
	cmdTopic    string

	stopOnce sync.Once

	// radio is the cached CI-V truth (poll + transceive folding); the mu
	// guard covers it together with the dedup snapshot, the armed permit
	// and the rejection error. Held only briefly; never across a publish.
	mu           sync.Mutex
	radio        radioState
	sessionState string
	hasLast      bool
	last         snap
	lastPub      time.Time
	armed        bool
	cmdErr       string

	// Safety core (U6): the max-TX timer and the owed reconnect PTT-off
	// (see safety.go).
	txWatch    *time.Timer
	pttOffOwed bool
}

// mqClient is the publish/subscribe surface the bridge needs — the paho
// adapter in Start; tests substitute a recording fake.
type mqClient interface {
	publish(topic string, qos byte, retained bool, payload []byte)
	subscribe(topic string, qos byte, h func(payload []byte))
	isConnected() bool
	disconnect(qosMs uint)
}

// New builds the bridge without a broker connection (tests wire a fake
// mqClient via wireClient; Start wires paho).
func New(o Options) (*Bridge, error) {
	if o.Site == "" || o.Station == "" || o.Slot == "" {
		return nil, fmt.Errorf("bridge: site, station and slot must be set")
	}
	if o.Manager == nil {
		return nil, fmt.Errorf("bridge: no radio manager wired")
	}
	if o.PollInterval <= 0 {
		return nil, fmt.Errorf("bridge: poll interval must be > 0")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	b := &Bridge{
		opts: o,
		mgr:  o.Manager,
		log:  o.Logger.With("component", "bridge"),
		jobs: make(chan func(), 256),

		statusTopic: schema.StatusTopic(o.Site, o.Station, o.Slot),
		metaTopic:   schema.MetaTopic(o.Site, o.Station, o.Slot),
		stateTopic:  schema.StateTopic(o.Site, o.Station, o.Slot),
		cmdTopic:    schema.CmdTopic(o.Site, o.Station, o.Slot),
	}
	b.registerSafety()
	return b, nil
}

// wireClient installs the client (fake in tests, paho adapter in Start)
// and performs the connect ritual: /status online, the retained birth
// certificate, the QoS-0 /cmd subscription, and a forced /state restore.
func (b *Bridge) wireClient(cli mqClient) {
	b.cli = cli
	if b.ctx == nil {
		// Tests: no Start-provided context; the jobs worker lives until
		// Close.
		b.ctx, b.cancel = context.WithCancel(context.Background())
	}
	go sharedmqtt.RunJobs(b.ctx, b.jobs)
	b.subscribeCmd()
	b.publishStatus("online")
	b.publishMeta()
	b.resetDedup()
	b.publishState(true)
}

func (b *Bridge) subscribeCmd() {
	b.cli.subscribe(b.cmdTopic, 0, b.onCmd)
}

func (b *Bridge) publishStatus(status string) {
	b.cli.publish(b.statusTopic, 1, true, []byte(status))
}

func (b *Bridge) publishMeta() {
	b.cli.publish(b.metaTopic, 1, true, mustJSON(b.metaPayload()))
}

// pahoAdapter adapts the paho client to mqClient.
type pahoAdapter struct {
	cl  paho.Client
	log *slog.Logger
}

func (p *pahoAdapter) publish(topic string, qos byte, retained bool, payload []byte) {
	token := p.cl.Publish(topic, qos, retained, payload)
	if token.Wait() && token.Error() != nil {
		p.log.Error("publish failed", "topic", topic, "err", token.Error())
	}
}

func (p *pahoAdapter) subscribe(topic string, qos byte, h func(payload []byte)) {
	if token := p.cl.Subscribe(topic, qos, func(_ paho.Client, m paho.Message) {
		// paho reuses the message buffer after the handler returns; copy it.
		h(append([]byte(nil), m.Payload()...))
	}); token.Wait() && token.Error() != nil {
		p.log.Error("subscribe failed", "topic", topic, "err", token.Error())
	}
}

func (p *pahoAdapter) isConnected() bool     { return p.cl.IsConnectionOpen() }
func (p *pahoAdapter) disconnect(qosMs uint) { p.cl.Disconnect(qosMs) }

// Start builds the paho client and connects. The initial connect is fatal
// by design (§8.1 item 10): an unreachable broker returns an error so main
// exits non-zero and systemd crash-loops the unit.
func (b *Bridge) Start(ctx context.Context) error {
	b.ctx, b.cancel = context.WithCancel(ctx)
	go sharedmqtt.RunJobs(b.ctx, b.jobs)

	clientID := b.opts.ClientID
	if clientID == "" {
		clientID = b.opts.Site + "-" + b.opts.Station + "-" + b.opts.Slot
	}
	log := b.log
	opts := paho.NewClientOptions().
		AddBroker(b.opts.Broker).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetCleanSession(false).
		SetWill(b.statusTopic, "offline", 1, true)
	if b.opts.User != "" {
		opts.SetUsername(b.opts.User)
	}
	if b.opts.Password != "" {
		opts.SetPassword(b.opts.Password)
	}
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		log.Warn("mqtt connection lost", "err", err)
	})
	opts.SetOnConnectHandler(func(cl paho.Client) {
		log.Info("mqtt connected", "broker", b.opts.Broker)
		// The connect ritual publishes on a paho goroutine (ultrabridge
		// precedent — blocking publishes inline); the /cmd handler itself
		// never blocks (it only enqueues).
		pa := &pahoAdapter{cl: cl, log: log}
		pa.publish(b.statusTopic, 1, true, []byte("online"))
		pa.publish(b.metaTopic, 1, true, mustJSON(b.metaPayload()))
		pa.subscribe(b.cmdTopic, 0, b.onCmd)
		b.resetDedup()
		sharedmqtt.Enqueue(b.jobs, func() { b.publishState(true) })
	})

	real := paho.NewClient(opts)
	b.cli = &pahoAdapter{cl: real, log: log}
	b.log.Info("mqtt connecting", "broker", b.opts.Broker, "client_id", clientID)
	if err := sharedmqtt.Connect(b.ctx, real); err != nil {
		b.cancel()
		return err
	}
	return nil
}

// Run drives the /state poll tick until the context is done. The radio
// manager's Run is the caller's concern (main starts both).
func (b *Bridge) Run() {
	// Session transitions republish out-of-band (the poll tick alone would
	// leave a session drop visible for up to a full interval).
	go b.followSession()
	go b.followTransceives()

	b.publishState(false) // immediate first snapshot
	ticker := time.NewTicker(b.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			b.poll()
			b.publishState(false)
		}
	}
}

// followSession republishes /state on every session state change (idle /
// connecting / live / error) — the session_state field is the consumer
// contract (R16) and must not wait for the poll tick.
func (b *Bridge) followSession() {
	notify := b.mgr.Notify()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-notify:
			b.publishState(false)
		}
	}
}

// followTransceives folds inbound transceive events (freq/mode/tx) into the
// cached radio state between polls.
func (b *Bridge) followTransceives() {
	trs := b.mgr.Transceives()
	for {
		select {
		case <-b.ctx.Done():
			return
		case tr := <-trs:
			b.foldTransceive(tr)
		}
	}
}

// Close is the clean shutdown: the LWT does not fire on a clean exit, so
// the slot self-publishes its retained offline /status before
// disconnecting.
func (b *Bridge) Close() {
	if b == nil {
		return
	}
	b.stopOnce.Do(func() {
		if b.cancel != nil {
			b.cancel()
		}
		if b.cli != nil && b.cli.isConnected() {
			b.cli.publish(b.statusTopic, 1, true, []byte("offline"))
			b.cli.disconnect(250)
		}
	})
}

// mustJSON marshals or returns null on failure (only hand-built maps flow
// through here; a marshal failure is a programming error).
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}
