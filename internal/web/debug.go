package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"ubctrl/internal/ub/protocol"
	"ubctrl/internal/ub/transport"
)

// DebugHub holds the global debug-trace toggle and fans out trace entries to
// any connected SSE subscribers. It implements transport.TraceSink via Publish
// and gates capture via Enabled, so the transport tracer only does work while
// the toggle is on.
type DebugHub struct {
	enabled atomic.Bool

	mu  sync.RWMutex
	chs map[chan []byte]struct{}
}

func NewDebugHub() *DebugHub {
	return &DebugHub{chs: make(map[chan []byte]struct{})}
}

// Enabled reports whether debug tracing is currently on. Passed to the
// transport tracer as its enable predicate.
func (h *DebugHub) Enabled() bool { return h.enabled.Load() }

// SetEnabled turns debug tracing on or off.
func (h *DebugHub) SetEnabled(v bool) { h.enabled.Store(v) }

// Publish formats a trace entry and broadcasts it to subscribers. Safe for
// concurrent use; non-blocking (slow subscribers drop entries).
func (h *DebugHub) Publish(t transport.Trace) {
	payload, err := json.Marshal(formatTrace(t))
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.chs {
		select {
		case ch <- payload:
		default:
		}
	}
}

func (h *DebugHub) subscribe() chan []byte {
	ch := make(chan []byte, 32)
	h.mu.Lock()
	h.chs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *DebugHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.chs, ch)
	h.mu.Unlock()
	close(ch)
}

// traceView is the JSON shape streamed to the UI.
type traceView struct {
	At   string `json:"at"`
	Dir  string `json:"dir"`
	Name string `json:"name"`
	Com  byte   `json:"com"`
	Data string `json:"data"`
	Err  string `json:"err,omitempty"`
}

func formatTrace(t transport.Trace) traceView {
	name := protocol.CommandName(t.Com)
	if t.Dir == "rx" {
		name = protocol.ReplyName(t.Com)
	}
	return traceView{
		At:   t.At.Format("15:04:05.000"),
		Dir:  t.Dir,
		Name: name,
		Com:  t.Com,
		Data: hexBytes(t.Data),
		Err:  t.Err,
	}
}

func hexBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	parts := make([]string, len(b))
	for i, by := range b {
		parts[i] = fmt.Sprintf("%02X", by)
	}
	return strings.Join(parts, " ")
}
