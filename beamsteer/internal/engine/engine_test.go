// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"beamsteer/internal/config"
)

type msg struct {
	topic    string
	retained bool
	body     map[string]any
}

type fakePub struct{ msgs []msg }

func (f *fakePub) Publish(topic string, retained bool, payload []byte) error {
	var m map[string]any
	_ = json.Unmarshal(payload, &m)
	f.msgs = append(f.msgs, msg{topic, retained, m})
	return nil
}

// cmds returns the /cmd messages for one topic since the last reset.
func (f *fakePub) cmds(topic string) []msg {
	var out []msg
	for _, m := range f.msgs {
		if m.topic == topic {
			out = append(out, m)
		}
	}
	return out
}

const (
	rotCmd = "muehle/hf/rotator/cmd"
	antCmd = "muehle/hf/ant-ctrl/cmd"
	self   = "muehle/hf/beam-steer"
)

func newEngine(t *testing.T) (*Engine, *fakePub) {
	t.Helper()
	cfg := config.Default()
	cfg.MQTT.Site, cfg.MQTT.Station = "muehle", "hf"
	cfg.Location, cfg.Host = "bauwagen", "shari"
	pub := &fakePub{}
	now := func() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }
	e := New(cfg, pub, slog.New(slog.NewTextHandler(io.Discard, nil)), now)
	return e, pub
}

// live brings all three siblings online: rotator at az, ant-ctrl in dir.
func live(e *Engine, az float64, dir string) {
	e.RotatorStatus([]byte("online"))
	e.RotatorState([]byte(`{"az":` + ftoa(az) + `,"moving":false,"device_online":true}`))
	e.AntStatus([]byte("online"))
	e.AntState([]byte(`{"direction":"` + dir + `","moving":false,"device_online":true}`))
	e.RadioStatus([]byte("online"))
	e.RadioState([]byte(`{"band":"20m","tx":"rx","device_online":true}`))
}

func ftoa(f float64) string { b, _ := json.Marshal(f); return string(b) }

func TestDisabledIsPassthrough(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.Goto(260.4)
	r := pub.cmds(rotCmd)
	if len(r) != 1 || r[0].body["action"] != "set_az" || r[0].body["az"] != 260.0 || r[0].retained {
		t.Fatalf("rotator cmds = %+v", r)
	}
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Errorf("ant-ctrl cmds = %+v, want none", a)
	}
}

func TestBehindFlipsWithoutRotation(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.Goto(265)
	if r := pub.cmds(rotCmd); len(r) != 0 {
		t.Errorf("rotator cmds = %+v, want none", r)
	}
	a := pub.cmds(antCmd)
	if len(a) != 1 || a[0].body["value"] != "reverse" || a[0].body["action"] != "direction" || !a[0].retained {
		t.Fatalf("ant-ctrl cmds = %+v", a)
	}
	if a[0].body["ts"] != "2026-09-26T12:00:00Z" {
		t.Errorf("ts = %v", a[0].body["ts"])
	}
}

func TestOutsideRotatesToCheaperLobe(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.Goto(330) // reverse on 150 (60) beats forward on 330 (240)
	r := pub.cmds(rotCmd)
	if len(r) != 1 || r[0].body["az"] != 150.0 {
		t.Fatalf("rotator cmds = %+v", r)
	}
	if a := pub.cmds(antCmd); len(a) != 1 || a[0].body["value"] != "reverse" {
		t.Fatalf("ant-ctrl cmds = %+v", a)
	}
}

func TestBidirNeverSwitches(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "bidirectional")
	e.SetEnabled(true)
	e.Goto(230) // back lobe ±45
	e.Goto(340) // outside → rotate to 160
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Errorf("ant-ctrl cmds = %+v, want none", a)
	}
	if r := pub.cmds(rotCmd); len(r) != 1 || r[0].body["az"] != 160.0 {
		t.Errorf("rotator cmds = %+v", r)
	}
}

func TestDirectionHeldWhileTransmittingOrMoving(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.RadioState([]byte(`{"band":"20m","tx":"tx","device_online":true}`))
	e.Goto(270)
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Fatalf("sent during TX: %+v", a)
	}
	// Elements moving after TX ends: still held.
	e.AntState([]byte(`{"direction":"forward","moving":true,"device_online":true}`))
	e.RadioState([]byte(`{"band":"20m","tx":"rx","device_online":true}`))
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Fatalf("sent while moving: %+v", a)
	}
	e.AntState([]byte(`{"direction":"forward","moving":false,"device_online":true}`))
	if a := pub.cmds(antCmd); len(a) != 1 || a[0].body["value"] != "reverse" {
		t.Fatalf("after clear: %+v", a)
	}
}

func TestNewerRequestSupersedesPending(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.RadioState([]byte(`{"band":"20m","tx":"tx","device_online":true}`))
	e.Goto(270) // would reverse, held
	e.Goto(95)  // front lobe: nothing to do
	e.RadioState([]byte(`{"band":"20m","tx":"rx","device_online":true}`))
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Errorf("stale pending sent: %+v", a)
	}
}

func TestStaleTXFromDeadRadioDoesNotHold(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.RadioState([]byte(`{"band":"20m","tx":"tx","device_online":true}`))
	e.RadioStatus([]byte("offline"))
	e.Goto(270)
	if a := pub.cmds(antCmd); len(a) != 1 {
		t.Errorf("ant-ctrl cmds = %+v, want reverse", a)
	}
}

func TestRotatorOfflineIgnored(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.RotatorStatus([]byte("offline"))
	e.SetEnabled(true)
	e.Goto(150)
	if r := pub.cmds(rotCmd); len(r) != 0 {
		t.Errorf("rotator cmds = %+v", r)
	}
	if _, ok := e.Readback(); ok {
		t.Error("readback valid with rotator offline")
	}
}

func TestAntCtrlOfflinePlainRotation(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.AntStatus([]byte("offline"))
	e.SetEnabled(true)
	e.Goto(265)
	if r := pub.cmds(rotCmd); len(r) != 1 || r[0].body["az"] != 265.0 {
		t.Errorf("rotator cmds = %+v", r)
	}
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Errorf("ant-ctrl cmds = %+v", a)
	}
}

func TestSixMetreNeverReverses(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.RadioState([]byte(`{"band":"6m","tx":"rx","device_online":true}`))
	e.SetEnabled(true)
	e.Goto(270)
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Errorf("ant-ctrl cmds = %+v", a)
	}
	if r := pub.cmds(rotCmd); len(r) != 1 || r[0].body["az"] != 270.0 {
		t.Errorf("rotator cmds = %+v", r)
	}
}

func TestReadbackIsEffectiveHeading(t *testing.T) {
	e, _ := newEngine(t)
	live(e, 90, "reverse")
	if h, ok := e.Readback(); !ok || h != 270 {
		t.Errorf("readback = %v %v, want 270", h, ok)
	}
}

func TestStateAndStop(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.Goto(265)
	e.Stop()
	r := pub.cmds(rotCmd)
	if len(r) != 1 || r[0].body["action"] != "stop" {
		t.Errorf("stop = %+v", r)
	}
	st := pub.cmds(self + "/state")
	last := st[len(st)-1]
	if !last.retained || last.body["enabled"] != true {
		t.Fatalf("state = %+v", last)
	}
	l := last.body["last"].(map[string]any)
	if l["bearing"] != 265.0 || l["direction"] != "reverse" || l["reason"] == "" {
		t.Errorf("last = %+v", l)
	}
}
