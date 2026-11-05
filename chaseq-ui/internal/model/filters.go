package model

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"chaseq-ui/internal/bus"
)

// Filters narrow what is shown. They never change chase-queue's ranking.
type Filters struct {
	Bands      []string `json:"bands,omitempty"` // empty = every band
	NeededOnly bool     `json:"needed_only,omitempty"`
	// Awards keeps the rows with a need for one of these award codes; empty
	// = no award filter.
	Awards []string `json:"awards,omitempty"`
	// Source keeps one row source: "cluster" or "bmrec"; "" = both.
	Source string `json:"source,omitempty"`
	// Modes are the active mode classes (ModeCW, ModePhone, ModeData). nil =
	// the default, phone only; an explicit list is persisted as such, so
	// "all three" survives a restart. A toggle never leaves the list empty.
	Modes []string `json:"modes,omitempty"`
	// LockBand makes Bands follow the band the radio reports (a single band);
	// the manual band chips are ignored meanwhile. Absent in old files = off.
	LockBand bool `json:"lock_band,omitempty"`
}

// Mode classes of the mode filter.
const (
	ModeCW    = "cw"
	ModePhone = "phone"
	ModeData  = "data"
)

// ModeClasses lists the classes in display order.
var ModeClasses = []string{ModeCW, ModePhone, ModeData}

// ModeClass maps a canonical mode to its class; "" = unknown (never hidden).
func ModeClass(mode string) string {
	switch mode {
	case "cw":
		return ModeCW
	case "usb", "lsb", "am", "fm":
		return ModePhone
	case "data":
		return ModeData
	}
	return ""
}

// ActiveModes returns the effective set of active classes (default: phone).
func (f Filters) ActiveModes() []string {
	if len(f.Modes) == 0 {
		return []string{ModePhone}
	}
	return f.Modes
}

func (f Filters) modesDefault() bool {
	m := f.ActiveModes()
	return len(m) == 1 && m[0] == ModePhone
}

// hasMode reports whether a row with this canonical mode passes; a row whose
// mode is empty or unknown always passes.
func (f Filters) hasMode(mode string) bool {
	c := ModeClass(mode)
	if c == "" {
		return true
	}
	for _, x := range f.ActiveModes() {
		if x == c {
			return true
		}
	}
	return false
}

// ToggleMode returns f with a mode class switched on or off. Switching off
// the last class shows all classes instead of nothing.
func (f Filters) ToggleMode(class string) Filters {
	var out []string
	found := false
	for _, c := range f.ActiveModes() {
		if c == class {
			found = true
			continue
		}
		out = append(out, c)
	}
	if !found {
		out = append(out, class)
	}
	if len(out) == 0 {
		out = append(out, ModeClasses...)
	}
	sort.Strings(out)
	f.Modes = out
	return f
}

// OnlyMode returns f showing just one mode class; "all" shows every class.
func (f Filters) OnlyMode(class string) Filters {
	if class == "all" {
		f.Modes = append([]string(nil), ModeClasses...)
		sort.Strings(f.Modes)
		return f
	}
	f.Modes = []string{class}
	return f
}

// AllModes reports whether every mode class is active.
func (f Filters) AllModes() bool { return len(f.ActiveModes()) == len(ModeClasses) }

// Active reports whether any filter differs from the default (no band, award
// or source filter, not needed-only, phone only).
func (f Filters) Active() bool {
	return (len(f.Bands) > 0 && !f.LockBand) || f.NeededOnly || len(f.Awards) > 0 || f.Source != "" || !f.modesDefault()
}

// hasAward reports whether a row with these needs passes the award filter.
func (f Filters) hasAward(needs []bus.Need) bool {
	if len(f.Awards) == 0 {
		return true
	}
	for _, n := range needs {
		for _, a := range f.Awards {
			if n.Award == a {
				return true
			}
		}
	}
	return false
}

func (f Filters) hasSource(cluster bool) bool {
	switch f.Source {
	case bus.SourceCluster:
		return cluster
	case bus.SourceBmrec:
		return !cluster
	}
	return true
}

// ToggleAward returns f with the award code switched on or off.
func (f Filters) ToggleAward(code string) Filters {
	var out []string
	found := false
	for _, a := range f.Awards {
		if a == code {
			found = true
			continue
		}
		out = append(out, a)
	}
	if !found {
		out = append(out, code)
	}
	sort.Strings(out)
	f.Awards = out
	return f
}

// ToggleSource returns f showing only src, or both when src is already set.
func (f Filters) ToggleSource(src string) Filters {
	if f.Source == src {
		src = ""
	}
	f.Source = src
	return f
}

func (f Filters) hasBand(b string) bool {
	if len(f.Bands) == 0 {
		return true
	}
	for _, x := range f.Bands {
		if x == b {
			return true
		}
	}
	return false
}

// ToggleBand returns f with band switched on or off.
func (f Filters) ToggleBand(band string) Filters {
	if f.LockBand {
		return f
	}
	var out []string
	found := false
	for _, b := range f.Bands {
		if b == band {
			found = true
			continue
		}
		out = append(out, b)
	}
	if !found {
		out = append(out, band)
	}
	sort.Strings(out)
	f.Bands = out
	return f
}

// LoadFilters reads path; a missing or corrupt file means "no filter".
func LoadFilters(path string) Filters {
	if path == "" {
		return Filters{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Filters{}
	}
	var f Filters
	if json.Unmarshal(b, &f) != nil {
		return Filters{}
	}
	return f
}

// saveMu serialises every write of the filters file: the HTTP handler and the
// radio handler both save, and a stale snapshot must not overwrite a newer one.
var saveMu sync.Mutex

// SaveFilters writes f to path atomically.
func SaveFilters(path string, f Filters) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	return writeFilters(path, f)
}

// SaveTo writes the model's CURRENT filters to path: the snapshot is taken
// inside the save lock, so concurrent savers always leave the newest state on disk.
func (m *Model) SaveTo(path string) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	return writeFilters(path, m.Filters())
}

func writeFilters(path string, f Filters) error {
	if path == "" {
		return nil
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
