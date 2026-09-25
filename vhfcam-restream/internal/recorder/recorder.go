// SPDX-License-Identifier: AGPL-3.0-or-later

// Package recorder records the preview stream (overlay + radio audio, exactly
// what the :8083 page shows) without running an extra ffmpeg while recording:
// it follows the preview's HLS playlist and appends each finished MPEG-TS
// segment to a part file, then stream-copies the parts into one MP4 when the
// recording stops. Append-only TS parts stay playable after a crash; leftover
// in-progress recordings are finalized at the next startup.
//
// Limits: a recording stops itself after record.max_minutes, and when free
// space on the recordings filesystem drops below record.min_free_gb; a start
// is refused unless there is room for a full-length recording on top of that
// floor. The oldest finished recordings are deleted to keep the folder under
// record.max_total_gb.
package recorder

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vhfcam-restream/internal/config"
)

// State is the recorder's state.
type State string

const (
	Idle       State = "idle"
	Recording  State = "recording"
	Finalizing State = "finalizing" // stopping: last segments + MP4 remux
)

var (
	ErrDisabled     = errors.New("recording is disabled (needs record.enabled and preview.enabled)")
	ErrBusy         = errors.New("a recording is already running or being finalized")
	ErrNoPreview    = errors.New("the preview stream is not running")
	ErrLowDisk      = errors.New("not enough free disk space")
	ErrNotRecording = errors.New("no recording is running")
	ErrActive       = errors.New("the recording is still being written")
)

const (
	// preroll segments already in the playlist at start (~6 s): about where
	// the page's player shows live, so the file starts at what the operator saw.
	preroll = 3
	// estBytesPerSec estimates the preview stream: x264 maxrate 2500 kbit/s
	// + AAC. Used to reserve room for a full-length recording.
	estBytesPerSec = (2_500_000 + 128_000) / 8
	// startRoomFactor: during the remux the TS parts and the MP4 both exist.
	startRoomFactor  = 2
	freeCheckEvery   = 5 * time.Second
	finalizeTimeout  = 5 * time.Minute
	shutdownFinalize = 20 * time.Second
)

// Status is the recorder state for the page and the console.
type Status struct {
	Enabled        bool       `json:"enabled"`
	State          State      `json:"state"`
	Name           string     `json:"name,omitempty"` // file the active recording becomes
	Started        *time.Time `json:"started,omitempty"`
	ElapsedS       int        `json:"elapsed_s"`
	RemainingS     int        `json:"remaining_s"`
	MaxS           int        `json:"max_s"`
	FreeBytes      uint64     `json:"free_bytes"`
	MinFreeBytes   uint64     `json:"min_free_bytes"`
	Stalled        bool       `json:"stalled"` // no new preview segment for a while
	Parts          int        `json:"parts"`
	MissedSegments int        `json:"missed_segments"`
	StopReason     string     `json:"stop_reason,omitempty"` // operator | max_duration | low_disk | shutdown | recovered
	LastError      string     `json:"last_error,omitempty"`
	LastFile       string     `json:"last_file,omitempty"`
}

// Listing is the status plus the finished recordings, newest first.
type Listing struct {
	Status     Status `json:"status"`
	Files      []File `json:"files"`
	TotalBytes uint64 `json:"total_bytes"`
	CapBytes   uint64 `json:"cap_bytes"`
}

type session struct {
	base    string // name without extension
	dir     string // .inprogress/<base>
	outDir  string // record.dir at start
	started time.Time
	fol     follower
	part    int // current part number; 0 = none opened yet
	out     *os.File
	lastSeg time.Time
	missed  int
}

// Recorder records the preview stream. Create with New, run Run in a
// goroutine, drive with Start/Stop.
type Recorder struct {
	cfgFn    func() config.Config
	log      *slog.Logger
	freqFn   func() (int64, bool)
	onActive func(active bool)

	// test hooks
	now       func() time.Time
	free      func(path string) (uint64, error)
	pollEvery time.Duration

	wake chan struct{}

	mu         sync.Mutex
	state      State
	starting   bool
	sess       *session
	stopReason string
	lastError  string
	lastFile   string
	freeCache  uint64
	freeAt     time.Time
	parts      int
	missed     int
	stalled    bool
}

// New creates a recorder reading the freshest config from cfgFn.
func New(cfgFn func() config.Config, log *slog.Logger) *Recorder {
	return &Recorder{
		cfgFn:     cfgFn,
		log:       log,
		now:       time.Now,
		free:      freeBytes,
		pollEvery: time.Second,
		wake:      make(chan struct{}, 1),
		state:     Idle,
	}
}

// WithFreq sets the source of the radio frequency used in file names.
func (r *Recorder) WithFreq(fn func() (hz int64, ok bool)) *Recorder { r.freqFn = fn; return r }

// WithOnActive sets a callback for recording start (true) and end (false) —
// used to hold the radio-audio demand while recording.
func (r *Recorder) WithOnActive(fn func(active bool)) *Recorder { r.onActive = fn; return r }

func gb(v float64) uint64 { return uint64(v * 1e9) }

func (r *Recorder) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run finalizes leftovers from a previous run, then follows the preview while
// recording until ctx ends; an active recording is finalized on the way out.
func (r *Recorder) Run(ctx context.Context) error {
	rc := r.cfgFn().Record
	if err := os.MkdirAll(filepath.Join(rc.Dir, inprogressDir), 0750); err != nil && rc.Enabled {
		r.log.Warn("recordings dir create failed", "dir", rc.Dir, "err", err)
	}
	r.recoverLeftovers()
	r.retention(0)
	r.refreshFree(true)

	t := time.NewTicker(r.pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.shutdown()
			return nil
		case <-t.C:
		case <-r.wake:
		}
		r.step()
	}
}

func (r *Recorder) step() {
	r.refreshFree(false)
	r.mu.Lock()
	st, s := r.state, r.sess
	r.mu.Unlock()
	if s == nil {
		return
	}
	if st == Recording {
		r.poll(s)
		r.checkLimits(s)
		r.mu.Lock()
		st = r.state
		r.mu.Unlock()
	}
	if st == Finalizing {
		r.finish(s, finalizeTimeout)
	}
}

// poll copies the preview segments that are new since the last poll.
func (r *Recorder) poll(s *session) {
	cfg := r.cfgFn().Preview
	b, err := os.ReadFile(filepath.Join(cfg.Dir, "live.m3u8"))
	if err == nil {
		if pl, ok := parsePlaylist(b); ok {
			mtime := func(name string) (time.Time, bool) {
				info, err := os.Stat(filepath.Join(cfg.Dir, name))
				if err != nil {
					return time.Time{}, false
				}
				return info.ModTime(), true
			}
			segs, reset, missed := s.fol.scan(pl, preroll, mtime)
			s.missed += missed
			if reset && s.out != nil {
				r.log.Info("preview restarted — starting a new recording part", "part", s.part+1)
				_ = s.out.Close()
				s.out = nil
			}
			for _, name := range segs {
				if err := r.appendSegment(s, filepath.Join(cfg.Dir, name), name); err != nil {
					s.missed++
					r.log.Warn("segment not recorded", "segment", name, "err", err)
				}
			}
		}
	}
	stallAfter := time.Duration(3*cfg.HlsTimeS)*time.Second + time.Second
	r.mu.Lock()
	r.parts = s.part
	r.missed = s.missed
	r.stalled = r.now().Sub(s.lastSeg) > stallAfter
	r.mu.Unlock()
}

func (r *Recorder) appendSegment(s *session, path, name string) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if s.out == nil {
		s.part++
		out, err := os.OpenFile(filepath.Join(s.dir, partName(s.part)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
		if err != nil {
			s.part--
			return err
		}
		s.out = out
	}
	if _, err := io.Copy(s.out, in); err != nil {
		return err
	}
	s.fol.markTaken(name, info.ModTime())
	s.lastSeg = r.now()
	return nil
}

func (r *Recorder) checkLimits(s *session) {
	rc := r.cfgFn().Record
	if r.now().Sub(s.started) >= time.Duration(rc.MaxMinutes)*time.Minute {
		r.requestStop("max_duration")
		return
	}
	r.mu.Lock()
	free, known := r.freeCache, !r.freeAt.IsZero()
	r.mu.Unlock()
	if known && free < gb(rc.MinFreeGB) {
		r.requestStop("low_disk")
	}
}

func (r *Recorder) requestStop(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == Recording {
		r.state = Finalizing
		r.stopReason = reason
		r.log.Info("recording stopping", "reason", reason, "name", r.sess.base)
	}
}

// finish copies the last segments, remuxes and returns to idle.
func (r *Recorder) finish(s *session, timeout time.Duration) {
	r.poll(s)
	if s.out != nil {
		_ = s.out.Close()
		s.out = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	name, err := finalize(ctx, r.cfgFn().FFmpegBin, s.dir, s.outDir, s.base)
	r.retention(0)
	r.refreshFree(true)

	r.mu.Lock()
	r.state = Idle
	r.sess = nil
	r.lastFile = name
	r.lastError = ""
	if err != nil {
		r.lastError = err.Error()
	}
	reason := r.stopReason
	r.mu.Unlock()

	if err != nil {
		r.log.Warn("recording finalized with error", "file", name, "reason", reason, "err", err)
	} else {
		r.log.Info("recording finished", "file", name, "reason", reason, "missed_segments", s.missed)
	}
	if r.onActive != nil {
		r.onActive(false)
	}
}

func (r *Recorder) shutdown() {
	r.mu.Lock()
	s := r.sess
	if r.state == Recording {
		r.state = Finalizing
		r.stopReason = "shutdown"
	}
	r.mu.Unlock()
	if s != nil {
		r.finish(s, shutdownFinalize)
	}
}

// recoverLeftovers finalizes in-progress recordings left by a crash, power
// loss or a shutdown that ran out of time.
func (r *Recorder) recoverLeftovers() {
	rc := r.cfgFn().Record
	root := filepath.Join(rc.Dir, inprogressDir)
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) == 0 {
		return
	}
	r.mu.Lock()
	r.state = Finalizing
	r.mu.Unlock()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		base := e.Name()
		if m, err := readMeta(dir); err == nil && ValidName(m.Name+".mp4") {
			base = m.Name
		}
		if !ValidName(base + ".mp4") {
			r.log.Warn("unrecognized in-progress directory left alone", "dir", dir)
			continue
		}
		base = uniqueBase(rc.Dir, base, false)
		ctx, cancel := context.WithTimeout(context.Background(), finalizeTimeout)
		name, err := finalize(ctx, r.cfgFn().FFmpegBin, dir, rc.Dir, base)
		cancel()
		r.mu.Lock()
		r.lastFile, r.stopReason, r.lastError = name, "recovered", ""
		if err != nil {
			r.lastError = err.Error()
		}
		r.mu.Unlock()
		r.log.Info("recovered interrupted recording", "file", name, "err", err)
	}
	r.mu.Lock()
	r.state = Idle
	r.mu.Unlock()
}

func (r *Recorder) retention(reserve uint64) {
	rc := r.cfgFn().Record
	deleted, err := enforceCap(rc.Dir, gb(rc.MaxTotalGB), reserve)
	for _, f := range deleted {
		r.log.Info("deleted oldest recording to stay under max_total_gb", "file", f.Name, "bytes", f.Size)
	}
	if err != nil {
		r.log.Warn("retention failed", "err", err)
	}
}

func (r *Recorder) refreshFree(force bool) {
	r.mu.Lock()
	due := force || r.now().Sub(r.freeAt) >= freeCheckEvery
	r.mu.Unlock()
	if !due {
		return
	}
	dir := r.cfgFn().Record.Dir
	free, err := r.free(dir)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.freeAt = time.Time{} // unknown: never auto-stop on a failed check
		return
	}
	r.freeCache, r.freeAt = free, r.now()
}

func estBytes(maxMinutes int) uint64 { return uint64(maxMinutes) * 60 * estBytesPerSec }

// Start begins a recording. Refused when disabled, busy, the preview is not
// producing segments, or free space would fall below min_free_gb during a
// full-length recording.
func (r *Recorder) Start() (Status, error) {
	err := r.start()
	if err == nil {
		if r.onActive != nil {
			r.onActive(true)
		}
		r.poke()
	}
	return r.Status(), err
}

func (r *Recorder) start() error {
	cfg := r.cfgFn()
	rc := cfg.Record
	if !rc.Enabled || !cfg.Preview.Enabled {
		return ErrDisabled
	}
	// Claim the start under the lock, then do the filesystem work without
	// it (retention may delete large files; status polls must not block).
	r.mu.Lock()
	if r.state != Idle || r.starting {
		r.mu.Unlock()
		return ErrBusy
	}
	r.starting = true
	r.mu.Unlock()

	sess, free, err := r.prepare(cfg)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.starting = false
	if err != nil {
		r.lastError = err.Error()
		return err
	}
	r.freeCache, r.freeAt = free, r.now()
	r.sess = sess
	r.state = Recording
	r.stopReason, r.lastError, r.lastFile = "", "", ""
	r.parts, r.missed, r.stalled = 0, 0, false
	r.log.Info("recording started", "name", sess.base, "max_minutes", rc.MaxMinutes, "free_bytes", free)
	return nil
}

// prepare checks the preconditions and creates the in-progress directory.
func (r *Recorder) prepare(cfg config.Config) (*session, uint64, error) {
	rc := cfg.Record
	fresh := time.Duration(3*cfg.Preview.HlsTimeS)*time.Second + 2*time.Second
	if info, err := os.Stat(filepath.Join(cfg.Preview.Dir, "live.m3u8")); err != nil || r.now().Sub(info.ModTime()) > fresh {
		return nil, 0, ErrNoPreview
	}
	if err := os.MkdirAll(filepath.Join(rc.Dir, inprogressDir), 0750); err != nil {
		return nil, 0, err
	}
	est := estBytes(rc.MaxMinutes)
	r.retention(est)
	free, err := r.free(rc.Dir)
	if err != nil {
		return nil, 0, err
	}
	if free < gb(rc.MinFreeGB)+startRoomFactor*est {
		return nil, free, ErrLowDisk
	}

	var hz int64
	if r.freqFn != nil {
		if f, ok := r.freqFn(); ok {
			hz = f
		}
	}
	start := r.now()
	base := uniqueBase(rc.Dir, baseName(start, hz), true)
	dir := filepath.Join(rc.Dir, inprogressDir, base)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, free, err
	}
	if err := writeMeta(dir, meta{Name: base, Started: start.UTC(), FreqHz: hz}); err != nil {
		_ = os.RemoveAll(dir)
		return nil, free, err
	}
	return &session{base: base, dir: dir, outDir: rc.Dir, started: start, lastSeg: start}, free, nil
}

// Stop ends the running recording; finalizing continues in the background
// (state "finalizing" until the MP4 is written).
func (r *Recorder) Stop() (Status, error) {
	r.mu.Lock()
	if r.state != Recording {
		r.mu.Unlock()
		return r.Status(), ErrNotRecording
	}
	r.mu.Unlock()
	r.requestStop("operator")
	r.poke()
	return r.Status(), nil
}

// Status reports the current state.
func (r *Recorder) Status() Status {
	cfg := r.cfgFn()
	rc := cfg.Record
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Status{
		Enabled:        rc.Enabled && cfg.Preview.Enabled,
		State:          r.state,
		MaxS:           rc.MaxMinutes * 60,
		FreeBytes:      r.freeCache,
		MinFreeBytes:   gb(rc.MinFreeGB),
		Stalled:        r.stalled && r.state == Recording,
		Parts:          r.parts,
		MissedSegments: r.missed,
		StopReason:     r.stopReason,
		LastError:      r.lastError,
		LastFile:       r.lastFile,
	}
	if s := r.sess; s != nil {
		st.Name = s.base + ".mp4"
		started := s.started.UTC()
		st.Started = &started
		st.ElapsedS = int(r.now().Sub(s.started).Seconds())
		st.RemainingS = st.MaxS - st.ElapsedS
		if st.RemainingS < 0 {
			st.RemainingS = 0
		}
	}
	return st
}

// List returns the status and the finished recordings, newest first.
func (r *Recorder) List() (Listing, error) {
	rc := r.cfgFn().Record
	files, err := listFiles(rc.Dir)
	if files == nil {
		files = []File{}
	}
	return Listing{Status: r.Status(), Files: files, TotalBytes: totalSize(files), CapBytes: gb(rc.MaxTotalGB)}, err
}

// Open opens a finished recording for download.
func (r *Recorder) Open(name string) (*os.File, os.FileInfo, error) {
	return openFile(r.cfgFn().Record.Dir, name)
}

// Delete removes a finished recording.
func (r *Recorder) Delete(name string) error {
	r.mu.Lock()
	if s := r.sess; s != nil && (name == s.base+".mp4" || name == s.base+".ts" || strings.HasPrefix(name, s.base+"_p")) {
		r.mu.Unlock()
		return ErrActive
	}
	r.mu.Unlock()
	err := deleteFile(r.cfgFn().Record.Dir, name)
	if err == nil {
		r.log.Info("recording deleted", "file", name)
	}
	return err
}
