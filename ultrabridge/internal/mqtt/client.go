package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"ultrabridge/internal/ub/service"
)

// bandCenterKHz maps a band label to its IARU Region 1 mid-band frequency in kHz.
var bandCenterKHz = map[string]uint16{
	"20m": 14175,
	"17m": 18118,
	"15m": 21225,
	"12m": 24940,
	"10m": 28850,
	"6m":  51000,
}

// bandOptions is the ordered list of bands exposed as a Home Assistant select.
var bandOptions = []string{"20m", "17m", "15m", "12m", "10m", "6m"}

type Client struct {
	client          paho.Client
	site            string
	station         string
	slot            string
	discoveryPrefix string
	location        string
	host            string
	// publishHADiscovery gates the legacy embedded HA discovery (integration model
	// §9). Default false: a standalone consumer (hadiscovery) renders discovery from
	// this bridge's consumer-neutral `expose` block in /meta instead.
	publishHADiscovery bool
	ctrl               *service.Controller

	mu           sync.Mutex
	hasLastState bool
	lastState    publishedState

	// jobs serializes handler work (cmd execution + HA-birth republish) on a single
	// goroutine that is NOT one of paho's dispatch goroutines. paho delivers message
	// handlers INLINE on its matchAndDispatch goroutine when OrderMatters is true (the
	// default — see the paho Subscribe docs: "callback must not block or call functions
	// within this package that may block (e.g. Publish) other than in a new go routine").
	// The cmd handler does serial I/O (RCU-06 round-trips) and a blocking publish (retract
	// clear); the HA-birth handler republishes state/discovery. Running any of that inline
	// blocks dispatch — and with retained bursts can deadlock it the same way hadiscovery
	// deadlocked live. Routing the work here lets each handler return immediately while
	// keeping cmd execution sequential (commands are applied in arrival order). The queue
	// + worker live in shared/mqtt (Enqueue/RunJobs) so the same fix is shared with every
	// other stationa consumer.
	//
	// ctx governs this client's lifecycle: a child of the ctx passed to New, so a SIGTERM
	// cancels it and Close cancels it directly to stop the worker before disconnecting.
	ctx    context.Context
	cancel context.CancelFunc
	jobs   chan func()
}

type publishedState struct {
	FreqHz    int64
	Band      string
	Mode      string
	Moving    bool
	Offline   bool
	LastError string
}

type statePayload struct {
	TS     string `json:"ts"`
	FreqHz int64  `json:"freq_hz"`
	Band   string `json:"band"`
	// Direction is the Ultrabeam element direction (forward/reverse/bidirectional).
	// It is deliberately NOT named "mode": on the station bus "mode" is the canonical
	// radio-mode vocabulary (cw/usb/lsb/...), a different concept (§4).
	Direction string `json:"direction"`
	Moving    bool   `json:"moving"`
	// DeviceOnline is the canonical device-reachability field (model §3): true while the
	// RCU-06 is reachable over serial, false when it is not (and the bridge itself is
	// still up). Published on every state snapshot so consumers (HA, historians) can
	// distinguish "online" from "no data" — bridge liveness is /status, never a state
	// field. Exposed as a read-only boolean so hadiscovery renders a binary_sensor for
	// RCU-06 reachability.
	DeviceOnline bool   `json:"device_online"`
	Error        string `json:"error,omitempty"`
}

func New(ctx context.Context, broker, clientID, site, station, slot, discoveryPrefix, location, host, user, password string, publishHADiscovery bool, ctrl *service.Controller) (*Client, error) {
	if site == "" || station == "" {
		return nil, fmt.Errorf("mqtt: site and station must be configured for station-model addressing")
	}
	if slot == "" {
		slot = "ant-ctrl"
	}
	if clientID == "" {
		// Client ID derives from the slot address (model §8) so a duplicate
		// connection is diagnosable on the broker.
		clientID = site + "-" + station + "-" + slot
	}
	if discoveryPrefix == "" {
		discoveryPrefix = "homeassistant"
	}

	c := &Client{
		site:               site,
		station:            station,
		slot:               slot,
		discoveryPrefix:    discoveryPrefix,
		location:           location,
		host:               host,
		publishHADiscovery: publishHADiscovery,
		ctrl:               ctrl,
		jobs:               make(chan func(), 256),
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	go sharedmqtt.RunJobs(c.ctx, c.jobs)

	opts := paho.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetCleanSession(false).
		SetWill(c.statusTopic(), "offline", 1, true)

	if user != "" {
		opts.SetUsername(user)
	}
	if password != "" {
		opts.SetPassword(password)
	}

	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		log.Printf("[mqtt] connection lost: %v", err)
	})
	opts.SetOnConnectHandler(func(_ paho.Client) {
		log.Printf("[mqtt] connected broker=%s client_id=%s slot=%s/%s/%s", broker, clientID, site, station, slot)
		c.publishString(c.statusTopic(), "online", 1, true)
		c.PublishMeta()
		// Re-subscribe on every connect in case the broker lost session state.
		c.subscribeCmd()
		c.subscribeHABirth()
	})

	c.client = paho.NewClient(opts)
	log.Printf("[mqtt] connecting to broker=%s client_id=%s slot=%s/%s/%s", broker, clientID, site, station, slot)
	// Context-aware connect: paho's Connect().Wait() blocks ignoring ctx, so a SIGTERM while
	// the broker is unreachable can't interrupt it and systemd must SIGKILL after
	// TimeoutStopSec. sharedmqtt.Connect bridges the wait through a goroutine + select on
	// ctx.Done (see the stationa memory on paho connect).
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
	// Stop the worker first so no cmd/HA-birth work (which does serial I/O + publishes)
	// races with shutdown.
	c.cancel()
	if c.client.IsConnectionOpen() {
		c.publishString(c.statusTopic(), "offline", 1, true)
		c.client.Disconnect(250)
	}
}

func (c *Client) PublishState(state service.State) {
	snap := stateSnapshot(state)
	if !c.shouldPublishState(snap) {
		return
	}
	payload := statePayload{
		TS:           time.Now().UTC().Format(time.RFC3339),
		FreqHz:       snap.FreqHz,
		Band:         snap.Band,
		Direction:    snap.Mode,
		Moving:       snap.Moving,
		DeviceOnline: !snap.Offline,
		Error:        snap.LastError,
	}
	c.publishJSON(c.stateTopic(), payload, 1, true)
}

func (c *Client) PublishMeta() {
	c.publishJSON(c.metaTopic(), c.metaPayload(), 1, true)
}

// metaPayload builds the retained /meta birth certificate, including the consumer-neutral
// `expose` block. Extracted from PublishMeta so the expose surface is testable without a
// broker connection.
func (c *Client) metaPayload() map[string]any {
	meta := map[string]any{
		"schema": "1.0",
		// Canonical role (model §4): a controller that tunes a passive antenna
		// resource. The device name (Ultrabeam RCU-06) lives in `device`, never in
		// the role or the address.
		"role": "ant-ctrl",
		"device": map[string]any{
			"model": "Ultrabeam RCU-06",
		},
		"link": "serial",
		"capabilities": map[string]any{
			"bands":      bandOptions,
			"directions": []string{"forward", "reverse", "bidirectional"},
		},
		// Consumer-neutral field surface (integration model §3.1, Appendix C). No
		// consumer vocabulary here — hadiscovery renders HA discovery from this.
		// ultrabridge is read-write: freq_hz/band/direction are writable setpoints backed
		// by /cmd, moving is a read-only boolean, and retract is a one-shot action.
		// The enum options resolve via options_ref against capabilities above.
		"expose": map[string]any{
			"device": map[string]any{
				"name":         "UltraBeam Antenna",
				"model":        "RCU-06",
				"manufacturer": "Ultrabeam",
				// No "area": hadiscovery supplies the deployment-wide default HA area
				// (config `area`, default "Bauwagen") for slots that do not name one
				// (integration model §9 — the bridge carries no HA/area knowledge).
			},
			"fields": []map[string]any{
				{
					"key": "freq_hz", "name": "Frequency", "type": "number",
					"unit": "Hz", "class": "frequency", "state_class": "measurement",
					"writable": true,
					"min":      1800000, "max": 54000000, "step": 1000,
					"command": map[string]any{"action": "frequency", "value_key": "freq_hz", "value_type": "int"},
				},
				{
					"key": "band", "name": "Band", "type": "enum", "options_ref": "bands",
					"writable": true,
					"command":  map[string]any{"action": "band", "value_key": "value", "value_type": "string"},
				},
				{
					"key": "direction", "name": "Direction", "type": "enum", "options_ref": "directions",
					"writable": true,
					"command":  map[string]any{"action": "direction", "value_key": "value", "value_type": "string"},
				},
				{"key": "moving", "name": "Moving", "type": "boolean"},
				// device_online and error are read-only diagnostics: RCU-06 reachability
				// (binary_sensor) and the last error string (sensor). The bridge stays up
				// (/status online) while the device is down, so these surface device health
				// that /status alone cannot.
				{"key": "device_online", "name": "Device online", "type": "boolean"},
				{"key": "error", "name": "Last error", "type": "string"},
			},
			"actions": []map[string]any{
				{
					"key": "retract", "name": "Retract",
					"command": map[string]any{"action": "retract"},
				},
			},
		},
	}
	if c.location != "" {
		meta["location"] = c.location
	}
	if c.host != "" {
		meta["host"] = c.host
	}
	return meta
}

func stateSnapshot(state service.State) publishedState {
	return publishedState{
		FreqHz:    int64(state.FrequencyKHz) * 1000,
		Band:      state.BandName,
		Mode:      state.ModeName,
		Moving:    state.MotorsMoving,
		Offline:   state.Offline,
		LastError: state.LastError,
	}
}

func (c *Client) shouldPublishState(snapshot publishedState) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasLastState && c.lastState == snapshot {
		return false
	}
	c.lastState = snapshot
	c.hasLastState = true
	return true
}

// PublishDiscoveryIfEnabled emits the legacy embedded HA discovery only when its gate is
// on. The standalone hadiscovery consumer renders discovery from /meta.expose otherwise
// (integration model §9).
func (c *Client) PublishDiscoveryIfEnabled() {
	if c.publishHADiscovery {
		c.PublishDiscovery()
	}
}

func (c *Client) PublishDiscovery() {
	nodeID := c.station + "-" + c.slot
	device := map[string]any{
		"identifiers":    []string{nodeID},
		"name":           "UltraBeam Antenna",
		"manufacturer":   "Ultrabeam",
		"model":          "RCU-06",
		"suggested_area": "Radio shack",
	}
	avail := []map[string]any{
		{"topic": c.statusTopic(), "payload_available": "online", "payload_not_available": "offline"},
	}
	st := c.stateTopic()
	cmd := c.cmdTopic()

	c.publishJSON(c.discoveryTopic("sensor", nodeID, "frequency"), map[string]any{
		"name":                nodeID + "_frequency",
		"unique_id":           nodeID + "_frequency",
		"state_topic":         st,
		"availability":        avail,
		"unit_of_measurement": "Hz",
		"device_class":        "frequency",
		"state_class":         "measurement",
		"value_template":      "{{ value_json.freq_hz }}",
		"device":              device,
	}, 1, true)
	c.publishJSON(c.discoveryTopic("sensor", nodeID, "band"), map[string]any{
		"name":           nodeID + "_band",
		"unique_id":      nodeID + "_band",
		"state_topic":    st,
		"availability":   avail,
		"icon":           "mdi:alpha-b-circle-outline",
		"value_template": "{{ value_json.band }}",
		"device":         device,
	}, 1, true)
	c.publishJSON(c.discoveryTopic("sensor", nodeID, "direction"), map[string]any{
		"name":           nodeID + "_direction",
		"unique_id":      nodeID + "_direction",
		"state_topic":    st,
		"availability":   avail,
		"value_template": "{{ value_json.direction }}",
		"device":         device,
	}, 1, true)
	c.publishJSON(c.discoveryTopic("binary_sensor", nodeID, "moving"), map[string]any{
		"name":           nodeID + "_moving",
		"unique_id":      nodeID + "_moving",
		"state_topic":    st,
		"availability":   avail,
		"payload_on":     "ON",
		"payload_off":    "OFF",
		"value_template": "{{ 'ON' if value_json.moving else 'OFF' }}",
		"device":         device,
	}, 1, true)
	c.publishJSON(c.discoveryTopic("number", nodeID, "frequency_set"), map[string]any{
		"name":                nodeID + "_frequency_set",
		"unique_id":           nodeID + "_frequency_set",
		"state_topic":         st,
		"command_topic":       cmd,
		"command_template":    `{"action":"frequency","freq_hz":{{ value | int }}}`,
		"availability":        avail,
		"mode":                "box",
		"min":                 1800000,
		"max":                 54000000,
		"step":                1000,
		"unit_of_measurement": "Hz",
		"value_template":      "{{ value_json.freq_hz }}",
		"retain":              true,
		"device":              device,
	}, 1, true)
	c.publishJSON(c.discoveryTopic("select", nodeID, "direction_set"), map[string]any{
		"name":             nodeID + "_direction_set",
		"unique_id":        nodeID + "_direction_set",
		"state_topic":      st,
		"command_topic":    cmd,
		"command_template": `{"action":"direction","value":"{{ value }}"}`,
		"availability":     avail,
		"options":          []string{"forward", "reverse", "bidirectional"},
		"value_template":   "{{ value_json.direction }}",
		"retain":           true,
		"device":           device,
	}, 1, true)
	c.publishJSON(c.discoveryTopic("select", nodeID, "band_set"), map[string]any{
		"name":             nodeID + "_band_set",
		"unique_id":        nodeID + "_band_set",
		"state_topic":      st,
		"command_topic":    cmd,
		"command_template": `{"action":"band","value":"{{ value }}"}`,
		"availability":     avail,
		"options":          bandOptions,
		"value_template":   "{{ value_json.band }}",
		"icon":             "mdi:radio-tower",
		"retain":           true,
		"device":           device,
	}, 1, true)
	c.publishJSON(c.discoveryTopic("button", nodeID, "retract"), map[string]any{
		"name":          nodeID + "_retract",
		"unique_id":     nodeID + "_retract",
		"command_topic": cmd,
		"payload_press": `{"action":"retract"}`,
		"availability":  avail,
		"device":        device,
	}, 1, true)
}

func (c *Client) BindCommands(ctx context.Context) {
	c.subscribeCmd()
	c.subscribeHABirth()
	_ = ctx
}

func (c *Client) subscribeCmd() {
	_ = c.subscribe(c.cmdTopic(), c.onCmd)
}

// onCmd is the /cmd handler, extracted so it is unit-testable. It parses the command in the
// paho handler (non-blocking) and runs the serial I/O + retract-clear publish on the worker
// so it never blocks paho's inline dispatch goroutine.
func (c *Client) onCmd(_ paho.Client, msg paho.Message) {
	var cmd struct {
		Action string `json:"action"`
		Value  string `json:"value,omitempty"`
		FreqHz int64  `json:"freq_hz,omitempty"`
	}
	if err := json.Unmarshal(msg.Payload(), &cmd); err != nil || cmd.Action == "" {
		log.Printf("[mqtt] rx invalid cmd topic=%s payload=%q", msg.Topic(), string(msg.Payload()))
		return
	}
	log.Printf("[mqtt] rx cmd action=%s value=%q freq_hz=%d", cmd.Action, cmd.Value, cmd.FreqHz)
	sharedmqtt.Enqueue(c.jobs, func() {
		switch cmd.Action {
		case "frequency":
			khz := uint16(cmd.FreqHz / 1000)
			_ = c.ctrl.SetFrequency(context.Background(), khz, c.ctrl.State().ModeName)
		case "direction", "mode": // "mode" accepted as a deprecated alias for "direction"
			_ = c.ctrl.SetMode(context.Background(), cmd.Value)
		case "band":
			khz, ok := bandCenterKHz[strings.TrimSpace(cmd.Value)]
			if !ok {
				log.Printf("[mqtt] unknown band %q in cmd", cmd.Value)
				return
			}
			_ = c.ctrl.SetFrequency(context.Background(), khz, c.ctrl.State().ModeName)
		case "retract":
			_ = c.ctrl.Retract(context.Background())
			// Clear retained cmd so retract doesn't re-execute on next connect.
			if token := c.client.Publish(c.cmdTopic(), 1, true, []byte{}); token.Wait() && token.Error() != nil {
				log.Printf("[mqtt] failed to clear cmd topic: %v", token.Error())
			}
		default:
			log.Printf("[mqtt] unknown cmd action=%q", cmd.Action)
		}
	})
}

func (c *Client) subscribeHABirth() {
	_ = c.subscribe("homeassistant/status", c.onHAStatus)
}

// onHAStatus is the homeassistant/status handler, extracted so it is unit-testable. The
// "online" check stays in the handler (non-blocking); the republish (which publishes
// synchronously) runs on the worker — never inline on paho's dispatch goroutine. (hadiscovery,
// the standalone consumer, owns HA discovery and its own rebirth from /meta otherwise; §9.)
func (c *Client) onHAStatus(_ paho.Client, msg paho.Message) {
	if !strings.EqualFold(strings.TrimSpace(string(msg.Payload())), "online") {
		return
	}
	sharedmqtt.Enqueue(c.jobs, func() {
		if c.publishHADiscovery {
			log.Printf("[mqtt] Home Assistant online -> re-publishing embedded discovery")
			c.PublishDiscovery()
		}
		c.PublishState(c.ctrl.State())
	})
}

func (c *Client) subscribe(topic string, handler paho.MessageHandler) error {
	if token := c.client.Subscribe(topic, 1, handler); token.Wait() && token.Error() != nil {
		log.Printf("[mqtt] subscribe failed topic=%s err=%v", topic, token.Error())
		return token.Error()
	}
	log.Printf("[mqtt] subscribed topic=%s", topic)
	return nil
}

func (c *Client) publishJSON(topic string, v any, qos byte, retained bool) {
	b, _ := json.Marshal(v)
	if token := c.client.Publish(topic, qos, retained, b); token.Wait() && token.Error() != nil {
		log.Printf("[mqtt] publish failed topic=%s err=%v", topic, token.Error())
		return
	}
	log.Printf("[mqtt] tx topic=%s qos=%d retained=%t payload=%s", topic, qos, retained, string(b))
}

func (c *Client) publishString(topic, payload string, qos byte, retained bool) {
	if token := c.client.Publish(topic, qos, retained, payload); token.Wait() && token.Error() != nil {
		log.Printf("[mqtt] publish failed topic=%s err=%v", topic, token.Error())
		return
	}
	log.Printf("[mqtt] tx topic=%s qos=%d retained=%t payload=%q", topic, qos, retained, payload)
}

// Topic helpers. The slot address format lives in shared/schema; these wrap it for the
// planes this bridge publishes. discoveryTopic is the HA discovery tree (prefix, not a
// slot), which schema does not model, so it stays local.

func (c *Client) stateTopic() string  { return schema.StateTopic(c.site, c.station, c.slot) }
func (c *Client) metaTopic() string   { return schema.MetaTopic(c.site, c.station, c.slot) }
func (c *Client) statusTopic() string { return schema.StatusTopic(c.site, c.station, c.slot) }
func (c *Client) cmdTopic() string    { return schema.CmdTopic(c.site, c.station, c.slot) }

func (c *Client) discoveryTopic(component, nodeID, objectID string) string {
	return fmt.Sprintf("%s/%s/%s/%s/config", c.discoveryPrefix, component, nodeID, objectID)
}
