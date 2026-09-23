// SPDX-License-Identifier: AGPL-3.0-or-later

package restream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"vhfcam-restream/internal/config"
)

// fakeFFmpeg writes an executable shell script and returns its path, for use
// as cfg.FFmpegBin in the tests below.
func fakeFFmpeg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// testSupervisor builds a Supervisor over a fake ffmpeg binary.
func testSupervisor(t *testing.T, ffmpegBin string, stallSec int) *Supervisor {
	t.Helper()
	return New(testCfg2(ffmpegBin, stallSec), func() (config.Config, error) {
		return testCfg2(ffmpegBin, stallSec), nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testCfg2(ffmpegBin string, stallSec int) config.Config {
	c := config.Default()
	c.SourceURL = "rtsps://cam/stream"
	c.StreamKey = "k"
	c.FFmpegBin = ffmpegBin
	c.StallTimeoutSec = stallSec
	c.StableRunSec = 3600
	c.RestartMinSec = 1
	c.RestartMaxSec = 2
	return c
}

func TestRunOnceProcessExit(t *testing.T) {
	bin := fakeFFmpeg(t, `echo "something failed" >&2; exit 3`)
	s := testSupervisor(t, bin, 5)

	err := s.runOnce(context.Background(), s.cfg(), "rtsps://cam/stream")
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *exec.ExitError", err)
	}
	if ee.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3", ee.ExitCode())
	}
}

func TestRunOnceStallWatchdog(t *testing.T) {
	// Prints one heartbeat, then goes silent forever. The watchdog must
	// SIGTERM it shortly after the stall timeout.
	bin := fakeFFmpeg(t, `echo frame=1; sleep 60`)
	s := testSupervisor(t, bin, 1)

	start := time.Now()
	err := s.runOnce(context.Background(), s.cfg(), "rtsps://cam/stream")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want kill error from stalled ffmpeg")
	}
	if elapsed > 5*time.Second {
		t.Errorf("stall detection too slow: %v", elapsed)
	}
}

func TestRunOnceContextCancel(t *testing.T) {
	bin := fakeFFmpeg(t, `sleep 60`)
	s := testSupervisor(t, bin, 60)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := s.runOnce(ctx, s.cfg(), "rtsps://cam/stream")
	if err == nil {
		t.Fatal("want signal error after cancel")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("cancel did not stop ffmpeg promptly: %v", elapsed)
	}
}

func TestRunReloadSwitchesConfig(t *testing.T) {
	bin := fakeFFmpeg(t, `sleep 60`)
	s := testSupervisor(t, bin, 60)

	// The reload function switches to the HD profile.
	hd := testCfg2(bin, 60)
	hd.Quality = "hd"
	s.reload = func() (config.Config, error) { return hd, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Wait for the run loop to start, then fire the reload.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && s.cfg().Quality != "sd" {
		time.Sleep(20 * time.Millisecond)
	}
	s.Reload()
	for time.Now().Before(deadline) && s.cfg().Quality != "hd" {
		time.Sleep(20 * time.Millisecond)
	}
	if s.cfg().Quality != "hd" {
		t.Fatal("reload did not switch the active config")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestRunReturnsNilOnShutdown(t *testing.T) {
	bin := fakeFFmpeg(t, `sleep 60`)
	s := testSupervisor(t, bin, 60)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
