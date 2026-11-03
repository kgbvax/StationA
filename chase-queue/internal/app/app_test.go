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

func TestWorkedBeforeOnBandFollowsTheTier(t *testing.T) {
	s := newSvc(t)
	s.eng.SetConfirmations(nil)
	at := time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC)
	if err := s.st.AddLogQSOs([]store.LogQSO{{Source: store.SourceLog4OM, Call: "EA8XYZ", Band: "20m", Mode: "SSB", At: at}}); err != nil {
		t.Fatal(err)
	}
	s.eng.AddDXWorked("EA8XYZ", "20m")
	s.onCandidates(state(1, row(1, "EA8XYZ/P")))
	st := s.compute()
	if len(st.Ranked) != 1 {
		t.Fatalf("%+v", st)
	}
	r := st.Ranked[0]
	if r.Needed || r.Tag != "" || r.Worked == nil || !r.Worked.ThisBand || r.Worked.QSOs != 1 {
		t.Fatalf("%+v %+v", r, r.Worked)
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
	cc := config.ClusterConf{Name: "log4om", Address: "127.0.0.1:1", Call: "DL9ET"}
	s.eng.SetConfirmations(nil)
	at := time.Now()
	s.onClusterSpot(cc, cluster.Spot{Spotter: "DL1ABC-#", Call: "EA8AAA", Band: "20m", FreqHz: 14025000, At: at}) // skimmer
	s.onClusterSpot(cc, cluster.Spot{Spotter: "DL9ET", Call: "EA8BBB", Band: "20m", FreqHz: 14026000, At: at})    // own spot
	s.onClusterSpot(cc, cluster.Spot{Spotter: "SP5XYZ", Call: "EA8CCC", Band: "20m", FreqHz: 14027000, At: at})
	st := s.compute()
	if len(st.Ranked) != 1 || st.Ranked[0].Call != "EA8CCC" || st.Ranked[0].Source != bus.SourceCluster || st.Ranked[0].ClusterName != "log4om" {
		t.Fatalf("%+v", st.Ranked)
	}
	late := func() time.Time { return time.Now().Add(-2 * time.Minute) }
	s.dxc = []*clusterLogin{{conf: cc, client: &cluster.Client{Addr: cc.Address, Call: cc.Call, Now: late}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.dxc[0].client.Run(ctx, nil) // records "disconnected since" two minutes ago, then stops
	if !strings.Contains(strings.Join(s.compute().Problems, "|"), "DX cluster log4om not connected") {
		t.Fatal("cluster problem not reported")
	}
}

func TestSeveralClustersRBNAndProblems(t *testing.T) {
	s := newSvc(t)
	s.eng.SetConfirmations(nil)
	human := config.ClusterConf{Name: "log4om", Address: "127.0.0.1:1", Call: "DL9ET"}
	rbn := config.ClusterConf{Name: "rbn", Address: "127.0.0.1:2", Call: "DL9ET", Skimmer: true}
	at := time.Now()
	s.onClusterSpot(human, cluster.Spot{Spotter: "DL1ABC-#", Call: "EA8AAA", Band: "20m", FreqHz: 14025000, At: at}) // skimmer on a cluster without skimmer
	s.onClusterSpot(rbn, cluster.Spot{Spotter: "DL1ABC-#", Call: "EA8AAA", Band: "20m", FreqHz: 14025000, At: at})
	s.onClusterSpot(human, cluster.Spot{Spotter: "SP5XYZ", Call: "EA8CCC", Band: "20m", FreqHz: 14027000, At: at})
	rows := map[string]bus.Ranked{}
	for _, r := range s.compute().Ranked {
		rows[r.Call] = r
	}
	if r := rows["EA8AAA"]; r.ClusterName != "rbn" || r.Spot == nil || !r.Spot.RBN {
		t.Fatalf("%+v", r)
	}
	if r := rows["EA8CCC"]; r.ClusterName != "log4om" || r.Spot.RBN {
		t.Fatalf("%+v", r)
	}
	late := func() time.Time { return time.Now().Add(-2 * time.Minute) }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, cc := range []config.ClusterConf{human, rbn} {
		l := &clusterLogin{conf: cc, client: &cluster.Client{Addr: cc.Address, Call: cc.Call, Now: late}}
		l.client.Run(ctx, nil)
		s.dxc = append(s.dxc, l)
	}
	p := strings.Join(s.compute().Problems, "|")
	if !strings.Contains(p, "DX cluster log4om not connected") || !strings.Contains(p, "DX cluster rbn not connected") {
		t.Fatal(p)
	}
}

func TestBmrecGateFromConfigAndProblem(t *testing.T) {
	s := newSvc(t)
	s.eng.SetConfirmations(nil)
	trusted, plain := row(1, "EA8XYZ"), row(2, "EA8ABC")
	trusted.Trusted, plain.Trusted = true, false
	s.onCandidates(state(1, trusted, plain))
	st := s.compute()
	if len(st.Ranked) != 1 || st.Ranked[0].RowID != 1 || st.BmrecUnqualified != 1 {
		t.Fatalf("default gate is trusted: %+v %d", st.Ranked, st.BmrecUnqualified)
	}
	s.eng.SetBmrecGate(config.GateQualified)
	s.onCandidates(state(2, trusted, plain))
	st = s.compute()
	if len(st.Ranked) != 0 || st.BmrecUnqualified != 2 || !strings.Contains(strings.Join(st.Problems, "|"), "bmrec sends no qualified flag") {
		t.Fatalf("%+v %d %v", st.Ranked, st.BmrecUnqualified, st.Problems)
	}
}

func TestBuddiesFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Bmrec.Gate = config.GateOff
	cfg.Buddies = []config.BuddyConf{{Call: "EA8XYZ", Note: "Pedro", Boost: 2}}
	st, err := store.Open(filepath.Join(dir, "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.onCandidates(state(1, row(1, "EA8XYZ")))
	got := s.compute().Ranked
	if len(got) != 1 || !got[0].Buddy || got[0].BuddyNote != "Pedro" {
		t.Fatalf("%+v", got)
	}
}

// fakeStationLog serves /v1/worked from the given body; down makes it fail.
func fakeStationLog(t *testing.T, body string, down *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func svcWithStationLog(t *testing.T, url string) *Service {
	t.Helper()
	s := newSvc(t)
	cfg := s.cfg
	cfg.StationLog = config.StationLogConf{URL: url, OwnCall: "DL9ET", Station: "hf", Slot: "station-log", ReloadMinutes: 5}
	cfg.Log4OM = config.Log4OMUDP{Listen: "127.0.0.1:0", AllowFrom: []string{"127.0.0.1"}}
	s2 := New(cfg, s.st, s.log)
	s2.eng.SetConfirmations(nil)
	return s2
}

func TestStationLogViewFeedsWorkedAndConfirmed(t *testing.T) {
	down := false
	srv := fakeStationLog(t, `{"worked":[
	 {"call":"EA8XYZ","band":"20m","mode_class":"cw","mode":"CW","at":"2025-05-01T10:00:00Z","confirmed":true,"dxcc":29},
	 {"call":"K1ABC","band":"20m","mode_class":"phone","mode":"SSB","at":"2025-04-01T10:00:00Z"}]}`, &down)
	s := svcWithStationLog(t, srv.URL)
	if s.l4 != nil {
		t.Fatal("the Log4OM listener must stay off when station-log owns the feed")
	}
	// the local log says K1ABC was never worked on 20m; the view must win once loaded
	s.st.AddLogQSOs([]store.LogQSO{{Source: store.SourceLog4OM, Call: "W1AW", Band: "20m", Mode: "CW", At: time.Now()}})
	s.onCandidates(state(1, row(1, "EA8XYZ"), row(2, "K1ABC"), row(3, "W1AW")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.view.Run(ctx)
	go s.syncLogView(ctx)
	deadline := time.Now().Add(5 * time.Second)
	var st bus.ChaseState
	for time.Now().Before(deadline) {
		st = s.compute()
		if s.view.Loaded() && s.eng.ConfirmationsLoaded() && len(st.Ranked) == 3 && st.Ranked[2].OnBand {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	by := map[string]bus.Ranked{}
	for _, r := range st.Ranked {
		by[r.Call] = r
	}
	if r := by["EA8XYZ"]; r.Needed || r.Worked == nil || !r.Worked.ThisBand || r.Worked.QSOs != 1 {
		t.Fatalf("EA8XYZ confirmed on 20m must be listed, not needed: %+v %+v", r, r.Worked)
	}
	if r := by["K1ABC"]; r.Worked == nil || !r.Worked.ThisBand {
		t.Fatalf("K1ABC worked in the view: %+v", r)
	}
	if r := by["W1AW"]; r.Worked != nil || r.OnBand {
		t.Fatalf("the local log is not used once the view loaded: %+v", r)
	}
	for _, p := range st.Problems {
		if strings.Contains(p, "Log4OM") || strings.Contains(p, "station-log") {
			t.Fatalf("unexpected problem %q", p)
		}
	}
	// an event for a new QSO reaches the engine
	s.view.Apply([]byte(`{"id":"5","action":"added","qso":{"own_call":"DL9ET","call":"W1AW","band":"20m","mode":"CW","mode_class":"cw","at":"2025-07-01T10:00:00Z","updated":"2025-07-01T10:00:01Z"}}`))
	onBand := func() bool {
		for _, r := range s.compute().Ranked {
			if r.Call == "W1AW" && r.OnBand && r.Worked != nil {
				return true
			}
		}
		return false
	}
	for time.Now().Before(deadline) && !onBand() {
		time.Sleep(20 * time.Millisecond)
	}
	if !onBand() {
		t.Fatal("event not applied")
	}
}

func TestStationLogUnreachableFallsBackAndReports(t *testing.T) {
	down := true
	srv := fakeStationLog(t, `{"worked":[]}`, &down)
	s := svcWithStationLog(t, srv.URL)
	s.st.AddLogQSOs([]store.LogQSO{{Source: store.SourceLog4OM, Call: "W1AW", Band: "40m", Mode: "CW", At: time.Now()}})
	if err := s.view.Load(context.Background()); err == nil {
		t.Fatal("must fail while down")
	}
	s.onCandidates(state(1, row(1, "W1AW")))
	st := s.compute()
	if len(st.Ranked) != 1 || st.Ranked[0].Worked == nil || st.Ranked[0].Worked.Bands[0] != "40m" {
		t.Fatalf("local log is the interim history: %+v", st.Ranked)
	}
	found := false
	for _, p := range st.Problems {
		found = found || strings.HasPrefix(p, "station-log not reachable")
	}
	if !found {
		t.Fatalf("%v", st.Problems)
	}
	down = false
	if err := s.view.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range s.compute().Problems {
		if strings.Contains(p, "station-log") {
			t.Fatal(p)
		}
	}
}

func TestWithoutStationLogLog4OMStaysOn(t *testing.T) {
	s := newSvc(t)
	cfg := s.cfg
	cfg.Log4OM = config.Log4OMUDP{Listen: "127.0.0.1:0", AllowFrom: []string{"127.0.0.1"}}
	s2 := New(cfg, s.st, s.log)
	if s2.view != nil || s2.l4 == nil {
		t.Fatal("without [station_log] the Log4OM listener and local log stay")
	}
}
