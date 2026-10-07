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
	// freqPresent: the snapshot CARRIED freq_hz. The bridge omits fields it
	// could not read (e.g. radio in standby: session live, CI-V deaf) — an
	// omitted frequency must render as "---", never as a zero.
	freqPresent bool
	online      bool
	// Session truth from the icom9700 bridge (2026-09-21 preview status):
	session string // idle|connecting|live|error
	demand  bool   // audio demand set (audio_on heartbeats active)
	// responding: the bridge's radio_responding (CI-V answered the latest
	// poll). sawRespField records whether the field existed at all — an old
	// bridge (pre-2026-09-21) omits it, and then freq presence is the
	// fallback signal (deploy-skew shim).
	responding   bool
	sawRespField bool
	at           time.Time
}

// satReading is the latest muehle/uhf/sat-track snapshot. The bridge
// publishes the satellite keys as null (never omits them) when nothing is
// tracked, so each snapshot replaces the previous one wholesale.
type satReading struct {
	name       *string
	rangeKm    *float64
	downlinkHz *int64 // OscarWatch's radio-corrected (Doppler-applied) frequencies
	uplinkHz   *int64
	tracking   bool
	online   bool
	at       time.Time
}

// satNameMax bounds the burned-in satellite name so a long catalogue name can
// never run into the FREQ field on the left (LoTW names are ≤ 8 chars:
// TEVEL2-5, SONATE-2).
const satNameMax = 8

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
	sat   satReading
	azUp  bool // source slot /status LWT ("online")
	elUp  bool
	radUp bool
	satUp bool
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
		Az               *float64 `json:"az"`
		El               *float64 `json:"el"`
		DeviceOnline     bool     `json:"device_online"`
		FreqHz           *int64   `json:"freq_hz"`
		SessionState     *string  `json:"session_state"`
		AudioDemand      *bool    `json:"audio_demand"`
		RadioResponding  *bool    `json:"radio_responding"`
		SatName          *string  `json:"sat_name"`
		RangeKm          *float64 `json:"range_km"`
		DownlinkHz       *int64   `json:"downlink_hz"`
		UplinkHz         *int64   `json:"uplink_hz"`
		Tracking         bool     `json:"tracking"`
		Ts               string   `json:"ts"`
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
	case cfg.TopicSat:
		o.sat = satReading{
			name: m.SatName, rangeKm: m.RangeKm, downlinkHz: m.DownlinkHz, uplinkHz: m.UplinkHz,
			tracking: m.Tracking, online: m.DeviceOnline, at: at,
		}
	case cfg.TopicRadio:
		r := o.radio // fields the snapshot omits carry over the last value
		freqHz := r.freqHz
		freqPresent := false
		if m.FreqHz != nil {
			freqHz, freqPresent = *m.FreqHz, true
		}
		session, demand := r.session, r.demand
		if m.SessionState != nil {
			session = *m.SessionState
		}
		if m.AudioDemand != nil {
			demand = *m.AudioDemand
		}
		responding, sawResp := r.responding, r.sawRespField
		if m.RadioResponding != nil {
			responding, sawResp = *m.RadioResponding, true
		}
		o.radio = radioReading{
			freqHz: freqHz, freqPresent: freqPresent, online: m.DeviceOnline,
			session: session, demand: demand,
			responding: responding, sawRespField: sawResp,
			at: at,
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
	case statusOf(cfg.TopicSat):
		o.satUp = up
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

// RadioLink is the radio slot's status truth for the preview indicators —
// every field is a single fact, freshness-gated (a stale snapshot yields zero
// values: unknown is not failure):
//   - BridgeOnline: the bridge's /status LWT is "online" (process alive).
//   - DeviceOnline: the CI-V control session is live (state.device_online).
//   - SessionState: the bridge's lifecycle word ("" when stale).
//   - AudioDemand:  the audio demand is set on the bridge.
//   - Responding:   the radio answers CI-V. Bridge-published
//     (radio_responding); with an older bridge the absence of that field
//     falls back to freq_hz presence (deploy-skew shim).
type RadioLink struct {
	BridgeOnline bool
	DeviceOnline bool
	SessionState string
	AudioDemand  bool
	Responding   bool
}

// RadioLink snapshots the radio slot's status truth.
func (o *Overlay) RadioLink() RadioLink {
	staleAfter := time.Duration(o.cfgFn().StaleAfterS * float64(time.Second))
	o.mu.Lock()
	defer o.mu.Unlock()
	fresh := o.up && o.radUp && o.radio.online && time.Since(o.radio.at) <= staleAfter
	responding := o.radio.responding || (!o.radio.sawRespField && o.radio.freqPresent)
	return RadioLink{
		BridgeOnline: o.up && o.radUp,
		DeviceOnline: fresh,
		SessionState: func() string {
			if fresh {
				return o.radio.session
			}
			return ""
		}(),
		AudioDemand: fresh && o.up && o.radUp && o.radio.demand,
		Responding:  fresh && responding,
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
	az, el, radio, sat := o.az, o.el, o.radio, o.sat
	azUp, elUp, radUp, satUp, up := o.azUp, o.elUp, o.radUp, o.satUp, o.up
	o.mu.Unlock()

	// SAT is blank unless a satellite is tracked.
	texts := map[string]string{"sat": ""}
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
	texts["freq"] = freqText(satTracked(up, satUp, sat, now, staleAfter), sat, radioOK, radio)
	if satFresh(up, satUp, sat, now, staleAfter) {
		texts["sat"] = fmt.Sprintf("%s %.0f km", truncate(*sat.name, satNameMax), *sat.rangeKm)
	}
	return texts
}

// satTracked: the sat-track snapshot is fresh (two-layer liveness, like every
// field) and a satellite is tracked.
func satTracked(up, satUp bool, s satReading, now time.Time, staleAfter time.Duration) bool {
	return up && satUp && s.online && s.tracking && now.Sub(s.at) <= staleAfter
}

// satFresh is the SAT field's rule: a tracked satellite with a name and a range.
func satFresh(up, satUp bool, s satReading, now time.Time, staleAfter time.Duration) bool {
	return satTracked(up, satUp, s, now, staleAfter) && s.name != nil && s.rangeKm != nil
}

// freqText is the FREQ field. While a satellite is tracked it shows
// OscarWatch's radio-corrected downlink/uplink ("↓145.850 ↑435.300", user
// 2026-10-07); otherwise the IC-9700's own frequency, or "FREQ ---".
func freqText(tracked bool, s satReading, radioOK bool, r radioReading) string {
	if tracked && (positive(s.downlinkHz) || positive(s.uplinkHz)) {
		var parts []string
		if positive(s.downlinkHz) {
			parts = append(parts, fmt.Sprintf("↓%.3f", float64(*s.downlinkHz)/1e6))
		}
		if positive(s.uplinkHz) {
			parts = append(parts, fmt.Sprintf("↑%.3f", float64(*s.uplinkHz)/1e6))
		}
		return strings.Join(parts, " ")
	}
	if hz, ok := freshFreq(radioOK, r); ok {
		return fmt.Sprintf("%.3f MHz", float64(hz)/1e6)
	}
	return "FREQ ---"
}

func positive(v *int64) bool { return v != nil && *v > 0 }

func truncate(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// freshFreq is the one rule for "the radio frequency is known": the radio slot
// is fresh and its snapshot carried a real freq_hz (freqPresent=false = the
// bridge omitted the field, e.g. radio in standby — a carried-over or zero
// value must never render as a real frequency).
func freshFreq(radioOK bool, r radioReading) (int64, bool) {
	if radioOK && r.freqPresent && r.freqHz > 0 {
		return r.freqHz, true
	}
	return 0, false
}

// FreqHz is the receive frequency matching the burned-in FREQ field — the
// tracked satellite's downlink, else the radio's frequency; ok=false when the
// overlay shows "FREQ ---". Names recordings.
func (o *Overlay) FreqHz() (int64, bool) {
	staleAfter := time.Duration(o.cfgFn().StaleAfterS * float64(time.Second))
	now := o.now()
	o.mu.Lock()
	radio, radUp, sat, satUp, up := o.radio, o.radUp, o.sat, o.satUp, o.up
	o.mu.Unlock()
	if satTracked(up, satUp, sat, now, staleAfter) && positive(sat.downlinkHz) {
		return *sat.downlinkHz, true
	}
	radioOK := up && radUp && radio.online && now.Sub(radio.at) <= staleAfter
	return freshFreq(radioOK, radio)
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
		{cfg.TopicSat, o.stateHandler()},
		{statusOf(cfg.TopicAZ), o.statusHandler()},
		{statusOf(cfg.TopicEL), o.statusHandler()},
		{statusOf(cfg.TopicRadio), o.statusHandler()},
		{statusOf(cfg.TopicSat), o.statusHandler()},
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
	azUp, elUp, radUp, satUp := o.azUp, o.elUp, o.radUp, o.satUp
	az, el, radio, sat := o.az, o.el, o.radio, o.sat
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
		"sat_stale":      !satFresh(up, satUp, sat, now, staleAfter),
	})
	if err != nil {
		return
	}
	topic := fmt.Sprintf("%s/%s/%s/state", cfg.Site, cfg.Station, cfg.Slot)
	client.Publish(topic, 0, true, payload) // async: writer goroutine must not block
}
