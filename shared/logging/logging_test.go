package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestPriorityPrefixPerLevel(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(NewPriorityHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	log.Debug("d")
	log.Info("i")
	log.Warn("w")
	log.Error("e")

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	want := []struct{ prefix, msg string }{
		{"<7>", "msg=d"}, {"<6>", "msg=i"}, {"<4>", "msg=w"}, {"<3>", "msg=e"},
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), out.String())
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w.prefix+"time=") || !strings.Contains(lines[i], w.msg) {
			t.Errorf("line %d = %q, want prefix %s and %s", i, lines[i], w.prefix, w.msg)
		}
	}
}

func TestPriorityChildrenKeepAttrsAndPrefix(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(NewPriorityHandler(&out, nil)).With("component", "x").WithGroup("g").With("slot", "s")
	log.Warn("degraded", "n", 1)
	got := out.String()
	if !strings.HasPrefix(got, "<4>time=") ||
		!strings.Contains(got, "component=x") || !strings.Contains(got, "g.slot=s") || !strings.Contains(got, "g.n=1") {
		t.Fatalf("unexpected line %q", got)
	}
}

func TestPriorityRespectsLevel(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(NewPriorityHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn}))
	log.Info("hidden")
	if out.Len() != 0 {
		t.Fatalf("info below level was written: %q", out.String())
	}
}

func TestPriorityConcurrentLinesStayWhole(t *testing.T) {
	var out syncBuf
	root := slog.New(NewPriorityHandler(&out, nil))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			l := root.With("g", g)
			for i := 0; i < 200; i++ {
				if i%2 == 0 {
					l.Error("e")
				} else {
					l.Info("i")
				}
			}
		}(g)
	}
	wg.Wait()
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		ok := (strings.HasPrefix(line, "<3>") && strings.Contains(line, "level=ERROR")) ||
			(strings.HasPrefix(line, "<6>") && strings.Contains(line, "level=INFO"))
		if !ok || strings.Count(line, "time=") != 1 {
			t.Fatalf("torn or mis-prefixed line %q", line)
		}
	}
}

func TestNewHandlerPlainOutsideJournal(t *testing.T) {
	t.Setenv("JOURNAL_STREAM", "")
	var out bytes.Buffer
	slog.New(NewHandler(&out, nil)).Error("e")
	if !strings.HasPrefix(out.String(), "time=") {
		t.Fatalf("plain handler expected outside the journal, got %q", out.String())
	}
}

func TestNewHandlerPrefixesUnderJournal(t *testing.T) {
	t.Setenv("JOURNAL_STREAM", "8:12345")
	var out bytes.Buffer
	slog.New(NewHandler(&out, nil)).Error("e")
	if !strings.HasPrefix(out.String(), "<3>time=") {
		t.Fatalf("priority prefix expected under the journal, got %q", out.String())
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
