// SPDX-License-Identifier: AGPL-3.0-or-later

package recorder

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"time"
)

// playlist is the part of an HLS media playlist the follower needs: the media
// sequence number of the first listed segment and the segment file names in
// order. The preview muxer runs with temp_file, so every listed segment is
// complete.
type playlist struct {
	seq  int
	segs []string
}

// parsePlaylist reads an HLS media playlist. ok is false when the data is not
// a playlist at all (no #EXTM3U header).
func parsePlaylist(b []byte) (pl playlist, ok bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first {
			if line != "#EXTM3U" {
				return playlist{}, false
			}
			first = false
			continue
		}
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			if n, err := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:")); err == nil {
				pl.seq = n
			}
		case strings.HasPrefix(line, "#"):
		default:
			pl.segs = append(pl.segs, line)
		}
	}
	return pl, !first
}

// follower decides which playlist segments are new for the current recording
// part. Segment names alone cannot identify content: after a preview ffmpeg
// restart the muxer starts again at seg_00000 with media sequence 0, reusing
// names already copied. A restart is detected by the sequence running
// backwards or a copied name reappearing with a different modification time.
type follower struct {
	started bool
	lastAbs int                  // absolute sequence number of the last copied segment
	taken   map[string]time.Time // name -> mtime at copy, for the current part
}

// scan returns the segments to append, in order. reset reports a preview
// restart: the caller must start a new part before appending segs (which then
// all belong to the new run). missed counts sequence numbers that were
// deleted from the playlist before they could be copied. mtime reports a
// segment's modification time (ok=false when it is already gone).
func (f *follower) scan(pl playlist, preroll int, mtime func(name string) (time.Time, bool)) (segs []string, reset bool, missed int) {
	if len(pl.segs) == 0 {
		return nil, false, 0
	}
	if !f.started {
		f.started = true
		f.taken = map[string]time.Time{}
		from := len(pl.segs) - preroll
		if from < 0 {
			from = 0
		}
		f.lastAbs = pl.seq + from - 1
	}

	lastListed := pl.seq + len(pl.segs) - 1
	if lastListed < f.lastAbs {
		reset = true
	} else {
		for _, name := range pl.segs {
			if at, ok := f.taken[name]; ok {
				if now, ok := mtime(name); ok && !now.Equal(at) {
					reset = true
					break
				}
			}
		}
	}

	if reset {
		f.taken = map[string]time.Time{}
		f.lastAbs = pl.seq - 1
	}

	if first := pl.seq; first > f.lastAbs+1 {
		missed = first - (f.lastAbs + 1)
	}
	for i, name := range pl.segs {
		abs := pl.seq + i
		if abs <= f.lastAbs {
			continue
		}
		segs = append(segs, name)
		f.lastAbs = abs
	}
	return segs, reset, missed
}

// markTaken records a copied segment's modification time.
func (f *follower) markTaken(name string, mtime time.Time) {
	if f.taken == nil {
		f.taken = map[string]time.Time{}
	}
	f.taken[name] = mtime
}
