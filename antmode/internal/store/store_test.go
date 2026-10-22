// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

type doc struct {
	Mode string   `json:"mode"`
	Seq  uint64   `json:"seq"`
	IDs  []string `json:"ids"`
}

func TestRoundTrip(t *testing.T) {
	f := File{Path: filepath.Join(t.TempDir(), "state.json")}
	var d doc
	if err := f.Load(&d); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	in := doc{Mode: "storm", Seq: 17, IDs: []string{"a", "b"}}
	if err := f.Save(in); err != nil {
		t.Fatal(err)
	}
	if err := f.Load(&d); err != nil || d.Mode != "storm" || d.Seq != 17 || len(d.IDs) != 2 {
		t.Fatalf("load: %+v %v", d, err)
	}
	st, err := os.Stat(f.Path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", st.Mode(), err)
	}
	// Overwrite; no temp files left behind.
	if err := f.Save(doc{Mode: "normal"}); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(filepath.Dir(f.Path))
	if len(ents) != 1 {
		t.Fatalf("leftover files: %v", ents)
	}
}

func TestCorruptFile(t *testing.T) {
	f := File{Path: filepath.Join(t.TempDir(), "state.json")}
	if err := os.WriteFile(f.Path, []byte(`{"mode":`), 0o600); err != nil {
		t.Fatal(err)
	}
	var d doc
	if err := f.Load(&d); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("corrupt file: %v", err)
	}
}

func TestSaveFailsWithoutDir(t *testing.T) {
	f := File{Path: filepath.Join(t.TempDir(), "missing", "state.json")}
	if err := f.Save(doc{}); err == nil {
		t.Fatal("save into a missing directory succeeded")
	}
}
