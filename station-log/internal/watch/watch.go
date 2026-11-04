// Package watch is the ADIF drop-directory feeder: every interval it lists the
// *.adi / *.adif files of one directory (non-recursive) and imports each file
// it has not imported yet through the same importer as POST /import (merge
// semantics, never replace_source). A file's identity is name + size + mtime,
// remembered in the settings table so a restart does not re-import. Files are
// never deleted or moved.
package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"codeberg.org/kgbvax/stationa/shared/logging"

	"station-log/internal/importer"
	"station-log/internal/store"
)

// StableAge is how long a file's mtime must lie in the past before it is
// imported (a file still being written is skipped).
const StableAge = 5 * time.Second

const settingKey = "watch_imported"

// Status is the /state.watch object.
type Status struct {
	Dir      string    `json:"dir"`
	LastFile string    `json:"last_file,omitempty"`
	LastAt   time.Time `json:"last_at,omitzero"`
	Files    int       `json:"files"`  // files imported since start
	Failed   int       `json:"failed"` // failed file imports since start
	Added    int       `json:"added"`  // QSOs added since start
	Merged   int       `json:"merged"` // QSOs merged since start
	Error    string    `json:"error,omitempty"`
}

type failure struct{ id, msg string }

// Watcher is the feeder.
type Watcher struct {
	Dir      string
	Interval time.Duration
	Source   string
	Store    *store.Store
	Importer *importer.Importer
	Log      *slog.Logger
	Now      func() time.Time
	// OnChange is called after a scan changed the status (may be nil).
	OnChange func()

	// Ready, when non-nil, delays the first scan until it is closed (the
	// service closes it once the MQTT link is up, so import events are not
	// dropped for lack of a publisher).
	Ready <-chan struct{}

	mu      sync.Mutex
	status  Status
	failed  map[string]failure // Scan only: files whose last import failed
	invalid map[string]string  // Scan only: imported files that had invalid records
	streak  logging.Streak
}

// Status returns a copy of the current status.
func (w *Watcher) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.status
	s.Dir = w.Dir
	return s
}

func identity(fi os.FileInfo) string {
	return fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
}

func (w *Watcher) loadSeen() map[string]string {
	seen := map[string]string{}
	if v, ok, err := w.Store.Setting(settingKey); err == nil && ok {
		_ = json.Unmarshal([]byte(v), &seen)
	}
	return seen
}

func (w *Watcher) saveSeen(seen map[string]string) error {
	b, _ := json.Marshal(seen)
	return w.Store.SetSetting(settingKey, string(b))
}

func isADIF(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".adi", ".adif":
		return true
	}
	return false
}

// Run waits for Ready, scans immediately and then every Interval until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	if w.Ready != nil {
		select {
		case <-w.Ready:
		case <-ctx.Done():
			return
		}
	}
	w.Scan(ctx)
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Scan(ctx)
		}
	}
}

// Scan does one pass over the directory. The status error is computed once at
// the end of the pass from that pass's failures, so a later good file cannot
// mask an earlier bad one. A file that failed stays failed (error visible, not
// retried, not counted again) until its name+size+mtime changes or it is gone.
func (w *Watcher) Scan(ctx context.Context) {
	if w.Now == nil {
		w.Now = time.Now
	}
	w.mu.Lock()
	before := w.status
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		changed := w.status != before
		w.mu.Unlock()
		if changed && w.OnChange != nil {
			w.OnChange()
		}
	}()
	entries, err := os.ReadDir(w.Dir)
	if err != nil {
		w.streak.Fail(w.Log, "ADIF drop directory unreadable", "dir", w.Dir, "err", err)
		w.setError("directory unreadable: " + err.Error())
		return
	}
	seen := w.loadSeen()
	present := map[string]bool{}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !isADIF(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if w.failed == nil {
		w.failed = map[string]failure{}
		w.invalid = map[string]string{}
	}
	dirty := false
	var failMsgs []string // this pass's failures, in name order
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		present[name] = true
		path := filepath.Join(w.Dir, name)
		fi, err := os.Stat(path)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		id := identity(fi)
		if seen[name] == id {
			delete(w.failed, name)
			continue
		}
		if f, ok := w.failed[name]; ok {
			if f.id == id {
				failMsgs = append(failMsgs, name+": "+f.msg) // unchanged: not retried
				continue
			}
			delete(w.failed, name) // the file changed
		}
		if w.Now().Sub(fi.ModTime()) < StableAge {
			continue // still being written
		}
		rep, err := w.importFile(ctx, path)
		if err != nil {
			w.streak.Fail(w.Log, "ADIF drop file import failed; file left in place", "file", name, "err", err)
			w.failed[name] = failure{id: id, msg: err.Error()}
			failMsgs = append(failMsgs, name+": "+err.Error())
			w.mu.Lock()
			w.status.Failed++
			w.status.LastFile, w.status.LastAt = name, w.Now().UTC()
			w.mu.Unlock()
			continue
		}
		seen[name] = id
		dirty = true
		w.mu.Lock()
		w.status.Files++
		w.status.Added += rep.Added
		w.status.Merged += rep.Merged
		w.status.LastFile, w.status.LastAt = name, w.Now().UTC()
		w.mu.Unlock()
		delete(w.invalid, name)
		if rep.SkippedInvalid > 0 {
			w.invalid[name] = fmt.Sprintf("%s: %d of %d records invalid", name, rep.SkippedInvalid, rep.Records)
		}
		w.Log.Info("ADIF drop file imported", "file", name, "records", rep.Records, "added", rep.Added, "merged", rep.Merged, "invalid", rep.SkippedInvalid)
	}
	for name := range w.failed { // forget failures of files that are gone
		if !present[name] {
			delete(w.failed, name)
		}
	}
	invalidMsg := ""
	for _, name := range names { // invalid-record warnings stay until the file is re-imported or gone
		if m, ok := w.invalid[name]; ok && present[name] {
			invalidMsg = m
			break
		}
	}
	for name := range w.invalid {
		if !present[name] {
			delete(w.invalid, name)
		}
	}
	for name := range seen { // forget files that are gone
		if !present[name] {
			delete(seen, name)
			dirty = true
		}
	}
	if dirty {
		if err := w.saveSeen(seen); err != nil {
			w.Log.Warn("remembering imported drop files failed", "err", err)
		}
	}
	switch {
	case len(failMsgs) > 0:
		w.setError(failMsgs[0])
	case invalidMsg != "":
		w.streak.Reset()
		w.setError(invalidMsg)
	default:
		w.streak.Reset()
		w.setError("")
	}
}

func (w *Watcher) importFile(ctx context.Context, path string) (importer.Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return importer.Report{}, err
	}
	rep, err := w.Importer.Import(ctx, string(b), importer.Options{Source: w.Source})
	if err != nil {
		return rep, err
	}
	if rep.Records == 0 {
		return rep, errors.New("no ADIF records found")
	}
	return rep, nil
}

func (w *Watcher) setError(msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status.Error = msg
}
