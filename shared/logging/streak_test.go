package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestStreakWarnsOncePerOutage(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(NewPriorityHandler(&out, nil))
	var s Streak
	s.Fail(log, "lost", "err", "no route")
	s.Fail(log, "lost", "err", "no route")
	s.Fail(log, "lost", "err", "no route")
	s.Reset()
	s.Fail(log, "lost", "err", "eof")

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	want := []struct{ prefix, has string }{
		{"<4>", "level=WARN"},
		{"<6>", "repeat=2"},
		{"<6>", "repeat=3"},
		{"<4>", "err=eof"},
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines:\n%s", len(lines), out.String())
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w.prefix) || !strings.Contains(lines[i], w.has) {
			t.Errorf("line %d = %q, want prefix %s containing %s", i, lines[i], w.prefix, w.has)
		}
	}
	if strings.Contains(lines[0], "repeat=") || strings.Contains(lines[3], "repeat=") {
		t.Errorf("first failure of a streak must not carry repeat: %q / %q", lines[0], lines[3])
	}
}
