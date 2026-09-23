package bridge

import (
	"time"

	"icom9700-radio-bridge/internal/radio"
)

// snap is the comparable /state snapshot: dedup compares this, never the
// serialized form. ts is stamped at publish time. The radio telemetry is
// the serial monitor's snapshot, folded in at assembly time.
type snap struct {
	session     string
	audioDemand bool
	monitor     bool
	err         string
	radio       RadioState
	responding  bool // only meaningful (and published) while monitor is on
	hasRadio    bool // any telemetry observed since monitor on
}

// snapshot builds the comparable /state snapshot from the manager's
// lifecycle snapshot and the serial monitor's telemetry.
func (b *Bridge) snapshot() snap {
	ss := b.mgr.Snapshot()
	b.mu.Lock()
	defer b.mu.Unlock()
	// The last /cmd rejection outranks the session's own error (the spid
	// slot's merge order); both clear independently.
	err := ss.Err
	if b.cmdErr != "" {
		err = b.cmdErr
	}
	sn := snap{
		session:     ss.SessionState,
		audioDemand: ss.AudioDemand,
		monitor:     b.monitorOn,
		err:         err,
	}
	if b.opts.Monitor != nil && b.monitorOn {
		sn.radio = b.opts.Monitor.Snapshot()
		sn.responding = sn.radio.Responding
		sn.hasRadio = sn.radio.FreqHz != 0 || sn.radio.Mode != "" ||
			sn.radio.SMeter != nil || sn.radio.SWR != nil || sn.radio.ALC != nil ||
			sn.radio.Satellite
	}
	return sn
}

// ---------------------------------------------------------------------------
// /state assembly + publish
// ---------------------------------------------------------------------------

// publishState dedups the snapshot (force bypasses, for the reconnect
// restore) and publishes the retained JSON with a fresh RFC3339 ts. The
// freshness heartbeat republishes an unchanged snapshot every 60 s
// (KTD14); telemetry/demand changes republish out-of-band on change.
func (b *Bridge) publishState(force bool) {
	sn := b.snapshot()
	b.mu.Lock()
	if !force && b.hasLast && b.last == sn && time.Since(b.lastPub) < stateHeartbeat {
		b.mu.Unlock()
		return
	}
	b.last = sn
	b.hasLast = true
	b.lastPub = time.Now()
	b.mu.Unlock()
	b.cli.publish(b.stateTopic, 1, true, mustJSON(b.statePayload(sn)))
}

func (b *Bridge) resetDedup() {
	b.mu.Lock()
	b.hasLast = false
	b.mu.Unlock()
}

// statePayload is the receive-only wire shape: capture-session state and
// the demand flags always; the radio-measured fields only while the serial
// monitor is on, and only while the radio is actually answering — never
// zeroed, never frozen (R6, extended 2026-09: the monitor owns telemetry,
// LAN polls no longer exist). `device_online` is CAPTURE liveness
// (session_state == live; healthy idle reads false, R16).
func (b *Bridge) statePayload(sn snap) map[string]any {
	live := sn.session == radio.StateLive
	p := map[string]any{
		"ts":            time.Now().UTC().Format(time.RFC3339),
		"session_state": sn.session,
		"audio_demand":  sn.audioDemand,
		"monitor":       sn.monitor,
		"device_online": live,
	}
	if sn.err != "" {
		p["error"] = sn.err
	}
	if !sn.monitor {
		return p
	}
	// Monitor-gated telemetry. radio_responding says the radio answers
	// serial CI-V — false while the monitor is on but the radio is deaf
	// (standby) or the serial port is down; the measured fields are
	// omitted while false (never zeroed, never frozen).
	p["radio_responding"] = sn.responding
	if !sn.responding || !sn.hasRadio {
		return p
	}
	if sn.radio.FreqHz != 0 {
		p["freq_hz"] = sn.radio.FreqHz
		if sn.radio.Band != "" {
			p["band"] = sn.radio.Band
		}
	}
	if sn.radio.Mode != "" {
		p["mode"] = sn.radio.Mode
	}
	p["satellite"] = sn.radio.Satellite
	if sn.radio.SMeter != nil {
		p["s_meter"] = *sn.radio.SMeter
	}
	if sn.radio.TXPower != nil {
		p["tx_power"] = *sn.radio.TXPower
	}
	if sn.radio.SWR != nil {
		p["swr"] = *sn.radio.SWR
	}
	if sn.radio.ALC != nil {
		p["alc"] = *sn.radio.ALC
	}
	return p
}

// ---------------------------------------------------------------------------
// /meta birth certificate
// ---------------------------------------------------------------------------

// metaPayload builds the retained birth certificate. The expose is
// READ-ONLY and control-free (2026-09 pivot: no tx, no armed — the radio
// is monitored and its receive audio captured, nothing else).
func (b *Bridge) metaPayload() map[string]any {
	model := b.opts.DeviceModel
	if model == "" {
		model = "Icom IC-9700"
	}
	return map[string]any{
		"schema": "1.0",
		"role":   "radio",
		"device": map[string]any{
			"model": model,
		},
		"location": b.opts.Location,
		"host":     b.opts.Host,
		"capabilities": map[string]any{
			"bands": []string{"2m", "70cm", "23cm"},
			"modes": []string{"cw", "usb", "lsb", "am", "fm", "data"},
		},
		"expose": map[string]any{
			"device": map[string]any{
				"name":  b.opts.Slot,
				"model": model,
			},
			"fields": []map[string]any{
				{"key": "freq_hz", "name": "Frequency", "type": "number", "unit": "Hz", "state_class": "measurement"},
				{"key": "mode", "name": "Mode", "type": "enum", "options_ref": "capabilities.modes"},
				{"key": "s_meter", "name": "S-meter", "type": "number", "state_class": "measurement"},
				{"key": "swr", "name": "SWR", "type": "number", "state_class": "measurement"},
				{"key": "alc", "name": "ALC", "type": "number", "state_class": "measurement"},
				{"key": "audio_demand", "name": "Audio capture", "type": "boolean"},
				{"key": "monitor", "name": "Serial monitor", "type": "boolean"},
				{"key": "radio_responding", "name": "Radio responding", "type": "boolean"},
				{"key": "session_state", "name": "Capture session", "type": "string"},
				{"key": "error", "name": "Last error", "type": "string"},
			},
		},
	}
}
