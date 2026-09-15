package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"

	"icom9700-radio-bridge/internal/civ"
	"icom9700-radio-bridge/internal/radio"
)

// The /cmd gate set, lifted from the spid-ercm mqttslot template (KTD6
// one-shot posture): QoS-0 subscription (done in the connect ritual — no
// broker offline backlog), 30 s ts staleness gate with unstamped
// tolerance, 4 KB size gate BEFORE Unmarshal, clear-after-execute-or-
// reject with the empty-payload echo guard, and every rejection clipped
// at the single setCmdErr choke point.
const (
	cmdMaxAge         = 30 * time.Second
	cmdMaxBytes       = 4 * 1024
	cmdErrMsgTooLarge = "cmd payload too large"
	cmdErrMax         = 200
)

// Rejection strings from the settled /state.error taxonomy (plan R5).
const (
	errPttNotArmed   = "ptt rejected: not armed"
	errPttNotLive    = "ptt rejected: session not live"
	errFreqOutOfBand = "freq rejected: out of band for sub"
	errPttOffUndeliv = "ptt-off undeliverable: session unavailable"
)

// busCmd is the /cmd payload: the stationa value-key convention with the
// optional per-VFO targeting (no select-VFO action exists — the cmd
// carries the target, R9).
type busCmd struct {
	Action string          `json:"action"`
	Value  json.RawMessage `json:"value,omitempty"`
	Vfo    string          `json:"vfo,omitempty"`
	Ts     string          `json:"ts,omitempty"`
}

func (b *busCmd) stringValue() (string, error) {
	var s string
	if err := json.Unmarshal(b.Value, &s); err == nil {
		return s, nil
	}
	var n json.Number
	if err := json.Unmarshal(b.Value, &n); err == nil {
		return n.String(), nil
	}
	if b.Value == nil {
		return "", nil
	}
	return "", fmt.Errorf("unsupported value form %s", string(b.Value))
}

func (b *busCmd) boolValue() (bool, error) {
	s, err := b.stringValue()
	if err != nil {
		return false, err
	}
	switch strings.ToLower(s) {
	case "on", "true", "1":
		return true, nil
	case "off", "false", "0":
		return false, nil
	}
	return false, fmt.Errorf("unsupported bool value %q", s)
}

// onCmd is the /cmd handler: gates inline (cheap), dispatch on the jobs
// worker. One-shot posture: the retained topic is cleared after the worker
// has acted on it, whatever the outcome.
func (b *Bridge) onCmd(payload []byte) {
	if len(payload) == 0 {
		// Our own retained-clear echo — never re-clear (echo guard).
		return
	}
	if len(payload) > cmdMaxBytes {
		b.log.Warn("rx oversized cmd", "topic", b.cmdTopic, "bytes", len(payload), "max", cmdMaxBytes)
		b.rejectAsync(cmdErrMsgTooLarge)
		return
	}

	var cmd busCmd
	if err := json.Unmarshal(payload, &cmd); err != nil || cmd.Action == "" {
		b.log.Error("rx invalid cmd", "payload", clipCmdErr(string(payload)))
		b.rejectAsync(fmt.Sprintf("invalid /cmd payload: %q", clipCmdErr(string(payload))))
		return
	}
	if cmd.Ts != "" {
		ts, err := time.Parse(time.RFC3339, cmd.Ts)
		if err != nil {
			b.rejectAsync(fmt.Sprintf("cmd ts unparseable: %q", cmd.Ts))
			return
		}
		if age := time.Since(ts); age > cmdMaxAge || age < -cmdMaxAge {
			b.log.Warn("dropping stale cmd", "action", cmd.Action, "age", age.Round(time.Second))
			b.rejectAsync(fmt.Sprintf("stale cmd (age %s, bound %s)", age.Round(time.Second), cmdMaxAge))
			return
		}
	}

	switch cmd.Action {
	case "arm":
		b.log.Info("rx cmd", "action", cmd.Action)
		sharedmqtt.Enqueue(b.jobs, func() { b.executeArm(true) })
	case "disarm":
		b.log.Info("rx cmd", "action", cmd.Action)
		sharedmqtt.Enqueue(b.jobs, func() { b.executeArm(false) })
	case "ptt":
		on, err := cmd.boolValue()
		if err != nil {
			b.rejectAsync(fmt.Sprintf("invalid ptt value: %v", err))
			return
		}
		b.log.Info("rx cmd", "action", cmd.Action, "value", on)
		sharedmqtt.Enqueue(b.jobs, func() { b.executePtt(on) })
	case "sat_mode":
		on, err := cmd.boolValue()
		if err != nil {
			b.rejectAsync(fmt.Sprintf("invalid sat_mode value: %v", err))
			return
		}
		sharedmqtt.Enqueue(b.jobs, func() { b.executeSatMode(on) })
	default:
		// set_freq / set_mode / set_data / set_power (+ unknown actions,
		// which the manager's dispatch rejects with a warning+drop).
		sharedmqtt.Enqueue(b.jobs, func() { b.executeViaManager(cmd.Action, string(cmd.Value), cmd.Vfo) })
	}
}

func onString(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// executeViaManager runs the manager's settled dispatch for the non-safety
// actions and clears the cmd either way (execute-or-reject).
func (b *Bridge) executeViaManager(action, rawValue, vfo string) {
	// The value rides as raw JSON when it already is (the console sends
	// JSON strings/numbers); bare words ("on") get quoted into strings.
	value := rawValue
	if value != "" && !json.Valid([]byte(value)) {
		value = strconv.Quote(value)
	}
	payload := fmt.Sprintf(`{"action":%q,"value":%s,"vfo":%q}`, action, value, vfo)
	if rawValue == "" {
		payload = fmt.Sprintf(`{"action":%q,"vfo":%q}`, action, vfo)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := b.mgr.Execute(ctx, []byte(payload))
	if err != nil {
		b.setCmdErr(err.Error())
	} else {
		b.setCmdErr("")
	}
	b.publishState(false)
	b.clearCmd()
}

// executeSatMode applies the settled gate (R10): satellite mode is
// rejected while tx is on or the armed permit is set — a satellite-mode
// switch mid-transmission re-routes the VFOs under a keyed carrier.
func (b *Bridge) executeSatMode(on bool) {
	b.mu.Lock()
	txOn := b.radio.tx == "tx"
	armed := b.armed
	b.mu.Unlock()
	if on && (txOn || armed) {
		b.reject("sat_mode rejected: tx is on or armed")
		return
	}
	b.executeViaManager("sat_mode", onString(on), "")
}

// executeArm is the arm/disarm demand (KTD4: an explicit operator permit —
// the permit itself is bridge-held and drops on session loss, R11; U6 adds
// the watchdog and loss-of-plane rules around it).
func (b *Bridge) executeArm(on bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := b.mgr.SetHold(ctx, on)
	b.mu.Lock()
	b.armed = on && err == nil
	b.mu.Unlock()
	if err != nil {
		b.setCmdErr(err.Error())
	} else {
		b.setCmdErr("")
	}
	b.publishState(false)
	b.clearCmd()
}

// executePtt applies the settled safety gate (R10) BEFORE any radio frame:
// not armed → the exact taxonomy rejection; not live → ditto. Armed and
// live, the PTT frame goes out and the state re-reports from the radio.
func (b *Bridge) executePtt(on bool) {
	if on {
		b.mu.Lock()
		armed := b.armed
		b.mu.Unlock()
		if !armed {
			b.reject(errPttNotArmed)
			return
		}
		if b.mgr.Snapshot().SessionState != radio.StateLive {
			b.reject(errPttNotLive)
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := b.mgr.Session().Demand(ctx, func(c *civ.Client) error {
		// Re-check at send time (R10: the gate holds at dispatch, not just
		// at acceptance — the state can move while the demand queued).
		if _, err := b.mgr.Session().RoundTrip(ctx, c, civ.CmdPTT(on), 5*time.Second); err != nil {
			return err
		}
		// Readback-only truth: confirm the keyed state from the radio
		// before it reaches /state (never tap optimism).
		if f, err := b.mgr.Session().RoundTrip(ctx, c, civ.BuildFrame(0x1C, []byte{0x00}), 5*time.Second); err == nil && len(f.Sub) == 1 {
			tx := "rx"
			if f.Sub[0] == 0x01 {
				tx = "tx"
			}
			b.mu.Lock()
			b.radio.tx = tx
			b.mu.Unlock()
		}
		return nil
	})
	if err != nil {
		if !on {
			// A PTT-off that could not reach the radio is the safety
			// taxonomy's worst fact (the watchdog bounds the rest, U6).
			b.setCmdErr(errPttOffUndeliv)
		} else {
			b.setCmdErr(err.Error())
		}
	} else {
		b.setCmdErr("")
	}
	b.publishState(false)
	b.clearCmd()
}

// reject records the rejection and clears the retained cmd (one-shot:
// rejection still clears).
func (b *Bridge) reject(errMsg string) {
	b.setCmdErr(errMsg)
	b.publishState(false)
	b.clearCmd()
}

func (b *Bridge) rejectAsync(errMsg string) {
	sharedmqtt.Enqueue(b.jobs, func() { b.reject(errMsg) })
}

// clearCmd publishes an empty retained payload so no stale /cmd can
// re-fire on the next (re)connect (the echo guard in onCmd keeps our own
// clear from looping).
func (b *Bridge) clearCmd() {
	b.cli.publish(b.cmdTopic, 1, true, []byte{})
}

// setCmdErr records the last rejection for /state.error — the single choke
// point, clipped to cmdErrMax runes so no producer-chosen unbounded string
// reaches the retained publish or the logs.
func (b *Bridge) setCmdErr(msg string) {
	msg = clipCmdErr(msg)
	b.mu.Lock()
	b.cmdErr = msg
	b.mu.Unlock()
}

func clipCmdErr(msg string) string {
	rs := []rune(msg)
	if len(rs) <= cmdErrMax {
		return msg
	}
	return string(rs[:cmdErrMax]) + "…"
}
