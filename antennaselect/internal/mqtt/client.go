// Package mqtt wires the reconciler to the station bus: it subscribes to the radio,
// station, ant-switch, and operator topics, feeds each update into the reconciler, and
// emits the resulting intents (ant-switch select, controller band-follow) plus its own
// state. All decision logic lives in package reconcile; this layer is deliberately thin.
package mqtt

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"antennaselect/internal/config"
	"antennaselect/internal/reconcile"
)

// Canonical sibling slot names on the same site/station. The reconciler binds to roles,
// not devices; these are the role addresses defined by the integration model §4, §7.1.
// The band-follow controller slot is NOT listed here — it comes from config
// (band_follow.slot), because which antenna follows the radio is site configuration.
const (
	slotRadio     = "radio"
	slotAntSwitch = "ant-switch"
)

// idleCheckInterval is how often the idle loop re-checks the walk-away timeout. Coarse
// enough to be negligible, fine enough that the antenna grounds within a few seconds of
// the configured [idle].timeout_minutes.
const idleCheckInterval = 5 * time.Second

type Client struct {
	client  paho.Client
	cfg     config.Config
	rec     *reconcile.Reconciler
	site    string
	station string
	slot    string

	mu              sync.Mutex
	in              reconcile.Inputs
	lastDecision    reconcile.Decision
	haveDecision    bool
	lastSelect      string
	lastFollowFreq  int64
	lastPaBand      string
	lastTunerInline *bool

	// lastActivity is the wall-clock time of the last radio activity (a VFO/frequency
	// change or a transmit). It drives the idle timeout (config [idle].timeout_minutes): when
	// time.Since(lastActivity) exceeds the timeout the reconciler grounds the antenna.
	// Only touched on the jobs worker (under c.mu), so no cross-goroutine race.
	lastActivity time.Time

	// RadioOnline (in.RadioOnline) is the AND of the two radio-liveness signals below;
	// both are recomputed in onRadioStatus/onRadioState under c.mu.
	//   radioBridgeOnline — from radio/status (broker LWT): the flexbridge process is up
	//     and publishing, so radio/state is being maintained rather than a frozen retained
	//     snapshot. This is the §10 "never trust retained state for safety" freshness gate.
	//   radioDeviceOnline — from radio/state.device_online: the FLEX radio link itself is
	//     up (handshake done). /status stays online while the radio link is down, so /status
	//     alone cannot tell the two apart — device_online can. flexbridge always publishes
	//     device_online (its statePayload comment says: "so consumers can distinguish a live
	//     radio from a frozen snapshot … /status is the MQTT/LWT bridge liveness, not the
	//     radio link").
	// Trusting radio-derived fields requires BOTH: bridge up (fresh state) AND radio link up.
	radioBridgeOnline bool
	radioDeviceOnline bool

	// jobs serializes reconcile+publish work on a single goroutine that is NOT one of
	// paho's dispatch goroutines. paho delivers message handlers INLINE on its
	// matchAndDispatch goroutine when OrderMatters is true (the default — see the paho
	// Subscribe docs: "callback must not block or call functions within this package that
	// may block (e.g. Publish) other than in a new go routine"). update() publishes
	// synchronously (publishJSON -> token.Wait()); running that inline stalls dispatch
	// after the first message: the handler blocks waiting for the outgoing PUBACK while
	// the read loop blocks pushing the next retained PUBLISH into the now-full message
	// channel the stalled dispatcher is no longer draining. This subscribes to five
	// topics, so a retained burst on connect deadlocked it after the first message — the
	// same live bug hadiscovery had. Routing the work here lets each handler return
	// immediately, unblocking paho, while keeping the reconciler single-threaded (its
	// decision/idempotency logic depends on sequential, ordered updates). The queue +
	// worker live in shared/mqtt (Enqueue/RunJobs) so the same fix is shared with every
	// other stationa consumer.
	//
	// ctx governs this client's lifecycle: a child of the ctx passed to New, so a SIGTERM
	// cancels it and Close cancels it directly to stop the worker before disconnecting.
	ctx    context.Context
	cancel context.CancelFunc
	jobs   chan func()
}

// New connects to the broker, registers the last-will, and subscribes to all inputs. ctx
// governs the connect (a SIGTERM while the broker is unreachable interrupts it — paho's
// Connect().Wait() alone ignores ctx) and the worker lifecycle.
func New(ctx context.Context, cfg config.Config, rec *reconcile.Reconciler) (*Client, error) {
	c := &Client{
		cfg:     cfg,
		rec:     rec,
		site:    cfg.MQTT.Site,
		station: cfg.MQTT.Station,
		slot:    cfg.MQTT.Slot,
		jobs:    make(chan func(), 256),
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.lastActivity = time.Now()
	go sharedmqtt.RunJobs(c.ctx, c.jobs)
	go c.idleLoop()

	opts := paho.NewClientOptions().
		AddBroker(cfg.MQTT.Broker).
		// Client ID derives from the slot address (model §8) so a duplicate
		// connection is diagnosable on the broker.
		SetClientID(orDefault(cfg.MQTT.ClientID, c.site+"-"+c.station+"-"+c.slot)).
		SetAutoReconnect(true).
		// Clean session (not persistent). On every (re)connect the broker drops any prior
		// session and creates fresh subscriptions, which is what re-delivers the retained
		// radio/state, radio/status, and ant-switch/state the reconciler seeds its inputs
		// from. A persistent session (CleanSession=false) resumes on reconnect and does
		// NOT replay retained for existing subscriptions — the reconciler wakes with empty
		// inputs, never resolves, and never follows the radio. That breaks the very
		// self-heal-from-retained behavior the PA/tuner follow bindings below depend on
		// (they re-emit on the retained radio/state replay at reconnect). The reconciler is
		// stateless and re-derives from retained state, so dropping messages published
		// during a brief offline window is acceptable. (Model §8 rule 2 solves the same
		// offline-backlog hazard differently for persistent sessions: /cmd consumers
		// subscribe their command topic at QoS 0 so the broker cannot queue a replay
		// backlog; this reconciler instead takes a fresh session and re-derives its
		// inputs from retained state — either is compliant, but never a QoS-1 /cmd
		// subscription under a persistent session.)
		SetCleanSession(true).
		SetWill(c.selfTopic("status"), "offline", 1, true)
	if cfg.MQTT.User != "" {
		opts.SetUsername(cfg.MQTT.User)
	}
	if cfg.MQTT.Password != "" {
		opts.SetPassword(cfg.MQTT.Password)
	}
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		slog.Warn("[mqtt] connection lost", "err", err)
	})
	opts.SetOnConnectHandler(func(_ paho.Client) {
		slog.Info("[mqtt] connected", "broker", cfg.MQTT.Broker, "slot", c.selfBase())
		c.publishString(c.selfTopic("status"), "online", 1, true)
		c.publishMeta()
		c.subscribeAll()
	})

	c.client = paho.NewClient(opts)
	if err := sharedmqtt.Connect(c.ctx, c.client); err != nil {
		c.cancel()
		return nil, err
	}
	return c, nil
}

func (c *Client) Close() {
	if c == nil {
		return
	}
	// Stop the worker first so no reconcile/publish work races with shutdown.
	c.cancel()
	if c.client.IsConnectionOpen() {
		c.publishString(c.selfTopic("status"), "offline", 1, true)
		c.client.Disconnect(250)
	}
}

// idleLoop periodically re-checks the idle timeout so the antenna grounds even when the
// radio is silent (no radio/state updates to trigger onRadioState). The actual check runs
// on the jobs worker (via Enqueue) so lastActivity is only read under c.mu.
func (c *Client) idleLoop() {
	ticker := time.NewTicker(idleCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sharedmqtt.Enqueue(c.jobs, c.checkIdle)
		case <-c.ctx.Done():
			return
		}
	}
}

// checkIdle marks the station inactive when the idle timeout has elapsed since the last
// activity. It re-checks time.Since(lastActivity) under lock so a late activity that
// arrived after the tick but before this job ran does not get clobbered.
func (c *Client) checkIdle() {
	c.update(func(in *reconcile.Inputs) {
		if time.Since(c.lastActivity) >= time.Duration(c.cfg.Idle.TimeoutMinutes)*time.Minute {
			in.StationActivity = "inactive"
		}
	})
}

// --- subscriptions ----------------------------------------------------------

func (c *Client) subscribeAll() {
	c.subscribe(c.siblingTopic(slotRadio, "state"), c.onRadioState)
	c.subscribe(c.siblingTopic(slotRadio, "status"), c.onRadioStatus)
	c.subscribe(c.siblingTopic(slotAntSwitch, "state"), c.onAntSwitchState)
	c.subscribe(c.selfTopic("cmd"), c.onOperatorCmd)
}

func (c *Client) onRadioState(_ paho.Client, msg paho.Message) {
	var s struct {
		Band         string `json:"band"`
		FreqHz       int64  `json:"freq_hz"`
		TX           string `json:"tx"`
		DeviceOnline bool   `json:"device_online"`
	}
	if err := json.Unmarshal(msg.Payload(), &s); err != nil {
		slog.Error("[mqtt] bad radio/state; dropping message", "err", err)
		return
	}
	sharedmqtt.Enqueue(c.jobs, func() {
		c.update(func(in *reconcile.Inputs) {
			c.radioDeviceOnline = s.DeviceOnline
			// Activity = a VFO/frequency change or a transmit. Either marks the station
			// active and resets the idle clock (walk-away safety, §10).
			if s.FreqHz != in.RadioFreqHz || s.TX == reconcile.TXTransmit {
				c.lastActivity = time.Now()
				in.StationActivity = "active"
			}
			in.RadioBand = s.Band
			in.RadioFreqHz = s.FreqHz
			in.RadioTX = s.TX
			in.RadioOnline = c.radioBridgeOnline && c.radioDeviceOnline
		})
	})
}

func (c *Client) onRadioStatus(_ paho.Client, msg paho.Message) {
	online := strings.EqualFold(strings.TrimSpace(string(msg.Payload())), "online")
	sharedmqtt.Enqueue(c.jobs, func() {
		c.update(func(in *reconcile.Inputs) {
			c.radioBridgeOnline = online
			in.RadioOnline = c.radioBridgeOnline && c.radioDeviceOnline
		})
	})
}

func (c *Client) onAntSwitchState(_ paho.Client, msg paho.Message) {
	var s struct {
		Selected string `json:"selected"`
		Settled  bool   `json:"settled"`
	}
	if err := json.Unmarshal(msg.Payload(), &s); err != nil {
		slog.Error("[mqtt] bad ant-switch/state; dropping message", "err", err)
		return
	}
	sharedmqtt.Enqueue(c.jobs, func() {
		c.update(func(in *reconcile.Inputs) {
			in.SwitchSelected = s.Selected
			in.SwitchSettled = s.Settled
		})
	})
}

func (c *Client) onOperatorCmd(_ paho.Client, msg paho.Message) {
	payload := strings.TrimSpace(string(msg.Payload()))
	if payload == "" {
		// Cleared retained hold — treat as release.
		sharedmqtt.Enqueue(c.jobs, func() { c.update(func(in *reconcile.Inputs) { in.OperatorRequest = "" }) })
		return
	}
	var cmd struct {
		Request string `json:"request"`
	}
	if err := json.Unmarshal(msg.Payload(), &cmd); err != nil {
		slog.Error("[mqtt] bad operator cmd; dropping message", "err", err)
		return
	}
	req := strings.TrimSpace(cmd.Request)
	if req == "" || req == reconcile.RequestAuto {
		// Release ("auto") is not evidence of presence: it withdraws the hold
		// and leaves the idle clock alone.
		sharedmqtt.Enqueue(c.jobs, func() { c.update(func(in *reconcile.Inputs) { in.OperatorRequest = req }) })
		return
	}
	sharedmqtt.Enqueue(c.jobs, func() { c.update(func(in *reconcile.Inputs) { c.applyOperatorHold(in, req) }) })
}

// applyOperatorHold records a hold and marks the station active. A hold is
// evidence that someone is at the console and explicitly wants an antenna: the
// only other activity source is a radio/state change, so without this the
// Tier-1 idle override (reconcile.go) silently defeats every operator command
// exactly when the radio link is down or silent — the rig is off, booting, or
// parked on a quiet frequency — leaving no manual re-arm at all. Walk-away
// safety is preserved: checkIdle grounds the antenna again after
// [idle].timeout_minutes with no further hold or radio activity.
func (c *Client) applyOperatorHold(in *reconcile.Inputs, req string) {
	in.OperatorRequest = req
	c.lastActivity = time.Now()
	in.StationActivity = "active"
}

// --- the reconcile step -----------------------------------------------------

// update applies a mutation to the input snapshot under lock, re-runs the reconciler, and
// dispatches any resulting actions. Publishing happens after the lock is released.
func (c *Client) update(mutate func(*reconcile.Inputs)) {
	c.mu.Lock()
	mutate(&c.in)
	act := c.rec.Next(c.in)

	var (
		pubState  *statePayload
		pubSelect string
		pubFreq   int64
		pubBand   string
		pubInline *bool
	)

	if !c.haveDecision || act.Decision != c.lastDecision {
		c.lastDecision = act.Decision
		c.haveDecision = true
		pubState = &statePayload{
			TS:     time.Now().UTC().Format(time.RFC3339),
			Mode:   act.Decision.Mode,
			Target: act.Decision.Target,
			Source: act.Decision.Source,
		}
	}
	if act.SelectPort != "" {
		// If the switch reports a known position and it already matches the target,
		// nothing to do. If it reports a different position, re-command it (the switch
		// firmware is idempotent, so a duplicate command for the current port only
		// republishes /state). While the switch position is still unknown (empty), use
		// lastSelect to command once and then wait for the first ant-switch/state.
		switchSelected := c.in.SwitchSelected
		if switchSelected == "" {
			if act.SelectPort != c.lastSelect {
				c.lastSelect = act.SelectPort
				pubSelect = act.SelectPort
			}
		} else if act.SelectPort != switchSelected {
			c.lastSelect = act.SelectPort
			pubSelect = act.SelectPort
		}
	}
	if act.FollowFreqHz != 0 && act.FollowFreqHz != c.lastFollowFreq {
		c.lastFollowFreq = act.FollowFreqHz
		pubFreq = act.FollowFreqHz
	}
	// PA band-follow: dedup against the last band we pushed so a retained radio/state
	// replay on reconnect doesn't re-emit an unchanged band. (acombridge's SetBand also
	// short-circuits current==target, so a duplicate is harmless, but we avoid the noise.)
	if act.SetBand != "" && act.SetBand != c.lastPaBand {
		c.lastPaBand = act.SetBand
		pubBand = act.SetBand
	}
	// Tuner in-line follow: dedup against the last inline intent we pushed so a retained
	// radio/state replay on reconnect doesn't re-emit an unchanged value. The tuner /cmd is
	// NOT retained (the ATU self-heals from this reconciler re-resolving on the retained
	// radio/state replay at its own reconnect), exactly like the PA /cmd above.
	if act.SetInline != nil && (c.lastTunerInline == nil || *c.lastTunerInline != *act.SetInline) {
		c.lastTunerInline = act.SetInline
		pubInline = act.SetInline
	}
	deferred := act.DeferredForTX
	c.mu.Unlock()

	if deferred {
		slog.Info("[reconcile] port change deferred: radio is transmitting (cold-switch)", "target", act.Decision.Target)
	}
	if pubState != nil {
		c.publishJSON(c.selfTopic("state"), pubState, 1, true)
		slog.Info("[reconcile] decision", "mode", pubState.Mode, "target", pubState.Target, "source", pubState.Source)
	}
	if pubSelect != "" {
		c.publishJSON(c.siblingTopic(slotAntSwitch, "cmd"), map[string]any{"select": pubSelect}, 1, true)
		slog.Info("[reconcile] emit ant-switch select", "port", pubSelect)
	}
	if pubFreq != 0 {
		c.publishJSON(c.siblingTopic(c.cfg.BandFollow.Slot, "cmd"),
			map[string]any{"action": "frequency", "freq_hz": pubFreq}, 1, true)
		slog.Info("[reconcile] band-follow", "slot", c.cfg.BandFollow.Slot, "freq_hz", pubFreq)
	}
	if pubBand != "" {
		// PA /cmd is NOT retained (acombridge subscribes not-retained; pa-mqtt-api.md).
		// Self-heal comes from this reconciler re-resolving on the retained radio/state
		// replay at its own reconnect — not from a retained pa/cmd.
		c.publishJSON(c.siblingTopic(c.cfg.PAFollow.Slot, "cmd"),
			map[string]any{"action": "set_band", "value": pubBand}, 1, false)
		slog.Info("[reconcile] pa band-follow", "slot", c.cfg.PAFollow.Slot, "band", pubBand)
	}
	if pubInline != nil {
		// Tuner /cmd is NOT retained (the ATU self-heals from this reconciler re-resolving on
		// the retained radio/state replay at reconnect), exactly like the PA /cmd above.
		c.publishJSON(c.siblingTopic(c.cfg.TunerFollow.Slot, "cmd"),
			map[string]any{"action": "set_inline", "value": *pubInline}, 1, false)
		slog.Info("[reconcile] tuner inline-follow", "slot", c.cfg.TunerFollow.Slot, "inline", *pubInline)
	}
}

// --- publish helpers --------------------------------------------------------

type statePayload struct {
	TS     string `json:"ts"`
	Mode   string `json:"mode"`
	Target string `json:"target"`
	Source string `json:"source"`
}

func (c *Client) publishMeta() {
	capabilities := map[string]any{
		"controls": slotAntSwitch,
		"ladder":   []string{reconcile.SourceIdle, reconcile.SourceOperator, reconcile.SourceAuto},
	}
	// `follows` advertises the radio-follow bindings this reconciler drives: a passive
	// antenna resource -> its controller slot (band-follow), plus the PA slot -> itself
	// (band-follow, §7.1) and the tuner slot -> itself (inline-follow, §7.1) when enabled.
	// Keyed by the resource/slot that tracks the radio.
	follows := map[string]string{}
	if c.cfg.BandFollow.Resource != "" {
		follows[c.cfg.BandFollow.Resource] = c.cfg.BandFollow.Slot
	}
	if c.cfg.PAFollow.Enabled {
		follows[c.cfg.PAFollow.Slot] = c.cfg.PAFollow.Slot
	}
	if c.cfg.TunerFollow.Enabled {
		follows[c.cfg.TunerFollow.Slot] = c.cfg.TunerFollow.Slot
	}
	if len(follows) > 0 {
		capabilities["follows"] = follows
	}
	meta := map[string]any{
		"schema":       "1.0",
		"role":         "reconciler",
		"link":         "none",
		"location":     c.cfg.Location,
		"host":         c.cfg.Host,
		"capabilities": capabilities,
		// Consumer-neutral field surface (integration model §3.1, Appendix C). The
		// reconciler is a logic slot: it reacts to state and emits intent (model §1),
		// so all of its /state fields are read-only sensors — no writable fields, no
		// actions. `source` is an enum whose options resolve via options_ref into
		// capabilities.ladder (the priority tiers). `target` and `mode` are strings
		// (the resolved port and the derived auto/manual mode); they have no fixed
		// capability list to reference. hadiscovery renders HA discovery from this.
		"expose": map[string]any{
			"device": map[string]any{
				"name": "Antenna selector",
			},
			"fields": []map[string]any{
				{"key": "source", "name": "Source", "type": "enum", "options_ref": "ladder"},
				{"key": "target", "name": "Target", "type": "string"},
				{"key": "mode", "name": "Mode", "type": "string"},
			},
		},
	}
	c.publishJSON(c.selfTopic("meta"), meta, 1, true)
}

func (c *Client) subscribe(topic string, handler paho.MessageHandler) {
	if token := c.client.Subscribe(topic, 1, handler); token.Wait() && token.Error() != nil {
		slog.Error("[mqtt] subscribe failed", "topic", topic, "err", token.Error())
		return
	}
	slog.Info("[mqtt] subscribed", "topic", topic)
}

func (c *Client) publishJSON(topic string, v any, qos byte, retained bool) {
	b, _ := json.Marshal(v)
	if token := c.client.Publish(topic, qos, retained, b); token.Wait() && token.Error() != nil {
		slog.Error("[mqtt] publish failed", "topic", topic, "err", token.Error())
	}
}

func (c *Client) publishString(topic, payload string, qos byte, retained bool) {
	if token := c.client.Publish(topic, qos, retained, payload); token.Wait() && token.Error() != nil {
		slog.Error("[mqtt] publish failed", "topic", topic, "err", token.Error())
	}
}

// --- topic helpers ----------------------------------------------------------
// The slot address format lives in shared/schema; these wrap it for the suffix style this
// consumer uses (own meta/state/status, sibling slots).

func (c *Client) selfBase() string { return schema.SlotBase(c.site, c.station, c.slot) }
func (c *Client) selfTopic(suffix string) string {
	return schema.SiblingTopic(c.site, c.station, c.slot, suffix)
}
func (c *Client) siblingTopic(slot, suffix string) string {
	return schema.SiblingTopic(c.site, c.station, slot, suffix)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
