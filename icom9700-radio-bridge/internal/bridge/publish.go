package bridge

// Publishing (dedup + freshness heartbeat, the spid KTD14 pattern) and the
// CI-V frame router: solicited poll replies and radio-initiated transceive
// broadcasts both arrive through OnCIVFrame and fold into the radio cache.
//
// Routing rests on the command table: 00/01 (freq/mode) and 1C 00 (PTT)
// frames are transceive broadcasts — the radio pushes them unsolicited for
// MAIN and SUB changes alike, and the same frame shapes answer a solicited
// 1C 00 read, so ParseTransceive covers both. Every other command byte is a
// solicited read/set reply this bridge issued (poll reads, cmd sets), applied
// to the VFO the radio had selected when the read went out (tracked from the
// 07 D2 answer that leads every poll tick — freq/mode/data-mode belong to
// the SELECTED band).

import (
	"encoding/json"
	"log/slog"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"icom9700-radio-bridge/internal/civ"
	"icom9700-radio-bridge/internal/radio"
)

// stateHeartbeatTicks bounds how stale the retained /state ts may get while
// the snapshot is unchanged: an unchanged slot still republishes every 60
// poll ticks (the m5stamp pol-ctrl 60 s freshness precedent at the shipped
// 1 s poll). Covers the stamped fields while idle too — the ticker drives
// Poll in every session state (R6).
const stateHeartbeatTicks = 60

// nowRFC3339 stamps the publish ts (UTC, the station-model wire convention).
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// publishState folds the current state into the dedup form and publishes the
// retained snapshot on change, edge, or freshness heartbeat. Runs on the
// jobs worker only. force bypasses the dedup (reconnect restore).
func (b *Bridge) publishState(force bool) {
	b.mu.Lock()
	sessSnap := b.sess.Snapshot()
	live := sessSnap.State == radio.StateLive
	snap := b.buildSnapshot(sessSnap, live)
	cl := b.client
	if !force && b.hasLast && b.last.equal(snap) && time.Since(b.lastPub) < b.heartbeat() {
		b.mu.Unlock()
		return
	}
	b.last = snap
	b.hasLast = true
	b.lastPub = time.Now()
	p := statePayloadOf(snap)
	b.mu.Unlock()

	if cl == nil {
		return // not attached yet: the OnMQTTConnect ritual publishes first
	}
	publishJSON(b.log, cl, topicState(b.opts), p)
}

// heartbeat is the freshness window in wall-clock terms (60 poll ticks, so a
// slower configured poll stretches it — never a per-tick firehose).
func (b *Bridge) heartbeat() time.Duration {
	return stateHeartbeatTicks * b.opts.PollInterval
}

// resetDedup makes the next publishState republish regardless of change
// (reconnect restore).
func (b *Bridge) resetDedup() {
	b.mu.Lock()
	b.hasLast = false
	b.mu.Unlock()
}

// --- the poll read cycle ----------------------------------------------------

// sendPollReads sends one tick's CI-V reads on the live transport. It only
// SENDS — replies route asynchronously through onCIVFrame (a poll that
// blocked per frame would freeze the session's manager goroutine). The first
// tick after a connect (or after an ambiguous transceive) upgrades to the
// full two-VFO read so the non-selected band's cache is authoritative too.
//
// NOTE (idle-policy seam): telemetry rides Session.Do, which re-arms the
// session's idle window — while live, polls hold the session open. The strict
// R1 idle semantics (meters are not work) need a no-reset telemetry path in
// the session stack; the frozen radio.Session API has none, so v1 holds the
// session while telemetry ticks and U6's idle policy owns the tradeoff.
func (b *Bridge) sendPollReads(tr *civ.Transport) error {
	if !tr.Live() {
		return nil // session died between Live() and here: never a connect demand
	}
	b.mu.Lock()
	full := b.recon
	b.recon = false
	sel := b.radio.selected
	b.mu.Unlock()

	c := b.codec
	// The leading 07 D2 read scopes everything after it (03/04/1A 06/14 0A).
	frames := [][]byte{c.BuildReadSelectedVFO(), c.BuildReadFreq(), c.BuildReadMode(), c.BuildReadDataMode()}
	if full {
		// Reconcile the OTHER VFO too: select it, re-read the selection
		// (the scoping gate — reply-side, see sendFrames), read, select back
		// and re-affirm. The momentary flip is why this is not the default.
		other := civ.VfoMain
		if sel == civ.VfoMain {
			other = civ.VfoSub
		}
		frames = append(frames,
			c.BuildSelectVFO(other), c.BuildReadSelectedVFO(), c.BuildReadFreq(), c.BuildReadMode(), c.BuildReadDataMode(),
			c.BuildSelectVFO(sel), c.BuildReadSelectedVFO())
	}
	frames = append(frames,
		c.BuildReadSatellite(),
		c.BuildReadMeter(civ.MeterS), c.BuildReadMeter(civ.MeterSWR), c.BuildReadMeter(civ.MeterALC),
		c.BuildReadPTT(),
	)
	// RF power (14 0A) applies to the currently selected band's VFO; the read
	// lands attributed to the selection the leading 07 D2 answer named.
	frames = append(frames, buildReadPower())
	return sendFrames(tr, frames)
}

// sendFrames writes a frame sequence. Selection tracking is deliberately
// REPLY-side only: the router follows the solicited 07 D2 read answers (and
// every sequence that flips the selection brackets itself with 07 D2 reads —
// see vfoScopedFrames and sendPollReads), because in-order CI-V delivery
// guarantees each read answer lands before the replies it scopes. A send-time
// update cannot guarantee that ordering: the radio's answer to frame N+1 can
// reach the worker before a marker enqueued after frame N left.
func sendFrames(tr *civ.Transport, frames [][]byte) error {
	for _, f := range frames {
		if err := tr.SendCIV(f); err != nil {
			return err
		}
	}
	return nil
}

// buildReadPower assembles the 14 0A read (no data area = read). The frozen
// civ.Codec has no builder for it (BuildSetPower only), and the package is
// closed to additions, so the six framing bytes are composed here from the
// exported constants — the exact FE FE A2 E0 14 0A FD shape the reply specs
// in commands.go accept as the read form.
func buildReadPower() []byte {
	return []byte{civ.Preamble, civ.Preamble, civ.AddrRadio, civ.AddrCtrl, civ.CmdRFPower, civ.PowerRF, civ.Postamble}
}

// --- inbound frames ----------------------------------------------------------

// onCIVFrame routes one parsed radio frame. Runs on the jobs worker (the
// hook only enqueued the bytes).
func (b *Bridge) onCIVFrame(chunk []byte) {
	rep, err := b.codec.Parse(chunk)
	if err != nil {
		b.log.Debug("dropped unparseable CI-V frame", "err", err)
		return
	}
	switch rep.Kind {
	case civ.KindData:
		switch rep.Cmd {
		case civ.CmdFreqTransceive, civ.CmdModeTransceive, civ.CmdStatus:
			b.applyTransceive(rep)
		default:
			b.applyReply(rep)
		}
	case civ.KindNG:
		// The FA frame carries no echo of the rejected command (commands.go):
		// surface the observed fact — the R10 taxonomy keeps the local
		// validation rejections exact, the radio's own refusals are facts.
		b.setCmdErr("radio rejected: " + civ.ErrNG.Error())
		b.publishState(false)
	case civ.KindAck:
		// FB: the set went in; the readback loop (poll tick / follow-up read)
		// is the confirmation plane. Nothing to publish from an ack alone.
	}
}

// applyTransceive folds a freq/mode/PTT broadcast (or a solicited 1C 00 read
// reply — the same parse) into the cache and republishes.
func (b *Bridge) applyTransceive(rep civ.Reply) {
	t, err := civ.ParseTransceive(rep)
	if err != nil {
		return
	}
	b.mu.Lock()
	switch t.Kind {
	case civ.TransceivePTT:
		b.radio.tx = t.PTTOn
	case civ.TransceiveFreq:
		v := &b.radio.vfo[b.radio.selected]
		ambiguous := v.hasFreq && v.freqHz != t.FreqHz
		setVFOFreq(v, t)
		if ambiguous {
			// The broadcast may have been the NON-selected band (the wire
			// does not attribute 00/01 frames): upgrade the next tick to the
			// full two-VFO read (R6 reconciliation).
			b.recon = true
		}
	case civ.TransceiveMode:
		v := &b.radio.vfo[b.radio.selected]
		if v.modeOK && v.mode != t.Mode {
			b.recon = true
		}
		setVFOMode(v, t)
	}
	b.mu.Unlock()
	b.publishState(false)
}

// setVFOFreq applies a frequency truth to one VFO (band derived, R7).
func setVFOFreq(v *vfoCache, t civ.Transceive) {
	v.hasFreq = true
	v.freqHz = t.FreqHz
	v.band = ""
	if t.BandOK {
		v.band = t.Band
	}
}

// setVFOMode applies a mode truth; a mode the bus never publishes raw (RTTY/
// DV/DD) omits the field instead of carrying a lookalike (R7).
func setVFOMode(v *vfoCache, t civ.Transceive) {
	v.mode, v.modeOK = t.Mode, t.ModeOK
}

// applyReply folds a solicited read/set reply into the cache. Freq/mode/
// data-mode reads belong to the band that was selected when they were
// issued; the 07 D2 answer updates that tracking first (in-order delivery).
func (b *Bridge) applyReply(rep civ.Reply) {
	published := b.applyReplyLocked(rep)
	if published {
		b.publishState(false)
	}
}

// applyReplyLocked folds one solicited reply; reports whether anything was
// published. Locking discipline: takes b.mu itself and releases it before
// returning so the caller can publish without holding it.
func (b *Bridge) applyReplyLocked(rep civ.Reply) bool {
	b.mu.Lock()
	switch {
	case rep.Cmd == civ.CmdSelectBand:
		v, err := rep.SelectedVFO()
		if err == nil {
			b.radio.selected = v
			b.mu.Unlock()
			return true
		}
	case rep.Cmd == civ.CmdReadFreq:
		hz, err := rep.FreqHz()
		if err == nil {
			v := &b.radio.vfo[b.radio.selected]
			bandName, bandOK := civ.BandForFreq(hz)
			setVFOFreq(v, civ.Transceive{FreqHz: hz, Band: bandName, BandOK: bandOK})
			b.mu.Unlock()
			return true
		}
	case rep.Cmd == civ.CmdReadMode:
		mode, _, ok, err := rep.Mode()
		if err == nil {
			setVFOMode(&b.radio.vfo[b.radio.selected], civ.Transceive{Mode: mode, ModeOK: ok})
			b.mu.Unlock()
			return true
		}
	case rep.Cmd == civ.CmdSetItem && rep.Sub == civ.SetItemDataMode:
		on, _, err := rep.DataMode()
		if err == nil {
			v := &b.radio.vfo[b.radio.selected]
			v.hasData, v.dataMode = true, on
			b.mu.Unlock()
			return true
		}
	case rep.Cmd == civ.CmdFunction && rep.Sub == civ.FuncSatellite:
		on, err := rep.OnOff()
		if err == nil {
			b.radio.satellite = on
			b.mu.Unlock()
			return true
		}
	case rep.Cmd == civ.CmdReadMeter:
		lvl, err := rep.MeterLevel()
		if err == nil {
			m := b.radio.meters
			switch rep.Sub {
			case civ.MeterSubS:
				m.sOK, m.s = true, lvl
			case civ.MeterSubSWR:
				m.swrOK, m.swr = true, lvl
			case civ.MeterSubALC:
				m.alcOK, m.alc = true, lvl
			}
			b.radio.meters = m
			b.mu.Unlock()
			return true
		}
	case rep.Cmd == civ.CmdRFPower:
		lvl, err := rep.PowerLevel()
		if err == nil {
			p := int(lvl)
			b.radio.vfo[b.radio.selected].power = &p
			b.mu.Unlock()
			return true
		}
	}
	b.mu.Unlock()
	return false
}

// --- publish helpers ----------------------------------------------------------

// publishJSON marshals and publishes retained QoS 1 (the station-model
// planes). Runs only on the jobs worker, the poll tick or the connect
// ritual — never inside a paho message handler.
func publishJSON(log *slog.Logger, cl paho.Client, topic string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Error("marshal publish payload", "topic", topic, "err", err)
		return
	}
	if tok := cl.Publish(topic, 1, true, data); tok.Wait() && tok.Error() != nil {
		log.Error("publish failed", "topic", topic, "err", tok.Error())
	}
}
