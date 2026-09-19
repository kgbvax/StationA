// SPDX-License-Identifier: AGPL-3.0-or-later

// Package restream supervises a single ffmpeg process that copies the camera
// RTSPS stream to an RTMP target (YouTube Live).
package restream

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"vhfcam-restream/internal/config"
)

// reloadFn re-reads the config (SIGHUP path).
type reloadFn func() (config.Config, error)

// Supervisor keeps one ffmpeg process running against the configured source,
// restarting it with exponential backoff when it exits and killing it when it
// stalls. A SIGHUP (Reload) re-reads the config and restarts the stream — that
// is how the sd/hd profile is switched live.
type Supervisor struct {
	log       *slog.Logger
	reload    reloadFn
	cfgPtr    atomic.Pointer[config.Config]
	reloadC   chan struct{}          // SIGHUP tokens
	wake      chan struct{}          // wakes the backoff wait when a reload lands
	reloadHit atomic.Bool            // set when the current run ended due to a reload
	curCancel atomic.Pointer[context.CancelFunc] // cancels the in-flight run, if any
	lastBeat  atomic.Int64           // unix nanos of the last ffmpeg progress line
}

// New builds a Supervisor. reload is called on SIGHUP to re-read the config.
func New(cfg config.Config, reload reloadFn, log *slog.Logger) *Supervisor {
	s := &Supervisor{
		log:     log,
		reload:  reload,
		reloadC: make(chan struct{}, 1),
		wake:    make(chan struct{}, 1),
	}
	s.cfgPtr.Store(&cfg)
	return s
}

// Reload requests a config re-read and stream restart (SIGHUP). Coalesces.
func (s *Supervisor) Reload() {
	select {
	case s.reloadC <- struct{}{}:
	default:
	}
}

// cfg returns the current config.
func (s *Supervisor) cfg() *config.Config { return s.cfgPtr.Load() }

// Run loops until ctx is cancelled (SIGTERM/SIGINT). It returns nil on clean
// shutdown; it never returns per-stream ffmpeg errors — those are logged and
// backed off.
func (s *Supervisor) Run(ctx context.Context) error {
	// One reload service goroutine for the whole run: consumes SIGHUP tokens,
	// re-reads the config, and kills the in-flight ffmpeg so the loop restarts
	// with the new config.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.reloadC:
				nc, err := s.reload()
				if err != nil {
					s.log.Error("reload failed — keeping current config and stream", "err", err)
					continue
				}
				s.cfgPtr.Store(&nc)
				s.reloadHit.Store(true)
				s.log.Info("config reloaded", "quality", nc.Quality)
				if cancel := s.curCancel.Load(); cancel != nil {
					(*cancel)() // no-op when no run is in flight (e.g. mid-backoff)
				}
				select {
				case s.wake <- struct{}{}:
				default:
				}
			}
		}
	}()

	backoff := time.Duration(s.cfg().RestartMinSec) * time.Second
	for {
		cfg := s.cfg()
		src, profile := cfg.EffectiveSource()
		s.log.Info("starting stream",
			"profile", profile,
			"source", RedactURL(src),
			"target", RedactURL(cfg.YouTubeURL))

		runCtx, cancel := context.WithCancel(ctx)
		s.curCancel.Store(&cancel)
		start := time.Now()
		err := s.runOnce(runCtx, cfg, src)
		cancel()
		if ctx.Err() != nil {
			return nil // service shutdown, not a stream failure
		}

		if s.reloadHit.Swap(false) {
			backoff = time.Duration(cfg.RestartMinSec) * time.Second
			continue // restart immediately with the freshly-loaded config
		}

		runtime := time.Since(start)
		if runtime >= time.Duration(cfg.StableRunSec)*time.Second {
			backoff = time.Duration(cfg.RestartMinSec) * time.Second
		}
		s.log.Warn("ffmpeg exited; restarting",
			"err", err,
			"runtime", runtime.Round(time.Second),
			"next_retry_in", backoff)

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		case <-s.wake: // reload landed while backing off — restart now
		}
		backoff *= 2
		if max := time.Duration(cfg.RestartMaxSec) * time.Second; backoff > max {
			backoff = max
		}
	}
}

// runOnce runs ffmpeg until it exits or ctx is cancelled. It returns when the
// process is gone (pipes drained).
func (s *Supervisor) runOnce(ctx context.Context, cfg *config.Config, src string) error {
	args := BuildArgs(cfg, src)
	s.log.Info("exec",
		"bin", cfg.FFmpegBin,
		"args", strings.Join(RedactArgs(args, src, cfg.StreamKey), " "))

	cmd := exec.CommandContext(ctx, cfg.FFmpegBin, args...)
	// SIGTERM (not the default SIGKILL) so ffmpeg tears the RTMP session down
	// cleanly; WaitDelay escalates to a kill if it ignores it.
	sigterm := func() error {
		if cmd.Process != nil {
			return cmd.Process.Signal(syscall.SIGTERM)
		}
		return nil
	}
	cmd.Cancel = sigterm
	cmd.WaitDelay = 10 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", cfg.FFmpegBin, err)
	}

	// Any stdout line (-progress output) is a heartbeat.
	s.lastBeat.Store(time.Now().UnixNano())
	var pumpWg sync.WaitGroup
	pumpWg.Add(2)
	go func() { defer pumpWg.Done(); s.pumpProgress(stdout) }()
	go func() { defer pumpWg.Done(); s.pumpStderr(stderr) }()

	// Stall watchdog: no heartbeat for stallTimeout -> SIGTERM. Exits when the
	// process is done (procDone, closed by runOnce below) or the run context
	// is cancelled. procDone has a single closer — runOnce.
	procDone := make(chan struct{})
	go func() {
		stall := time.Duration(cfg.StallTimeoutSec) * time.Second
		// Tick at stall/4 so detection latency tracks the configured timeout,
		// clamped to [100ms, 5s] for production values.
		tick := stall / 4
		if tick < 100*time.Millisecond {
			tick = 100 * time.Millisecond
		}
		if tick > 5*time.Second {
			tick = 5 * time.Second
		}
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-procDone:
				return
			case <-t.C:
				silent := time.Since(time.Unix(0, s.lastBeat.Load()))
				if silent > stall {
					s.log.Error("ffmpeg stalled — no progress; terminating",
						"silent_for", silent.Round(time.Second))
					_ = sigterm()
				}
			}
		}
	}()

	waitErr := cmd.Wait()
	close(procDone)
	pumpWg.Wait() // bounded by WaitDelay: the pipes are closed after it elapses
	return waitErr
}

func (s *Supervisor) pumpProgress(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var streamTime string
	var lastBeatLog time.Time
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		s.lastBeat.Store(time.Now().UnixNano())
		if t, ok := strings.CutPrefix(line, "out_time_us="); ok {
			streamTime = t
		}
		if line == "progress=continue" && time.Since(lastBeatLog) >= time.Minute {
			lastBeatLog = time.Now()
			s.log.Info("streaming", "stream_time_us", streamTime)
		}
	}
}

func (s *Supervisor) pumpStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		low := strings.ToLower(line)
		if strings.Contains(low, "error") || strings.Contains(low, "failed") || strings.Contains(low, "unable") {
			s.log.Warn("ffmpeg", "msg", line)
		} else {
			s.log.Info("ffmpeg", "msg", line)
		}
	}
}
