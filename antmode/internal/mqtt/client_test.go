// SPDX-License-Identifier: AGPL-3.0-or-later

package mqtt

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"antmode/internal/config"
	"antmode/internal/engine"
)

type nopSaver struct{}

func (nopSaver) Save(any) error { return nil }

func testClient(t *testing.T, loaded *engine.Persisted) *Client {
	t.Helper()
	cfg := config.Default()
	cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Broker = "muehle", "hf", "tcp://127.0.0.1:1"
	cfg.Location, cfg.Host = "bauwagen", "scmino"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return New(ctx, cfg, nopSaver{}, loaded, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestOptions(t *testing.T) {
	o := testClient(t, nil).buildOptions()
	if !o.CleanSession {
		t.Fatal("CleanSession must be true (a persistent session poisons itself)")
	}
	if !o.AutoReconnect || !o.ConnectRetry || !o.WillEnabled || !o.WillRetained ||
		o.WillTopic != "muehle/hf/ant-mode/status" || string(o.WillPayload) != "offline" {
		t.Fatalf("options: auto %v retry %v will %v %q %q", o.AutoReconnect, o.ConnectRetry, o.WillEnabled, o.WillTopic, o.WillPayload)
	}
	if o.ClientID != "muehle-hf-ant-mode" {
		t.Fatalf("client id %q", o.ClientID)
	}
}

func TestTopics(t *testing.T) {
	want := []struct {
		topic string
		qos   byte
	}{
		{"muehle/hf/ant-mode/state", 1}, // reconciled with the state file during the hold
		{"muehle/hf/ant-mode/cmd", 0},
		{"muehle/hf/power-seq/status", 1}, {"muehle/hf/power-seq/state", 1},
		{"muehle/hf/rotator/status", 1}, {"muehle/hf/rotator/state", 1},
		{"muehle/hf/ant-ctrl/status", 1}, {"muehle/hf/ant-ctrl/state", 1},
		{"muehle/hf/ant-switch/status", 1}, {"muehle/hf/ant-switch/state", 1},
		{"muehle/hf/radio/status", 1}, {"muehle/hf/radio/state", 1},
		{"muehle/power/antennas/status", 1}, {"muehle/power/antennas/state", 1},
	}
	got := testClient(t, nil).Topics()
	if len(got) != len(want) {
		t.Fatalf("got %d subscriptions", len(got))
	}
	for i, w := range want {
		if got[i].Topic != w.topic || got[i].QoS != w.qos || got[i].F == nil {
			t.Errorf("sub %d = %s qos %d, want %s qos %d", i, got[i].Topic, got[i].QoS, w.topic, w.qos)
		}
	}
	// With a state file the own /state is read too (a stale file must not
	// override a newer retained lock).
	if subs := testClient(t, &engine.Persisted{}).Topics(); subs[0].Topic != "muehle/hf/ant-mode/state" {
		t.Fatalf("first sub with a state file = %s", subs[0].Topic)
	}
}

type fakeMsg struct{ p []byte }

func (m fakeMsg) Duplicate() bool   { return false }
func (m fakeMsg) Qos() byte         { return 0 }
func (m fakeMsg) Retained() bool    { return true }
func (m fakeMsg) Topic() string     { return "t" }
func (m fakeMsg) MessageID() uint16 { return 0 }
func (m fakeMsg) Payload() []byte   { return m.p }
func (m fakeMsg) Ack()              {}

// The handler copies the payload and runs f on the worker, not inline.
func TestHandlerEnqueues(t *testing.T) {
	c := testClient(t, nil)
	got := make(chan string, 1)
	buf := []byte("online")
	c.on(func(p []byte) { got <- string(p) })(nil, fakeMsg{buf})
	buf[0] = 'X' // paho may reuse the buffer
	select {
	case s := <-got:
		if s != "online" {
			t.Fatalf("payload %q (not copied)", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job not run")
	}
}

func TestPublishFailsFastWhenDown(t *testing.T) {
	if err := (&publisher{}).Publish("x", 1, true, nil); err == nil {
		t.Fatal("publish without a link succeeded")
	}
}
