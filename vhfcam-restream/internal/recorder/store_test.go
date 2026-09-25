// SPDX-License-Identifier: AGPL-3.0-or-later

package recorder

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBaseName(t *testing.T) {
	at := time.Date(2026, 9, 25, 18, 12, 44, 0, time.FixedZone("CEST", 2*3600))
	if got := baseName(at, 435_000_000); got != "vhfcam_2026-09-25T1612Z_435.000MHz" {
		t.Errorf("with freq = %q", got)
	}
	if got := baseName(at, 0); got != "vhfcam_2026-09-25T1612Z" {
		t.Errorf("without freq = %q", got)
	}
	if !ValidName(baseName(at, 1_296_250_000) + ".mp4") {
		t.Error("23cm name not valid")
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"vhfcam_2026-09-25T1812Z_435.000MHz.mp4":   true,
		"vhfcam_2026-09-25T1812Z.mp4":              true,
		"vhfcam_2026-09-25T1812Z_2.mp4":            true,
		"vhfcam_2026-09-25T1812Z_435.000MHz_p2.ts": true,
		"../etc/passwd":                            false,
		"vhfcam_2026-09-25T1812Z.mp4/..":           false,
		".inprogress":                              false,
		".vhfcam_2026-09-25T1812Z.mp4.tmp":         false,
		"vhfcam_2026-09-25T1812Z.mkv":              false,
		"vhfcam_2026-09-25T1812Z%2F.mp4":           false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

func writeFile(t *testing.T, path string, size int, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestUniqueBase(t *testing.T) {
	dir := t.TempDir()
	base := "vhfcam_2026-09-25T1812Z"
	if got := uniqueBase(dir, base, true); got != base {
		t.Fatalf("free name = %q", got)
	}
	writeFile(t, filepath.Join(dir, base+".mp4"), 1, time.Now())
	if err := os.MkdirAll(filepath.Join(dir, inprogressDir, base+"_2"), 0750); err != nil {
		t.Fatal(err)
	}
	if got := uniqueBase(dir, base, true); got != base+"_3" {
		t.Fatalf("taken name = %q, want _3", got)
	}
	// Recovery ignores in-progress dirs: _2 is only in progress.
	if got := uniqueBase(dir, base, false); got != base+"_2" {
		t.Fatalf("recovery name = %q, want _2", got)
	}
}

func TestListOpenDeleteRejectsNonRecordings(t *testing.T) {
	dir := t.TempDir()
	good := "vhfcam_2026-09-25T1812Z.mp4"
	writeFile(t, filepath.Join(dir, good), 10, time.Now())
	writeFile(t, filepath.Join(dir, "notes.txt"), 10, time.Now())
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "vhfcam_2026-09-25T1813Z.mp4")); err != nil {
		t.Fatal(err)
	}
	files, err := listFiles(dir)
	if err != nil || len(files) != 1 || files[0].Name != good {
		t.Fatalf("list = %+v err=%v", files, err)
	}
	if _, _, err := openFile(dir, "vhfcam_2026-09-25T1813Z.mp4"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("symlink served: %v", err)
	}
	if _, _, err := openFile(dir, "../"+good); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("traversal served: %v", err)
	}
	if err := deleteFile(dir, "notes.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("non-recording deleted: %v", err)
	}
	if err := deleteFile(dir, good); err != nil {
		t.Errorf("delete: %v", err)
	}
}

func TestEnforceCapDeletesOldestFirst(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	names := []string{
		"vhfcam_2026-09-20T1000Z.mp4", // oldest
		"vhfcam_2026-09-21T1000Z.mp4",
		"vhfcam_2026-09-22T1000Z.mp4", // newest
	}
	for i, n := range names {
		writeFile(t, filepath.Join(dir, n), 100, now.Add(time.Duration(i)*time.Hour))
	}
	if err := os.MkdirAll(filepath.Join(dir, inprogressDir, "vhfcam_2026-09-19T1000Z"), 0750); err != nil {
		t.Fatal(err)
	}
	deleted, err := enforceCap(dir, 250, 0)
	if err != nil || len(deleted) != 1 || deleted[0].Name != names[0] {
		t.Fatalf("cap 250: deleted %+v err=%v", deleted, err)
	}
	// Reserve room for the next recording: 200 on disk + 100 reserve > 250.
	deleted, _ = enforceCap(dir, 250, 100)
	if len(deleted) != 1 || deleted[0].Name != names[1] {
		t.Fatalf("with reserve: deleted %+v", deleted)
	}
	if _, err := os.Stat(filepath.Join(dir, inprogressDir, "vhfcam_2026-09-19T1000Z")); err != nil {
		t.Error("in-progress directory touched by retention")
	}
}
