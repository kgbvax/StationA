package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	paho "github.com/eclipse/paho.mqtt.golang"

	"ubctrl/internal/ub/protocol"
	"ubctrl/internal/ub/service"
)

// bandCenterKHz maps a band label to its IARU Region 1 mid-band frequency.
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
	client   paho.Client
	prefix   string
	ctrl     *service.Controller
	deviceID string

	mu           sync.Mutex
	hasLastState bool
	lastState    publishedState
}

type publishedState struct {
	FrequencyKHz uint16
	BandName     string
	BandIndex    byte
	ModeName     string
	MotorsMoving bool
	MotorBits    byte
	Offline      bool
	LastError    string
}

func New(broker, clientID, prefix, user, password string, ctrl *service.Controller) (*Client, error) {
	if clientID == "" {
		clientID = "ubctrl"
	}
	if prefix == "" {
		prefix = "ubctrl"
	}
	opts := paho.NewClientOptions().AddBroker(broker).SetClientID(clientID).SetAutoReconnect(true).SetCleanSession(true)
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
		log.Printf("[mqtt] connected to broker=%s client_id=%s prefix=%s user_set=%t", broker, clientID, prefix, user != "")
	})
	c := &Client{client: paho.NewClient(opts), prefix: prefix, ctrl: ctrl, deviceID: "ubctrl_primary"}
	log.Printf("[mqtt] connecting to broker=%s client_id=%s prefix=%s user_set=%t", broker, clientID, prefix, user != "")
	if token := c.client.Connect(); token.Wait() && token.Error() != nil {
		return nil, token.Error()
	}
	log.Printf("[mqtt] initial connection established")
	return c, nil
}

func (c *Client) Close() {
	if c != nil && c.client.IsConnectionOpen() {
		c.client.Disconnect(250)
	}
}

func (c *Client) PublishState(state service.State) {
	snapshot := stateSnapshot(state)
	if !c.shouldPublishState(snapshot) {
		return
	}

	c.publishJSON(c.topic("status/frequency"), map[string]any{"frequency": state.FrequencyKHz, "band": state.BandName, "mode": state.ModeName}, 0, true)
	c.publishJSON(c.topic("status/motors"), map[string]any{"moving": state.MotorsMoving, "motor_bits": state.MotorBits}, 0, true)
	c.publishString(c.topic("status/availability"), availabilityPayload(state), 0, true)
	c.publishJSON(c.topic("status/raw"), state, 0, true)
}

func stateSnapshot(state service.State) publishedState {
	return publishedState{
		FrequencyKHz: state.FrequencyKHz,
		BandName:     state.BandName,
		BandIndex:    state.BandIndex,
		ModeName:     state.ModeName,
		MotorsMoving: state.MotorsMoving,
		MotorBits:    state.MotorBits,
		Offline:      state.Offline,
		LastError:    state.LastError,
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

func availabilityPayload(state service.State) string {
	if state.Offline {
		return "offline"
	}
	return "online"
}

func (c *Client) PublishDiscovery() {
	device := map[string]any{
		"identifiers":    []string{c.deviceID},
		"name":           "UltraBeam Antenna",
		"manufacturer":   "Ultrabeam",
		"model":          "RCU-06",
		"suggested_area": "Radio shack",
	}
	c.publishJSON(c.discoveryTopic("sensor", "frequency"), map[string]any{
		"name":                  "UBCtrl Frequency",
		"unique_id":             "ubctrl_frequency",
		"state_topic":           c.topic("status/frequency"),
		"availability_topic":    c.topic("status/availability"),
		"payload_available":     "online",
		"payload_not_available": "offline",
		"unit_of_measurement":   "kHz",
		"value_template":        "{{ value_json.frequency }}",
		"device":                device,
	}, 1, true)
	c.publishJSON(c.discoveryTopic("sensor", "band"), map[string]any{
		"name":                  "UBCtrl Band",
		"unique_id":             "ubctrl_band",
		"state_topic":           c.topic("status/frequency"),
		"availability_topic":    c.topic("status/availability"),
		"payload_available":     "online",
		"payload_not_available": "offline",
		"icon":                  "mdi:alpha-b-circle-outline",
		"value_template":        "{{ value_json.band }}",
		"device":                device,
	}, 1, true)
	c.publishJSON(c.discoveryTopic("binary_sensor", "motors_moving"), map[string]any{
		"name":                  "UBCtrl Motors Moving",
		"unique_id":             "ubctrl_motors_moving",
		"state_topic":           c.topic("status/motors"),
		"availability_topic":    c.topic("status/availability"),
		"payload_available":     "online",
		"payload_not_available": "offline",
		"payload_on":            "ON",
		"payload_off":           "OFF",
		"value_template":        "{{ 'ON' if value_json.moving else 'OFF' }}",
		"device":                device,
	}, 1, true)
	c.publishJSON(c.discoveryTopic("number", "frequency_set"), map[string]any{
		"name":                  "UBCtrl Frequency Set",
		"unique_id":             "ubctrl_frequency_set",
		"command_topic":         c.topic("command/frequency"),
		"state_topic":           c.topic("status/frequency"),
		"availability_topic":    c.topic("status/availability"),
		"payload_available":     "online",
		"payload_not_available": "offline",
		"mode":                  "box",
		"min":                   1,
		"max":                   65535,
		"step":                  1,
		"unit_of_measurement":   "kHz",
		"value_template":        "{{ value_json.frequency }}",
		"device":                device,
	}, 1, true)
	c.publishJSON(c.discoveryTopic("select", "mode"), map[string]any{
		"name":                  "UBCtrl Mode",
		"unique_id":             "ubctrl_mode",
		"command_topic":         c.topic("command/mode"),
		"state_topic":           c.topic("status/frequency"),
		"availability_topic":    c.topic("status/availability"),
		"payload_available":     "online",
		"payload_not_available": "offline",
		"options":               []string{"forward", "reverse", "bidirectional"},
		"value_template":        "{{ value_json.mode }}",
		"device":                device,
	}, 1, true)
	c.publishJSON(c.discoveryTopic("select", "band"), map[string]any{
		"name":                  "UBCtrl Band",
		"unique_id":             "ubctrl_band_set",
		"command_topic":         c.topic("command/band"),
		"state_topic":           c.topic("status/frequency"),
		"availability_topic":    c.topic("status/availability"),
		"payload_available":     "online",
		"payload_not_available": "offline",
		"options":               bandOptions,
		"value_template":        "{{ value_json.band }}",
		"icon":                  "mdi:radio-tower",
		"device":                device,
	}, 1, true)
	c.publishJSON(c.discoveryTopic("button", "retract"), map[string]any{
		"name":                  "UBCtrl Retract",
		"unique_id":             "ubctrl_retract",
		"command_topic":         c.topic("command/retract"),
		"availability_topic":    c.topic("status/availability"),
		"payload_available":     "online",
		"payload_not_available": "offline",
		"device":                device,
	}, 1, true)
}

func (c *Client) BindCommands(ctx context.Context) {
	_ = c.subscribe(c.topic("command/frequency"), func(_ paho.Client, msg paho.Message) {
		log.Printf("[mqtt] rx topic=%s payload=%q", msg.Topic(), string(msg.Payload()))
		freq, _ := strconv.Atoi(string(msg.Payload()))
		_ = c.ctrl.SetFrequency(context.Background(), uint16(freq), c.ctrl.State().ModeName)
	})
	_ = c.subscribe(c.topic("command/mode"), func(_ paho.Client, msg paho.Message) {
		log.Printf("[mqtt] rx topic=%s payload=%q", msg.Topic(), string(msg.Payload()))
		_ = c.ctrl.SetMode(context.Background(), modeFromPayload(msg.Payload()))
	})
	_ = c.subscribe(c.topic("command/retract"), func(_ paho.Client, _ paho.Message) {
		log.Printf("[mqtt] rx topic=%s", c.topic("command/retract"))
		_ = c.ctrl.Retract(context.Background())
	})
	_ = c.subscribe(c.topic("command/band"), func(_ paho.Client, msg paho.Message) {
		log.Printf("[mqtt] rx topic=%s payload=%q", msg.Topic(), string(msg.Payload()))
		khz, ok := bandCenterKHz[strings.TrimSpace(string(msg.Payload()))]
		if !ok {
			return
		}
		_ = c.ctrl.SetFrequency(context.Background(), khz, c.ctrl.State().ModeName)
	})
	// Re-announce discovery whenever Home Assistant comes online, so the device
	// appears even if HA (re)starts after ubctrl. This is the standard MQTT
	// discovery birth-message pattern.
	_ = c.subscribe("homeassistant/status", func(_ paho.Client, msg paho.Message) {
		if strings.EqualFold(strings.TrimSpace(string(msg.Payload())), "online") {
			log.Printf("[mqtt] Home Assistant online -> re-publishing discovery")
			c.PublishDiscovery()
			c.PublishState(c.ctrl.State())
		}
	})
	_ = ctx
}

func (c *Client) subscribe(topic string, handler paho.MessageHandler) error {
	if token := c.client.Subscribe(topic, 0, handler); token.Wait() && token.Error() != nil {
		log.Printf("[mqtt] subscribe failed topic=%s err=%v", topic, token.Error())
		return token.Error()
	}
	log.Printf("[mqtt] subscribed topic=%s", topic)
	return nil
}

func (c *Client) publishJSON(topic string, v any, qos byte, retained bool) {
	b, _ := json.Marshal(v)
	if token := c.client.Publish(topic, qos, retained, b); token.Wait() && token.Error() != nil {
		log.Printf("[mqtt] publish failed topic=%s qos=%d retained=%t payload=%s err=%v", topic, qos, retained, string(b), token.Error())
		return
	}
	log.Printf("[mqtt] tx topic=%s qos=%d retained=%t payload=%s", topic, qos, retained, string(b))
}

func (c *Client) publishString(topic, payload string, qos byte, retained bool) {
	if token := c.client.Publish(topic, qos, retained, payload); token.Wait() && token.Error() != nil {
		log.Printf("[mqtt] publish failed topic=%s qos=%d retained=%t payload=%q err=%v", topic, qos, retained, payload, token.Error())
		return
	}
	log.Printf("[mqtt] tx topic=%s qos=%d retained=%t payload=%q", topic, qos, retained, payload)
}

func (c *Client) topic(suffix string) string { return fmt.Sprintf("%s/%s", c.prefix, suffix) }
func (c *Client) discoveryTopic(kind, name string) string {
	return fmt.Sprintf("homeassistant/%s/%s/%s/config", kind, c.prefix, name)
}

func modeFromPayload(payload []byte) string {
	s := string(payload)
	switch s {
	case "forward", "reverse", "bidirectional", "normal", "180", "bidir":
		return s
	default:
		return protocol.ModeName(protocol.ModeNormal)
	}
}
