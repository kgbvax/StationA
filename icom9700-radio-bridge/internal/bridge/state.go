package bridge

import (
	"context"
	"strings"
	"time"

	"icom9700-radio-bridge/internal/civ"
	"icom9700-radio-bridge/internal/radio"
)

// stateHeartbeatPolls bounds how stale the retained /state ts may get while
// the snapshot is unchanged (the KTD14 always-fresh-ts rule, the pol-ctrl
// 60 s precedent).
const stateHeartbeatPolls = 60

// vfoState is one VFO's detail object in the hybrid /state shape (R5).
type vfoState struct {
	Band       string `json:"band"`
	FreqHz     uint64 `json:"freq_hz"`
	Mode       string `json:"mode"`
	DataMode   bool   `json:"data_mode"`
	Preamp     bool   `json:"preamp"`
	Attenuator bool   `json:"attenuator"`
}

// radioState is the cached radio truth, refreshed by the poll (while live)
// and folded forward by transceive events. Zero value = never read: the
// /state assembly omits radio-measured fields until a live poll lands
// (R6 — never zeroed, never frozen).
type radioState struct {
	selectedVFO string // bridge-held: survives off-session (R6)
	satellite   bool
	main        vfoState
	sub         vfoState
	tx          string // "tx"|"rx"
	sMeter      *int
	txPower     *int
	swr         *int
	alc         *int
	readAt      time.Time // last successful live poll
	// responding: the radio answered CI-V on the latest poll (or a
	// transceive arrived). False while the session is live but deaf — the
	// standby signature (2026-09-21 preview indicators). Always present in
	// /state while live; radio-measured fields are omitted while false.
	responding bool
}

// snap is the comparable /state snapshot: dedup compares this, never the
// serialized form. ts is stamped at publish time.
type snap struct {
	session     string
	armed       bool
	audioDemand bool
	err         string
	radio       radioState
}

// poll reads the radio over the live session (one Demand, several
// round-trips): selected VFO, satellite mode, both VFOs' freq+mode, PTT,
// meters. Runs on the poll tick; never called off-session.
func (b *Bridge) poll() {
	// KTD-2/R2: the poll is not itself a demand. Telemetry only flows
	// while a session is already live (arm, a /cmd, or the safety
	// PTT-off delivery opened it); from idle or error the poll must
	// return without dialing — a demanding poll would grab the radio's
	// single LAN session around the clock and starve manual wfview.
	if b.mgr.Snapshot().SessionState != radio.StateLive {
		return
	}
	// The poll's own bound is generous (a dozen sequential round-trips on
	// a LAN session); the TICK is the cadence, not the deadline. A poll
	// that outlives its ctx is abandoned — the next tick re-reads. Ride,
	// never Demand: telemetry is a free rider on the live session and must
	// not restart its idle clock (KTD-2 — a demanding poll would keep the
	// radio's session open around the clock and starve manual wfview).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := b.mgr.Session().Ride(ctx, func(c *civ.Client) error {
		rs := radioState{}
		wait := b.opts.PollInterval
		answered := 0

		// rt counts answered round-trips: an err==nil frame means the radio
		// executed CI-V (FB acknowledge included) — the responsiveness fact
		// the /state assembly publishes. Zero answers = the radio is deaf
		// (standby): the cached radio state stays completely untouched.
		rt := func(frame []byte) (civ.Frame, error) {
			f, err := b.mgr.Session().RoundTrip(ctx, c, frame, wait)
			if err == nil {
				answered++
			}
			return f, err
		}

		// Real firmware repeats the sub bytes in replies to sub-command
		// queries (FE FE E0 A2 15 02 <hi> <lo> FD) — stripSub drops that
		// echo. Single-command reads (03/04) have no echo.
		stripSub := func(f civ.Frame) []byte {
			if len(f.Sub) > 1 {
				return f.Sub[1:]
			}
			return f.Sub
		}

		if f, err := rt(civ.CmdReadSelectedVFO()); err == nil && len(f.Sub) >= 1 {
			if v, err := civ.ParseVFOReply(stripSub(f)); err == nil {
				rs.selectedVFO = v
			}
		}
		if f, err := rt(civ.CmdReadSatelliteMode()); err == nil && len(f.Sub) >= 1 {
			rs.satellite, _ = civ.ParseSatelliteReply(stripSub(f))
		}
		rs.main = b.pollVFO(ctx, c, "main", wait, rt)
		rs.sub = b.pollVFO(ctx, c, "sub", wait, rt)

		// PTT read: `1C 00` with no data.
		if f, err := rt(civ.BuildFrame(0x1C, []byte{0x00})); err == nil && len(f.Sub) >= 1 {
			if sub := stripSub(f); len(sub) == 1 {
				if sub[0] == 0x01 {
					rs.tx = "tx"
				} else {
					rs.tx = "rx"
				}
			}
		}

		if f, err := rt(civ.CmdReadSMeter()); err == nil && len(f.Sub) >= 1 {
			v, err := civ.ParseMeter(stripSub(f))
			if err == nil {
				rs.sMeter = &v
			}
		}
		if f, err := rt(civ.CmdReadSWR()); err == nil && len(f.Sub) >= 1 {
			v, err := civ.ParseMeter(stripSub(f))
			if err == nil {
				rs.swr = &v
			}
		}
		if f, err := rt(civ.CmdReadALC()); err == nil && len(f.Sub) >= 1 {
			v, err := civ.ParseMeter(stripSub(f))
			if err == nil {
				rs.alc = &v
			}
		}

		b.mu.Lock()
		if answered > 0 {
			// Bridge-held fields survive a failed sub-read.
			if rs.selectedVFO != "" {
				b.radio.selectedVFO = rs.selectedVFO
			}
			rs.selectedVFO = b.radio.selectedVFO
			b.radio.merge(rs)
		}
		// A zero-answer poll leaves the cached radio state completely
		// untouched — merge would fabricate satellite=false, clobber the
		// meters with nils and stamp readAt (defeating the /state dedup).
		b.setResponding(answered > 0)
		b.mu.Unlock()
		return nil
	})
	if err != nil {
		b.log.Warn("poll demand failed", "err", err)
	}
}

// deafTicksToClear is the hysteresis before the standby signature flips
// radio_responding off: a single CI-V collision while operating must not
// flash "FREQ ---" into the video burn-in. Documented in docs/mqtt-api.md.
const deafTicksToClear = 2

// setResponding records whether the latest poll got any CI-V answer. An
// answer sets the flag immediately; deafTicksToClear consecutive deaf polls
// clear it (and rate-limited-warn the standby signature). Callers hold b.mu.
func (b *Bridge) setResponding(ok bool) {
	if ok {
		b.radio.responding = true
		b.deafStreak = 0
		return
	}
	b.deafStreak++
	if b.deafStreak >= deafTicksToClear {
		if b.radio.responding {
			b.radio.responding = false
		}
		if time.Since(b.deafWarnAt) >= time.Minute {
			b.deafWarnAt = time.Now()
			b.log.Warn("ci-v polls unanswered (radio in standby?)")
		}
	}
}

// pollVFO reads one VFO's freq and mode (select, read, read). rt is the
// caller's round-trip wrapper so answer counting covers these trips too.
func (b *Bridge) pollVFO(ctx context.Context, c *civ.Client, vfo string, wait time.Duration, rt func([]byte) (civ.Frame, error)) vfoState {
	vs := vfoState{}
	sel, err := civ.CmdSelectVFO(vfo)
	if err != nil {
		return vs
	}
	if _, err := rt(sel); err != nil {
		return vs
	}
	if f, err := rt(civ.CmdReadFreq()); err == nil {
		if hz, err := civ.ParseFreqReply(f.Sub); err == nil {
			vs.FreqHz = hz
			vs.Band, _ = civ.BandForFreq(hz)
		}
	}
	if f, err := rt(civ.CmdReadMode()); err == nil {
		if mode, _, ok, _ := civ.ParseModeReply(f.Sub); ok {
			vs.Mode = mode
		}
	}
	return vs
}

// merge folds a poll result over the cached state; fields the poll failed
// to read keep their previous values ONLY while the session stays live —
// the /state assembly omits everything when the session is down (R6).
func (r *radioState) merge(n radioState) {
	if n.selectedVFO != "" {
		r.selectedVFO = n.selectedVFO
	}
	r.satellite = n.satellite
	if n.main.FreqHz != 0 {
		r.main = n.main
	}
	if n.sub.FreqHz != 0 {
		r.sub = n.sub
	}
	if n.tx != "" {
		r.tx = n.tx
	}
	r.sMeter, r.txPower, r.swr, r.alc = n.sMeter, n.txPower, n.swr, n.alc
	r.readAt = time.Now()
}

// foldTransceive applies one inbound transceive event to the cached state
// (attributed per the satellite semantics: SUB is the uplink/TX VFO while
// satellite mode is on, KTD-10).
func (b *Bridge) foldTransceive(tr civ.Transceive) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.snapSession() != radio.StateLive {
		return // stale events off-session are not state
	}
	// An unsolicited transceive frame is direct proof the radio executes
	// CI-V — same evidence as an answered poll.
	b.radio.responding = true
	b.deafStreak = 0
	vfo := b.radio.selectedVFO
	if b.radio.satellite {
		vfo = "sub"
	}
	switch tr.Kind {
	case "freq":
		b.radio.touchVFO(vfo)
		b.radio.vfo(vfo).FreqHz = tr.FreqHz
		if band, err := civ.BandForFreq(tr.FreqHz); err == nil {
			b.radio.vfo(vfo).Band = band
		}
	case "mode":
		b.radio.touchVFO(vfo)
		b.radio.vfo(vfo).Mode = tr.Mode
	case "tx":
		b.radio.tx = tr.TX
	}
}

// snapshot builds the comparable /state snapshot from the manager's
// lifecycle snapshot and the cached radio state, refreshing the cached
// session state the transceive folder reads.
func (b *Bridge) snapshot() snap {
	ss := b.mgr.Snapshot()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessionState = ss.SessionState
	// The last /cmd rejection outranks the session's own error (the spid
	// slot's merge order); both clear independently.
	err := ss.Err
	if b.cmdErr != "" {
		err = b.cmdErr
	}
	return snap{
		session:     ss.SessionState,
		armed:       b.armed,
		audioDemand: ss.AudioDemand,
		err:         err,
		radio:       b.radio,
	}
}

// snapSession reads the last-seen session state under the state mutex
// (foldTransceive holds that mutex when it asks).
func (b *Bridge) snapSession() string { return b.sessionState }

// ---------------------------------------------------------------------------
// /state assembly + publish
// ---------------------------------------------------------------------------

// publishState dedups the snapshot (force bypasses, for the reconnect
// restore) and publishes the retained JSON with a fresh RFC3339 ts. The
// freshness heartbeat republishes an unchanged snapshot every 60 poll
// ticks (KTD14).
func (b *Bridge) publishState(force bool) {
	sn := b.snapshot()
	b.mu.Lock()
	if !force && b.hasLast && b.last == sn && time.Since(b.lastPub) < stateHeartbeatPolls*b.opts.PollInterval {
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

// statePayload is the hybrid wire shape (R5) with the per-state payload
// rules (R6): off-session the radio-measured fields are OMITTED (never
// zeroed, never frozen) while the stamped bridge-held fields remain —
// `device_online` is CI-V control-session liveness (healthy idle = false,
// R16), `session_state` is the idle-vs-fault discriminator.
func (b *Bridge) statePayload(sn snap) map[string]any {
	live := sn.session == radio.StateLive
	p := map[string]any{
		"ts":            time.Now().UTC().Format(time.RFC3339),
		"session_state": sn.session,
		"armed":         sn.armed,
		"audio_demand":  sn.audioDemand,
		"device_online": live,
	}
	// radio_responding is NOT device_online (that is CI-V control-session
	// liveness): it says the radio answered CI-V on the latest poll — false
	// while the session is live but deaf (standby). Present while live,
	// omitted off-session with the other radio-measured fields.
	if live {
		p["radio_responding"] = sn.radio.responding
	}
	if sn.err != "" {
		p["error"] = sn.err
	}
	if sn.radio.selectedVFO != "" {
		p["selected_vfo"] = sn.radio.selectedVFO
	}
	if !live {
		return p
	}
	// Radio-measured fields are OMITTED while the radio is not answering
	// (live-but-deaf: standby) exactly as off-session — never zeroed, never
	// frozen (R6, extended 2026-09-21 for the preview indicators).
	// selected_vfo and tx stay above/below the gate on purpose: selected_vfo
	// is bridge-held, and tx is the safety mirror — an operator must see a
	// keyed transmitter even while the radio is otherwise deaf.
	if !sn.radio.responding {
		if sn.radio.tx != "" {
			p["tx"] = sn.radio.tx
		}
		return p
	}
	// Top-level active-TX fields mirror the TX VFO: SUB in satellite mode
	// (SUB is the uplink; the inversion gpredict #181 documents), MAIN
	// otherwise (KTD-10).
	txVFO := sn.radio.selectedVFO
	if sn.radio.satellite {
		txVFO = "sub"
	}
	vs := sn.radio.vfo(txVFO)
	if vs.FreqHz != 0 {
		p["freq_hz"] = vs.FreqHz
		if vs.Band != "" {
			p["band"] = vs.Band
		}
	}
	if vs.Mode != "" {
		p["mode"] = vs.Mode
	}
	if sn.radio.tx != "" {
		p["tx"] = sn.radio.tx
	}
	if sn.radio.main.FreqHz != 0 {
		p["main"] = sn.radio.main
	}
	if sn.radio.sub.FreqHz != 0 {
		p["sub"] = sn.radio.sub
	}
	p["satellite"] = sn.radio.satellite
	if sn.radio.sMeter != nil {
		p["s_meter"] = *sn.radio.sMeter
	}
	if sn.radio.txPower != nil {
		p["tx_power"] = *sn.radio.txPower
	}
	if sn.radio.swr != nil {
		p["swr"] = *sn.radio.swr
	}
	if sn.radio.alc != nil {
		p["alc"] = *sn.radio.alc
	}
	return p
}

// ---------------------------------------------------------------------------
// /meta birth certificate
// ---------------------------------------------------------------------------

// metaPayload builds the retained birth certificate. The expose is
// READ-ONLY (v1 posture: freq + mode as sensors, tx as an on/off boolean —
// no PTT/arm widgets in HA; the action set lives in docs/mqtt-api.md and an
// expose actions[] is additive later without breaking anything, R8).
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
			"bands":     []string{"2m", "70cm", "23cm"},
			"modes":     []string{"cw", "usb", "lsb", "am", "fm", "data"},
			"bias_t":    true,
			"satellite": true,
			"vfos":      []string{"main", "sub"},
		},
		"expose": map[string]any{
			"device": map[string]any{
				"name":  b.opts.Slot,
				"model": model,
			},
			"fields": []map[string]any{
				{"key": "freq_hz", "name": "Frequency", "type": "number", "unit": "Hz", "state_class": "measurement"},
				{"key": "mode", "name": "Mode", "type": "enum", "options_ref": "capabilities.modes"},
				{"key": "tx", "name": "Transmitting", "type": "boolean", "on": "tx", "off": "rx"},
				{"key": "s_meter", "name": "S-meter", "type": "number", "state_class": "measurement"},
				{"key": "armed", "name": "TX armed", "type": "boolean"},
				{"key": "audio_demand", "name": "Audio demand", "type": "boolean"},
				{"key": "radio_responding", "name": "Radio responding", "type": "boolean"},
				{"key": "session_state", "name": "Session state", "type": "string"},
				{"key": "error", "name": "Last error", "type": "string"},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// cached-radio-state helpers
// ---------------------------------------------------------------------------

func (r *radioState) vfo(name string) *vfoState {
	if name == "main" {
		return &r.main
	}
	return &r.sub
}

func (r *radioState) touchVFO(name string) {
	v := r.vfo(name)
	if v.FreqHz == 0 {
		// First sight of this VFO without a poll: mark the band unknown
		// until the next poll fills it in.
		v.Band = strings.ToLower(v.Band)
	}
}
