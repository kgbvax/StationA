package model

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// Filters narrow what is shown. They never change chase-queue's ranking.
type Filters struct {
	Bands      []string `json:"bands,omitempty"` // empty = every band
	NeededOnly bool     `json:"needed_only,omitempty"`
}

// Active reports whether any filter is set.
func (f Filters) Active() bool { return len(f.Bands) > 0 || f.NeededOnly }

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

// SaveFilters writes f to path atomically.
func SaveFilters(path string, f Filters) error {
	if path == "" {
		return nil
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
