package mqtt

import (
	"encoding/json"
	"math"
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"pelcobridge2/internal/control"
)

// Logger is the minimal logging surface the slot uses.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// Submitter hands an intent to the engine (control.Submit bound to the request
// channel). Non-blocking by contract: a saturated queue refuses the request and
// the command is dropped with a log line — paho's dispatch goroutine must never
// block.
type Submitter func(control.Intent) error

// Slot publishes the four planes of the rotator slot.
type Slot struct {
	cfg    Config
	pub    Publisher
	log    Logger
	submit Submitter
	// clients reports the current rotctld client count for /state.
	clients func() int

	jobs chan func() // /cmd work queue; drained by RunJobs

	mu       sync.Mutex
	lastBody string // last published /state body (dedup key, ts+age excluded)
	lastData []byte // last published /state payload, republished on reconnect
}

func NewSlot(cfg Config, pub Publisher, log Logger, submit Submitter, clients func() int) *Slot {
	return &Slot{
		cfg: cfg, pub: pub, log: log, submit: submit, clients: clients,
		jobs: make(chan func(), 32),
	}
}

// Jobs is the slot's /cmd work queue; run sharedmqtt.RunJobs on it from main.
func (s *Slot) Jobs() <-chan func() { return s.jobs }

// OnConnect is the paho OnConnect callback: publish the online birth on
// /status, refresh the retained /meta certificate, and (re)subscribe /cmd. It
// runs inside paho's connect flow, so it publishes through the callback's own
// client — the slot's publisher is not wired yet at that point.
func (s *Slot) OnConnect(c pahomqtt.Client) {
	_ = c.Publish(s.cfg.StatusTopic(), 1, true, []byte("online"))
	s.publishMeta(&PahoPublisher{Client: c})
	// Republish the last /state: a quiescent parked head emits no snapshots
	// (no polling), so without this consumers that subscribe after a broker
	// restart see no rotator state at all.
	s.mu.Lock()
	data := s.lastData
	s.mu.Unlock()
	if data != nil {
		_ = c.Publish(s.cfg.StateTopic(), 1, true, data)
	}
	tok := c.Subscribe(s.cfg.CmdTopic(), 1, func(_ pahomqtt.Client, m pahomqtt.Message) {
		payload := append([]byte(nil), m.Payload()...)
		sharedmqtt.Enqueue(s.jobs, func() { s.HandleCmd(payload) })
	})
	if tok.Wait() && tok.Error() != nil {
		s.log.Warnf("subscribe %s: %v", s.cfg.CmdTopic(), tok.Error())
	}
}

// HandleCmd parses one /cmd payload. ONLY {"action":"stop"} is accepted —
// there is no motion, arming, or calibration path from MQTT by design.
// Anything else is logged and ignored. Runs on the jobs worker.
func (s *Slot) HandleCmd(payload []byte) {
	var c schema.CmdPayload
	if err := json.Unmarshal(payload, &c); err != nil {
		s.log.Warnf("cmd: parse: %v (%s)", err, truncate(payload))
		return
	}
	if c.Action != "stop" {
		s.log.Warnf("cmd: unsupported action %q ignored (stop is the only action)", c.Action)
		return
	}
	if s.submit == nil {
		s.log.Warnf("cmd stop: no engine wired")
		return
	}
	if err := s.submit(control.StopIntent{}); err != nil {
		s.log.Warnf("cmd stop: %v", err)
	}
}

// --- /state -------------------------------------------------------------------

// statePayload is the retained /state snapshot (model §8: one JSON document).
// Position pointers are nil when there is no readback (JSON null, not NaN).
type statePayload struct {
	Ts             string   `json:"ts"`
	Az             *float64 `json:"az"`
	El             *float64 `json:"el"`
	PhysAz         *float64 `json:"phys_az"`
	PhysEl         *float64 `json:"phys_el"`
	ReadbackValid  bool     `json:"readback_valid"`
	ReadbackAgeS   float64  `json:"readback_age_s"`
	Armed          bool     `json:"armed"`
	AzOffsetDeg    *float64 `json:"az_offset_deg"`
	Moving         bool     `json:"moving"`
	TargetAz       *float64 `json:"target_az,omitempty"`
	TargetEl       *float64 `json:"target_el,omitempty"`
	SetStatus      string   `json:"set_status,omitempty"`
	SelfCheck      string   `json:"self_check"` // engine-canonical "on" | "off" | "unknown"; "unknown" is honest — AND with device_online
	JogSpeed       int      `json:"jog_speed"`
	RotctldClients int      `json:"rotctld_clients"`
	DeviceOnline   bool     `json:"device_online"`
	Link           string   `json:"link"`
	Error          string   `json:"error,omitempty"`
}

// f2Ptr renders degrees as a JSON number rounded to 0.01°, nil for NaN.
func f2Ptr(d float64) *float64 {
	if math.IsNaN(d) {
		return nil
	}
	v := math.Round(d*100) / 100
	return &v
}

// stateBody builds the published shape; Ts is stamped only on real publishes so
// the dedup key can compare bodies.
func stateBody(snap *control.Snapshot, clients int) statePayload {
	link := "down"
	if snap.DeviceOnline {
		link = "ok"
	}
	// SelfCheck publishes verbatim: the engine owns the canonical tri-state
	// ("on" | "off" | "unknown"), so no remap here — an empty value would be
	// an engine bug that must show up, not be papered over.
	return statePayload{
		Az:             f2Ptr(snap.Az),
		El:             f2Ptr(snap.El),
		PhysAz:         f2Ptr(snap.PhysAz),
		PhysEl:         f2Ptr(snap.PhysEl),
		ReadbackValid:  snap.ReadbackValid,
		ReadbackAgeS:   math.Round(snap.PanAge.Seconds()*10) / 10,
		Armed:          snap.Armed,
		AzOffsetDeg:    f2Ptr(snap.Offset),
		Moving:         snap.Moving,
		TargetAz:       f2Ptr(snap.TargetAz),
		TargetEl:       f2Ptr(snap.TargetEl),
		SetStatus:      snap.SetStatus,
		SelfCheck:      snap.SelfCheck,
		JogSpeed:       int(snap.JogSpeed),
		RotctldClients: clients,
		DeviceOnline:   snap.DeviceOnline,
		Link:           link,
		Error:          snap.Error,
	}
}

// PublishState publishes the retained /state snapshot, but only when a
// published field changed since the last snapshot (the engine publishes on
// every state change; ages and timestamps are excluded from the comparison so
// a stationary rotator produces no bus churn).
func (s *Slot) PublishState(snap *control.Snapshot) {
	clients := 0
	if s.clients != nil {
		clients = s.clients()
	}
	body := stateBody(snap, clients)
	age := body.ReadbackAgeS
	body.Ts = ""
	body.ReadbackAgeS = 0 // age churns every publish; excluded like the ts
	key, err := json.Marshal(body)
	if err != nil {
		s.log.Warnf("marshal state: %v", err)
		return
	}
	s.mu.Lock()
	changed := string(key) != s.lastBody
	if changed {
		s.lastBody = string(key)
	}
	s.mu.Unlock()
	if !changed {
		return
	}
	body.ReadbackAgeS = age
	body.Ts = time.Now().UTC().Format(time.RFC3339)
	data, err := json.Marshal(body)
	if err != nil {
		s.log.Warnf("marshal state ts: %v", err)
		return
	}
	s.mu.Lock()
	s.lastData = data
	s.mu.Unlock()
	_ = s.pub.Publish(s.cfg.StateTopic(), true, data)
}

// --- /meta --------------------------------------------------------------------

// metaPayload is the retained birth certificate (model §3/§5, Appendix C).
type metaPayload struct {
	Schema       string           `json:"schema"`
	Role         string           `json:"role"`
	Device       metaDevice       `json:"device"`
	Link         string           `json:"link,omitempty"`
	Host         string           `json:"host,omitempty"`
	Capabilities metaCapabilities `json:"capabilities"`
	Expose       *metaExpose      `json:"expose,omitempty"`
}

type metaDevice struct {
	Model string `json:"model"`
}

type metaCapabilities struct {
	Axes []string `json:"axes"`
}

type metaExpose struct {
	Device  metaExposeDevice   `json:"device"`
	Fields  []metaExposeField  `json:"fields"`
	Actions []metaExposeAction `json:"actions,omitempty"`
}

type metaExposeDevice struct {
	Name         string `json:"name,omitempty"`
	Model        string `json:"model,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
}

// metaExposeField describes one /state field (consumer-neutral; hadiscovery
// renders the HA entities from it).
type metaExposeField struct {
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Unit       string   `json:"unit,omitempty"`
	Class      string   `json:"class,omitempty"`
	StateClass string   `json:"state_class,omitempty"`
	Min        *float64 `json:"min,omitempty"`
	Max        *float64 `json:"max,omitempty"`
}

type metaExposeAction struct {
	Key     string      `json:"key"`
	Name    string      `json:"name"`
	Command metaCommand `json:"command"`
}

// metaCommand describes how an action is encoded on /cmd (Appendix C).
type metaCommand struct {
	Action string `json:"action,omitempty"`
}

func (s *Slot) publishMeta(pub Publisher) {
	azMin, azMax := 0.0, 360.0
	elMin, elMax := 0.0, 90.0
	p := metaPayload{
		Schema: "1.0",
		Role:   "rotator",
		Device: metaDevice{Model: s.cfg.DeviceModel},
		Link:   s.cfg.DeviceLink,
		Host:   s.cfg.Host,
		Capabilities: metaCapabilities{
			Axes: []string{"az", "el"},
		},
		Expose: &metaExpose{
			Device: metaExposeDevice{
				Name:         s.cfg.DeviceName,
				Model:        s.cfg.DeviceModel,
				Manufacturer: "Pelco",
			},
			Fields: []metaExposeField{
				{Key: "az", Name: "Azimuth", Type: "number", Unit: "°", Class: "azimuth", StateClass: "measurement", Min: &azMin, Max: &azMax},
				{Key: "el", Name: "Elevation", Type: "number", Unit: "°", Class: "elevation", StateClass: "measurement", Min: &elMin, Max: &elMax},
				{Key: "target_az", Name: "Target Azimuth", Type: "number", Unit: "°", Class: "azimuth", StateClass: "measurement"},
				{Key: "target_el", Name: "Target Elevation", Type: "number", Unit: "°", Class: "elevation", StateClass: "measurement"},
				{Key: "moving", Name: "Moving", Type: "boolean"},
				{Key: "armed", Name: "Armed", Type: "boolean"},
				{Key: "self_check", Name: "Self Check", Type: "string"},
				{Key: "device_online", Name: "Device Online", Type: "boolean"},
			},
			Actions: []metaExposeAction{
				{Key: "stop", Name: "Stop", Command: metaCommand{Action: "stop"}},
			},
		},
	}
	data, err := json.Marshal(p)
	if err != nil {
		s.log.Warnf("marshal meta: %v", err)
		return
	}
	_ = pub.Publish(s.cfg.MetaTopic(), true, data)
}

// PublishMeta publishes the retained birth certificate through the slot's own
// publisher.
func (s *Slot) PublishMeta() {
	s.publishMeta(s.pub)
}

func truncate(b []byte) string {
	if len(b) > 80 {
		return string(b[:80]) + "…"
	}
	return string(b)
}
