// SPDX-License-Identifier: AGPL-3.0-or-later

// Package overlay feeds the drawtext burn-in: it subscribes to the station's
// retained MQTT state snapshots (uhf rotators + radio), keeps the freshest
// reading per field, and re-renders the drawtext textfiles at a fixed cadence.
//
// The textfiles MUST exist before ffmpeg inits its drawtext filters (drawtext
// reads them at filter-init), so Start creates them synchronously; the writer
// then keeps them current whether or not the overlay is enabled — that is what
// makes a SIGHUP toggle of overlay.enabled safe.
package overlay

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	"vhfcam-restream/internal/config"
)

// reading is the freshest value from one upstream slot.
type reading struct {
	val    *float64
	online bool
	at     time.Time
}

type radioReading struct {
	freqHz int64
	tx     bool
	online bool
	// Session truth from the icom9700 bridge (2026-09-21 preview status):
	session string // idle|connecting|live|error
	demand  bool   // audio demand set (audio_on heartbeats active)
	at      time.Time
}

// Overlay is the MQTT consumer + textfile writer. All mutable state is guarded
// by mu; paho handlers only Enqueue.
type Overlay struct {
	log   *slog.Logger
	cfgFn func() config.OverlayConfig
	now   func() time.Time // injectable clock for tests

	jobs   chan func()
	client pahomqtt.Client // set by the connect loop; guarded by mu

	mu    sync.Mutex
	az    reading
	el    reading
	radio radioReading
	azUp  bool // source slot /status LWT ("online")
	elUp  bool
	radUp bool
	up    bool // our mqtt connection up
}

// New builds an Overlay. cfgFn is consulted on every render so SIGHUP config
// reloads take effect without restarting the writer.
func New(cfgFn func() config.OverlayConfig, log *slog.Logger) *Overlay {
	return &Overlay{
		log:   log,
		cfgFn: cfgFn,
		now:   time.Now,
		jobs:  make(chan func(), 32),
	}
}

// Start creates the textfile directory and seeds every file (synchronously —
// call before starting the supervisor), then launches the MQTT loop, the
// writer, and the jobs worker in the background. It returns after the files
// exist; the loops run until ctx is done.
func (o *Overlay) Start(ctx context.Context) error {
	cfg := o.cfgFn()
	if err := os.MkdirAll(cfg.Dir, 0755); err != nil {
		return fmt.Errorf("overlay dir: %w", err)
	}
	// Seed before anything can start ffmpeg: drawtext reads these at init.
	if err := o.writeTexts(o.render(o.now())); err != nil {
		return err
	}

	go sharedmqtt.RunJobs(ctx, o.jobs) // serializes state mutation
	go o.connectLoop(ctx)
	go o.writerLoop(ctx)
	return nil
}

// apply merges one retained state snapshot into the freshest readings. The
// snapshot's own ts (RFC3339, stamped by the producing bridge at publish) is
// the value timestamp — bridges publish change-only, so silence is normal and
// liveness is judged from /status LWT + device_online, not from republish
// cadence. topic must be one of the configured topics; unknown ones are
// ignored.
func (o *Overlay) apply(topic string, payload []byte) {
	cfg := o.cfgFn()
	var m struct {
		Az           *float64 `json:"az"`
		El           *float64 `json:"el"`
		DeviceOnline bool     `json:"device_online"`
		FreqHz       int64    `json:"freq_hz"`
		Tx           string   `json:"tx"`
		SessionState string   `json:"session_state"`
		AudioDemand  bool     `json:"audio_demand"`
		Ts           string   `json:"ts"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		o.log.Warn("overlay: bad state payload", "topic", topic, "err", err)
		return
	}
	now := o.now()
	at := now
	if m.Ts != "" {
		if ts, err := time.Parse(time.RFC3339, m.Ts); err == nil {
			at = ts
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	switch topic {
	case cfg.TopicAZ:
		o.az = reading{val: m.Az, online: m.DeviceOnline, at: at}
	case cfg.TopicEL:
		o.el = reading{val: m.El, online: m.DeviceOnline, at: at}
	case cfg.TopicRadio:
		o.radio = radioReading{
			freqHz: m.FreqHz, tx: m.Tx == "tx", online: m.DeviceOnline,
			session: m.SessionState, demand: m.AudioDemand, at: at,
		}
	}
}

// applyStatus records a source slot's /status LWT payload ("online"/"offline").
func (o *Overlay) applyStatus(topic, payload string) {
	cfg := o.cfgFn()
	up := payload == "online"
	o.mu.Lock()
	defer o.mu.Unlock()
	switch topic {
	case statusOf(cfg.TopicAZ):
		o.azUp = up
	case statusOf(cfg.TopicEL):
		o.elUp = up
	case statusOf(cfg.TopicRadio):
		o.radUp = up
	}
}

func statusOf(stateTopic string) string {
	return strings.TrimSuffix(stateTopic, "/state") + "/status"
}

func (o *Overlay) setUp(up bool) {
	o.mu.Lock()
	o.up = up
	o.mu.Unlock()
}

// Client returns the current MQTT client (nil until connectLoop built it).
// Used for other radio-audio demand publications on the same connection.
func (o *Overlay) Client() pahomqtt.Client {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.client
}

// RadioLink is the radio slot's status truth for the preview indicators:
// BridgeOnline is the bridge's /status LWT, DeviceOnline the CI-V control
// session liveness (state.device_online — two-layer liveness per the station
// model), SessionState the bridge's own lifecycle word, AudioDemand whether
// the audio demand is set.
type RadioLink struct {
	BridgeOnline bool
	DeviceOnline bool
	SessionState string
	AudioDemand  bool
}

// RadioLink snapshots the radio slot's status truth.
func (o *Overlay) RadioLink() RadioLink {
	staleAfter := time.Duration(o.cfgFn().StaleAfterS * float64(time.Second))
	o.mu.Lock()
	defer o.mu.Unlock()
	return RadioLink{
		BridgeOnline: o.up && o.radUp,
		DeviceOnline: o.up && o.radUp && o.radio.online && time.Since(o.radio.at) <= staleAfter,
		SessionState: o.radio.session,
		AudioDemand:  o.up && o.radUp && o.radio.demand,
	}
}

// render produces the drawtext textfile contents keyed by file base name.
// A field is fresh only when our MQTT link is up, the source slot's /status is
// "online", the snapshot says device_online, and the snapshot's ts is younger
// than stale_after_s. Stale fields render as "---" — never the last value (a
// frozen reading misleads the operator).
func (o *Overlay) render(now time.Time) map[string]string {
	cfg := o.cfgFn()
	staleAfter := time.Duration(cfg.StaleAfterS * float64(time.Second))

	o.mu.Lock()
	az, el, radio := o.az, o.el, o.radio
	azUp, elUp, radUp, up := o.azUp, o.elUp, o.radUp, o.up
	o.mu.Unlock()

	texts := map[string]string{"tx": ""} // TX line is blank unless actively transmitting
	if up && azUp && az.online && az.val != nil && now.Sub(az.at) <= staleAfter {
		texts["az"] = fmt.Sprintf("AZ %03.0f°", *az.val)
	} else {
		texts["az"] = "AZ ---"
	}
	if up && elUp && el.online && el.val != nil && now.Sub(el.at) <= staleAfter {
		texts["el"] = fmt.Sprintf("EL %03.0f°", *el.val)
	} else {
		texts["el"] = "EL ---"
	}
	radioOK := up && radUp && radio.online && now.Sub(radio.at) <= staleAfter
	if radioOK {
		texts["freq"] = fmt.Sprintf("%.3f MHz", float64(radio.freqHz)/1e6)
	} else {
		texts["freq"] = "FREQ ---"
	}
	if radioOK && radio.tx {
		texts["tx"] = "TX"
	}
	return texts
}

func (o *Overlay) writeTexts(texts map[string]string) error {
	cfg := o.cfgFn()
	for name, content := range texts {
		path := filepath.Join(cfg.Dir, name+".txt")
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			return fmt.Errorf("rename %s: %w", path, err)
		}
	}
	return nil
}

func (o *Overlay) writerLoop(ctx context.Context) {
	cfg := o.cfgFn()
	interval := time.Duration(cfg.RefreshS * float64(time.Second))
	t := time.NewTicker(interval)
	defer t.Stop()
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			texts := o.render(now)
			if err := o.writeTexts(texts); err != nil {
				o.log.Error("overlay: writing textfiles", "err", err)
			}
			tick++
			if tick%20 == 0 { // ~10 s at the default cadence
				o.publishState(now, texts)
			}
		}
	}
}

// connectLoop establishes the MQTT session (ctx-aware via shared/mqtt) and
// returns when the context is done; paho AutoReconnect maintains it after that.
func (o *Overlay) connectLoop(ctx context.Context) {
	cfg := o.cfgFn()
	statusTopic := fmt.Sprintf("%s/%s/%s/status", cfg.Site, cfg.Station, cfg.Slot)

	opts := pahomqtt.NewClientOptions().
		AddBroker(cfg.MQTTBroker).
		SetClientID(fmt.Sprintf("%s-%s-%s-overlay", cfg.Site, cfg.Station, cfg.Slot)).
		SetKeepAlive(30 * time.Second).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(10 * time.Second).
		SetWill(statusTopic, "offline", 0, true).
		SetOnConnectHandler(o.onConnect).
		SetConnectionLostHandler(func(_ pahomqtt.Client, err error) {
			o.setUp(false)
			o.log.Warn("overlay: mqtt connection lost", "err", err)
		})
	if cfg.MQTTUser != "" {
		opts = opts.SetUsername(cfg.MQTTUser)
		if cfg.MQTTPassword != "" {
			opts = opts.SetPassword(cfg.MQTTPassword)
		}
	}
	o.mu.Lock()
	o.client = pahomqtt.NewClient(opts)
	o.mu.Unlock()

	// ConnectRetry makes the token resolve only on success; shared/mqtt's
	// ctx-aware Connect unblocks the wait on ctx cancellation.
	if err := sharedmqtt.Connect(ctx, o.client); err != nil {
		o.log.Info("overlay: mqtt connect ended", "err", err)
	}
}

func (o *Overlay) onConnect(c pahomqtt.Client) {
	cfg := o.cfgFn()
	o.setUp(true)
	o.log.Info("overlay: mqtt connected", "broker", cfg.MQTTBroker)
	subs := []struct{ topic string; h pahomqtt.MessageHandler }{
		{cfg.TopicAZ, o.stateHandler()},
		{cfg.TopicEL, o.stateHandler()},
		{cfg.TopicRadio, o.stateHandler()},
		{statusOf(cfg.TopicAZ), o.statusHandler()},
		{statusOf(cfg.TopicEL), o.statusHandler()},
		{statusOf(cfg.TopicRadio), o.statusHandler()},
	}
	for _, s := range subs {
		if tok := c.Subscribe(s.topic, 0, s.h); tok.Wait() && tok.Error() != nil {
			o.log.Error("overlay: subscribe", "topic", s.topic, "err", tok.Error())
		}
	}
	statusTopic := fmt.Sprintf("%s/%s/%s/status", cfg.Site, cfg.Station, cfg.Slot)
	c.Publish(statusTopic, 0, true, "online").Wait() // broker-presence plane
}

func (o *Overlay) stateHandler() pahomqtt.MessageHandler {
	return func(_ pahomqtt.Client, m pahomqtt.Message) {
		sharedmqtt.Enqueue(o.jobs, func() { o.apply(m.Topic(), m.Payload()) })
	}
}

func (o *Overlay) statusHandler() pahomqtt.MessageHandler {
	return func(_ pahomqtt.Client, m pahomqtt.Message) {
		sharedmqtt.Enqueue(o.jobs, func() { o.applyStatus(m.Topic(), string(m.Payload())) })
	}
}

// publishState reports the overlay's own health on the station/state plane
// (retained; minimal planes — no /meta or /cmd yet).
func (o *Overlay) publishState(now time.Time, texts map[string]string) {
	o.mu.Lock()
	client := o.client
	up := o.up
	azUp, elUp, radUp := o.azUp, o.elUp, o.radUp
	az, el, radio := o.az, o.el, o.radio
	o.mu.Unlock()
	if client == nil || !up {
		return
	}
	cfg := o.cfgFn()
	staleAfter := time.Duration(cfg.StaleAfterS * float64(time.Second))
	azStale := !up || !azUp || !az.online || az.val == nil || now.Sub(az.at) > staleAfter
	elStale := !up || !elUp || !el.online || el.val == nil || now.Sub(el.at) > staleAfter
	radioStale := !up || !radUp || !radio.online || now.Sub(radio.at) > staleAfter

	payload, err := json.Marshal(map[string]any{
		"ts":             now.Format(time.RFC3339),
		"mqtt_connected": up,
		"az_stale":       azStale,
		"el_stale":       elStale,
		"radio_stale":    radioStale,
		"tx":             texts["tx"] != "",
	})
	if err != nil {
		return
	}
	topic := fmt.Sprintf("%s/%s/%s/state", cfg.Site, cfg.Station, cfg.Slot)
	client.Publish(topic, 0, true, payload) // async: writer goroutine must not block
}
