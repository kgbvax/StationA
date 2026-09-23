package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
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

// busCmd is the /cmd payload: the stationa value-key convention. The five
// surviving actions carry no value — the action name is the whole intent.
type busCmd struct {
	Action string `json:"action"`
	Ts     string `json:"ts,omitempty"`
}

// onCmd is the /cmd handler: gates inline (cheap), dispatch on the jobs
// worker. One-shot posture: the retained topic is cleared after the worker
// has acted on it, whatever the outcome. The action set is the complete
// receive-only surface (2026-09 pivot): audio_on, audio_off, power_on,
// monitor_on, monitor_off. There is no LAN CI-V command path.
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
	case "audio_on":
		b.log.Info("rx cmd", "action", cmd.Action)
		sharedmqtt.Enqueue(b.jobs, func() { b.executeAudioDemand(true) })
	case "audio_off":
		b.log.Info("rx cmd", "action", cmd.Action)
		sharedmqtt.Enqueue(b.jobs, func() { b.executeAudioDemand(false) })
	case "monitor_on":
		b.log.Info("rx cmd", "action", cmd.Action)
		sharedmqtt.Enqueue(b.jobs, func() { b.executeMonitor(true) })
	case "monitor_off":
		b.log.Info("rx cmd", "action", cmd.Action)
		sharedmqtt.Enqueue(b.jobs, func() { b.executeMonitor(false) })
	case "power_on":
		b.log.Info("rx cmd", "action", cmd.Action)
		sharedmqtt.Enqueue(b.jobs, func() { b.executePowerOn() })
	default:
		b.rejectAsync(fmt.Sprintf("unknown cmd action %q", cmd.Action))
	}
}

// executeAudioDemand applies the audio_on/audio_off demand (the session's
// only hold source — receive-only, no arm gate exists anymore).
func (b *Bridge) executeAudioDemand(on bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := b.mgr.SetAudioDemand(ctx, on)
	if err != nil {
		b.setCmdErr(err.Error())
	} else {
		b.setCmdErr("")
	}
	b.publishState(false)
	b.clearCmd()
}

// executeMonitor toggles the serial CI-V telemetry reader (sticky, no
// TTL — the serial wire is dedicated to this process).
func (b *Bridge) executeMonitor(on bool) {
	if b.opts.Monitor == nil {
		b.reject("monitor unavailable: serial.device not configured")
		return
	}
	if err := b.opts.Monitor.SetMonitor(on); err != nil {
		b.setCmdErr(err.Error())
	} else {
		b.mu.Lock()
		b.monitorOn = on
		b.mu.Unlock()
		b.setCmdErr("")
	}
	b.publishState(false)
	b.clearCmd()
}

// executePowerOn sends the CI-V remote-wake frame over the serial port (a
// blind, untracked send — a standby radio answers no ack). The wake lives
// on serial CI-V by design: no LAN CI-V command path exists.
func (b *Bridge) executePowerOn() {
	if b.opts.Monitor == nil {
		b.reject("power_on not configured (serial.device empty)")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.opts.Monitor.Wake(ctx); err != nil {
		b.setCmdErr(err.Error())
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
