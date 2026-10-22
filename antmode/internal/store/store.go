// SPDX-License-Identifier: AGPL-3.0-or-later

// Package store persists antmode's state as one JSON file, written
// atomically (temp file in the same directory, fsync, rename, fsync of the
// directory), so a crash or power cut leaves either the old or the new file,
// never a torn one.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// File is a JSON state file.
type File struct{ Path string }

// Load decodes the file into v. A missing file returns an error wrapping
// fs.ErrNotExist.
func (f File) Load(v any) error {
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decode %s: %w", f.Path, err)
	}
	return nil
}

// Save writes v atomically (0600: the file carries no secret, but it is the
// station's lock authority and only the service user writes it).
func (f File) Save(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.Path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(f.Path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, f.Path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
