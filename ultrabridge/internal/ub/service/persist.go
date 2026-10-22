package service

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Retract-latch persistence. The RCU-06 status reply carries no retract flag,
// so `retracted` is bridge-side knowledge. Without a record a bridge restart
// after an antenna-mode unlock (or while the RCU-06 is unpowered under the
// lock) would publish retracted:false with the elements still in — the PA arm
// would open — and the SetFrequency deadband would swallow the band-follow
// re-tune that brings them out. The record holds the published latch and the
// deadband bypass (extendDue); it is rewritten on every change.

// latchRecord is the on-disk retract latch (<state_dir>/retract.json).
type latchRecord struct {
	Retracted bool `json:"retracted"`
	ExtendDue bool `json:"extend_due"`
}

// PersistTo loads the retract latch from path and keeps it updated there.
// Call it once at startup, before the poll loop or any command runs. A missing
// file is a fresh start (not retracted); an unreadable one is logged and
// ignored.
func (c *Controller) PersistTo(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.latchPath = path
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var rec latchRecord
	if err == nil {
		err = json.Unmarshal(data, &rec)
	}
	if err != nil {
		c.log.Warn("retract latch record unreadable, ignoring", "path", path, "err", err)
		return
	}
	c.state.Retracted = rec.Retracted
	c.extendDue = rec.ExtendDue || rec.Retracted
	if c.extendDue {
		c.log.Info("retract latch restored", "retracted", rec.Retracted, "extend_due", c.extendDue)
	}
}

// updateLatch applies fn to the retract latch (state.Retracted) and the
// deadband bypass (extendDue) under c.mu and persists the result when it
// changed. Never call it with c.mu or c.lmu held. It writes a small file: call
// it from the poll loop, the /cmd worker or HTTP handlers, never from a paho
// handler.
func (c *Controller) updateLatch(fn func(retracted, extendDue *bool)) {
	c.mu.Lock()
	r, e := c.state.Retracted, c.extendDue
	fn(&c.state.Retracted, &c.extendDue)
	changed := r != c.state.Retracted || e != c.extendDue
	c.mu.Unlock()
	if changed {
		c.saveLatch()
	}
}

// saveLatch writes the current latch atomically (temp file + rename). The
// values are read under saveMu, so the last write always carries the latest
// latch. A failure is logged; the in-memory latch stays authoritative.
func (c *Controller) saveLatch() {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.RLock()
	rec, path := latchRecord{Retracted: c.state.Retracted, ExtendDue: c.extendDue}, c.latchPath
	c.mu.RUnlock()
	if path == "" {
		return
	}
	data, err := json.Marshal(rec)
	if err == nil {
		err = writeFileAtomic(path, data)
	}
	if err != nil {
		c.log.Warn("retract latch record not saved", "path", path, "err", err)
	}
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(data, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}
