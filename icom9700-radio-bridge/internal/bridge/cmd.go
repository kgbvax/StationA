package bridge

// The one-shot /cmd dispatch (plan U5, R9/R10, KTD-6): size gate before
// parse, ts staleness gate with unstamped tolerance, clear-after-execute-
// or-reject with the empty-payload echo guard, and the 200-rune rejection
// clip at the single choke point (the spid-ercm mqttslot template). Every
// rejection is an observed fact in /state.error — the exact-string R10
// taxonomy for the gated actions, clipped producer text only where the plan
// leaves the wording open.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"icom9700-radio-bridge/internal/civ"
)

// cmdMaxAge bounds how old a stamped /cmd may be. Unstamped payloads are
// tolerated (the QoS-0 subscription + clear-after defenses cover them).
const cmdMaxAge = 30 * time.Second

// cmdMaxBytes bounds the /cmd payload size, gated BEFORE Unmarshal: an
// oversized payload is never parsed, logged or echoed — the rejection is the
// fixed string below, so no producer-chosen bytes can reach the retained
// /state.error (the spid OOM vector). 4 KB is an order of magnitude above
// the largest legitimate cmd.
const cmdMaxBytes = 4 * 1024

// cmdErrMsgTooLarge is the oversized-payload rejection: a FIXED string.
const cmdErrMsgTooLarge = "cmd payload too large"

// cmdErrMax bounds every rejection message surfaced through /state.error.
const cmdErrMax = 200

// doTimeout bounds one action's Session.Do round-trip (connect series wait
// included) so a wedged radio cannot hold the single jobs worker forever.
// Generous: an arm-driven series can legitimately span MaxAttempts x
// AttemptSpacing; the arm cmd itself uses opts.ArmTimeout (wider).
const doTimeout = 30 * time.Second

// cmdMsg is the local /cmd payload: the shared Action/Value convention plus
// the optional vfo selector (main|sub, default selected) and the optional
// producer ts (RFC3339).
type cmdMsg struct {
	Action string `json:"action"`
	Value  string `json:"value,omitempty"`
	Vfo    string `json:"vfo,omitempty"`
	Ts     string `json:"ts,omitempty"`
}

// Execute applies one /cmd payload. Runs on the jobs worker (main's paho
// handler enqueues; nothing here may block a paho goroutine — note the arm
// path legitimately blocks the WORKER on the connect series).
func (b *Bridge) Execute(ctx context.Context, payload []byte) error {
	if len(payload) == 0 {
		// The retained-clear marker (our own clear echo). Never re-clear —
		// that would echo another empty payload and loop forever.
		return nil
	}
	if len(payload) > cmdMaxBytes {
		b.log.Warn("rx oversized cmd", "bytes", len(payload), "max", cmdMaxBytes)
		b.reject(cmdErrMsgTooLarge)
		return nil
	}

	var cmd cmdMsg
	if err := json.Unmarshal(payload, &cmd); err != nil || cmd.Action == "" {
		b.log.Error("rx invalid cmd", "payload", clipCmdErr(string(payload)))
		b.reject(fmt.Sprintf("invalid /cmd payload: %q", clipCmdErr(string(payload))))
		return nil
	}

	if cmd.Ts != "" {
		ts, err := time.Parse(time.RFC3339, cmd.Ts)
		if err != nil {
			b.reject(fmt.Sprintf("cmd ts unparseable: %q", cmd.Ts))
			return nil
		}
		if age := time.Since(ts); age > cmdMaxAge || age < -cmdMaxAge {
			b.log.Warn("dropping stale cmd", "action", cmd.Action, "age", age.Round(time.Second))
			b.reject(fmt.Sprintf("stale cmd (age %s, bound %s)", age.Round(time.Second), cmdMaxAge))
			return nil
		}
	}

	b.dispatch(ctx, cmd)
	return nil
}

// dispatch routes one admitted-shape cmd. An unknown action is warn + drop
// (no /state.error — the plan's taxonomy reserves error for observed
// facts); the retained cmd is still cleared (one-shot, everything clears).
func (b *Bridge) dispatch(ctx context.Context, cmd cmdMsg) {
	switch cmd.Action {
	case "set_freq":
		b.cmdSetFreq(ctx, cmd)
	case "set_mode":
		b.cmdSetMode(ctx, cmd)
	case "set_data":
		b.cmdSetData(ctx, cmd)
	case "set_preamp":
		b.cmdSetPreamp(ctx, cmd)
	case "set_attenuator":
		b.cmdSetAttenuator(ctx, cmd)
	case "set_power":
		b.cmdSetPower(ctx, cmd)
	case "sat_mode":
		b.cmdSatMode(ctx)
	case "arm":
		b.cmdArm(ctx, true)
	case "disarm":
		b.cmdArm(ctx, false)
	case "ptt":
		b.cmdPTT(ctx, cmd)
	default:
		b.log.Warn("unknown cmd action — dropped", "action", cmd.Action)
		b.clearCmd()
	}
}

// reject records the rejection in /state.error, republishes and STILL clears
// the retained cmd (execute-or-reject, one-shot).
func (b *Bridge) reject(msg string) {
	b.setCmdErr(clipCmdErr(msg))
	b.publishState(false)
	b.clearCmd()
}

// admit clears the previous rejection (the next admitted intent acks it —
// the spid cmdErr pattern), then dispatches the frame sequence; a send
// failure rejects with the observed fact. Returns the send outcome so
// callers can skip echo-recording on a failed set; the retained /cmd is
// cleared on BOTH paths (execute-or-reject, KTD-6).
func (b *Bridge) admit(ctx context.Context, frames [][]byte) error {
	b.setCmdErr("")
	b.publishState(false)
	if err := b.send(ctx, frames); err != nil {
		b.reject(err.Error())
		return err
	}
	b.clearCmd()
	return nil
}

// send runs the frame sequence through the session. A cmd-driven connect is
// the point: while not live, Do performs the on-demand connect (R2); the
// returned error is the observed failure fact ("radio: login refused", ...).
func (b *Bridge) send(ctx context.Context, frames [][]byte) error {
	dctx, cancel := context.WithTimeout(ctx, doTimeout)
	defer cancel()
	return b.sess.Do(dctx, func(tr *civ.Transport) error {
		// No liveness guard: a transport that died between connect and
		// dispatch makes SendCIV fail, and the error becomes the rejection —
		// a queued cmd is never silently dropped (plan U4 pin).
		return sendFrames(tr, frames)
	})
}

// setCmdErr records the last /cmd rejection (the single choke point — clips
// to cmdErrMax runes so no producer-chosen unbounded string reaches the
// retained publish or the logs).
func (b *Bridge) setCmdErr(msg string) {
	b.mu.Lock()
	b.cmdErr = clipCmdErr(msg)
	b.mu.Unlock()
}

// clearCmd publishes the empty retained payload so nothing re-fires on the
// next (re)connect. Jobs worker only (blocking publish).
func (b *Bridge) clearCmd() {
	b.mu.Lock()
	cl := b.client
	b.mu.Unlock()
	if cl == nil {
		return
	}
	if tok := cl.Publish(topicCmd(b.opts), 1, true, []byte{}); tok.Wait() && tok.Error() != nil {
		b.log.Error("cmd clear failed", "err", tok.Error())
	}
}

func clipCmdErr(msg string) string {
	rs := []rune(msg)
	if len(rs) <= cmdErrMax {
		return msg
	}
	return string(rs[:cmdErrMax]) + "…"
}

// resolveVFO maps the optional vfo selector: "" → the radio's current
// selection; main|sub → that VFO; anything else is an error.
func (b *Bridge) resolveVFO(name string) (civ.VFO, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.radio.selected, nil
	case "main":
		return civ.VfoMain, nil
	case "sub":
		return civ.VfoSub, nil
	default:
		return 0, fmt.Errorf("vfo must be \"main\" or \"sub\", got %q", name)
	}
}

// vfoScopedFrames wraps a per-VFO set in the band-select frames the CI-V
// command scoping needs (05/06/1A 06/16 02/11/14 0A all apply to the
// SELECTED band): select the target, apply, select back — each flip
// BRACKETED by a 07 D2 read so the reply-side router re-scopes before the
// set's own transceive echo lands (see sendFrames for why this must be
// reply-side).
func vfoScopedFrames(c *civ.Codec, target, current civ.VFO, apply [][]byte) [][]byte {
	var frames [][]byte
	if target != current {
		frames = append(frames, c.BuildSelectVFO(target), c.BuildReadSelectedVFO())
	}
	frames = append(frames, apply...)
	if target != current {
		frames = append(frames, c.BuildSelectVFO(current), c.BuildReadSelectedVFO())
	}
	return frames
}

// selectedVFO reads the cached selection.
func (b *Bridge) selectedVFO() civ.VFO {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.radio.selected
}

// --- actions ------------------------------------------------------------------

// cmdSetFreq: per-VFO set. Out-of-vocabulary or SUB-23cm frequencies are
// rejected locally, before any frame exists (the codec validates too — this
// is the exact-string /state.error layer, R10).
func (b *Bridge) cmdSetFreq(ctx context.Context, cmd cmdMsg) {
	vfo, err := b.resolveVFO(cmd.Vfo)
	if err != nil {
		b.reject(fmt.Sprintf("freq rejected: %v", err))
		return
	}
	hz64, err := strconv.ParseUint(strings.TrimSpace(cmd.Value), 10, 32)
	if err != nil {
		b.reject(fmt.Sprintf("freq rejected: invalid value %q", cmd.Value))
		return
	}
	hz := uint32(hz64)
	if err := civ.ValidateFreq(vfo, hz); err != nil {
		b.reject(fmt.Sprintf("freq rejected: out of band for %s", vfo))
		return
	}
	freqFrame, err := b.codec.BuildSetFreq(vfo, hz)
	if err != nil {
		// The codec re-validates; the exact-string rejection above already
		// covers the taxonomy, so this branch is belt-and-braces.
		b.reject(fmt.Sprintf("freq rejected: out of band for %s", vfo))
		return
	}
	b.admit(ctx, vfoScopedFrames(b.codec, vfo, b.selectedVFO(), [][]byte{freqFrame}))
}

// cmdSetMode: canonical operating modes only. "data" is the 1A 06 modifier
// (set_data), and every other name — DV/DD/RTTY included — is unsupported on
// the bus (R7: never published raw, never set).
func (b *Bridge) cmdSetMode(ctx context.Context, cmd cmdMsg) {
	vfo, err := b.resolveVFO(cmd.Vfo)
	if err != nil {
		b.reject(fmt.Sprintf("mode rejected: %v", err))
		return
	}
	mode := strings.ToLower(strings.TrimSpace(cmd.Value))
	if mode == civ.ModeData {
		b.reject(fmt.Sprintf("mode rejected: %q is set via the set_data action", mode))
		return
	}
	frames, err := b.codec.BuildSetMode(mode, civ.FilterOmit)
	if err != nil {
		b.reject(fmt.Sprintf("mode rejected: unsupported %q", cmd.Value))
		return
	}
	b.admit(ctx, vfoScopedFrames(b.codec, vfo, b.selectedVFO(), [][]byte{frames}))
}

// cmdSetData: the 1A 06 data-mode modifier, on|off (on carries FIL1 — the
// filter choice has no bus consumer in v1; documented in mqtt-api.md).
func (b *Bridge) cmdSetData(ctx context.Context, cmd cmdMsg) {
	vfo, err := b.resolveVFO(cmd.Vfo)
	if err != nil {
		b.reject(fmt.Sprintf("data rejected: %v", err))
		return
	}
	on, ok := parseOnOff(cmd.Value)
	if !ok {
		b.reject(fmt.Sprintf("data rejected: invalid value %q (want on|off)", cmd.Value))
		return
	}
	frames, err := b.codec.BuildSetDataMode(on, civ.Filter1)
	if err != nil {
		b.reject(fmt.Sprintf("data rejected: %v", err))
		return
	}
	b.admit(ctx, vfoScopedFrames(b.codec, vfo, b.selectedVFO(), [][]byte{frames}))
}

// cmdSetPreamp: 0-3 (00 both off, 01 P.AMP, 02 EXT-P.AMP, 03 both).
func (b *Bridge) cmdSetPreamp(ctx context.Context, cmd cmdMsg) {
	vfo, err := b.resolveVFO(cmd.Vfo)
	if err != nil {
		b.reject(fmt.Sprintf("preamp rejected: %v", err))
		return
	}
	lvl64, err := strconv.ParseUint(strings.TrimSpace(cmd.Value), 10, 8)
	if err != nil || lvl64 > 3 {
		b.reject(fmt.Sprintf("preamp rejected: invalid value %q (want 0-3)", cmd.Value))
		return
	}
	frames, err := b.codec.BuildSetPreamp(byte(lvl64))
	if err != nil {
		b.reject(fmt.Sprintf("preamp rejected: %v", err))
		return
	}
	lvl := int(lvl64)
	// The echo is recorded only on a delivered set — a failed admit leaves
	// the cache untouched (v1 never polls these fields; a recorded failure
	// would persist as wrong state).
	if err := b.admit(ctx, vfoScopedFrames(b.codec, vfo, b.selectedVFO(), [][]byte{frames})); err != nil {
		return
	}
	b.rememberVFO(vfo, func(v *vfoCache) { v.preamp = &lvl })
}

// cmdSetAttenuator: on|off (11 00 off / 11 10 = 10 dB).
func (b *Bridge) cmdSetAttenuator(ctx context.Context, cmd cmdMsg) {
	vfo, err := b.resolveVFO(cmd.Vfo)
	if err != nil {
		b.reject(fmt.Sprintf("attenuator rejected: %v", err))
		return
	}
	on, ok := parseOnOff(cmd.Value)
	if !ok {
		b.reject(fmt.Sprintf("attenuator rejected: invalid value %q (want on|off)", cmd.Value))
		return
	}
	if err := b.admit(ctx, vfoScopedFrames(b.codec, vfo, b.selectedVFO(), [][]byte{b.codec.BuildSetAttenuator(on)})); err != nil {
		return
	}
	b.rememberVFO(vfo, func(v *vfoCache) { v.attenuator = &on })
}

// cmdSetPower: RF output power 0-255, applied to the target VFO's band
// (KTD-10: select before set).
func (b *Bridge) cmdSetPower(ctx context.Context, cmd cmdMsg) {
	vfo, err := b.resolveVFO(cmd.Vfo)
	if err != nil {
		b.reject(fmt.Sprintf("power rejected: %v", err))
		return
	}
	lvl64, err := strconv.ParseUint(strings.TrimSpace(cmd.Value), 10, 8)
	if err != nil {
		b.reject(fmt.Sprintf("power rejected: invalid value %q (want 0-255)", cmd.Value))
		return
	}
	frames, err := b.codec.BuildSetPower(int(lvl64))
	if err != nil {
		b.reject(fmt.Sprintf("power rejected: %v", err))
		return
	}
	b.admit(ctx, vfoScopedFrames(b.codec, vfo, b.selectedVFO(), [][]byte{frames}))
}

// cmdSatMode: the satellite-mode toggle. Rejected while TX is on or the arm
// permit is set (R10) — the gate reads the cached radio truth, not the cmd.
func (b *Bridge) cmdSatMode(ctx context.Context) {
	b.mu.Lock()
	txOn := b.radio.tx
	b.mu.Unlock()
	switch {
	case txOn:
		b.reject(ErrSatModeTX)
		return
	case b.sess.Armed():
		b.reject(ErrSatModeArm)
		return
	}
	b.mu.Lock()
	cur := b.radio.satellite
	b.mu.Unlock()
	b.admit(ctx, [][]byte{b.codec.BuildSetSatellite(!cur), b.codec.BuildReadSatellite()})
}

// cmdArm / cmdArm(false) = disarm: the blocking SetArmed. The failure text
// from the returned error is the observed fact (R2: a failed arm-triggered
// connect rejects the arm; the permit drops, fail-disarmed).
func (b *Bridge) cmdArm(ctx context.Context, on bool) {
	timeout := b.opts.ArmTimeout
	if timeout <= 0 {
		timeout = doTimeout
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	b.setCmdErr("")
	if err := b.sess.SetArmed(actx, on); err != nil {
		b.reject(err.Error())
		return
	}
	b.publishState(false)
	b.clearCmd()
}

// cmdPTT: gated on the ArmGate seam (v1: armed AND live, R10 exact strings);
// the U6 safety core replaces the gate without touching this dispatch. The
// PTT set is followed by a PTT read — the reply rides the transceive parse
// into /state.tx, so the published state stays radio readback, never tap
// optimism.
func (b *Bridge) cmdPTT(ctx context.Context, cmd cmdMsg) {
	on, ok := parseOnOff(cmd.Value)
	if !ok {
		b.reject(fmt.Sprintf("ptt rejected: invalid value %q (want on|off)", cmd.Value))
		return
	}
	gate := b.opts.ArmGate
	if gate == nil {
		gate = defaultArmGate
	}
	if err := gate(b.sess.Armed(), b.sess.Live()); err != nil {
		b.reject(err.Error())
		return
	}
	b.setCmdErr("")
	b.publishState(false)
	if err := b.send(ctx, [][]byte{b.codec.BuildPTT(on), b.codec.BuildReadPTT()}); err != nil {
		b.reject(err.Error())
		return
	}
	if on && b.opts.OnPTTOn != nil {
		b.opts.OnPTTOn()
	}
	if !on {
		// A completed key-down ends the watchdog window: keyed that outlives
		// its cause makes a later permit drop force-unkey an unrelated
		// carrier (and dial while down) — review finding, U6 lifecycle.
		b.safety.pttOff()
	}
	b.clearCmd()
}

// rememberVFO folds a cmd echo into the per-VFO cache (the "where known"
// preamp/attenuator detail fields).
func (b *Bridge) rememberVFO(vfo civ.VFO, f func(*vfoCache)) {
	b.mu.Lock()
	f(&b.radio.vfo[vfo])
	b.mu.Unlock()
	b.publishState(false)
}

func parseOnOff(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on":
		return true, true
	case "off":
		return false, true
	default:
		return false, false
	}
}

// (no mustBuild: every codec builder call above validates its argument
// first, so a builder error here would be a programming bug — the local
// validation branch rejects before the frame is ever built)

// slot topic helpers (one slot, so they read off Options).
func topicState(o Options) string { return schema.StateTopic(o.Site, o.Station, o.Slot) }
func topicMeta(o Options) string  { return schema.MetaTopic(o.Site, o.Station, o.Slot) }
func topicCmd(o Options) string   { return schema.CmdTopic(o.Site, o.Station, o.Slot) }
