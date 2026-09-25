// SPDX-License-Identifier: AGPL-3.0-or-later

package recorder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vhfcam-restream/internal/config"
)

// fakePreview writes an HLS playlist + segments like the preview muxer.
type fakePreview struct {
	t    *testing.T
	dir  string
	next int
}

func (p *fakePreview) add(n int) {
	p.t.Helper()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("seg_%05d.ts", p.next)
		if err := os.WriteFile(filepath.Join(p.dir, name), []byte(name+";"), 0644); err != nil {
			p.t.Fatal(err)
		}
		p.next++
	}
	first := p.next - 6
	if first < 0 {
		first = 0
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:%d\n", first)
	for i := first; i < p.next; i++ {
		fmt.Fprintf(&b, "#EXTINF:2.0,\nseg_%05d.ts\n", i)
	}
	if err := os.WriteFile(filepath.Join(p.dir, "live.m3u8"), []byte(b.String()), 0644); err != nil {
		p.t.Fatal(err)
	}
}

type harness struct {
	t       *testing.T
	cfg     config.Config
	pv      *fakePreview
	rec     *Recorder
	clock   atomic.Int64 // unix nanos
	free    atomic.Uint64
	mu      sync.Mutex
	actives []bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// fakeFFmpeg concatenates the listed parts into the output (last argument),
// so the test can check the MP4 holds every copied segment in order.
const fakeFFmpegOK = `#!/bin/sh
for a; do out="$a"; done
list=""; prev=""
for a; do [ "$prev" = "-i" ] && list="$a"; prev="$a"; done
: > "$out"
sed -e "s/^file '//" -e "s/'$//" "$list" | while read -r p; do cat "$p" >> "$out"; done
`

func newHarness(t *testing.T, ffmpegScript string) *harness {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(bin, []byte(ffmpegScript), 0755); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, pv: &fakePreview{t: t, dir: t.TempDir()}}
	h.cfg = config.Default()
	h.cfg.FFmpegBin = bin
	h.cfg.Preview.Enabled = true
	h.cfg.Preview.Dir = h.pv.dir
	h.cfg.Record.Dir = t.TempDir()
	h.clock.Store(time.Now().UnixNano())
	h.free.Store(100e9)
	h.rec = New(func() config.Config { return h.cfg }, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithFreq(func() (int64, bool) { return 435_000_000, true }).
		WithOnActive(func(a bool) { h.mu.Lock(); h.actives = append(h.actives, a); h.mu.Unlock() })
	h.rec.now = func() time.Time { return time.Unix(0, h.clock.Load()) }
	h.rec.free = func(string) (uint64, error) { return h.free.Load(), nil }
	h.rec.pollEvery = 5 * time.Millisecond
	return h
}

func (h *harness) run() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan struct{})
	go func() { _ = h.rec.Run(ctx); close(h.done) }()
	h.t.Cleanup(func() { cancel(); <-h.done })
}

func (h *harness) advance(d time.Duration) { h.clock.Add(int64(d)) }

func (h *harness) waitState(want State) Status {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := h.rec.Status(); st.State == want {
			return st
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("state never became %s (now %+v)", want, h.rec.Status())
	return Status{}
}

func (h *harness) waitParts(n int) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.rec.Status().Parts >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("parts never reached %d", n)
}

func (h *harness) read(name string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.cfg.Record.Dir, name))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func TestStartRefusals(t *testing.T) {
	h := newHarness(t, fakeFFmpegOK)
	h.run()

	if _, err := h.rec.Start(); !errors.Is(err, ErrNoPreview) {
		t.Errorf("no playlist: err = %v", err)
	}
	h.pv.add(6)

	h.free.Store(8e9 + 100) // above the floor, but no room for a 30-min recording
	st, err := h.rec.Start()
	if !errors.Is(err, ErrLowDisk) || st.LastError == "" {
		t.Errorf("low disk: err = %v, last_error %q", err, st.LastError)
	}

	h.cfg.Record.Enabled = false
	if _, err := h.rec.Start(); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled: err = %v", err)
	}
	h.cfg.Record.Enabled = true

	h.free.Store(100e9)
	if _, err := h.rec.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := h.rec.Start(); !errors.Is(err, ErrBusy) {
		t.Errorf("second start: err = %v", err)
	}
}

func TestRecordStopProducesMP4(t *testing.T) {
	h := newHarness(t, fakeFFmpegOK)
	h.pv.add(6) // seg 0..5; pre-roll takes 3..5
	h.run()

	st, err := h.rec.Start()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(st.Name, "_435.000MHz.mp4") || st.State != Recording || st.RemainingS != 1800 {
		t.Fatalf("status after start = %+v", st)
	}
	h.waitParts(1)
	h.pv.add(2) // 6, 7
	time.Sleep(50 * time.Millisecond)
	h.advance(90 * time.Second)
	if st := h.rec.Status(); st.ElapsedS != 90 || st.RemainingS != 1710 {
		t.Errorf("elapsed/remaining = %d/%d", st.ElapsedS, st.RemainingS)
	}

	if _, err := h.rec.Stop(); err != nil {
		t.Fatal(err)
	}
	st = h.waitState(Idle)
	if st.LastFile != strings.TrimSuffix(st.LastFile, ".mp4")+".mp4" || st.StopReason != "operator" || st.LastError != "" {
		t.Fatalf("after stop = %+v", st)
	}
	got := h.read(st.LastFile)
	want := "seg_00003.ts;seg_00004.ts;seg_00005.ts;seg_00006.ts;seg_00007.ts;"
	if got != want {
		t.Errorf("recording content = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(h.cfg.Record.Dir, inprogressDir, strings.TrimSuffix(st.LastFile, ".mp4"))); !os.IsNotExist(err) {
		t.Error("in-progress directory not removed")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if fmt.Sprint(h.actives) != "[true false]" {
		t.Errorf("onActive edges = %v", h.actives)
	}
	if _, err := h.rec.Stop(); !errors.Is(err, ErrNotRecording) {
		t.Errorf("stop while idle: err = %v", err)
	}
}

func TestAutoStopAtMaxDuration(t *testing.T) {
	h := newHarness(t, fakeFFmpegOK)
	h.pv.add(6)
	h.run()
	if _, err := h.rec.Start(); err != nil {
		t.Fatal(err)
	}
	h.waitParts(1)
	h.advance(30 * time.Minute)
	if st := h.waitState(Idle); st.StopReason != "max_duration" || st.LastFile == "" {
		t.Fatalf("after max duration = %+v", st)
	}
}

func TestAutoStopOnLowDisk(t *testing.T) {
	h := newHarness(t, fakeFFmpegOK)
	h.pv.add(6)
	h.run()
	if _, err := h.rec.Start(); err != nil {
		t.Fatal(err)
	}
	h.waitParts(1)
	h.free.Store(7e9)
	h.advance(freeCheckEvery + time.Second) // next free-space check is due
	if st := h.waitState(Idle); st.StopReason != "low_disk" {
		t.Fatalf("after low disk = %+v", st)
	}
}

func TestRemuxFailureKeepsTS(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\necho boom >&2\nexit 1\n")
	h.pv.add(6)
	h.run()
	if _, err := h.rec.Start(); err != nil {
		t.Fatal(err)
	}
	h.waitParts(1)
	_, _ = h.rec.Stop()
	st := h.waitState(Idle)
	if !strings.HasSuffix(st.LastFile, ".ts") || !strings.Contains(st.LastError, "boom") {
		t.Fatalf("after failed remux = %+v", st)
	}
	if got := h.read(st.LastFile); !strings.HasPrefix(got, "seg_00003.ts;") {
		t.Errorf("kept part content = %q", got)
	}
}

func TestShutdownFinalizes(t *testing.T) {
	h := newHarness(t, fakeFFmpegOK)
	h.pv.add(6)
	h.run()
	if _, err := h.rec.Start(); err != nil {
		t.Fatal(err)
	}
	h.waitParts(1)
	h.cancel()
	<-h.done
	st := h.rec.Status()
	if st.State != Idle || st.StopReason != "shutdown" || !strings.HasSuffix(st.LastFile, ".mp4") {
		t.Fatalf("after shutdown = %+v", st)
	}
}

func TestRecoversLeftoverAtStartup(t *testing.T) {
	h := newHarness(t, fakeFFmpegOK)
	base := "vhfcam_2026-09-25T1812Z_435.000MHz"
	dir := filepath.Join(h.cfg.Record.Dir, inprogressDir, base)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(dir, meta{Name: base}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, partName(1)), []byte("left over"), 0640); err != nil {
		t.Fatal(err)
	}
	h.run()
	deadline := time.Now().Add(3 * time.Second)
	for h.rec.Status().LastFile == "" && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	st := h.waitState(Idle)
	if st.LastFile != base+".mp4" || st.StopReason != "recovered" {
		t.Fatalf("after recovery = %+v", st)
	}
	if got := h.read(base + ".mp4"); got != "left over" {
		t.Errorf("recovered content = %q", got)
	}
}

func TestPreviewRestartStartsNewPart(t *testing.T) {
	h := newHarness(t, fakeFFmpegOK)
	h.pv.add(10) // 4..9 listed
	h.run()
	if _, err := h.rec.Start(); err != nil {
		t.Fatal(err)
	}
	h.waitParts(1)
	// ffmpeg restarts: sequence starts over at 0.
	h.pv.next = 0
	h.pv.add(2)
	h.waitParts(2)
	_, _ = h.rec.Stop()
	st := h.waitState(Idle)
	got := h.read(st.LastFile)
	want := "seg_00007.ts;seg_00008.ts;seg_00009.ts;seg_00000.ts;seg_00001.ts;"
	if got != want {
		t.Errorf("content across restart = %q, want %q", got, want)
	}
}

func TestDeleteRefusesActiveRecording(t *testing.T) {
	h := newHarness(t, fakeFFmpegOK)
	h.pv.add(6)
	h.run()
	st, err := h.rec.Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.rec.Delete(st.Name); !errors.Is(err, ErrActive) {
		t.Errorf("delete active: err = %v", err)
	}
}
