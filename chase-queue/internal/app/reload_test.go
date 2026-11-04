package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"chase-queue/internal/config"
)

// lockedBuf is a log sink safe for concurrent writers.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), sub)
}

const dxFile = `
[[award]]
code = "DXA"
kind = "field"
field = "STATE"
dxcc = [291]
items = ["TX"]
`

// reloadSvc is a service with was.toml loaded and a goal on WAS, logging to buf.
func reloadSvc(t *testing.T) (*Service, *lockedBuf) {
	t.Helper()
	s := svcWithAwards(t, "", config.GoalConf{Award: "was"})
	buf := &lockedBuf{}
	s.log = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if s.awardErr != nil {
		t.Fatal(s.awardErr)
	}
	return s, buf
}

func hasProblem(s *Service, sub string) bool {
	for _, p := range s.compute().Problems {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func hasCode(s *Service, code string) bool {
	s.awardMu.Lock()
	defer s.awardMu.Unlock()
	return slices.Contains(s.awardCodes, code)
}

func write(t *testing.T, s *Service, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.cfg.AwardsDir(), name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReloadPicksUpNewFile(t *testing.T) {
	s, _ := reloadSvc(t)
	if s.reloadAwards(false) {
		t.Fatal("reload without a change")
	}
	if hasCode(s, "DXA") {
		t.Fatal("DXA before the file exists")
	}
	write(t, s, "dxa.toml", dxFile)
	if !s.reloadAwards(false) || !hasCode(s, "DXA") || !hasCode(s, "WAS") {
		t.Fatal("new file not picked up")
	}
	meta, _ := s.metaPayload()
	if !strings.Contains(string(meta), "DXA") {
		t.Fatalf("meta lacks DXA: %s", meta)
	}
	if hasProblem(s, "reload failed") {
		t.Fatal("spurious problem")
	}
}

func TestReloadBrokenFileKeepsCatalogWarnsOnceAndRecovers(t *testing.T) {
	s, buf := reloadSvc(t)
	write(t, s, "dxa.toml", "[[award]\nbroken")
	for i := 0; i < 3; i++ { // repeated polls must not re-parse or re-warn
		if s.reloadAwards(false) {
			t.Fatal("broken file swapped in")
		}
	}
	if n := buf.count("reload failed"); n != 1 {
		t.Fatalf("%d warns", n)
	}
	if !hasCode(s, "WAS") || !hasProblem(s, "reload failed") {
		t.Fatalf("catalog lost or problem missing: %v", s.compute().Problems)
	}
	// a changed, still-broken file retries but the same message does not warn again
	write(t, s, "dxa.toml", "[[award]\nbroken more")
	s.reloadAwards(false)
	if n := buf.count("reload failed"); n != 1 {
		t.Fatalf("%d warns after a changed broken file", n)
	}
	write(t, s, "dxa.toml", dxFile)
	if !s.reloadAwards(false) || !hasCode(s, "DXA") {
		t.Fatal("fixed file not picked up")
	}
	if hasProblem(s, "reload failed") {
		t.Fatalf("problem not cleared: %v", s.compute().Problems)
	}
}

func TestReloadIOErrorRetriesNextPoll(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not apply to root")
	}
	s, _ := reloadSvc(t)
	p := filepath.Join(s.cfg.AwardsDir(), "dxa.toml")
	write(t, s, "dxa.toml", dxFile)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0o600) })
	s.awardMu.Lock()
	before := s.awardSig
	s.awardMu.Unlock()
	if s.reloadAwards(false) {
		t.Fatal("unreadable file swapped in")
	}
	s.awardMu.Lock()
	after := s.awardSig
	s.awardMu.Unlock()
	if after != before {
		t.Fatal("signature stored after an I/O failure")
	}
	if !hasProblem(s, "reload failed") {
		t.Fatal("no problem line")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if !s.reloadAwards(false) || !hasCode(s, "DXA") {
		t.Fatal("next poll did not retry")
	}
}

func TestReloadVanishedAwardKeepsGoalWithProblem(t *testing.T) {
	s, _ := reloadSvc(t)
	write(t, s, "other.toml", dxFile)
	s.reloadAwards(false)
	if err := os.Remove(filepath.Join(s.cfg.AwardsDir(), "was.toml")); err != nil {
		t.Fatal(err)
	}
	if !s.reloadAwards(false) {
		t.Fatal("no reload after removing a file")
	}
	if hasCode(s, "WAS") || !hasCode(s, "DXA") {
		t.Fatal("catalog not replaced")
	}
	if len(s.cfg.Goals) != 1 || s.cfg.Goals[0].Award != "was" {
		t.Fatal("goal dropped")
	}
	if !hasProblem(s, "Goal names award was") {
		t.Fatalf("no problem line: %v", s.compute().Problems)
	}
	// the award comes back: problem clears
	write(t, s, "was.toml", wasFile)
	s.reloadAwards(false)
	if hasProblem(s, "Goal names award") {
		t.Fatal("problem not cleared")
	}
}

// ReplaceAwards swaps while Compute reads: run with -race.
func TestReplaceAwardsConcurrentWithCompute(t *testing.T) {
	s, _ := reloadSvc(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s.compute()
					s.metaPayload()
				}
			}
		}()
	}
	for i := 0; i < 30; i++ {
		body := dxFile
		if i%2 == 0 {
			body += "\n# " + strings.Repeat("x", i) + "\n"
		}
		write(t, s, "dxa.toml", body)
		s.reloadAwards(true)
	}
	close(stop)
	wg.Wait()
}

func TestWatchAwardsPollAndHUP(t *testing.T) {
	s, _ := reloadSvc(t)
	s.awardPoll = 10 * time.Millisecond
	hup := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.watchAwards(ctx, hup); close(done) }()
	defer func() { cancel(); <-done }()

	wait := func(what string, ok func() bool) {
		t.Helper()
		for d := time.Now().Add(5 * time.Second); time.Now().Before(d); time.Sleep(10 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatalf("timeout: %s", what)
	}
	write(t, s, "dxa.toml", dxFile)
	wait("poll reload", func() bool { return hasCode(s, "DXA") })

	// SIGHUP forces a reload even though the signature is unchanged
	s.awardMu.Lock()
	s.awardCodes = []string{"stale"}
	s.awardMu.Unlock()
	hup <- os.Interrupt
	wait("hup reload", func() bool { return hasCode(s, "DXA") })
}
