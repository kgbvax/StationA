// testui — schema-aware MQTT monitor + stimulator.
//
// The browser never speaks MQTT directly. It connects to this HTTP/SSE relay,
// which holds the broker credentials and forwards the <site>/# topic tree.
// See docs/station-integration-model.md for the three-plane schema.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/pelletier/go-toml/v2"

	"testui/static"
)

var validPlane = map[string]bool{
	"meta":   true,
	"state":  true,
	"status": true,
	"cmd":    true,
}

// findStaticDir returns a local static/ directory if one exists next to the
// executable or in the current working directory, otherwise "".
func findStaticDir() string {
	candidates := []string{"static"}
	if exe, err := os.Executable(); err == nil {
		candidates = append([]string{filepath.Join(filepath.Dir(exe), "static")}, candidates...)
	}
	for _, d := range candidates {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
	}
	return ""
}

type MQTTConfig struct {
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	User     string `toml:"user"`
	Password string `toml:"password"`
}

type Config struct {
	HTTPAddr string     `toml:"http_addr"`
	Site     string     `toml:"site"`
	MQTT     MQTTConfig `toml:"mqtt"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if env := os.Getenv("TESTUI_MQTT_PASSWORD"); env != "" {
		cfg.MQTT.Password = env
	}
	return &cfg, nil
}

// PlaneMsg is how a single plane (meta/state/status/cmd) is exposed to the UI.
// For JSON payloads Payload carries the decoded value and Object repeats it so the
// frontend can use either. For raw string payloads (e.g. /status "online") Payload
// is the string and Object is omitted.
type PlaneMsg struct {
	Topic    string      `json:"topic"`
	Payload  interface{} `json:"payload"`
	Retained bool        `json:"retained"`
	TS       string      `json:"ts"`
	Object   interface{} `json:"object,omitempty"`
}

type Slot struct {
	Address string    `json:"address"`
	Meta    *PlaneMsg `json:"meta,omitempty"`
	State   *PlaneMsg `json:"state,omitempty"`
	Status  *PlaneMsg `json:"status,omitempty"`
	Cmd     *PlaneMsg `json:"cmd,omitempty"`
}

type snapshot struct {
	Order []string `json:"order"`
	Slots []*Slot  `json:"slots"`
}

type updateEvent struct {
	Address string `json:"address"`
	Plane   string `json:"plane"`
	Cleared bool   `json:"cleared,omitempty"`
	PlaneMsg
}

type rawMsg struct {
	topic    string
	payload  []byte
	retained bool
}

type Bus struct {
	cfg      *Config
	sitePrefix string

	mu     sync.RWMutex
	slots  map[string]*Slot
	order  []string

	incoming chan *rawMsg

	clientsMu sync.RWMutex
	clients   map[chan *updateEvent]struct{}
}

func NewBus(cfg *Config) *Bus {
	return &Bus{
		cfg:        cfg,
		sitePrefix: cfg.Site + "/",
		slots:      make(map[string]*Slot),
		incoming:   make(chan *rawMsg, 256),
		clients:    make(map[chan *updateEvent]struct{}),
	}
}

func (b *Bus) Run() {
	for m := range b.incoming {
		b.process(m)
	}
}

func (b *Bus) process(m *rawMsg) {
	if !strings.HasPrefix(m.topic, b.sitePrefix) {
		return
	}
	parts := strings.Split(m.topic, "/")
	if len(parts) < 2 {
		return
	}
	plane := parts[len(parts)-1]
	if !validPlane[plane] {
		return
	}
	addr := strings.Join(parts[:len(parts)-1], "/")

	b.mu.Lock()
	slot, ok := b.slots[addr]
	if !ok {
		slot = &Slot{Address: addr}
		b.slots[addr] = slot
		b.order = append(b.order, addr)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	cleared := len(m.payload) == 0

	var ev *updateEvent
	if cleared {
		switch plane {
		case "meta":
			slot.Meta = nil
		case "state":
			slot.State = nil
		case "status":
			slot.Status = nil
		case "cmd":
			slot.Cmd = nil
		}
		ev = &updateEvent{
			Address: addr,
			Plane:   plane,
			Cleared: true,
			PlaneMsg: PlaneMsg{
				Topic:   m.topic,
				Payload: nil,
				TS:      now,
			},
		}
	} else {
		pm := decodePlaneMsg(m.topic, m.payload, m.retained, now)
		switch plane {
		case "meta":
			slot.Meta = pm
		case "state":
			slot.State = pm
		case "status":
			slot.Status = pm
		case "cmd":
			slot.Cmd = pm
		}
		ev = &updateEvent{
			Address:  addr,
			Plane:    plane,
			PlaneMsg: *pm,
		}
	}
	b.mu.Unlock()

	b.broadcast(ev)
}

func decodePlaneMsg(topic string, payload []byte, retained bool, ts string) *PlaneMsg {
	var obj interface{}
	if err := json.Unmarshal(payload, &obj); err == nil {
		return &PlaneMsg{
			Topic:    topic,
			Payload:  obj,
			Retained: retained,
			TS:       ts,
			Object:   obj,
		}
	}
	return &PlaneMsg{
		Topic:    topic,
		Payload:  string(payload),
		Retained: retained,
		TS:       ts,
	}
}

func (b *Bus) broadcast(ev *updateEvent) {
	b.clientsMu.RLock()
	defer b.clientsMu.RUnlock()
	for ch := range b.clients {
		select {
		case ch <- ev:
		default:
			// slow client: drop update rather than block the bus
		}
	}
}

func (b *Bus) snapshot() *snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	order := make([]string, len(b.order))
	copy(order, b.order)
	slots := make([]*Slot, 0, len(b.order))
	for _, addr := range b.order {
		s := b.slots[addr]
		cp := &Slot{
			Address: s.Address,
			Meta:    s.Meta,
			State:   s.State,
			Status:  s.Status,
			Cmd:     s.Cmd,
		}
		slots = append(slots, cp)
	}
	return &snapshot{Order: order, Slots: slots}
}

func (b *Bus) subscribe(c mqtt.Client) {
	topic := b.cfg.Site + "/#"
	if tok := c.Subscribe(topic, 1, func(_ mqtt.Client, msg mqtt.Message) {
		b.incoming <- &rawMsg{
			topic:    msg.Topic(),
			payload:  msg.Payload(),
			retained: msg.Retained(),
		}
	}); tok.Wait() && tok.Error() != nil {
		log.Printf("[mqtt] subscribe failed topic=%s err=%v", topic, tok.Error())
	} else {
		log.Printf("[mqtt] subscribe ok topic=%s", topic)
	}
}

func (b *Bus) setupMQTT() mqtt.Client {
	opts := mqtt.NewClientOptions().
		AddBroker(b.cfg.MQTT.Broker).
		SetClientID(b.cfg.MQTT.ClientID).
		SetUsername(b.cfg.MQTT.User).
		SetPassword(b.cfg.MQTT.Password).
		SetCleanSession(false).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30 * time.Second).
		SetConnectionLostHandler(func(c mqtt.Client, err error) {
			log.Printf("[mqtt] connection lost: %v", err)
		}).
		SetOnConnectHandler(func(c mqtt.Client) {
			log.Printf("[mqtt] connected broker=%s sub=%s/#", b.cfg.MQTT.Broker, b.cfg.Site)
			b.subscribe(c)
		}).
		SetOrderMatters(false)

	client := mqtt.NewClient(opts)
	if tok := client.Connect(); tok.WaitTimeout(10*time.Second) && tok.Error() != nil {
		log.Fatalf("mqtt connect failed: %v", tok.Error())
	}
	return client
}

func (b *Bus) handleStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ch := make(chan *updateEvent, 16)
	b.clientsMu.Lock()
	b.clients[ch] = struct{}{}
	b.clientsMu.Unlock()
	defer func() {
		b.clientsMu.Lock()
		delete(b.clients, ch)
		b.clientsMu.Unlock()
		close(ch)
	}()

	snap := b.snapshot()
	data, _ := json.Marshal(snap)
	fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data)
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: update\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

type publishRequest struct {
	Topic    string      `json:"topic"`
	Payload  interface{} `json:"payload"`
	Retain   bool        `json:"retain"`
	QOS      byte        `json:"qos"`
}

func (b *Bus) handlePublish(w http.ResponseWriter, r *http.Request) {
	var req publishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(req.Topic, b.sitePrefix) {
		http.Error(w, "topic outside configured site", http.StatusBadRequest)
		return
	}
	parts := strings.Split(req.Topic, "/")
	if len(parts) < 2 {
		http.Error(w, "invalid topic", http.StatusBadRequest)
		return
	}
	plane := parts[len(parts)-1]
	if !validPlane[plane] {
		http.Error(w, "unknown plane", http.StatusBadRequest)
		return
	}
	if plane == "cmd" && req.Retain {
		http.Error(w, "retained publish to /cmd is rejected (integration model §8)", http.StatusBadRequest)
		return
	}

	var payload []byte
	if s, ok := req.Payload.(string); ok {
		payload = []byte(s)
	} else {
		var err error
		payload, err = json.Marshal(req.Payload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	tok := mqttClient.Publish(req.Topic, req.QOS, req.Retain, payload)
	if tok.Wait() && tok.Error() != nil {
		http.Error(w, tok.Error().Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"retained":%v,"topic":%q}`, req.Retain, req.Topic)
}

type clearRequest struct {
	Topic string `json:"topic"`
}

func (b *Bus) handleClear(w http.ResponseWriter, r *http.Request) {
	var req clearRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(req.Topic, b.sitePrefix) {
		http.Error(w, "topic outside configured site", http.StatusBadRequest)
		return
	}
	tok := mqttClient.Publish(req.Topic, 1, true, []byte{})
	if tok.Wait() && tok.Error() != nil {
		http.Error(w, tok.Error().Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"cleared":true,"topic":%q}`, req.Topic)
}

var mqttClient mqtt.Client

func main() {
	configPath := flag.String("config", "config.toml", "path to TOML config")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = "127.0.0.1:8090"
	}
	if cfg.Site == "" {
		cfg.Site = "muehle"
	}

	bus := NewBus(cfg)
	go bus.Run()

	mqttClient = bus.setupMQTT()

	// Prefer static files on disk (next to the binary, then cwd) so local edits are
	// served without rebuilding. Fall back to the embedded assets so the binary is
	// self-contained.
	var fileServer http.Handler
	if dir := findStaticDir(); dir != "" {
		fileServer = http.FileServer(http.Dir(dir))
		log.Printf("serving static/ from disk (%s)", dir)
	} else {
		fileServer = http.FileServer(http.FS(static.FS))
		log.Println("serving embedded static files")
	}

	http.Handle("/", fileServer)
	http.HandleFunc("/api/stream", bus.handleStream)
	http.HandleFunc("/api/publish", bus.handlePublish)
	http.HandleFunc("/api/clear", bus.handleClear)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})

	log.Printf("testui listening on http://%s  (broker %s, sub %s/#)", cfg.HTTPAddr, cfg.MQTT.Broker, cfg.Site)
	if err := http.ListenAndServe(cfg.HTTPAddr, nil); err != nil {
		log.Fatalf("http server: %v", err)
	}
}
