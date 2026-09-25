// SPDX-License-Identifier: AGPL-3.0-or-later

package recorder

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mkPlaylist(seq int, n int) playlist {
	pl := playlist{seq: seq}
	for i := 0; i < n; i++ {
		pl.segs = append(pl.segs, fmt.Sprintf("seg_%05d.ts", seq+i))
	}
	return pl
}

func TestParsePlaylist(t *testing.T) {
	src := "#EXTM3U\r\n#EXT-X-VERSION:3\r\n#EXT-X-TARGETDURATION:2\r\n#EXT-X-MEDIA-SEQUENCE:41\r\n" +
		"#EXTINF:2.000000,\r\nseg_00041.ts\r\n#EXTINF:2.000000,\r\nseg_00042.ts\r\n"
	pl, ok := parsePlaylist([]byte(src))
	if !ok || pl.seq != 41 || !reflect.DeepEqual(pl.segs, []string{"seg_00041.ts", "seg_00042.ts"}) {
		t.Fatalf("parse = %+v ok=%v", pl, ok)
	}
	if _, ok := parsePlaylist([]byte("<html>")); ok {
		t.Error("non-playlist accepted")
	}
}

// stableMtime: every segment keeps one mtime (no preview restart).
func stableMtime(string) (time.Time, bool) { return time.Unix(100, 0), true }

func take(f *follower, segs []string, mt func(string) (time.Time, bool)) {
	for _, s := range segs {
		at, _ := mt(s)
		f.markTaken(s, at)
	}
}

func TestFollowerPrerollThenOnlyNew(t *testing.T) {
	var f follower
	segs, reset, missed := f.scan(mkPlaylist(10, 6), preroll, stableMtime)
	if reset || missed != 0 || !reflect.DeepEqual(segs, []string{"seg_00013.ts", "seg_00014.ts", "seg_00015.ts"}) {
		t.Fatalf("first scan = %v reset=%v missed=%d", segs, reset, missed)
	}
	take(&f, segs, stableMtime)

	segs, reset, _ = f.scan(mkPlaylist(10, 6), preroll, stableMtime)
	if len(segs) != 0 || reset {
		t.Fatalf("unchanged playlist gave %v reset=%v", segs, reset)
	}

	segs, _, _ = f.scan(mkPlaylist(12, 6), preroll, stableMtime) // 16, 17 new
	if !reflect.DeepEqual(segs, []string{"seg_00016.ts", "seg_00017.ts"}) {
		t.Fatalf("next scan = %v", segs)
	}
}

func TestFollowerCountsMissedSegments(t *testing.T) {
	var f follower
	segs, _, _ := f.scan(mkPlaylist(0, 3), preroll, stableMtime) // takes 0..2
	take(&f, segs, stableMtime)
	segs, reset, missed := f.scan(mkPlaylist(6, 3), preroll, stableMtime) // 3..5 already gone
	if reset || missed != 3 || !reflect.DeepEqual(segs, []string{"seg_00006.ts", "seg_00007.ts", "seg_00008.ts"}) {
		t.Fatalf("scan = %v reset=%v missed=%d", segs, reset, missed)
	}
}

func TestFollowerDetectsRestartBySequence(t *testing.T) {
	var f follower
	segs, _, _ := f.scan(mkPlaylist(100, 6), preroll, stableMtime)
	take(&f, segs, stableMtime)
	segs, reset, _ := f.scan(mkPlaylist(0, 2), preroll, stableMtime) // ffmpeg restarted
	if !reset || !reflect.DeepEqual(segs, []string{"seg_00000.ts", "seg_00001.ts"}) {
		t.Fatalf("restart scan = %v reset=%v", segs, reset)
	}
}

// A restart that already produced more segments than we had copied reuses
// names; the changed mtime of a copied name gives it away.
func TestFollowerDetectsRestartByReusedName(t *testing.T) {
	var f follower
	segs, _, _ := f.scan(mkPlaylist(0, 2), preroll, stableMtime) // takes 0, 1
	take(&f, segs, stableMtime)
	newRun := func(name string) (time.Time, bool) {
		if strings.HasSuffix(name, "00000.ts") || strings.HasSuffix(name, "00001.ts") {
			return time.Unix(999, 0), true
		}
		return time.Unix(100, 0), true
	}
	segs, reset, _ := f.scan(mkPlaylist(0, 4), preroll, newRun)
	if !reset || len(segs) != 4 {
		t.Fatalf("reused-name scan = %v reset=%v", segs, reset)
	}
}
