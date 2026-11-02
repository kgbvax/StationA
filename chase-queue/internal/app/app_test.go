package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chase-queue/internal/bus"
	"chase-queue/internal/config"
	"chase-queue/internal/store"
)

const secret = "cl_super_secret_token"

func newSvc(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"cty.dat", "cty.csv"} {
		b, err := os.ReadFile("testdata/" + f)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, f), b, 0o600)
	}
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.ClubLog.Token, cfg.ClubLog.Call = secret, "DL1ABC"
	st, err := store.Open(filepath.Join(dir, "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func state(seq int64, rows ...bus.Candidate) []byte {
	b, _ := json.Marshal(bus.CQFinderState{Seq: seq, TS: time.Now(), Rows: rows})
	return b
}

func row(id int64, call string) bus.Candidate {
	return bus.Candidate{RowID: id, Call: call, Trusted: true, Band: "20m", FreqHz: 14.2e6, FirstUTC: time.Now(), LastUTC: time.Now()}
}

func cmd(action, value string) []byte {
	b, _ := json.Marshal(map[string]string{"action": action, "value": value})
	return b
}

func TestDXSkipThroughCommandAndUndo(t *testing.T) {
	s := newSvc(t)
	s.eng.SetConfirmations(nil)
	s.onCandidates(state(1, row(1, "EA8XYZ")))
	if st := s.compute(); len(st.Ranked) != 1 || !st.Ranked[0].Needed {
		t.Fatalf("%+v", st)
	}
	s.onCmd(cmd("skip", `{"row_id":1,"band":"20m"}`))
	if st := s.compute(); len(st.Ranked) != 0 || st.Undo == "" {
		t.Fatalf("%+v", st)
	}
	// a second window acting on the same row is ignored
	s.onCmd(cmd("skip", `{"row_id":1,"band":"20m"}`))
	s.onCmd(cmd("undo", ""))
	if st := s.compute(); len(st.Ranked) != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestUploadSwitchAndHandledInContest(t *testing.T) {
	s := newSvc(t)
	def, _ := os.ReadFile("testdata/dxcc-only.txt")
	f, err := s.Upload("definition", "dxcc-only.txt", string(def))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SwitchContest(bus.SwitchReq{New: true, DefinitionID: f.ID, StartDate: "2026-11-01"}); err != nil {
		t.Fatal(err)
	}
	s.onCandidates(state(1, row(1, "EA8XYZ")))
	st := s.compute()
	if st.Activity != bus.ActivityContest || st.Contest == nil || len(st.Ranked) != 1 || st.Ranked[0].Tag != bus.TagMult {
		t.Fatalf("%+v", st)
	}
	s.onCmd(cmd("handled", `{"row_id":1,"band":"20m","call":"EA8XYZ"}`))
	if st := s.compute(); len(st.Ranked) != 0 {
		t.Fatalf("%+v", st)
	}
	// the running instance cannot be deleted; DX mode releases it
	if err := s.DeleteInstance(st.Contest.ID); err == nil {
		t.Fatal("deleted the running instance")
	}
	s.onCmd(cmd("set_mode", "dx"))
	if got := s.compute(); got.Activity != bus.ActivityDX {
		t.Fatal(got.Activity)
	}
	if err := s.DeleteInstance(st.Contest.ID); err != nil {
		t.Fatal(err)
	}
}

func TestResumeActiveAfterRestart(t *testing.T) {
	s := newSvc(t)
	def, _ := os.ReadFile("testdata/dxcc-only.txt")
	f, _ := s.Upload("definition", "d.txt", string(def))
	if err := s.SwitchContest(bus.SwitchReq{New: true, DefinitionID: f.ID, StartDate: "2026-11-01"}); err != nil {
		t.Fatal(err)
	}
	s2 := New(s.cfg, s.st, s.log)
	s2.resumeActive()
	if s2.eng.Compute().Activity != bus.ActivityContest {
		t.Fatal("contest not resumed")
	}
}

func TestHTTPOverviewAndUploadErrors(t *testing.T) {
	s := newSvc(t)
	h := s.handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/contest", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"instances"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/contest/files", bytes.NewReader([]byte(`{"kind":"nope","name":"x","text":"y"}`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/contest/files", bytes.NewReader([]byte(`{"kind":"definition","name":"x.txt","text":"not a definition"}`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestProblemsAndTokenHygiene(t *testing.T) {
	s := newSvc(t)
	st := s.compute()
	joined := strings.Join(st.Problems, "|")
	if !strings.Contains(joined, "No candidates from bmrec") || !strings.Contains(joined, "Club Log confirmations not loaded") {
		t.Fatalf("%v", st.Problems)
	}
	// the token must not appear in anything published or served
	b, _ := json.Marshal(st)
	meta, _ := s.metaPayload()
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest("GET", "/contest", nil))
	for _, out := range [][]byte{b, meta, rec.Body.Bytes()} {
		if bytes.Contains(out, []byte(secret)) {
			t.Fatalf("token leaked: %s", out)
		}
	}
	s.mu.Lock()
	s.clState = "token rejected"
	s.mu.Unlock()
	if !strings.Contains(strings.Join(s.compute().Problems, "|"), "rejected the token") {
		t.Fatal("rejected token not reported")
	}
}

func TestStaleCandidatesProblem(t *testing.T) {
	s := newSvc(t)
	s.onCandidates(state(1, row(1, "EA8XYZ")))
	if strings.Contains(strings.Join(s.compute().Problems, "|"), "No candidates") {
		t.Fatal("fresh candidates reported missing")
	}
	s.now = func() time.Time { return time.Now().Add(3 * time.Minute) }
	if !strings.Contains(strings.Join(s.compute().Problems, "|"), "No candidates") {
		t.Fatal("stale candidates not reported")
	}
}
