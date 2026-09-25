// SPDX-License-Identifier: AGPL-3.0-or-later

package preview

import (
	"reflect"
	"testing"
)

func TestAudioDemandHolders(t *testing.T) {
	var d AudioDemand
	if b, a := d.Set(HolderPage, true); b || !a {
		t.Fatalf("page on: before=%v after=%v", b, a)
	}
	if b, a := d.Set(HolderRecording, true); !b || !a {
		t.Fatalf("recording on: before=%v after=%v", b, a)
	}
	// The page disconnects mid-recording: demand stays (no audio_off edge).
	if b, a := d.Set(HolderPage, false); !b || !a {
		t.Fatalf("page off while recording: before=%v after=%v", b, a)
	}
	if got := d.Holders(); !reflect.DeepEqual(got, []string{HolderRecording}) {
		t.Fatalf("holders = %v", got)
	}
	if b, a := d.Set(HolderRecording, false); !b || a {
		t.Fatalf("recording off: before=%v after=%v", b, a)
	}
	if d.On() {
		t.Error("demand left on")
	}
	// Releasing an absent holder is a no-op, not an edge.
	if b, a := d.Set(HolderPage, false); b || a {
		t.Errorf("double release: before=%v after=%v", b, a)
	}
}
