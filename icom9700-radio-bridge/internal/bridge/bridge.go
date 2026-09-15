// Package bridge is the four-plane MQTT surface of slot muehle/uhf/radio
// (plan U5): the retained /meta birth certificate (R8), the retained /state
// hybrid VFO snapshot with the per-state payload rules and <=1 Hz meters
// (R5/R6/KTD-3/KTD-8), and the one-shot /cmd dispatch for the settled action
// set (R9/R10, KTD-6).
//
// Wiring: the bridge owns the radio.Session (constructed here with the
// outbound hooks) and serializes ALL state mutation + publishing on one jobs
// worker (the stationa paho-handler rule). Session hooks fire on the session
// manager goroutine and CI-V frames on the transport read goroutine — both
// only Enqueue. main.go's telemetry ticker calls Poll, which sends the poll
// reads through Session.Do and enqueues the heartbeat/dedup check onto the
// same worker.
//
// The U6 seam: PTT dispatch passes through ArmGate (default: armed AND live),
// and a successful PTT-on fires Options.OnPTTOn — U6's watchdog re-arms
// there and replaces the gate without touching this package's dispatch.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"icom9700-radio-bridge/internal/civ"
	"icom9700-radio-bridge/internal/radio"
)

// Options wires the bridge. Addressing and identity come from config — no
// site/station/slot constants live here.
type Options struct {
	// Site/Station/Slot form the slot address muehle/uhf/radio.
	Site    string
	Station string
	Slot    string
	// Location is the /meta location label (config).
	Location string

	// PollInterval is the telemetry tick (config radio.poll_interval): poll
	// reads while live, meter cadence <=1 Hz (KTD-8), and the heartbeat's
	// unit (60 ticks, the spid pattern).
	PollInterval time.Duration

	// ArmTimeout bounds the blocking SetArmed connect series on the arm cmd
	// (MaxAttempts x AttemptSpacing + handshake margin at wiring time).
	ArmTimeout time.Duration

	// Radio-side session parameters (config radio_host / [civ] / [session]).
	// Username and Password are never logged (the civ never-log pin).
	RadioHost      string
	ControlPort    int
	Username       string
	Password       string
	IdleTimeout    time.Duration
	MaxAttempts    int
	AttemptSpacing time.Duration
	ErrorDecay     time.Duration
	// TransportOpts mutates the civ transport options per dial (test
	// cadences). Production wiring leaves it nil.
	TransportOpts func(*civ.Opts)

	// ArmGate decides whether a PTT request may pass. Nil uses the v1
	// default (armed AND live, the R10 exact rejection strings). U6 replaces
	// it with the safety core's gate (watchdog, loss-of-plane rules).
	ArmGate ArmGateFunc
	// OnPTTOn fires on the jobs worker after a PTT-on dispatch left for the
	// radio — the TX-watchdog seam (U6 arms its timer here). Nil = no-op.
	OnPTTOn func()

	// Log receives bridge lifecycle lines; it should carry the component and
	// slot attrs (logging convention). Nil falls back to the default logger.
	Log *slog.Logger
}

// ArmGateFunc is the PTT admission seam: called with the session's arm-permit
// and liveness state at dispatch time; a non-nil error rejects the PTT with
// its text in /state.error (the R10 exact strings from the default).
type ArmGateFunc func(armed, live bool) error

// The R10 rejection strings — exact /state.error taxonomy.
const (
	ErrPTTNotArmed = "ptt rejected: not armed"
	ErrPTTNotLive  = "ptt rejected: session not live"
	ErrSatModeTX   = "sat_mode rejected: tx active"
	ErrSatModeArm  = "sat_mode rejected: armed"
)

// defaultArmGate is the v1 admission rule (R10): armed AND live, armed
// checked first so a neither-armed-nor-live session reports the missing
// permit, not the missing session.
func defaultArmGate(armed, live bool) error {
	switch {
	case !armed:
		return fmt.Errorf("%s", ErrPTTNotArmed)
	case !live:
		return fmt.Errorf("%s", ErrPTTNotLive)
	default:
		return nil
	}
}

// Bridge is the slot surface. Build with New; Close on shutdown.
type Bridge struct {
	opts  Options
	log   *slog.Logger
	codec *civ.Codec
	sess  *radio.Session

	jobs chan func()

	// client is the paho client, attached by main after the MQTT connect
	// (OnMQTTConnect). Before attachment, publishes are dropped (nothing on
	// the bus yet — the retained state lands with the connect ritual).
	mu      sync.Mutex // guards the fields below; held only briefly, never across a publish
	client  paho.Client
	radio   radioCache
	cmdErr  string // last /cmd rejection; cleared by the next admitted intent
	last    stateSnapshot
	hasLast bool
	lastPub time.Time // freshness heartbeat clock
	recon   bool      // full two-VFO reconciliation wanted on the next poll tick
	// notLiveSeen arms the recon edge: any non-live state change sets it;
	// the next live change consumes it and schedules the full read.
	notLiveSeen bool
}

// New builds the bridge and its radio session. The session's outbound hooks
// enqueue onto the bridge's single jobs worker, which is started here.
func New(ctx context.Context, o Options) *Bridge {
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	log := o.Log
	if log == nil {
		log = slog.Default().With("component", "icom9700-radio-bridge")
	}
	log = log.With("slot", schema.SlotBase(o.Site, o.Station, o.Slot))

	b := &Bridge{
		opts:  o,
		log:   log,
		codec: civ.NewCodec(),
		jobs:  make(chan func(), jobsCap),
		radio: radioCache{
			selected: civ.VfoMain, // bridge-held default (R6: survives sessions)
		},
	}
	b.sess = radio.NewSession(radioConfig(o), radio.Hooks{
		OnStateChange: func(snap radio.Snapshot) {
			// The snapshot rides the closure: by the time the worker dequeues
			// this, the session may already be in a later state, and the
			// live-transition detection below must see the ACTUAL edge.
			sharedmqtt.Enqueue(b.jobs, func() { b.onSessionEvent(snap) })
		},
		OnSessionDown: func(reason string) {
			// Enqueued before the matching OnStateChange (the session fires
			// this hook first), so the caches are already dropped when the
			// error-state publish runs — stale telemetry cannot be frozen
			// into the retained snapshot (flexbridge Reset lesson).
			sharedmqtt.Enqueue(b.jobs, func() {
				b.mu.Lock()
				b.radio.clear()
				b.mu.Unlock()
				b.publishState(false)
			})
		},
		OnArmDrop: func(string) {
			sharedmqtt.Enqueue(b.jobs, func() { b.onSessionEvent(radio.Snapshot{}) })
		},
		OnCIVFrame: func(chunk []byte) {
			// Transport read goroutine: copy and hand off — never parse or
			// publish inline (the stationa handler rule). Bounded drop on a
			// saturated worker (R18); the next poll tick re-reads the world.
			c := append([]byte(nil), chunk...)
			sharedmqtt.Enqueue(b.jobs, func() { b.onCIVFrame(c) })
		},
	})
	go sharedmqtt.RunJobs(ctx, b.jobs)
	return b
}

// jobsCap bounds the bridge's jobs queue (R18: bounded buffers; the spid
// 256-deep slot shape).
const jobsCap = 256

// radioConfig maps the wiring figures onto the session config — everything
// from the bridge Options, so main's wiring stays a straight field copy from
// config.Config.
func radioConfig(o Options) radio.Config {
	return radio.Config{
		Host:           o.RadioHost,
		ControlPort:    o.ControlPort,
		Username:       o.Username,
		Password:       o.Password,
		IdleTimeout:    o.IdleTimeout,
		MaxAttempts:    o.MaxAttempts,
		AttemptSpacing: o.AttemptSpacing,
		ErrorDecay:     o.ErrorDecay,
		Opts:           o.TransportOpts,
		Log:            o.Log,
	}
}

// Session returns the underlying radio session (main wires Close, and U6
// will wire RequestPTTOff on MQTT-plane loss).
func (b *Bridge) Session() *radio.Session { return b.sess }

// Jobs returns the bridge's jobs channel: the paho /cmd handler Enqueues
// onto it (never blocks, never publishes inline).
func (b *Bridge) Jobs() chan func() { return b.jobs }

// Manager adapts the bridge onto the U1 radio.Manager seam main.go's loop
// drives (Connect/Poll/Execute/Disconnect).
type managerAdapter struct{ b *Bridge }

// Manager returns the radio.Manager view for main.go.
func (b *Bridge) Manager() radio.Manager { return managerAdapter{b} }

// Connect is a no-op by design (KTD-2/R2): the session is on-demand — login
// attempts happen only cmd-driven, arm-driven or safety-driven, so the
// process-level loop must not dial. Telemetry ticks start with Poll and go
// quiet until a session exists.
func (managerAdapter) Connect(context.Context) error { return nil }

// Disconnect tears a live session down politely (no arm-permit change).
func (a managerAdapter) Disconnect() { a.b.sess.Disconnect() }

// Execute applies one /cmd payload (gates, dispatch, clear-after).
func (a managerAdapter) Execute(ctx context.Context, payload []byte) error {
	return a.b.Execute(ctx, payload)
}

// Poll is one telemetry tick: poll reads while a session is live (R6
// reconciliation + KTD-8 meters), then the dedup/heartbeat check — which
// must run while IDLE too, or the retained ts would go stale exactly when
// the bridge has nothing to say (R6 freshness rule).
func (a managerAdapter) Poll(ctx context.Context) error {
	a.b.Poll(ctx)
	return nil
}

// Poll performs the telemetry tick. Safe from any goroutine; the state
// mutation lands on the jobs worker.
func (b *Bridge) Poll(ctx context.Context) {
	if b.sess.Live() {
		// Shrink the Live()->Do race to nothing observable: a telemetry tick
		// must never become a connect demand (R2 — no login attempts except
		// cmd/arm/safety-driven). Do's closure re-checks the transport and
		// bails without sending if the session died in between.
		_ = b.sess.Do(ctx, b.sendPollReads)
	}
	sharedmqtt.Enqueue(b.jobs, func() { b.publishState(false) })
}

// OnMQTTConnect is the connect ritual (called by main's paho OnConnect on
// every (re)connect): the retained birth certificate and a forced /state
// restore, so a broker wipe that dropped retained messages cannot leave the
// slot stateless (the spid onConnect shape). Runs on a paho goroutine — the
// publishes enqueue onto the jobs worker like every other path.
func (b *Bridge) OnMQTTConnect(cl paho.Client) {
	b.mu.Lock()
	b.client = cl
	b.mu.Unlock()
	sharedmqtt.Enqueue(b.jobs, func() {
		b.mu.Lock()
		cl := b.client
		b.mu.Unlock()
		if cl == nil {
			return
		}
		publishJSON(b.log, cl, topicMeta(b.opts),
			metaPayload(b.opts.Site, b.opts.Station, b.opts.Slot, b.opts.Location))
		b.resetDedup()
		b.publishState(true)
	})
}

// onSessionEvent republishes the session-driven part of /state (state
// changes, arm drops) and schedules the full two-VFO reconciliation on every
// non-live → live edge (the cache came from the previous session or is empty
// — either way read both bands before trusting them). The edge is detected
// with an armed flag rather than the published snapshot's live flag: events
// may interleave with faster session transitions, and a late connecting
// event must not mark the session as already-live for the live event that
// follows. snap is the state change that scheduled this run; the publish
// itself re-reads the current snapshot. Runs on the jobs worker.
func (b *Bridge) onSessionEvent(snap radio.Snapshot) {
	b.mu.Lock()
	if snap.State != radio.StateLive {
		b.notLiveSeen = true
	} else if b.notLiveSeen {
		b.notLiveSeen = false
		b.recon = true
	}
	b.mu.Unlock()
	b.publishState(false)
}

// Close shuts the session stack down (the bridge's jobs worker dies with the
// context passed to New).
func (b *Bridge) Close() { b.sess.Close() }
