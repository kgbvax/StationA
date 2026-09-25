// SPDX-License-Identifier: AGPL-3.0-or-later

package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// meta is written into each in-progress directory at start, so a recording
// interrupted by a crash or power loss can be finalized under its real name.
type meta struct {
	Name    string    `json:"name"` // base name, no extension
	Started time.Time `json:"started"`
	FreqHz  int64     `json:"freq_hz,omitempty"`
}

func writeMeta(dir string, m meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), b, 0640)
}

func readMeta(dir string) (meta, error) {
	var m meta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

func partName(n int) string { return fmt.Sprintf("part%03d.ts", n) }

// partsIn lists the non-empty part files of an in-progress directory in order.
func partsIn(dir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, "part*.ts"))
	sort.Strings(matches)
	var out []string
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.Size() > 0 {
			out = append(out, m)
		}
	}
	return out
}

// errNothingRecorded means no segment reached the recording at all.
var errNothingRecorded = errors.New("no video was captured")

// finalize turns an in-progress directory into a finished recording in
// outDir and removes the in-progress directory. The parts are MPEG-TS cut
// from the preview's HLS; ffmpeg's concat demuxer joins them (shifting each
// part's timestamps, so a preview restart between parts is harmless) and
// stream-copies into MP4 — no re-encode. If ffmpeg fails, the parts are kept
// as .ts files so nothing recorded is lost; the error is still returned.
func finalize(ctx context.Context, ffmpegBin, inDir, outDir, base string) (string, error) {
	parts := partsIn(inDir)
	if len(parts) == 0 {
		_ = os.RemoveAll(inDir)
		return "", errNothingRecorded
	}

	list := filepath.Join(inDir, "list.txt")
	var b strings.Builder
	for _, p := range parts {
		// concat list quoting: single quotes, a literal ' becomes '\''.
		fmt.Fprintf(&b, "file '%s'\n", strings.ReplaceAll(p, "'", `'\''`))
	}
	if err := os.WriteFile(list, []byte(b.String()), 0640); err != nil {
		return keepParts(inDir, outDir, base, parts, err)
	}

	final := filepath.Join(outDir, base+".mp4")
	tmp := filepath.Join(outDir, "."+base+".mp4.tmp")
	cmd := exec.CommandContext(ctx, ffmpegBin,
		"-hide_banner", "-loglevel", "warning", "-nostdin",
		"-f", "concat", "-safe", "0", "-i", list,
		"-map", "0", "-c", "copy", "-bsf:a", "aac_adtstoasc",
		"-f", "mp4", "-y", tmp)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return keepParts(inDir, outDir, base, parts, fmt.Errorf("remux failed: %v: %s", err, msg))
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return keepParts(inDir, outDir, base, parts, err)
	}
	_ = os.RemoveAll(inDir)
	return filepath.Base(final), nil
}

// keepParts moves the raw parts into outDir (<base>.ts, or <base>_pN.ts for
// several) when the MP4 could not be made, then removes the in-progress dir.
func keepParts(inDir, outDir, base string, parts []string, cause error) (string, error) {
	var first string
	for i, p := range parts {
		name := base + ".ts"
		if len(parts) > 1 {
			name = fmt.Sprintf("%s_p%d.ts", base, i+1)
		}
		if err := os.Rename(p, filepath.Join(outDir, name)); err != nil {
			// Leave the directory for the next startup recovery.
			return first, fmt.Errorf("%v; keeping parts failed: %v", cause, err)
		}
		if first == "" {
			first = name
		}
	}
	_ = os.RemoveAll(inDir)
	return first, cause
}
