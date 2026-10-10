// Package logging builds the station's root slog handler (docs/conventions/
// logging.md §1): slog text to stderr, and — when the process runs under
// systemd with stderr connected to the journal — each line prefixed with its
// syslog priority (`<3>` error, `<4>` warning, `<6>` info, `<7>` debug).
//
// Without the prefix journald files every stderr line at priority info, so
// `journalctl -p warning` never saw a slog Warn/Error (verified on shari
// 2026-10-09: the filter only showed systemd's own "Failed with result" lines).
// journald strips the prefix (SyslogLevelPrefix= defaults to yes), so
// `journalctl -o cat` output is unchanged. Outside the journal (a terminal,
// tests, the pelcobridge2 TUI) the plain text handler is returned.
package logging

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
)

// NewHandler returns the station root handler writing to w. The priority
// prefix is added only when systemd has connected stdout/stderr to the
// journal (it sets JOURNAL_STREAM for exactly that case).
func NewHandler(w io.Writer, opts *slog.HandlerOptions) slog.Handler {
	if os.Getenv("JOURNAL_STREAM") == "" {
		return slog.NewTextHandler(w, opts)
	}
	return NewPriorityHandler(w, opts)
}

// NewPriorityHandler returns a text handler that prefixes every record with
// its syslog priority, regardless of the environment.
func NewPriorityHandler(w io.Writer, opts *slog.HandlerOptions) slog.Handler {
	st := &prioState{out: w}
	return &prioHandler{inner: slog.NewTextHandler(&st.buf, opts), st: st}
}

// prioState is shared by a handler and every WithAttrs/WithGroup child: the
// text handlers all format into buf, and mu serializes format+write so each
// record reaches w as one prefixed line.
type prioState struct {
	mu  sync.Mutex
	buf bytes.Buffer
	out io.Writer
}

type prioHandler struct {
	inner slog.Handler
	st    *prioState
}

func (h *prioHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *prioHandler) Handle(ctx context.Context, r slog.Record) error {
	h.st.mu.Lock()
	defer h.st.mu.Unlock()
	h.st.buf.Reset()
	h.st.buf.WriteString(prefix(r.Level))
	if err := h.inner.Handle(ctx, r); err != nil {
		return err
	}
	_, err := h.st.out.Write(h.st.buf.Bytes())
	return err
}

func (h *prioHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &prioHandler{inner: h.inner.WithAttrs(as), st: h.st}
}

func (h *prioHandler) WithGroup(name string) slog.Handler {
	return &prioHandler{inner: h.inner.WithGroup(name), st: h.st}
}

// prefix maps a slog level onto the sd-daemon(3) priority prefix.
func prefix(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "<3>"
	case l >= slog.LevelWarn:
		return "<4>"
	case l >= slog.LevelInfo:
		return "<6>"
	default:
		return "<7>"
	}
}
