package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"chase-queue/internal/bus"
	"chase-queue/internal/config"
)

const wasFile = `
[[award]]
code = "WAS"
kind = "field"
field = "STATE"
dxcc = [291]
items = ["NY", "CA"]
per_band = false
`

func svcWithAwards(t *testing.T, url string, goals ...config.GoalConf) *Service {
	t.Helper()
	s := newSvc(t)
	cfg := s.cfg
	if err := os.MkdirAll(cfg.AwardsDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.AwardsDir(), "was.toml"), []byte(wasFile), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Goals = goals
	if url != "" {
		cfg.StationLog = config.StationLogConf{URL: url, OwnCall: "DL9ET", Station: "hf", Slot: "station-log", ReloadMinutes: 5}
	}
	s2 := New(cfg, s.st, s.log)
	s2.eng.SetConfirmations(nil)
	return s2
}

func TestFieldAwardNeedsFromStationLogView(t *testing.T) {
	down := false
	srv := fakeStationLog(t, `{"worked":[
	 {"call":"K1ABC","band":"40m","mode_class":"phone","mode":"SSB","at":"2025-04-01T10:00:00Z","dxcc":291,"state":"ny"}]}`, &down)
	on := true
	s := svcWithAwards(t, srv.URL, config.GoalConf{Award: "was", Enabled: &on})
	s.onCandidates(state(1, row(1, "K1ABC"), row(2, "EA8XYZ")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.view.Run(ctx)
	go s.syncLogView(ctx)
	var st bus.ChaseState
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if st = s.compute(); s.view.Loaded() && s.eng.ConfirmationsLoaded() && len(st.Ranked) == 2 && st.Ranked[0].Call == "K1ABC" && len(st.Ranked[0].Needs) == 2 {
			break
		}
	}
	k := st.Ranked[0]
	// K1ABC was worked on 40m only: NY is worked but not confirmed (all bands), DXCC K is open on 20m
	if k.Call != "K1ABC" || len(k.Needs) != 2 {
		t.Fatalf("%+v", st.Ranked)
	}
	got := map[string]bus.Need{}
	for _, n := range k.Needs {
		got[n.Award] = n
	}
	if n := got["WAS"]; n.Key != "NY" || n.Status != "worked-unconfirmed" {
		t.Fatalf("%+v", got)
	}
	if n := got["dxcc"]; n.Key != "K" || n.Band != "20m" || n.Status != "unworked" {
		t.Fatalf("%+v", got)
	}
}

func TestUnknownGoalAwardRefusesToStart(t *testing.T) {
	s := svcWithAwards(t, "", config.GoalConf{Award: "nosuch"})
	if s.awardErr == nil {
		t.Fatal("goal for an unknown award accepted")
	}
	if err := s.Run(context.Background()); err == nil || err != s.awardErr {
		t.Fatalf("%v", err)
	}
}

func TestBadAwardFileRefusesToStart(t *testing.T) {
	s := newSvc(t)
	cfg := s.cfg
	os.MkdirAll(cfg.AwardsDir(), 0o750)
	os.WriteFile(filepath.Join(cfg.AwardsDir(), "x.toml"), []byte("[[award]]\ncode='SOTA'\nkind='reference'\n"), 0o600)
	if s2 := New(cfg, s.st, s.log); s2.awardErr == nil {
		t.Fatal("a reference award was accepted")
	}
}
