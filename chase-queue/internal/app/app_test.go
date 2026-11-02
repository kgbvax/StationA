package app

import (
	"bytes"
	"context"
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
	"chase-queue/internal/cluster"
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
	post := func(body string, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/contest/files", bytes.NewReader([]byte(body)))
		r.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if c := post(`{"kind":"definition","name":"x","text":"y"}`, "Origin", "http://evil.example").Code; c != http.StatusForbidden {
		t.Fatalf("browser origin accepted: %d", c)
	}
	rec = post(`{"kind":"nope","name":"x","text":"y"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d", rec.Code)
	}
	rec = post(`{"kind":"definition","name":"x.txt","text":"not a definition"}`)
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

func TestWorkedBeforeOnState(t *testing.T) {
	s := newSvc(t)
	s.eng.SetConfirmations(nil)
	at := time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC)
	if err := s.st.AddLogQSOs([]store.LogQSO{{Source: store.SourceLog4OM, Call: "EA8XYZ", Band: "40m", Mode: "CW", At: at}}); err != nil {
		t.Fatal(err)
	}
	s.onCandidates(state(1, row(1, "EA8XYZ"), row(2, "K1ABC")))
	st := s.compute()
	if len(st.Ranked) != 2 {
		t.Fatalf("%+v", st)
	}
	for _, r := range st.Ranked {
		switch r.Call {
		case "EA8XYZ":
			if r.Worked == nil || r.Worked.QSOs != 1 || r.Worked.ThisBand || r.Worked.Bands[0] != "40m" {
				t.Fatalf("%+v", r.Worked)
			}
		case "K1ABC":
			if r.Worked != nil {
				t.Fatalf("%+v", r.Worked)
			}
		}
		if r.Info != nil {
			t.Fatal("callbook data without a QRZ login")
		}
	}
}

func TestQRZPasswordHygiene(t *testing.T) {
	const pw = "qrz_super_secret_pw"
	t.Setenv("CHASE_QUEUE_QRZ_PASSWORD", pw)
	dir := t.TempDir()
	path := filepath.Join(dir, "c.toml")
	os.WriteFile(path, []byte("data_dir = \""+dir+"\"\n[qrz]\nuser = \"DL1ABC\"\n"), 0o600)
	cfg, err := config.Load(&config.Flags{ConfigPath: path})
	if err != nil || !cfg.QRZ.Enabled() {
		t.Fatalf("%v %+v", err, cfg.QRZ)
	}
	st, err := store.Open(filepath.Join(dir, "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.onCandidates(state(1, row(1, "EA8XYZ")))
	b, _ := json.Marshal(s.compute())
	meta, _ := s.metaPayload()
	for _, out := range [][]byte{b, meta} {
		if bytes.Contains(out, []byte(pw)) {
			t.Fatalf("password leaked: %s", out)
		}
	}
}

func TestClusterSpotFiltersAndProblem(t *testing.T) {
	s := newSvc(t)
	s.cfg.Cluster.Call = "DL9ET"
	s.eng.SetConfirmations(nil)
	at := time.Now()
	s.onClusterSpot(cluster.Spot{Spotter: "DL1ABC-#", Call: "EA8AAA", Band: "20m", FreqHz: 14025000, At: at}) // skimmer
	s.onClusterSpot(cluster.Spot{Spotter: "DL9ET", Call: "EA8BBB", Band: "20m", FreqHz: 14026000, At: at})    // own spot
	s.onClusterSpot(cluster.Spot{Spotter: "SP5XYZ", Call: "EA8CCC", Band: "20m", FreqHz: 14027000, At: at})
	st := s.compute()
	if len(st.Ranked) != 1 || st.Ranked[0].Call != "EA8CCC" || st.Ranked[0].Source != bus.SourceCluster {
		t.Fatalf("%+v", st.Ranked)
	}
	s.dxc = &cluster.Client{Addr: "127.0.0.1:1", Call: "DL9ET", Now: func() time.Time { return time.Now().Add(-2 * time.Minute) }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.dxc.Run(ctx, nil) // records "disconnected since" two minutes ago, then stops
	if !strings.Contains(strings.Join(s.compute().Problems, "|"), "DX cluster not connected") {
		t.Fatal("cluster problem not reported")
	}
}
