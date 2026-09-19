// SPDX-License-Identifier: AGPL-3.0-or-later

package overlay

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vhfcam-restream/internal/config"
)

func testCfg(dir string) config.OverlayConfig {
	c := config.Default().Overlay
	c.Dir = dir
	return c
}

func newTestOverlay(t *testing.T) (*Overlay, string) {
	t.Helper()
	dir := t.TempDir()
	o := New(func() config.OverlayConfig { return testCfg(dir) },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return o, dir
}

func f(v float64) *float64 { return &v }

func TestRenderFreshValues(t *testing.T) {
	o, _ := newTestOverlay(t)
	now := time.Now()
	o.setUp(true)
	o.applyStatus(statusOf(testCfg("").TopicAZ), "online")
	o.applyStatus(statusOf(testCfg("").TopicEL), "online")
	o.applyStatus(statusOf(testCfg("").TopicRadio), "online")
	// Snapshots carry the producing bridge's RFC3339 ts (change-only publishers:
	// silence is normal, liveness rides /status + device_online).
	o.apply(testCfg("").TopicAZ, []byte(`{"az":214.4,"device_online":true,"ts":"`+now.Format(time.RFC3339)+`"}`))
	o.apply(testCfg("").TopicEL, []byte(`{"el":37.0,"device_online":true,"ts":"`+now.Format(time.RFC3339)+`"}`))
	o.apply(testCfg("").TopicRadio, []byte(`{"freq_hz":435100000,"tx":"tx","device_online":true,"ts":"`+now.Format(time.RFC3339)+`"}`))

	texts := o.render(now)
	if texts["az"] != "AZ 214°" {
		t.Errorf("az = %q", texts["az"])
	}
	if texts["el"] != "EL 037°" {
		t.Errorf("el = %q", texts["el"])
	}
	if texts["freq"] != "435.100 MHz" {
		t.Errorf("freq = %q", texts["freq"])
	}
	if texts["tx"] != "TX" {
		t.Errorf("tx = %q", texts["tx"])
	}
}

func TestRenderStaleAndOffline(t *testing.T) {
	o, _ := newTestOverlay(t)
	now := time.Now()
	o.setUp(true)
	for _, topic := range []string{testCfg("").TopicAZ, testCfg("").TopicEL, testCfg("").TopicRadio} {
		o.applyStatus(statusOf(topic), "online")
	}
	// fresh az
	o.apply(testCfg("").TopicAZ, []byte(`{"az":100,"device_online":true,"ts":"`+now.Format(time.RFC3339)+`"}`))
	// ancient el snapshot (older than stale_after_s)
	old := now.Add(-2 * time.Hour).Format(time.RFC3339)
	o.apply(testCfg("").TopicEL, []byte(`{"el":10,"device_online":true,"ts":"`+old+`"}`))
	// offline radio
	o.apply(testCfg("").TopicRadio, []byte(`{"freq_hz":435100000,"tx":"tx","device_online":false,"ts":"`+now.Format(time.RFC3339)+`"}`))

	texts := o.render(now)
	if texts["az"] != "AZ 100°" {
		t.Errorf("az = %q", texts["az"])
	}
	if texts["el"] != "EL ---" {
		t.Errorf("ancient el = %q", texts["el"])
	}
	if texts["freq"] != "FREQ ---" {
		t.Errorf("offline radio freq = %q", texts["freq"])
	}
	if texts["tx"] != "" {
		t.Errorf("offline radio tx = %q, want empty", texts["tx"])
	}
}

func TestRenderSourceStatusGates(t *testing.T) {
	o, _ := newTestOverlay(t)
	now := time.Now()
	o.setUp(true)
	ts := now.Format(time.RFC3339)
	// az: snapshot fresh but source slot /status went offline (LWT)
	o.apply(testCfg("").TopicAZ, []byte(`{"az":100,"device_online":true,"ts":"`+ts+`"}`))
	o.applyStatus(statusOf(testCfg("").TopicAZ), "offline")
	// el: source online in /status but device_online=false in the snapshot
	o.applyStatus(statusOf(testCfg("").TopicEL), "online")
	o.apply(testCfg("").TopicEL, []byte(`{"el":10,"device_online":false,"ts":"`+ts+`"}`))

	texts := o.render(now)
	if texts["az"] != "AZ ---" {
		t.Errorf("az with offline source = %q", texts["az"])
	}
	if texts["el"] != "EL ---" {
		t.Errorf("el with device_online=false = %q", texts["el"])
	}
}

func TestRenderMQTTDownBlankEverything(t *testing.T) {
	o, _ := newTestOverlay(t)
	now := time.Now()
	o.setUp(false) // connection lost
	texts := o.render(now)
	for _, k := range []string{"az", "el", "freq"} {
		if texts[k] == "" {
			t.Errorf("%s vanished; want --- form", k)
		}
	}
	if texts["tx"] != "" {
		t.Errorf("tx = %q while mqtt down", texts["tx"])
	}
}

func TestStartSeedsAllFiles(t *testing.T) {
	o, dir := newTestOverlay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := o.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, name := range []string{"az", "el", "freq", "tx"} {
		if _, err := os.Stat(filepath.Join(dir, name+".txt")); err != nil {
			t.Errorf("file %s.txt not seeded: %v", name, err)
		}
	}
}

func TestApplyIgnoresUnknownTopic(t *testing.T) {
	o, _ := newTestOverlay(t)
	o.apply("muehle/other/state", []byte(`{"az":1}`))
	if o.az.val != nil {
		t.Error("unknown topic mutated az state")
	}
}
