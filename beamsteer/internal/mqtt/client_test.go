// SPDX-License-Identifier: AGPL-3.0-or-later

package mqtt

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"beamsteer/internal/config"
)

func TestParseBearing(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{`265`, 265, true},
		{`265.4`, 265.4, true},
		{`"120"`, 120, true},
		{`-10`, -10, true},
		{`720`, 720, true},
		{`721`, 0, false},
		{`"north"`, 0, false},
		{`true`, 0, false},
		{`null`, 0, false},
	}
	for _, tc := range cases {
		got, ok := parseBearing(json.RawMessage(tc.in))
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseBearing(%s) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func testClient(t *testing.T) (*Client, *bytes.Buffer) {
	t.Helper()
	cfg := config.Default()
	cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Broker = "muehle", "hf", "tcp://127.0.0.1:1"
	cfg.Location, cfg.Host = "bauwagen", "shari"
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return New(ctx, cfg, slog.New(slog.NewTextHandler(&buf, nil))), &buf
}

// The 2026-10-01 blindness: a persistent session poisoned itself. Pin it.
func TestCleanSession(t *testing.T) {
	c, _ := testClient(t)
	o := c.buildOptions()
	if !o.CleanSession {
		t.Fatal("CleanSession must be true (persistent session poisons itself, see buildOptions)")
	}
	if !o.AutoReconnect || !o.WillEnabled || !o.WillRetained || string(o.WillPayload) != "offline" {
		t.Fatalf("options = auto %v will %v retained %v payload %q", o.AutoReconnect, o.WillEnabled, o.WillRetained, o.WillPayload)
	}
}

func TestOnCmd(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		retained bool
		wantLog  string
	}{
		{"enable", `{"action":"enable"}`, true, "enabled=true"},
		{"aim reaches the engine", `{"action":"aim","value":265}`, false, "rotate request ignored: not connected"},
		{"retained aim refused", `{"action":"aim","value":265}`, true, "retained aim ignored"},
		{"bad aim value", `{"action":"aim","value":"north"}`, false, "bad aim value"},
		{"unknown action", `{"action":"spin"}`, false, "unknown /cmd action"},
		{"cleared retained cmd", ``, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, buf := testClient(t)
			c.onCmd([]byte(tc.payload), tc.retained)
			if tc.wantLog == "" {
				if buf.Len() != 0 {
					t.Fatalf("log = %q, want nothing", buf.String())
				}
				return
			}
			if !strings.Contains(buf.String(), tc.wantLog) {
				t.Fatalf("log = %q, want %q", buf.String(), tc.wantLog)
			}
		})
	}
}

// fakeMsg is a minimal paho.Message.
type fakeMsg struct {
	payload  []byte
	retained bool
}

func (m fakeMsg) Duplicate() bool   { return false }
func (m fakeMsg) Qos() byte         { return 1 }
func (m fakeMsg) Retained() bool    { return m.retained }
func (m fakeMsg) Topic() string     { return "muehle/hf/ant-mode/state" }
func (m fakeMsg) MessageID() uint16 { return 0 }
func (m fakeMsg) Payload() []byte   { return m.payload }
func (m fakeMsg) Ack()              {}

// runOnWorker runs f on the jobs worker and waits for it (the engine is
// worker-owned; calling it from the test goroutine would race the tick).
func runOnWorker(t *testing.T, c *Client, f func()) {
	t.Helper()
	done := make(chan struct{})
	c.jobs <- func() { f(); close(done) }
	<-done
}

// The ant-mode handler records the lock on the paho goroutine and the worker
// applies it: an aim from the console is then ignored with the lock reason.
func TestAntModeLockReachesEngine(t *testing.T) {
	c, buf := testClient(t)
	runOnWorker(t, c, func() { c.eng.SetConnected(true) })
	c.onAntMode(nil, fakeMsg{payload: []byte(`{"ant_mode":"storm","lock":{"seq":3}}`), retained: true})
	runOnWorker(t, c, func() { c.onCmd([]byte(`{"action":"aim","value":265}`), false) })
	if !strings.Contains(buf.String(), "antenna mode lock changed") ||
		!strings.Contains(buf.String(), "rotate request ignored: antenna locked") {
		t.Fatalf("log = %q", buf.String())
	}

	// Malformed: logged at Error, lock kept.
	buf.Reset()
	c.onAntMode(nil, fakeMsg{payload: []byte(`{"ant_mode":`)})
	runOnWorker(t, c, func() { c.onCmd([]byte(`{"action":"aim","value":265}`), false) })
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "lock=\"antenna mode storm\"") {
		t.Fatalf("log = %q", buf.String())
	}
}
