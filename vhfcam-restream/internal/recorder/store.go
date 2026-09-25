// SPDX-License-Identifier: AGPL-3.0-or-later

package recorder

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"
)

// inprogressDir holds one sub-directory per unfinished recording (parts +
// meta.json). Its name can never match nameRe, so it is never listed, served
// or deleted through the HTTP API.
const inprogressDir = ".inprogress"

// nameRe is the only shape of file the recorder lists, serves or deletes:
// vhfcam_<UTC start>[_<freq>MHz][_<n>][_p<part>].(mp4|ts). It rules out path
// separators, "..", dot-files and anything not written by the recorder.
var nameRe = regexp.MustCompile(`^vhfcam_\d{4}-\d{2}-\d{2}T\d{4}Z(_\d{1,5}\.\d{3}MHz)?(_\d+)?(_p\d+)?\.(mp4|ts)$`)

// ValidName reports whether name is a recording file name the API may touch.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// File is one finished recording.
type File struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// baseName builds the recording name (without extension) from the UTC start
// time and, when known, the radio frequency at start:
// vhfcam_2026-09-25T1812Z_435.000MHz.
func baseName(start time.Time, freqHz int64) string {
	s := "vhfcam_" + start.UTC().Format("2006-01-02T1504Z")
	if freqHz > 0 {
		s += fmt.Sprintf("_%.3fMHz", float64(freqHz)/1e6)
	}
	return s
}

// uniqueBase appends _2, _3, … when base is already used by a finished file
// (any extension or part) or, with inProgress, an in-progress recording —
// two recordings in the same minute on the same frequency. Recovery passes
// inProgress=false: the leftover's own directory is not a clash.
func uniqueBase(dir, base string, inProgress bool) string {
	taken := func(b string) bool {
		paths := []string{b + ".mp4", b + ".ts", b + "_p1.ts"}
		if inProgress {
			paths = append(paths, filepath.Join(inprogressDir, b))
		}
		for _, p := range paths {
			if _, err := os.Lstat(filepath.Join(dir, p)); err == nil {
				return true
			}
		}
		return false
	}
	if !taken(base) {
		return base
	}
	for n := 2; ; n++ {
		if b := fmt.Sprintf("%s_%d", base, n); !taken(b) {
			return b
		}
	}
}

// listFiles returns the finished recordings in dir, newest first (the UTC
// timestamp in the name sorts chronologically; mtime breaks ties).
func listFiles(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []File
	for _, e := range entries {
		if !ValidName(e.Name()) || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, File{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name > out[j].Name
		}
		return out[i].ModTime.After(out[j].ModTime)
	})
	return out, nil
}

// openFile opens a finished recording by name for download. Only regular
// files matching nameRe are served (no symlinks).
func openFile(dir, name string) (*os.File, os.FileInfo, error) {
	if !ValidName(name) {
		return nil, nil, fs.ErrNotExist
	}
	p := filepath.Join(dir, name)
	info, err := os.Lstat(p)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fs.ErrNotExist
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	return f, info, nil
}

// deleteFile removes a finished recording by name.
func deleteFile(dir, name string) error {
	if !ValidName(name) {
		return fs.ErrNotExist
	}
	p := filepath.Join(dir, name)
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fs.ErrNotExist
	}
	return os.Remove(p)
}

// enforceCap deletes the oldest finished recordings while their total size
// plus reserve exceeds capBytes. It never touches in-progress recordings
// (they live in .inprogress and are not listed). Returns the deleted files.
func enforceCap(dir string, capBytes, reserve uint64) ([]File, error) {
	files, err := listFiles(dir)
	if err != nil {
		return nil, err
	}
	var total uint64
	for _, f := range files {
		total += uint64(f.Size)
	}
	var deleted []File
	for i := len(files) - 1; i >= 0 && total+reserve > capBytes; i-- {
		f := files[i] // oldest first
		if err := os.Remove(filepath.Join(dir, f.Name)); err != nil {
			return deleted, err
		}
		total -= uint64(f.Size)
		deleted = append(deleted, f)
	}
	return deleted, nil
}

// totalSize sums the finished recordings.
func totalSize(files []File) uint64 {
	var t uint64
	for _, f := range files {
		t += uint64(f.Size)
	}
	return t
}

// freeBytes reports the space available to this (unprivileged) service on
// the filesystem holding path. Bavail, not Bfree: root-reserved blocks are
// not usable by the service user. The conversions compile on linux and darwin.
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
