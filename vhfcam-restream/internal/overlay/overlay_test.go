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

// The bridge omits fields it could not read (radio in standby: session live,
// CI-V deaf). An omitted frequency must render "---" — never a zero and
// never a stale value presented as current.
func TestRenderOmittedFreq(t *testing.T) {
	o, _ := newTestOverlay(t)
	now := time.Now()
	o.setUp(true)
	o.applyStatus(statusOf(testCfg("").TopicRadio), "online")

	o.apply(testCfg("").TopicRadio, []byte(`{"freq_hz":437775000,"tx":"rx","device_online":true,"ts":"`+now.Format(time.RFC3339)+`"}`))
	if texts := o.render(now); texts["freq"] != "437.775 MHz" {
		t.Errorf("freq with value = %q", texts["freq"])
	}

	// Next snapshot: freq omitted (standby) — render --- even though fresh.
	o.apply(testCfg("").TopicRadio, []byte(`{"device_online":true,"audio_demand":true,"session_state":"live","ts":"`+now.Format(time.RFC3339)+`"}`))
	texts := o.render(now)
	if texts["freq"] != "FREQ ---" {
		t.Errorf("omitted freq = %q, want FREQ ---", texts["freq"])
	}
	if !o.RadioLink().DeviceOnline {
		t.Error("session live with omitted freq should still read device online")
	}
	if o.RadioLink().Responding {
		t.Error("Responding = true while freq omitted")
	}
}

// Deploy-skew shim: with a bridge that predates radio_responding, the
// absence of that field falls back to freq_hz presence as the
// "radio answers CI-V" signal.
func TestRadioLinkRespondingDeploySkew(t *testing.T) {
	o, _ := newTestOverlay(t)
	now := time.Now()
	o.setUp(true)
	o.applyStatus(statusOf(testCfg("").TopicRadio), "online")
	ts := now.Format(time.RFC3339)

	// Old bridge: no radio_responding field, freq present -> responding.
	o.apply(testCfg("").TopicRadio, []byte(`{"freq_hz":437775000,"device_online":true,"ts":"` + ts + `"}`))
	if !o.RadioLink().Responding {
		t.Error("old-bridge freq presence should map to Responding")
	}

	// Old bridge, freq omitted (standby) -> not responding.
	o.apply(testCfg("").TopicRadio, []byte(`{"device_online":true,"ts":"` + ts + `"}`))
	if o.RadioLink().Responding {
		t.Error("old-bridge omitted freq should clear Responding")
	}

	// New bridge: radio_responding drives the bit regardless of freq.
	o.apply(testCfg("").TopicRadio, []byte(`{"freq_hz":437775000,"device_online":true,"radio_responding":false,"ts":"` + ts + `"}`))
	if o.RadioLink().Responding {
		t.Error("bridge-published radio_responding=false overridden by freq presence")
	}
}

// FreqHz agrees with the burned-in text: known only while the radio slot is
// fresh and the snapshot carries a frequency.
func TestFreqHzMatchesOverlay(t *testing.T) {
	o, _ := newTestOverlay(t)
	if _, ok := o.FreqHz(); ok {
		t.Error("freq known before any data")
	}
	now := time.Now()
	o.setUp(true)
	o.applyStatus(statusOf(testCfg("").TopicRadio), "online")
	o.apply(testCfg("").TopicRadio, []byte(`{"freq_hz":435100000,"device_online":true,"ts":"`+now.Format(time.RFC3339)+`"}`))
	if hz, ok := o.FreqHz(); !ok || hz != 435100000 {
		t.Errorf("FreqHz = %d, %v", hz, ok)
	}
	o.applyStatus(statusOf(testCfg("").TopicRadio), "offline")
	if _, ok := o.FreqHz(); ok {
		t.Error("freq known with the radio bridge offline")
	}
}
