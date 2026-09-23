// Package bridge is the four-plane MQTT surface for the muehle/uhf/radio
// slot (plan U5), lifted from the spid-ercm mqttslot template (the newest
// gate set) and adapted to the receive-only radio posture (2026-09 pivot):
//
//	/meta    retained birth certificate (role `radio`, capabilities, the
//	         READ-ONLY expose — no control fields at all)
//	/state   retained snapshot: capture-session state + audio/monitor
//	         demands + serial-CIV telemetry (on change, dedup'd, 60 s
//	         freshness heartbeat, KTD14)
//	/status  retained online|offline LWT, self-published offline on clean exit
//	/cmd     one-shot actions (KTD6): QoS-0 subscription, ts gate, 4 KB size
//	         gate, clear-after-execute-or-reject with the echo guard, clipped
//	         rejections. Exactly five actions survive: audio_on, audio_off,
//	         power_on, monitor_on, monitor_off. There is no LAN CI-V command
//	         path anymore — remote TX control was removed (see
//	         docs/known-issues.md for the history).
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

// stateHeartbeat bounds how stale the retained /state ts may get while the
// snapshot is unchanged (the KTD14 always-fresh-ts rule, the pol-ctrl 60 s
// precedent). Telemetry updates republish out-of-band; this is only the
// unchanged-snapshot refresh.
const stateHeartbeat = 60 * time.Second

// RadioState is the serial CI-V monitor's telemetry snapshot: every value
// observed (serial reads / transceive broadcasts), never extrapolated. The
// /state assembly omits it entirely while the monitor is off.
type RadioState struct {
	Responding bool // the radio answers CI-V (false = live-but-deaf / standby / serial down)
	FreqHz     uint64
	Band       string
	Mode       string
	Satellite  bool
	SMeter     *int
	SWR        *int
	ALC        *int
	TXPower    *int // populated only if the bench proves the CI-V read
}

// Monitor is the serial CI-V telemetry reader surface (implemented by
// internal/civserial; a fake in tests). nil Options.Monitor = no serial
// port configured: the monitor_* and power_on cmds are rejected with the
// observed fact.
type Monitor interface {
	// SetMonitor toggles the telemetry reader. Sticky — no TTL: the serial
	// wire is dedicated to this process, there is nothing to release.
	SetMonitor(on bool) error
	// Wake transient-opens the serial port to send the power-on frame
	// (a standby radio answers no ack — blind send).
	Wake(ctx context.Context) error
	// Snapshot returns the latest telemetry.
	Snapshot() RadioState
	// Updates nudges on every telemetry change (coalesced).
	Updates() <-chan struct{}
	// Close shuts the monitor down.
	Close()
}

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

	// Manager is the radio capture-session manager (U4). The bridge owns no
	// CI-V path of its own — everything goes through SetAudioDemand.
	Manager *radio.Manager

	// Monitor is the serial CI-V telemetry reader. nil = no serial port
	// configured (monitor_* / power_on cmds rejected with the fact).
	Monitor Monitor

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

	// radio is the serial monitor's latest telemetry; mu guards it together
	// with the dedup snapshot, the monitor flag and the rejection error.
	// Held only briefly; never across a publish.
	mu        sync.Mutex
	monitorOn bool
	radio     RadioState
	cmdErr    string

	hasLast bool
	last    snap
	lastPub time.Time
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
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Bridge{
		opts: o,
		mgr:  o.Manager,
		log:  o.Logger.With("component", "bridge"),
		jobs: make(chan func(), 256),

		statusTopic: schema.StatusTopic(o.Site, o.Station, o.Slot),
		metaTopic:   schema.MetaTopic(o.Site, o.Station, o.Slot),
		stateTopic:  schema.StateTopic(o.Site, o.Station, o.Slot),
		cmdTopic:    schema.CmdTopic(o.Site, o.Station, o.Slot),
	}, nil
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
		// paho auto-reconnects; there is no radio-side safety action to
		// take (no TX path exists — receive-only posture).
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

// Run drives the /state heartbeat until the context is done. The radio
// manager's Run is the caller's concern (main starts both).
func (b *Bridge) Run() {
	// State changes republish out-of-band (the heartbeat alone would leave
	// a demand flip or telemetry change visible for up to a full interval).
	go b.followSession()
	if b.opts.Monitor != nil {
		go b.followMonitor()
	}

	b.publishState(false) // immediate first snapshot
	ticker := time.NewTicker(stateHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			b.publishState(false) // unchanged snapshot -> fresh ts only
		}
	}
}

// followSession republishes /state on every session state change (idle /
// connecting / live / error) — the session_state field is the consumer
// contract (R16) and must not wait for the heartbeat.
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

// followMonitor republishes /state on every serial telemetry change.
func (b *Bridge) followMonitor() {
	updates := b.opts.Monitor.Updates()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-updates:
			b.publishState(false)
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
		if b.opts.Monitor != nil {
			b.opts.Monitor.Close()
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
