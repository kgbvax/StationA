// SPDX-License-Identifier: AGPL-3.0-or-later

package preview

import (
	"sort"
	"sync"
)

// Radio-audio demand holders. The IC-9700 audio flows (and its CI-V session
// is held) while any holder wants it: the page's connect button, or a running
// recording. The page's disconnect only releases the page's hold, so it can
// never cut the audio out of a recording.
const (
	HolderPage      = "page"
	HolderRecording = "recording"
)

// AudioDemand is the set of current radio-audio demand holders.
type AudioDemand struct {
	mu      sync.Mutex
	holders map[string]bool
}

// Set adds (on) or removes a holder and reports whether demand existed
// before and after — the caller publishes audio_on / audio_off on an edge.
func (d *AudioDemand) Set(holder string, on bool) (before, after bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.holders == nil {
		d.holders = map[string]bool{}
	}
	before = len(d.holders) > 0
	if on {
		d.holders[holder] = true
	} else {
		delete(d.holders, holder)
	}
	return before, len(d.holders) > 0
}

// On reports whether anyone wants the radio audio.
func (d *AudioDemand) On() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.holders) > 0
}

// Holders lists the current holders, sorted.
func (d *AudioDemand) Holders() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.holders))
	for h := range d.holders {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
