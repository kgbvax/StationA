// Package mqtt wires the reconciler to the station bus: it subscribes to the radio,
// station, ant-switch, antenna-mode, and operator topics, feeds each update into the reconciler, and
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

	"codeberg.org/kgbvax/stationa/shared/antmode"
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

	// antLock is the last known hf/ant-mode lock label ("" = unlocked), kept
	// for the lock-change log line. A malformed ant-mode/state never reaches
	// it (the handler drops it, so the last known lock stays in force).
	antLock string
	// reextendPending is set on unlock and cleared once the band-follow
	// controller (<band_follow.slot>/state) reports the elements out
	// (device_online && !retracted). While it is set, a device_online
	// false→true edge with the elements still retracted resets the follow
	// dedup: ultrabridge drops a frequency it cannot deliver (RCU-06 off, e.g.
	// antenna power still off after a powerseq standby wake) and does not
	// re-apply it when the controller returns, so the one re-extend after a
	// lock would otherwise be lost until the VFO moves. followSeen /
	// followDevOnline are the last reported device_online, for the edge.
	reextendPending bool
	followSeen      bool
	followDevOnline bool

	// lastActivity is the wall-clock time of the last radio activity (a VFO/frequency
	// change or a transmit). It drives the idle timeout (config [idle].timeout_minutes): when
	// time.Since(lastActivity) exceeds the timeout the reconciler grounds the antenna.
	// Only touched on the jobs worker (under c.mu), so no cross-goroutine race.
	lastActivity time.Time
	// activitySeen is true once real activity was observed in this process (a VFO
	// change / TX after the first radio/state, or an operator hold). Until then /state
	// carries no last_activity: lastActivity is only the process start time, and
	// publishing it would read as fresh activity to powerseq (a restart would wake a
	// station in standby). seenRadio marks the first radio/state as the baseline.
	activitySeen bool
	seenRadio    bool
	// liveFreqHz is the freq_hz of the last radio/state from a live radio link
	// (device_online=true, freq_hz>0); 0 = none yet. A VFO change is measured
	// against it, never against a link-down sample, so a radio power-off or a
	// link drop and its reconnect (freq_hz → 0 → back) is not activity. A
	// freq_hz 0 sample clears it: flexbridge publishes 0 while its slice/pan
	// tracking resets (a SmartSDR client connecting, link up), and the slice it
	// then reports may sit elsewhere — a new baseline, not a VFO turn (that
	// woke a powerseq standby 2026-10-19 16:38 with nobody at the shack PC).
	liveFreqHz int64
	// pubActivity / pubHasLast are what the last published /state carried, so /state
	// is re-published when the activity flips (not only when the decision changes —
	// in manual mode the decision never changes) and when last_activity first appears.
	pubActivity string
	pubHasLast  bool

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
	// The antenna-mode lock first: its retained state is then delivered — and,
	// over the single FIFO worker, applied — before the retained radio/state
	// can resolve a select or a band-follow the lock would forbid. (subscribe
	// waits for each SUBACK, and mosquitto sends a subscription's retained
	// messages right after its SUBACK, so the retained ant-mode/state is queued
	// ahead of the later subscriptions' replays.) No startup hold beyond that
	// ordering: should a broker ever reorder, the worst case is one select and
	// one band-follow before the lock lands, and ultrabridge refuses the
	// frequency itself while locked.
	if c.cfg.MQTT.AntModeSlot != "" {
		c.subscribe(c.siblingTopic(c.cfg.MQTT.AntModeSlot, "state"), c.onAntModeState)
	}
	c.subscribe(c.siblingTopic(slotRadio, "state"), c.onRadioState)
	c.subscribe(c.siblingTopic(slotRadio, "status"), c.onRadioStatus)
	c.subscribe(c.siblingTopic(slotAntSwitch, "state"), c.onAntSwitchState)
	if c.cfg.MQTT.AntModeSlot != "" && c.cfg.BandFollow.Slot != "" && c.cfg.BandFollow.Resource != "" {
		c.subscribe(c.siblingTopic(c.cfg.BandFollow.Slot, "state"), c.onFollowState)
	}
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
			// active and resets the idle clock (walk-away safety, §10). The first
			// radio/state after start (the retained replay) is the baseline, not
			// activity: the idle clock already starts at process start. Only a live
			// radio link counts: a link drop publishes freq_hz 0 and the reconnect the
			// real frequency again — counted, that read as activity and woke a
			// station in standby (powerseq) after any radio/client disconnect.
			live := s.DeviceOnline && s.FreqHz > 0
			vfo := live && c.liveFreqHz != 0 && s.FreqHz != c.liveFreqHz
			tx := c.seenRadio && s.DeviceOnline && s.TX == reconcile.TXTransmit
			if vfo || tx {
				c.markActive(in)
			}
			if live || s.FreqHz == 0 {
				c.liveFreqHz = s.FreqHz
			}
			c.seenRadio = true
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
	if !validRequest(req) {
		// Reject at the door: a malformed request must never become a hold on a
		// port no switch has. (A UI once sent "manual" as a mode toggle; the hold
		// it created silently overrode auto selection forever.)
		slog.Warn("[mqtt] unknown operator request; dropping cmd", "request", req)
		return
	}
	if req == "" || req == reconcile.RequestAuto {
		// Release ("auto") is not evidence of presence: it withdraws the hold
		// and leaves the idle clock alone.
		sharedmqtt.Enqueue(c.jobs, func() { c.update(func(in *reconcile.Inputs) { in.OperatorRequest = req }) })
		return
	}
	sharedmqtt.Enqueue(c.jobs, func() { c.update(func(in *reconcile.Inputs) { c.applyOperatorHold(in, req) }) })
}

// onAntModeState reads the hf/ant-mode lock through shared/antmode (reading
// rules: empty/cleared = normal; a valid locked snapshot is enforced even while
// ant-mode/status is offline — /status is deliberately not consulted; malformed
// = keep the last known lock; ts is not checked). Parsing happens here on the
// paho goroutine (pure, no blocking); the reconcile + publish runs on the worker.
func (c *Client) onAntModeState(_ paho.Client, msg paho.Message) {
	st, err := antmode.Parse(msg.Payload())
	if err != nil {
		slog.Error("[mqtt] bad ant-mode/state; keeping last known lock", "err", err)
		return
	}
	sharedmqtt.Enqueue(c.jobs, func() { c.update(func(in *reconcile.Inputs) { c.applyAntMode(in, st) }) })
}

// applyAntMode records the antenna-mode lock (caller holds c.mu via update).
// It is deliberately not activity: entering or leaving storm/maintenance never
// calls markActive, so powerseq's idle standby and last_activity are untouched.
//
// On unlock it resets the follow dedup. While locked no band-follow is emitted,
// so lastFollowFreq still holds the pre-lock frequency; without the reset the
// unchanged radio frequency would be deduplicated and never re-sent, and
// ultrabridge (which skips its deadband while retracted) would never re-extend
// the elements. lastSelect and lastTunerInline are reset for the same reason
// (the switch was grounded and the tuner intent stood down meanwhile).
// lastPaBand is kept: PA band-follow continues under the lock, so its dedup is
// current.
func (c *Client) applyAntMode(in *reconcile.Inputs, st antmode.State) {
	wasLocked := in.Locked
	in.Locked = st.Locked()
	in.LockGround = st.L().Ground
	if label := st.Label(); label != c.antLock {
		slog.Info("[reconcile] antenna-mode lock changed", "lock", label, "was", c.antLock, "ground", in.LockGround)
		c.antLock = label
	}
	if wasLocked && !in.Locked {
		c.lastFollowFreq = 0
		c.lastSelect = ""
		c.lastTunerInline = nil
		c.reextendPending = true
	}
}

// onFollowState reads the band-follow controller's /state (ant-ctrl at Mühle)
// for the unlock re-extend only: device_online and the retracted latch. Parsing
// happens here on the paho goroutine; the bookkeeping runs on the worker.
func (c *Client) onFollowState(_ paho.Client, msg paho.Message) {
	var s struct {
		DeviceOnline bool `json:"device_online"`
		Retracted    bool `json:"retracted"`
	}
	if len(msg.Payload()) == 0 {
		return // cleared retained topic: no information
	}
	if err := json.Unmarshal(msg.Payload(), &s); err != nil {
		slog.Error("[mqtt] bad band-follow controller state; dropping message", "slot", c.cfg.BandFollow.Slot, "err", err)
		return
	}
	sharedmqtt.Enqueue(c.jobs, func() {
		c.update(func(in *reconcile.Inputs) { c.applyFollowState(in, s.DeviceOnline, s.Retracted) })
	})
}

// applyFollowState re-arms the unlock re-extend (caller holds c.mu via
// update). Only while one is pending (reextendPending, set on unlock): the
// elements reported out confirm it; a controller that comes back online with
// the elements still retracted means the re-send was lost, so the follow
// dedup is reset and this same update() re-sends the frequency if the
// followed antenna is the target. Outside a pending re-extend (an operator
// retract, a plain RCU-06 outage) a reconnect re-sends nothing.
func (c *Client) applyFollowState(in *reconcile.Inputs, online, retracted bool) {
	cameBack := c.followSeen && !c.followDevOnline && online
	c.followSeen, c.followDevOnline = true, online
	if !c.reextendPending || in.Locked {
		return
	}
	if online && !retracted {
		c.reextendPending = false
		return
	}
	if cameBack && retracted && c.lastFollowFreq != 0 {
		slog.Info("[reconcile] band-follow controller back online with the elements retracted; re-sending the frequency",
			"slot", c.cfg.BandFollow.Slot, "freq_hz", c.lastFollowFreq)
		c.lastFollowFreq = 0
	}
}

// validRequest reports whether req is a well-formed operator request: a release
// ("" | "auto"), the manual stand-down, the grounded port, or one of the six
// physical switch ports (whether or not the wiring map currently names an
// antenna for it).
func validRequest(req string) bool {
	switch req {
	case "", reconcile.RequestAuto, reconcile.RequestManual, reconcile.PortOff:
		return true
	}
	if len(req) == len("portN") && strings.HasPrefix(req, "port") && req[4] >= '1' && req[4] <= '6' {
		return true
	}
	return false
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
	c.markActive(in)
}

// markActive records real station activity now (caller holds c.mu via update).
func (c *Client) markActive(in *reconcile.Inputs) {
	c.lastActivity = time.Now()
	c.activitySeen = true
	in.StationActivity = "active"
}

// activityLabel maps the reconciler input to the published value: unknown ("")
// is treated as active, exactly as the reconciler treats it.
func activityLabel(a string) string {
	if a == "inactive" {
		return "inactive"
	}
	return "active"
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

	activity := activityLabel(c.in.StationActivity)
	if !c.haveDecision || act.Decision != c.lastDecision ||
		activity != c.pubActivity || (c.activitySeen && !c.pubHasLast) {
		c.lastDecision = act.Decision
		c.haveDecision = true
		c.pubActivity = activity
		c.pubHasLast = c.activitySeen
		pubState = &statePayload{
			TS:       time.Now().UTC().Format(time.RFC3339),
			Mode:     act.Decision.Mode,
			Target:   act.Decision.Target,
			Source:   act.Decision.Source,
			Activity: activity,
		}
		if c.activitySeen {
			pubState.LastActivity = c.lastActivity.UTC().Format(time.RFC3339Nano)
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
	// Activity is the station activity the idle ladder tier uses: "active" |
	// "inactive" (after [idle].timeout_minutes with no VFO change, TX or operator
	// hold). powerseq keys its idle standby on it.
	Activity string `json:"activity"`
	// LastActivity (RFC3339Nano) is the time of the last real activity seen by this
	// process; absent until the first one (a restart publishes none, so it never
	// reads as fresh activity). Re-published on every activity flip, not on every
	// VFO step.
	LastActivity string `json:"last_activity,omitempty"`
}

func (c *Client) publishMeta() {
	capabilities := map[string]any{
		"controls": slotAntSwitch,
		"ladder":   []string{reconcile.SourceLock, reconcile.SourceIdle, reconcile.SourceOperator, reconcile.SourceAuto},
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
		// capabilities.ladder (the priority tiers, `lock` = the hf/ant-mode
		// storm/maintenance lock). `target` and `mode` are strings
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
				{"key": "activity", "name": "Activity", "type": "enum", "options": []string{"active", "inactive"}},
				{"key": "last_activity", "name": "Last activity", "type": "string"},
			},
		},
	}
	c.publishJSON(c.selfTopic("meta"), meta, 1, true)
}

// tokenTimeout bounds every paho token Wait (publishes AND subscribes). A
// broker that accepts the TCP connection but stalls PUBACK/SUBACK must not
// park the single jobs worker — the reconciler's idle walk-away grounding
// runs on it, and a parked worker silently stops that safety function while
// /status stays online (review S1a).
const tokenTimeout = 10 * time.Second

// waitToken bounds a token Wait; false = it did not complete in time (already
// logged). The caller still checks tok.Error() on true.
func waitToken(tok paho.Token, what, topic string) bool {
	if !tok.WaitTimeout(tokenTimeout) {
		slog.Error("[mqtt] "+what+" timed out", "topic", topic, "after", tokenTimeout)
		return false
	}
	return true
}

func (c *Client) subscribe(topic string, handler paho.MessageHandler) {
	tok := c.client.Subscribe(topic, 1, handler)
	if !waitToken(tok, "subscribe", topic) {
		return
	}
	if err := tok.Error(); err != nil {
		slog.Error("[mqtt] subscribe failed", "topic", topic, "err", err)
		return
	}
	slog.Info("[mqtt] subscribed", "topic", topic)
}

func (c *Client) publishJSON(topic string, v any, qos byte, retained bool) {
	b, _ := json.Marshal(v)
	tok := c.client.Publish(topic, qos, retained, b)
	if !waitToken(tok, "publish", topic) {
		return
	}
	if err := tok.Error(); err != nil {
		slog.Error("[mqtt] publish failed", "topic", topic, "err", err)
	}
}

func (c *Client) publishString(topic, payload string, qos byte, retained bool) {
	tok := c.client.Publish(topic, qos, retained, payload)
	if !waitToken(tok, "publish", topic) {
		return
	}
	if err := tok.Error(); err != nil {
		slog.Error("[mqtt] publish failed", "topic", topic, "err", err)
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
