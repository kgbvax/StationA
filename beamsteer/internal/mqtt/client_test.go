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
