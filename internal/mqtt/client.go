// Package mqtt wires the reconciler to the station bus: it subscribes to the radio,
// station, ant-switch, and operator topics, feeds each update into the reconciler, and
// emits the resulting intents (ant-switch select, controller band-follow) plus its own
// state. All decision logic lives in package reconcile; this layer is deliberately thin.
package mqtt

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

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

type Client struct {
	client  paho.Client
	cfg     config.Config
	rec     *reconcile.Reconciler
	site    string
	station string
	slot    string

	mu             sync.Mutex
	in             reconcile.Inputs
	lastDecision   reconcile.Decision
	haveDecision   bool
	lastSelect     string
	lastFollowFreq int64

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
	// decision/idempotency logic depends on sequential, ordered updates).
	jobs chan func()
	done chan struct{}
}

// New connects to the broker, registers the last-will, and subscribes to all inputs.
func New(cfg config.Config, rec *reconcile.Reconciler) (*Client, error) {
	c := &Client{
		cfg:     cfg,
		rec:     rec,
		site:    cfg.MQTT.Site,
		station: cfg.MQTT.Station,
		slot:    cfg.MQTT.Slot,
		jobs:    make(chan func(), 256),
		done:    make(chan struct{}),
	}
	go c.runJobs()

	opts := paho.NewClientOptions().
		AddBroker(cfg.MQTT.Broker).
		// Client ID derives from the slot address (model §8) so a duplicate
		// connection is diagnosable on the broker.
		SetClientID(orDefault(cfg.MQTT.ClientID, c.site+"-"+c.station+"-"+c.slot)).
		SetAutoReconnect(true).
		SetCleanSession(false).
		SetWill(c.selfTopic("status"), "offline", 1, true)
	if cfg.MQTT.User != "" {
		opts.SetUsername(cfg.MQTT.User)
	}
	if cfg.MQTT.Password != "" {
		opts.SetPassword(cfg.MQTT.Password)
	}
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		log.Printf("[mqtt] connection lost: %v", err)
	})
	opts.SetOnConnectHandler(func(_ paho.Client) {
		log.Printf("[mqtt] connected broker=%s slot=%s", cfg.MQTT.Broker, c.selfBase())
		c.publishString(c.selfTopic("status"), "online", 1, true)
		c.publishMeta()
		c.subscribeAll()
	})

	c.client = paho.NewClient(opts)
	if token := c.client.Connect(); token.Wait() && token.Error() != nil {
		return nil, token.Error()
	}
	return c, nil
}

func (c *Client) Close() {
	if c == nil {
		return
	}
	// Stop the worker first so no reconcile/publish work races with shutdown.
	close(c.done)
	if c.client.IsConnectionOpen() {
		c.publishString(c.selfTopic("status"), "offline", 1, true)
		c.client.Disconnect(250)
	}
}

// enqueue hands work to the worker without blocking paho's dispatch goroutine. It blocks
// only if the queue is full AND the service is not shutting down — impossible in practice
// (input traffic is low, drained by the worker as fast as it can publish); the `done` arm
// guarantees it never blocks during shutdown.
func (c *Client) enqueue(job func()) {
	select {
	case c.jobs <- job:
	case <-c.done:
	}
}

// runJobs is the single goroutine that owns reconcile+publish. It exits when Close closes
// `done` (finishing any in-flight job first).
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

// --- subscriptions ----------------------------------------------------------

func (c *Client) subscribeAll() {
	c.subscribe(c.siblingTopic(slotRadio, "state"), c.onRadioState)
	c.subscribe(c.siblingTopic(slotRadio, "status"), c.onRadioStatus)
	c.subscribe(c.siblingTopic(slotAntSwitch, "state"), c.onAntSwitchState)
	c.subscribe(c.selfTopic("cmd"), c.onOperatorCmd)
	// The station node carries the operator-set activity flag (integration model §7).
	// It is published at the station path itself; exact-topic subscribe avoids the
	// site/station/# wildcard (which would capture our own slot).
	c.subscribe(c.stationTopic(), c.onStationNode)
}

func (c *Client) onRadioState(_ paho.Client, msg paho.Message) {
	var s struct {
		Band   string `json:"band"`
		FreqHz int64  `json:"freq_hz"`
		TX     string `json:"tx"`
	}
	if err := json.Unmarshal(msg.Payload(), &s); err != nil {
		log.Printf("[mqtt] bad radio/state: %v", err)
		return
	}
	c.enqueue(func() {
		c.update(func(in *reconcile.Inputs) {
			in.RadioBand = s.Band
			in.RadioFreqHz = s.FreqHz
			in.RadioTX = s.TX
		})
	})
}

func (c *Client) onRadioStatus(_ paho.Client, msg paho.Message) {
	online := strings.EqualFold(strings.TrimSpace(string(msg.Payload())), "online")
	c.enqueue(func() { c.update(func(in *reconcile.Inputs) { in.RadioOnline = online }) })
}

func (c *Client) onAntSwitchState(_ paho.Client, msg paho.Message) {
	var s struct {
		Selected string `json:"selected"`
		Settled  bool   `json:"settled"`
	}
	if err := json.Unmarshal(msg.Payload(), &s); err != nil {
		log.Printf("[mqtt] bad ant-switch/state: %v", err)
		return
	}
	c.enqueue(func() {
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
		c.enqueue(func() { c.update(func(in *reconcile.Inputs) { in.OperatorRequest = "" }) })
		return
	}
	var cmd struct {
		Request string `json:"request"`
	}
	if err := json.Unmarshal(msg.Payload(), &cmd); err != nil {
		log.Printf("[mqtt] bad operator cmd: %v", err)
		return
	}
	c.enqueue(func() { c.update(func(in *reconcile.Inputs) { in.OperatorRequest = strings.TrimSpace(cmd.Request) }) })
}

func (c *Client) onStationNode(_ paho.Client, msg paho.Message) {
	// Copy the payload out of the paho message: it is only valid for the duration of the
	// handler, but update runs later, off this goroutine, on the worker.
	payload := append([]byte(nil), msg.Payload()...)
	c.enqueue(func() { c.update(func(in *reconcile.Inputs) { in.StationActivity = parseActivity(payload) }) })
}

// parseActivity reads the station activity flag leniently: it accepts a JSON object with
// an "activity" field, or a bare "active"/"inactive" string. Anything else is unknown ("").
func parseActivity(payload []byte) string {
	var obj struct {
		Activity string `json:"activity"`
	}
	if err := json.Unmarshal(payload, &obj); err == nil && obj.Activity != "" {
		return strings.ToLower(strings.TrimSpace(obj.Activity))
	}
	s := strings.ToLower(strings.TrimSpace(string(payload)))
	if s == "active" || s == "inactive" {
		return s
	}
	return ""
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
	if act.SelectPort != "" && act.SelectPort != c.lastSelect {
		c.lastSelect = act.SelectPort
		pubSelect = act.SelectPort
	}
	if act.FollowFreqHz != 0 && act.FollowFreqHz != c.lastFollowFreq {
		c.lastFollowFreq = act.FollowFreqHz
		pubFreq = act.FollowFreqHz
	}
	deferred := act.DeferredForTX
	c.mu.Unlock()

	if deferred {
		log.Printf("[reconcile] port change to %q deferred: radio is transmitting (cold-switch)", act.Decision.Target)
	}
	if pubState != nil {
		c.publishJSON(c.selfTopic("state"), pubState, 1, true)
		log.Printf("[reconcile] decision mode=%s target=%s source=%s", pubState.Mode, pubState.Target, pubState.Source)
	}
	if pubSelect != "" {
		c.publishJSON(c.siblingTopic(slotAntSwitch, "cmd"), map[string]any{"select": pubSelect}, 1, true)
		log.Printf("[reconcile] emit ant-switch select=%s", pubSelect)
	}
	if pubFreq != 0 {
		c.publishJSON(c.siblingTopic(c.cfg.BandFollow.Slot, "cmd"),
			map[string]any{"action": "frequency", "freq_hz": pubFreq}, 1, true)
		log.Printf("[reconcile] band-follow %s freq_hz=%d", c.cfg.BandFollow.Slot, pubFreq)
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
	if c.cfg.BandFollow.Resource != "" {
		capabilities["follows"] = map[string]string{c.cfg.BandFollow.Resource: c.cfg.BandFollow.Slot}
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
		log.Printf("[mqtt] subscribe failed topic=%s err=%v", topic, token.Error())
		return
	}
	log.Printf("[mqtt] subscribed topic=%s", topic)
}

func (c *Client) publishJSON(topic string, v any, qos byte, retained bool) {
	b, _ := json.Marshal(v)
	if token := c.client.Publish(topic, qos, retained, b); token.Wait() && token.Error() != nil {
		log.Printf("[mqtt] publish failed topic=%s err=%v", topic, token.Error())
	}
}

func (c *Client) publishString(topic, payload string, qos byte, retained bool) {
	if token := c.client.Publish(topic, qos, retained, payload); token.Wait() && token.Error() != nil {
		log.Printf("[mqtt] publish failed topic=%s err=%v", topic, token.Error())
	}
}

// --- topic helpers ----------------------------------------------------------

func (c *Client) stationTopic() string { return c.site + "/" + c.station }
func (c *Client) selfBase() string     { return c.site + "/" + c.station + "/" + c.slot }
func (c *Client) selfTopic(suffix string) string {
	return c.selfBase() + "/" + suffix
}
func (c *Client) siblingTopic(slot, suffix string) string {
	return fmt.Sprintf("%s/%s/%s/%s", c.site, c.station, slot, suffix)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
