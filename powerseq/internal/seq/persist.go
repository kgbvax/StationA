package seq

// Phase persistence. The sequencer phase survives a process restart (a deploy,
// a crash, a power cut of the Pi): it is written to a state file (Config.StateFile,
// systemd StateDirectory) and restored by New.
//
// Restore rules:
//
//	idle | running | standby   restored as is, with fault, stop_reason and the
//	                           idle clocks (running/standby entry, last watch
//	                           change, last power-hold end), so the standby
//	                           timeout and the idle-shutdown clock keep counting
//	                           from the real events, not from the restart.
//	starting | stopping |      a sequence was in flight at a crash/kill. It is
//	entering_standby | waking  NOT re-run and nothing is switched: phase=idle
//	                           with fault "<step>: interrupted by a powerseq
//	                           restart (<phase>)" — the same end state as an
//	                           in-process abort (no rollback; the slots keep the
//	                           retained /cmd they last got). The operator decides:
//	                           start (re-run startup) or stop (idempotent teardown).
//	standby, but [[standby]]   idle with a fault (the wake list that would revive
//	no longer configured       the station is gone; start runs the full startup).
//	missing / corrupt file     idle, logged as Error (the phase is unknown).
//
// The auto_start chain record is saved with the phase. A chain that was
// running is not resumed: it comes back failed with "interrupted by a
// powerseq restart" (ready and failed come back as they were, so the ready
// window of the session hold keeps counting).
//
// A restart never emits a /cmd: Run publishes the restored /state and waits for
// inputs like before. The writes happen on the runner goroutine only (never in
// a paho handler): every phase/step change is written at once, before the
// first step of a sequence runs; clock-only changes (a watch moved, a power hold
// ended — set on the paho goroutine) are written by the idle tick at most every
// PersistEvery and flushed when Run exits.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// stateFileVersion is the state file schema version (a different version is
// treated like a corrupt file: idle + Error).
const stateFileVersion = 1

// DefaultPersistEvery is the write throttle for clock-only changes.
const DefaultPersistEvery = 10 * time.Second

// savedState is the state file content.
type savedState struct {
	Version      int       `json:"version"`
	Phase        string    `json:"phase"`
	Step         string    `json:"step,omitempty"`
	Fault        string    `json:"fault,omitempty"`
	StopReason   string    `json:"stop_reason,omitempty"`
	RunningSince time.Time `json:"running_since,omitzero"`
	StandbySince time.Time `json:"standby_since,omitzero"`
	LastWatch    time.Time `json:"last_watch,omitzero"`
	HoldEndedAt  time.Time `json:"hold_ended_at,omitzero"`
	// SessionHoldEndedAt is the last end of the 75auto session hold.
	SessionHoldEndedAt time.Time   `json:"session_hold_ended_at,omitzero"`
	Chain              *chainState `json:"chain,omitempty"`
	SavedAt            time.Time   `json:"saved_at"`
}

func (a savedState) samePhase(b savedState) bool {
	if (a.Chain == nil) != (b.Chain == nil) || (a.Chain != nil && !a.Chain.equal(*b.Chain)) {
		return false
	}
	return a.Phase == b.Phase && a.Step == b.Step && a.Fault == b.Fault && a.StopReason == b.StopReason
}

func (a savedState) sameClocks(b savedState) bool {
	return a.RunningSince.Equal(b.RunningSince) && a.StandbySince.Equal(b.StandbySince) &&
		a.LastWatch.Equal(b.LastWatch) && a.HoldEndedAt.Equal(b.HoldEndedAt) &&
		a.SessionHoldEndedAt.Equal(b.SessionHoldEndedAt)
}

func transitional(phase string) bool {
	switch phase {
	case PhaseStarting, PhaseStopping, PhaseEnteringStandby, PhaseWaking:
		return true
	}
	return false
}

func knownPhase(phase string) bool {
	switch phase {
	case PhaseIdle, PhaseRunning, PhaseStandby:
		return true
	}
	return transitional(phase)
}

// snapshot captures the persisted fields under mu.
func (s *Sequencer) snapshot() savedState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := savedState{
		Version:            stateFileVersion,
		Phase:              s.phase,
		Step:               s.step,
		Fault:              s.fault,
		StopReason:         s.stopReason,
		RunningSince:       s.runningSince,
		StandbySince:       s.standbySince,
		LastWatch:          s.lastWatch,
		HoldEndedAt:        s.holdEndedAt,
		SessionHoldEndedAt: s.sessionHoldEndedAt,
	}
	if s.chain.State != "" {
		c := s.chain
		st.Chain = &c
	}
	return st
}

// persist writes the state file when the snapshot differs from the last one
// written. clockTick (the idle tick) throttles clock-only changes to
// PersistEvery; a phase/step/fault change is always written at once. A failed
// write is logged once per snapshot (Error: a restart would restore an older
// phase) and retried with the next change. Runner goroutine only.
func (s *Sequencer) persist(now time.Time, clockTick bool) {
	if s.cfg.StateFile == "" {
		return
	}
	snap := s.snapshot()
	if s.saved != nil && snap.samePhase(*s.saved) {
		if snap.sameClocks(*s.saved) {
			return
		}
		if clockTick && now.Sub(s.savedAt) < s.persistEvery {
			return
		}
	}
	snap.SavedAt = now.UTC()
	if err := writeStateFile(s.cfg.StateFile, snap); err != nil {
		s.log.Errorf("state file %s: write failed, a restart would restore an older phase: %v", s.cfg.StateFile, err)
	}
	s.saved, s.savedAt = &snap, now
}

// restore loads the state file into a fresh Sequencer (New, before Run). See
// the file comment for the rules.
func (s *Sequencer) restore(now time.Time) {
	path := s.cfg.StateFile
	st, err := readStateFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		s.log.Errorf("state file %s missing: last phase unknown, starting idle (expected once, on the first start with a state file)", path)
		return
	}
	if err != nil {
		s.log.Errorf("state file %s unusable, last phase unknown, starting idle: %v", path, err)
		return
	}
	loaded := st
	s.saved, s.savedAt = &loaded, now

	s.stopReason = st.StopReason
	s.runningSince = st.RunningSince
	s.standbySince = st.StandbySince
	s.lastWatch = st.LastWatch
	s.holdEndedAt = st.HoldEndedAt
	s.sessionHoldEndedAt = st.SessionHoldEndedAt
	if st.Chain != nil {
		s.chain = *st.Chain
		if s.chain.State == ChainRunning {
			s.chain.State = ChainFailed
			s.chain.Fault = "interrupted by a powerseq restart"
			s.chain.TS = now
			s.log.Warnf("restart during an auto_start chain (step %q): not resumed, chain failed", st.Chain.Step)
		}
		// A ready chain inside its window holds the idle logic again; the
		// next idle check sees the hold and records its end.
		s.sessionHeld = s.chain.State == ChainReady
	}
	// Watch anchors are empty after a restart: the first value of each watch
	// is a baseline, and readbacks that settle right after the restart are not
	// activity.
	s.settleUntil = now.Add(s.cfg.WatchSettle)
	switch {
	case transitional(st.Phase):
		at := st.Step
		if at == "" {
			at = st.Phase
		}
		s.phase = PhaseIdle
		s.fault = fmt.Sprintf("%s: interrupted by a powerseq restart (%s)", at, st.Phase)
		s.log.Warnf("restart during phase %s (step %q): sequence not re-run, nothing switched; phase idle with fault, start or stop to recover", st.Phase, st.Step)
	case st.Phase == PhaseStandby && !s.standbyEnabled():
		s.phase = PhaseIdle
		s.fault = "standby: restored, but [[standby]]/[[wake]] are no longer configured"
		s.log.Warnf("restored phase standby without a standby/wake config: phase idle with fault, start or stop to recover")
	default:
		s.phase = st.Phase
		s.fault = st.Fault
	}
	s.log.Infof("restored phase=%s (saved %s as %s) from %s", s.phase, st.SavedAt.Format(time.RFC3339), st.Phase, path)
}

// readStateFile reads and checks a state file.
func readStateFile(path string) (savedState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return savedState{}, err
	}
	var st savedState
	if err := json.Unmarshal(b, &st); err != nil {
		return savedState{}, fmt.Errorf("decode: %w", err)
	}
	if st.Version != stateFileVersion {
		return savedState{}, fmt.Errorf("version %d, want %d", st.Version, stateFileVersion)
	}
	if !knownPhase(st.Phase) {
		return savedState{}, fmt.Errorf("unknown phase %q", st.Phase)
	}
	return st, nil
}

// writeStateFile writes st atomically: a temp file in the same directory,
// fsync, rename over path, then a best-effort fsync of the directory so the
// rename survives a power cut. A crash leaves either the old or the new file.
func writeStateFile(path string, st savedState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
