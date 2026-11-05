package wavelog

// What these tests encode: the documented Wavelog API v2
// (https://docs.wavelog.org/developer/api-v2/, Wavelog >= 3.1.0), through the
// httptest fake in fake_test.go (which cites the doc sections). Real-instance
// behaviour is verified only up to the unauthenticated probe (status endpoint,
// invalid_token error shape); everything behind a token is unverified until
// the operator provides one.
//
//	(a) auth is "Authorization: Bearer wl2_..." only; the token is in no URL,
//	    body, log line, error text or /state;
//	(b) pull: GET /api/v2/qso?format=adif&since_id&station_id&per_page, rows
//	    ascending by id, data.adif/exported/lastfetchedid, meta.has_more, ADIF
//	    frequency in MHz; the cursor is lastfetchedid, written only after the
//	    page was imported; full resync = since_id 0;
//	(c) push: POST /api/v2/qso {import_type:"adif", station_profile_id:N,
//	    adif} -> data.parsed/imported/skipped/messages; skipped = duplicate =
//	    success; 400 validation_error = a record problem (bisected);
//	(d) errors: 401 / 403 insufficient_scope = auth (scope named), 429 honours
//	    Retry-After, 404/405/5xx/HTML = endpoint errors without attempts;
//	(e) a QSO is pushed once: no re-push for gained confirmations.
//
// The data is synthetic.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"station-log/internal/adif"
	"station-log/internal/importer"
	"station-log/internal/store"
)

func itoa(n int) string { return strconv.Itoa(n) }

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

type env struct {
	t    *testing.T
	f    *fakeWavelog
	st   *store.Store
	imp  *importer.Importer
	s    *Syncer
	logb *bytes.Buffer
	now  time.Time
}

func newEnv(t *testing.T, pull, push bool) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, f: newFake(t), st: st, logb: &bytes.Buffer{}, now: t0}
	e.imp = &importer.Importer{Store: st, Window: 5 * time.Minute}
	cl, err := NewClient(e.f.srv.URL, e.f.key, "")
	if err != nil {
		t.Fatal(err)
	}
	cl.MinGap = 0
	cl.HTTP = e.f.srv.Client()
	e.s = &Syncer{
		Cfg: Config{Pull: pull, Push: push, URLHost: "wl.test", Profiles: []Profile{{"DL9ET", "1"}},
			Interval: time.Minute, FullResync: 24 * time.Hour, Batch: 50, PullLimit: 2,
			PushSources: []string{"log4om", "adif"}, PushSince: t0.Add(-24 * time.Hour)},
		Client: cl, Store: st, Importer: e.imp, Now: func() time.Time { return e.now },
		Log: slog.New(slog.NewTextHandler(e.logb, nil)),
	}
	return e
}

func rec(call string, at time.Time, extra ...string) adif.Record {
	r := adif.Record{"CALL": call, "QSO_DATE": at.Format("20060102"), "TIME_ON": at.Format("150405"),
		"BAND": "20m", "MODE": "SSB", "FREQ": "14.200000"}
	for i := 0; i+1 < len(extra); i += 2 {
		r[extra[i]] = extra[i+1]
	}
	return r
}

func (e *env) logQSO(call string, at time.Time, own string, extra ...string) {
	e.t.Helper()
	r := rec(call, at, extra...)
	r["STATION_CALLSIGN"] = own
	r["RST_SENT"] = "59"
	if _, err := e.imp.Import(context.Background(), adif.Format(r), importer.Options{Source: "log4om"}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) count() int {
	n, err := e.st.Count()
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) cycle() error { return e.s.Cycle(context.Background(), true) }

func TestPullPagingCursorAndIncremental(t *testing.T) {
	e := newEnv(t, true, false)
	for i := 0; i < 5; i++ {
		e.f.addRow("1", rec("DK"+itoa(i)+"AA", t0.Add(time.Duration(i)*time.Hour))) // no STATION_CALLSIGN: profile own call applies
	}
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if e.count() != 5 {
		t.Fatalf("count %d, want 5", e.count())
	}
	if v, _, _ := e.st.Setting("wavelog_cursor_1"); v != "5" {
		t.Fatalf("cursor %q", v)
	}
	if got := ints(e.f.fetches); got != "0,2,4" {
		t.Fatalf("since_id sequence %s, want 0,2,4", got)
	}
	e.f.fetches = nil
	e.f.addRow("1", rec("DK9ZZ", t0.Add(9*time.Hour)))
	e.now = e.now.Add(time.Hour) // no full resync yet
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if e.count() != 6 || ints(e.f.fetches) != "5" {
		t.Fatalf("count %d fetches %v: incremental pull should start at the cursor", e.count(), e.f.fetches)
	}
	if qs, _ := e.st.ByCall("DK9ZZ", "DL9ET"); len(qs) != 1 || qs[0].Source != "wavelog" || qs[0].FreqHz != 14200000 {
		t.Fatalf("pulled QSO %+v (ADIF FREQ is MHz)", qs)
	}
}

func ints(v []int64) string {
	var s []string
	for _, n := range v {
		s = append(s, strconv.FormatInt(n, 10))
	}
	return strings.Join(s, ",")
}

func TestCursorOnlyAdvancesAfterGoodPage(t *testing.T) {
	e := newEnv(t, true, false)
	for i := 0; i < 5; i++ {
		e.f.addRow("1", rec("DK"+itoa(i)+"AA", t0.Add(time.Duration(i)*time.Hour)))
	}
	e.f.failPage = 2 // the second request fails
	if err := e.cycle(); err == nil {
		t.Fatal("want an error")
	}
	// full resync pass: the cursor is only written when the whole pass ended
	if v, ok, _ := e.st.Setting("wavelog_cursor_1"); ok && v != "0" {
		t.Fatalf("cursor %q after a failed pass", v)
	}
	if e.count() != 2 {
		t.Fatalf("first page should be stored, count %d", e.count())
	}
	e.f.failPage = 0
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if e.count() != 5 {
		t.Fatalf("count %d after resume", e.count())
	}
}

func TestFullResyncCatchesConfirmationOnOldRow(t *testing.T) {
	e := newEnv(t, true, false)
	e.f.addRow("1", rec("DK1AA", t0))
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	e.f.rows[0].rec["LOTW_QSL_RCVD"] = "Y" // Wavelog's own LoTW sync, same row id
	e.now = e.now.Add(time.Hour)
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if qs, _ := e.st.ByCall("DK1AA", ""); qs[0].LoTWRcvd {
		t.Fatal("an incremental pull must not see a change on an old row")
	}
	e.now = e.now.Add(25 * time.Hour)
	e.f.fetches = nil
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if qs, _ := e.st.ByCall("DK1AA", ""); !qs[0].LoTWRcvd {
		t.Fatal("the full resync should merge the confirmation")
	}
	if ints(e.f.fetches)[:1] != "0" {
		t.Fatalf("full resync starts at 0, got %v", e.f.fetches)
	}
}

func TestPushOnceAndRoundTripMerges(t *testing.T) {
	e := newEnv(t, true, true)
	e.logQSO("DK1AA", t0, "DL9ET")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != 1 {
		t.Fatalf("posts %d, want 1", len(e.f.posts))
	}
	if !strings.Contains(e.f.posts[0], "<RST_SENT:2>59") {
		t.Fatalf("the raw record's RST should survive the push: %q", e.f.posts[0])
	}
	// the push cycle ran after the pull; the next pull returns it (fake stored it)
	e.now = e.now.Add(time.Hour)
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if e.count() != 1 {
		t.Fatalf("count %d: the pushed QSO coming back must merge, not add", e.count())
	}
	if len(e.f.posts) != 1 {
		t.Fatalf("posts %d: pushed QSO must not be pushed again", len(e.f.posts))
	}
	if st := e.s.Status(); st.Pushed != 1 || st.PushPending != 0 {
		t.Fatalf("status %+v", st)
	}
}

func TestPulledQSONeverPushed(t *testing.T) {
	e := newEnv(t, true, true)
	e.f.addRow("1", rec("DK2BB", t0, "STATION_CALLSIGN", "DL9ET"))
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if e.count() != 1 || len(e.f.posts) != 0 {
		t.Fatalf("count %d posts %d", e.count(), len(e.f.posts))
	}
}

func TestPushSinceDefaultsToEnableTime(t *testing.T) {
	e := newEnv(t, false, true)
	e.s.Cfg.PushSince = time.Time{}
	e.logQSO("DK1OLD", t0.Add(-48*time.Hour), "DL9ET")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	e.logQSO("DK1NEW", t0.Add(time.Minute), "DL9ET")
	e.now = e.now.Add(time.Minute)
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != 1 || !strings.Contains(e.f.posts[0], "DK1NEW") {
		t.Fatalf("only the QSO after the enable time goes out: %q", e.f.posts)
	}
}

func TestNoProfileSkipped(t *testing.T) {
	e := newEnv(t, false, true)
	e.logQSO("DK1AA", t0, "DA0XYZ")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != 0 || e.s.Status().SkippedNoProfile != 1 {
		t.Fatalf("posts %d status %+v", len(e.f.posts), e.s.Status())
	}
}

func TestDuplicateResponseCountsAsPushed(t *testing.T) {
	e := newEnv(t, false, true)
	e.f.dup = true
	e.logQSO("DK1AA", t0, "DL9ET")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	qs, _ := e.st.ByCall("DK1AA", "")
	if msg, _ := e.st.PushError(qs[0].ID, "1"); msg != "duplicate" {
		t.Fatalf("last_error %q", msg)
	}
	n := len(e.f.posts)
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != n {
		t.Fatal("a duplicate must not be retried")
	}
}

func TestBatchFallsBackToSingleRecords(t *testing.T) {
	e := newEnv(t, false, true)
	e.f.rejectMulti = true
	e.logQSO("DK1AA", t0, "DL9ET")
	e.logQSO("DK2BB", t0.Add(time.Hour), "DL9ET")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != 3 || e.s.Status().Pushed != 2 { // the pair is refused, then one request per record
		t.Fatalf("posts %d status %+v", len(e.f.posts), e.s.Status())
	}
}

func TestPermanentFailureStopsAfterFiveAttempts(t *testing.T) {
	// a record that fails alone while a sibling was stored is the record's
	// own failure; it gets 5 attempts across cycles, then push_failed
	e := newEnv(t, false, true)
	e.f.badCalls = map[string]bool{"BAD": true}
	e.logQSO("DK1AA", t0, "DL9ET")
	e.logQSO("BAD", t0.Add(time.Hour), "DL9ET")
	for i := 0; i < 8; i++ {
		if err := e.cycle(); err != nil {
			t.Fatal(err)
		}
	}
	// cycle 1: pair (refused), DK1AA, BAD; cycles 2-5: BAD alone
	if len(e.f.posts) != 7 {
		t.Fatalf("posts %d, want 7", len(e.f.posts))
	}
	if st := e.s.Status(); st.PushFailed != 1 || st.PushPending != 0 || st.Pushed != 1 || e.attempts("2") != 5 {
		t.Fatalf("status %+v attempts %d", st, e.attempts("2"))
	}
}

func TestAuthErrorBacksOffAndKeyNeverLeaks(t *testing.T) {
	e := newEnv(t, true, true)
	e.s.Client.Key = "wrong"
	err := e.cycle()
	ae, ok := err.(*APIError)
	if !ok || !ae.Auth {
		t.Fatalf("err %v, want an auth APIError", err)
	}
	e.s.finish(err)
	if st := e.s.Status(); st.BackoffSeconds < int(maxBackoff.Seconds()) || st.Error == "" {
		t.Fatalf("status %+v", st)
	}
	if ae.Status != 401 || ae.Code != "invalid_token" {
		t.Fatalf("%+v", ae)
	}
	for _, s := range []string{e.logb.String(), err.Error(), strings.Join(e.f.reqs, "\n"), strings.Join(e.f.bodies, "\n")} {
		if strings.Contains(s, "wrong") || strings.Contains(s, e.f.key) {
			t.Fatalf("token in %q", s)
		}
	}
	b, _ := json.Marshal(e.s.Status())
	if strings.Contains(string(b), e.f.key) || strings.Contains(string(b), "wrong") {
		t.Fatal("key in status JSON")
	}
}

func TestTokenInEchoedBodyIsRedacted(t *testing.T) {
	c := &Client{Key: "secret-key-xyz"}
	if s := c.snippet([]byte("error for secret-key-xyz here")); strings.Contains(s, "secret-key-xyz") {
		t.Fatal(s)
	}
	f := newFake(t)
	f.status, f.body = 400, `{"error":{"code":"validation_error","message":"bad value `+f.key+`"}}`
	cl, _ := NewClient(f.srv.URL, f.key, "")
	cl.MinGap = 0
	if _, err := cl.PostADIF(context.Background(), "1", "<EOR>"); err == nil || strings.Contains(err.Error(), f.key) {
		t.Fatalf("err %v", err)
	}
}

func TestOutageWarnsOnceAndBackoffDoublesAndResets(t *testing.T) {
	e := newEnv(t, true, true)
	fail := &APIError{Status: 500, Msg: "HTTP 500: boom"}
	var secs []int
	for i := 0; i < 5; i++ {
		e.s.finish(fail)
		secs = append(secs, e.s.Status().BackoffSeconds)
	}
	if n := strings.Count(e.logb.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings for one outage:\n%s", n, e.logb.String())
	}
	for i, want := range []int{30, 60, 120, 240, 480} {
		if secs[i] < want || secs[i] > want*6/5+1 {
			t.Fatalf("backoff %v, step %d want ~%d", secs, i, want)
		}
	}
	if e.s.Status().Error == "" {
		t.Fatal("error should be set")
	}
	e.s.finish(nil)
	if st := e.s.Status(); st.Error != "" || st.BackoffSeconds != 0 {
		t.Fatalf("status %+v", st)
	}
	e.s.finish(fail)
	if n := strings.Count(e.logb.String(), "level=WARN"); n != 2 {
		t.Fatalf("a new outage should warn again, got %d", n)
	}
}

func TestRetryAfterHonoured(t *testing.T) {
	e := newEnv(t, true, false)
	e.f.status, e.f.retry = 429, "600"
	e.f.body = `{"error":{"code":"rate_limited","message":"slow down","details":{"retry_after":5}}}`
	err := e.cycle()
	ae, ok := err.(*APIError)
	if !ok || ae.RetryAfter != 10*time.Minute {
		t.Fatalf("err %#v", err)
	}
	e.s.finish(err)
	if e.s.Status().BackoffSeconds < 600 {
		t.Fatalf("backoff %d", e.s.Status().BackoffSeconds)
	}
}

func TestBuildADIFOverlaysMergedValuesOnRaw(t *testing.T) {
	c := store.PushCandidate{
		Raw: adif.Format(adif.Record{"CALL": "DK1AA", "RST_SENT": "59", "NAME": "Test", "GRIDSQUARE": "JO32"}),
		QSO: store.QSO{OwnCall: "DL9ET", Call: "DK1AA", Band: "20m", Mode: "SSB", At: t0, FreqHz: 14200000, Grid: "JO32WE", LoTWRcvd: true, DXCC: 230},
	}
	r := adif.Parse(BuildADIF(c))[0]
	if r["NAME"] != "Test" || r["RST_SENT"] != "59" || r["GRIDSQUARE"] != "JO32WE" || r["LOTW_QSL_RCVD"] != "Y" ||
		r["DXCC"] != "230" || r["FREQ"] != "14.200000" || r["TIME_ON"] != "120000" || r["STATION_CALLSIGN"] != "DL9ET" {
		t.Fatalf("%v", r)
	}
	// struct-only (no raw)
	r = adif.Parse(BuildADIF(store.PushCandidate{QSO: c.QSO}))[0]
	if r["CALL"] != "DK1AA" || r["BAND"] != "20m" {
		t.Fatalf("%v", r)
	}
}

func TestPanicInCycleIsRecovered(t *testing.T) {
	e := newEnv(t, true, false)
	e.s.Client = nil // nil dereference inside the pull
	if err := e.cycle(); err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("err %v", err)
	}
}

// restart builds a second Syncer over the same store, as after a service
// restart.
func (e *env) restart() *Syncer {
	old := e.s
	s := &Syncer{Cfg: old.Cfg, Client: old.Client, Store: old.Store, Importer: old.Importer,
		Now: old.Now, Log: old.Log}
	e.s = s
	return s
}

func (e *env) attempts(qsoID string) int {
	e.t.Helper()
	cs, err := e.st.PushCandidates(store.PushQuery{OwnCalls: []string{"DL9ET"}, ProfileID: "1"})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range cs {
		if c.QSO.ID == qsoID {
			return c.Attempts
		}
	}
	return -1
}

func TestPullBadBodiesAreErrorsNotSuccess(t *testing.T) {
	for _, body := range []string{`{"hello":"world"}`, `<html>login</html>`, `{"data":[]}`, `{"error":{}}`} {
		e := newEnv(t, true, false)
		e.f.addRow("1", rec("DK1AA", t0))
		e.f.okBody = body
		if err := e.cycle(); err == nil {
			t.Fatalf("body %s: want an error", body)
		}
		if st := e.s.Status(); !st.LastPullAt.IsZero() || !st.LastFullResync.IsZero() {
			t.Fatalf("status %+v: nothing may be marked done", st)
		}
		if _, ok, _ := e.st.Setting("wavelog_full_1"); ok {
			t.Fatal("full resync marked done after a failed pull")
		}
	}
}

func TestPushBadBodiesAreEndpointErrors(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":{"parsed":"x"}}`, `<html>login</html>`, `{"data":{"imported":1}}`} {
		e := newEnv(t, false, true)
		e.f.okBody = body
		e.logQSO("DK1AA", t0, "DL9ET")
		for i := 0; i < 7; i++ {
			if err := e.cycle(); err == nil {
				t.Fatalf("body %s: want an error", body)
			}
		}
		if a := e.attempts("1"); a > 0 {
			t.Fatalf("body %s: %d attempts counted against the QSO", body, a)
		}
	}
}

func TestEndpointErrorsCountNoAttempts(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{404, "not found"}, {404, `{"error":{"code":"not_found","message":"Unknown resource"}}`},
		{405, `{"error":{"code":"method_not_allowed","message":"nope"}}`}, {408, "timeout"}, {413, "too big"},
		{400, "<html>Bad Request</html>"}, {400, `{"error":{"code":"invalid_json","message":"bad"}}`},
		{500, `{"error":{"code":"internal_error","message":"boom"}}`}, {502, "<html>Bad Gateway</html>"}} {
		e := newEnv(t, false, true)
		e.f.status, e.f.body = tc.status, tc.body
		e.logQSO("DK1AA", t0, "DL9ET")
		for i := 0; i < 8; i++ {
			if err := e.cycle(); err == nil {
				t.Fatalf("%d: want an error", tc.status)
			}
		}
		if a := e.attempts("1"); a > 0 {
			t.Fatalf("%d: %d attempts counted", tc.status, a)
		}
		if st := e.s.Status(); st.PushFailed != 0 || st.PushPending != 1 {
			t.Fatalf("%d: status %+v", tc.status, st)
		}
		// the endpoint comes back: the backlog goes out
		e.f.status = 0
		if err := e.cycle(); err != nil || len(e.f.posts) != 1 {
			t.Fatalf("%d: recovery err %v posts %d", tc.status, err, len(e.f.posts))
		}
	}
}

func TestValidationErrorIsPerQSOFailure(t *testing.T) {
	e := newEnv(t, false, true)
	e.f.badCalls = map[string]bool{"BAD": true}
	e.logQSO("DK1AA", t0, "DL9ET")
	e.logQSO("BAD", t0.Add(time.Hour), "DL9ET")
	for i := 0; i < 8; i++ {
		if err := e.cycle(); err != nil {
			t.Fatal(err)
		}
	}
	if st := e.s.Status(); st.PushFailed != 1 || e.attempts("2") != 5 {
		t.Fatalf("status %+v attempts %d", st, e.attempts("2"))
	}
	if msg, _ := e.st.PushError("2", "1"); !strings.Contains(msg, "invalid CALL BAD") {
		t.Fatalf("reason %q", msg)
	}
}

func TestExhaustedAttemptsResetAfter24hAndOnURLChange(t *testing.T) {
	// a record that fails alone next to a stored sibling: cycle 1 costs 3
	// POSTs (pair, sibling, record), cycles 2-5 one each = 7, then it is exhausted
	setup := func() *env {
		e := newEnv(t, false, true)
		e.f.badCalls = map[string]bool{"BAD": true}
		e.logQSO("DK1AA", t0, "DL9ET")
		e.logQSO("BAD", t0.Add(time.Hour), "DL9ET")
		for i := 0; i < 6; i++ {
			e.cycle()
		}
		if len(e.f.posts) != 7 || e.s.Status().PushFailed != 1 {
			t.Fatalf("posts %d status %+v", len(e.f.posts), e.s.Status())
		}
		e.f.badCalls = nil
		return e
	}
	e := setup()
	e.now = e.now.Add(23 * time.Hour)
	e.cycle()
	if len(e.f.posts) != 7 {
		t.Fatal("reset before 24 h")
	}
	e.now = e.now.Add(2 * time.Hour)
	if err := e.cycle(); err != nil || len(e.f.posts) != 8 || e.s.Status().PushFailed != 0 {
		t.Fatalf("err %v posts %d status %+v", err, len(e.f.posts), e.s.Status())
	}

	// profile map change resets too
	e2 := setup()
	e2.restart().Cfg.Profiles = []Profile{{"DL9ET", "1"}, {"DL1XX", "7"}}
	if err := e2.cycle(); err != nil || len(e2.f.posts) != 8 {
		t.Fatalf("err %v posts %d", err, len(e2.f.posts))
	}
}

func TestURLChangeResetsCursorsAndLedger(t *testing.T) {
	// push-only: a pulled-back copy would carry the wavelog source and never be pushed again
	e := newEnv(t, false, true)
	e.s.Cfg.URL = "https://a.example"
	if err := e.st.SetSetting("wavelog_cursor_1", "7"); err != nil {
		t.Fatal(err)
	}
	e.logQSO("DK1BB", t0, "DL9ET")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := e.st.Setting("wavelog_cursor_1"); v != "7" || len(e.f.posts) != 1 {
		t.Fatalf("cursor %q posts %d", v, len(e.f.posts))
	}
	// same URL after a restart: nothing resets
	e.restart()
	if err := e.cycle(); err != nil || len(e.f.posts) != 1 {
		t.Fatalf("err %v posts %d", err, len(e.f.posts))
	}
	s := e.restart()
	s.Cfg.URL = "https://b.example"
	e.f.posts = nil
	e.now = e.now.Add(time.Minute)
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != 1 {
		t.Fatalf("ledger not reset: %d posts", len(e.f.posts))
	}
	if !strings.Contains(s.Status().Note, "reset") || !strings.Contains(e.logb.String(), "level=WARN") {
		t.Fatalf("note %q log %s", s.Status().Note, e.logb.String())
	}
	if v, ok, _ := e.st.Setting("wavelog_cursor_1"); ok {
		t.Fatalf("cursor %q survived the URL change", v)
	}
}

func TestPushSincePersistedAcrossRestart(t *testing.T) {
	e := newEnv(t, false, true)
	e.s.Cfg.PushSince = time.Time{}
	e.logQSO("DK1OLD", t0.Add(-48*time.Hour), "DL9ET")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(48 * time.Hour) // the restart is much later
	e.restart()
	e.logQSO("DK1MID", t0.Add(time.Hour), "DL9ET") // after enable, before the restart
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != 1 || !strings.Contains(e.f.posts[0], "DK1MID") {
		t.Fatalf("persisted push_since not used: %q", e.f.posts)
	}
}

func TestUnreadablePushSinceReinitialisesToNow(t *testing.T) {
	for _, bad := range []string{"garbage", ""} {
		e := newEnv(t, false, true)
		e.s.Cfg.PushSince = time.Time{}
		if err := e.st.SetSetting("wavelog_push_since", bad); err != nil {
			t.Fatal(err)
		}
		e.logQSO("DK1OLD", t0.Add(-48*time.Hour), "DL9ET")
		if err := e.cycle(); err != nil {
			t.Fatal(err)
		}
		if len(e.f.posts) != 0 {
			t.Fatalf("%q: history was bulk-pushed: %q", bad, e.f.posts)
		}
		v, _, _ := e.st.Setting("wavelog_push_since")
		if tm, err := time.Parse(time.RFC3339, v); err != nil || !tm.Equal(t0) {
			t.Fatalf("%q: setting %q", bad, v)
		}
		if !strings.Contains(e.logb.String(), "level=WARN") {
			t.Fatalf("%q: no warning", bad)
		}
	}
}

func TestProfilesAreIndependent(t *testing.T) {
	e := newEnv(t, true, true)
	e.s.Cfg.Profiles = []Profile{{"DL9ET", "1"}, {"DL1XX", "2"}}
	e.f.forbidStation = "1" // a per-profile refusal, not an endpoint failure
	e.f.addRow("2", rec("DK2AA", t0))
	e.logQSO("DK1AA", t0, "DL9ET")
	e.logQSO("DK2BB", t0, "DL1XX")
	if err := e.cycle(); err != nil {
		t.Fatalf("a refused profile must not fail the cycle: %v", err)
	}
	if e.count() != 3 {
		t.Fatalf("count %d: profile 2 must still be pulled", e.count())
	}
	if len(e.f.posts) != 1 || e.s.Status().Pushed != 1 {
		t.Fatalf("posts %d: profile 2's QSO must be pushed, profile 1 skipped", len(e.f.posts))
	}
	if !e.s.Status().LastPullAt.IsZero() {
		t.Fatal("last_pull_at must not advance on a partial pull")
	}
}

func TestKickRespectsBackoff(t *testing.T) {
	e := newEnv(t, false, true)
	e.s.Debounce = time.Millisecond
	e.s.Cfg.Interval = time.Hour
	e.f.status = 500
	e.logQSO("DK1AA", t0, "DL9ET")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	hits := func() int { e.f.mu.Lock(); defer e.f.mu.Unlock(); return e.f.hits }
	deadline := time.Now().Add(5 * time.Second)
	for e.s.Status().Error == "" {
		if time.Now().After(deadline) {
			t.Fatal("first cycle never failed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	before := hits()
	e.s.Kick()
	time.Sleep(200 * time.Millisecond)
	if hits() != before {
		t.Fatalf("kick pushed during backoff: %d -> %d requests", before, hits())
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if d := parseRetryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now); d != 90*time.Second {
		t.Fatalf("date form: %v", d)
	}
	if d := parseRetryAfter("120", now); d != 2*time.Minute {
		t.Fatalf("seconds form: %v", d)
	}
	if d := parseRetryAfter(now.Add(-time.Hour).Format(http.TimeFormat), now); d != 0 {
		t.Fatalf("past date: %v", d)
	}
	// end to end through the client
	e := newEnv(t, true, false)
	e.f.status, e.f.retry = 429, time.Now().Add(10*time.Minute).UTC().Format(http.TimeFormat)
	e.f.body = `{"error":{"code":"rate_limited","message":"slow","details":{"retry_after":1}}}`
	var ae *APIError
	if err := e.cycle(); !errors.As(err, &ae) || ae.RetryAfter < 9*time.Minute {
		t.Fatalf("err %#v", err)
	}
}

func (e *env) lastPost() string {
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	return e.f.bodies[len(e.f.bodies)-1]
}

func TestRequestShapes(t *testing.T) {
	e := newEnv(t, true, true)
	e.s.Cfg.PullLimit = 1000
	e.logQSO("DK1AA", t0, "DL9ET")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	for i, a := range e.f.auths {
		if a != "Bearer "+e.f.key {
			t.Fatalf("request %d Authorization %q", i, a)
		}
	}
	q := e.f.query(0)
	if q.Get("format") != "adif" || q.Get("since_id") != "0" || q.Get("station_id") != "1" || q.Get("per_page") != "1000" {
		t.Fatalf("pull query %v", q)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(e.lastPost()), &body); err != nil {
		t.Fatal(err)
	}
	if body["import_type"] != "adif" || body["station_profile_id"] != float64(1) || !strings.Contains(body["adif"].(string), "<CALL:5>DK1AA") || len(body) != 3 {
		t.Fatalf("push body %v", body)
	}
	for _, r := range e.f.reqs {
		if strings.Contains(r, e.f.key) || strings.Contains(r, "key=") {
			t.Fatalf("token in URL %q", r)
		}
	}
	// a legacy (v1) path never gets called
	for _, r := range e.f.reqs {
		if !strings.Contains(r, "/index.php/api/v2/qso") {
			t.Fatalf("unexpected request %q", r)
		}
	}
}

func TestHasMoreDrivesPagingNotCount(t *testing.T) {
	e := newEnv(t, true, false)
	e.f.addRow("1", rec("DK1AA", t0))
	e.f.addRow("1", rec("DK2BB", t0.Add(time.Hour))) // exactly one full page (PullLimit 2)
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if got := ints(e.f.fetches); got != "0" || e.count() != 2 {
		t.Fatalf("fetches %s count %d: has_more=false must end the pull", got, e.count())
	}
}

func TestEmptyPageNullADIF(t *testing.T) {
	e := newEnv(t, true, false)
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if e.count() != 0 || e.s.Status().LastPullAt.IsZero() {
		t.Fatalf("count %d status %+v", e.count(), e.s.Status())
	}
	if v, _, _ := e.st.Setting("wavelog_cursor_1"); v != "" && v != "0" {
		t.Fatalf("cursor %q", v)
	}
}

func TestPullOnlySeesTheProfilesStation(t *testing.T) {
	e := newEnv(t, true, false)
	e.f.addRow("1", rec("DK1AA", t0))
	e.f.addRow("9", rec("DK9XX", t0))
	if err := e.cycle(); err != nil || e.count() != 1 {
		t.Fatalf("err %v count %d", err, e.count())
	}
}

func TestNoRepushWhenAPushedQSOGainsConfirmations(t *testing.T) {
	e := newEnv(t, false, true)
	e.logQSO("DK1AA", t0, "DL9ET")
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	// a second source confirms it (LoTW) and the QSO changes
	e.logQSO("DK1AA", t0, "DL9ET", "LOTW_QSL_RCVD", "Y", "NAME", "Changed")
	e.now = e.now.Add(time.Hour)
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != 1 {
		t.Fatalf("posts %d: a QSO is pushed once", len(e.f.posts))
	}
	if st := e.s.Status(); st.PushPending != 0 || st.Pushed != 1 {
		t.Fatalf("status %+v", st)
	}
}

func TestSkippedAndImportedMixInABatchIsSuccess(t *testing.T) {
	e := newEnv(t, false, true)
	e.logQSO("DK1AA", t0, "DL9ET")
	e.logQSO("DK2BB", t0.Add(time.Hour), "DL9ET")
	// Wavelog already has the first one (its own LoTW import, say)
	e.f.addRow("1", rec("DK1AA", t0))
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) != 1 || e.s.Status().Pushed != 2 || e.s.Status().PushPending != 0 {
		t.Fatalf("posts %d status %+v", len(e.f.posts), e.s.Status())
	}
}

func TestBadRecordIsBisectedOthersGoThrough(t *testing.T) {
	for _, partial := range []bool{false, true} {
		e := newEnv(t, false, true)
		if partial {
			e.f.partialBad = map[string]bool{"BAD": true} // 201 with imported < parsed
		} else {
			e.f.badCalls = map[string]bool{"BAD": true} // 400 validation_error for the whole request
		}
		for i, c := range []string{"DK1AA", "DK2BB", "BAD", "DK4DD", "DK5EE"} {
			e.logQSO(c, t0.Add(time.Duration(i)*time.Hour), "DL9ET")
		}
		if err := e.cycle(); err != nil {
			t.Fatalf("partial=%v: %v", partial, err)
		}
		st := e.s.Status()
		if st.Pushed != 4 || e.attempts("3") != 1 || st.PushPending != 1 { // the bad one stays pending until 5 attempts
			t.Fatalf("partial=%v: status %+v bad attempts %d", partial, st, e.attempts("3"))
		}
		if n, _ := e.st.PushedCount("1"); n != 4 {
			t.Fatalf("partial=%v: pushed %d", partial, n)
		}
		if msg, _ := e.st.PushError("3", "1"); msg == "" {
			t.Fatalf("partial=%v: the bad record carries no reason", partial)
		}
		// the good ones are in the fake, the bad one is not
		if len(e.f.rows) != 4 {
			t.Fatalf("partial=%v: wavelog holds %d rows", partial, len(e.f.rows))
		}
	}
}

func TestInsufficientScopeNamesScopeAndKeepsOtherDirection(t *testing.T) {
	// no qso:write: the pull still works, the push reports the scope
	e := newEnv(t, true, true)
	e.f.scopes = []string{"qso:read"}
	e.f.addRow("1", rec("DK9ZZ", t0, "STATION_CALLSIGN", "DL9ET"))
	e.logQSO("DK1AA", t0, "DL9ET")
	err := e.cycle()
	var ae *APIError
	if !errors.As(err, &ae) || !ae.Auth || ae.Scope != "qso:write" {
		t.Fatalf("err %v", err)
	}
	e.s.finish(err)
	st := e.s.Status()
	if !strings.Contains(st.Error, "qso:write") || st.BackoffSeconds < int(maxBackoff.Seconds()) {
		t.Fatalf("status %+v", st)
	}
	if e.count() != 2 { // the pulled one plus the logged one
		t.Fatalf("count %d: pull must not be blocked by a missing write scope", e.count())
	}
	if a := e.attempts("1"); a > 0 {
		t.Fatalf("%d attempts counted for a scope problem", a)
	}

	// no qso:read: the push still goes out
	e = newEnv(t, true, true)
	e.f.scopes = []string{"qso:write"}
	e.logQSO("DK1AA", t0, "DL9ET")
	err = e.cycle()
	if !errors.As(err, &ae) || ae.Scope != "qso:read" {
		t.Fatalf("err %v", err)
	}
	if len(e.f.posts) != 1 || e.s.Status().Pushed != 1 {
		t.Fatalf("posts %d: push must not be blocked by a missing read scope", len(e.f.posts))
	}
}

func TestRateLimitedDetailsRetryAfterWithoutHeader(t *testing.T) {
	e := newEnv(t, true, false)
	e.f.status = 429
	e.f.body = `{"error":{"code":"rate_limited","message":"slow down","details":{"retry_after":240}}}`
	err := e.cycle()
	var ae *APIError
	if !errors.As(err, &ae) || ae.RetryAfter != 4*time.Minute || ae.Auth {
		t.Fatalf("err %#v", err)
	}
	e.s.finish(err)
	if e.s.Status().BackoffSeconds < 240 {
		t.Fatalf("backoff %d", e.s.Status().BackoffSeconds)
	}
}

func TestInvalidTokenShapeFromRealProbe(t *testing.T) {
	// verbatim shape of the real instance's answer to an invalid token
	e := newEnv(t, true, false)
	e.f.status = 401
	e.f.body = `{"error":{"code":"invalid_token","message":"Invalid or revoked API token"}}`
	var ae *APIError
	if err := e.cycle(); !errors.As(err, &ae) || !ae.Auth || ae.Code != "invalid_token" || ae.Scope != "" {
		t.Fatalf("err %#v", err)
	}
}

func TestClientTokenAndStations(t *testing.T) {
	f := newFake(t)
	cl, _ := NewClient(f.srv.URL, f.key, "")
	cl.MinGap = 0
	ti, err := cl.Token(context.Background())
	if err != nil || len(ti.Scopes) != 3 || ti.Scopes[0] != "qso:read" {
		t.Fatalf("%+v %v", ti, err)
	}
	sts, err := cl.Stations(context.Background())
	if err != nil || len(sts) != 2 || sts[1].ID != "7" || sts[1].Callsign != "DL9ET/P" || sts[1].Name != "Portable" {
		t.Fatalf("%+v %v", sts, err)
	}
	f.scopes = []string{"qso:read"}
	var ae *APIError
	if _, err := cl.Stations(context.Background()); !errors.As(err, &ae) || ae.Scope != "station:read" {
		t.Fatalf("err %v", err)
	}
}

func TestPostADIFRejectsNonNumericProfile(t *testing.T) {
	f := newFake(t)
	cl, _ := NewClient(f.srv.URL, f.key, "")
	if _, err := cl.PostADIF(context.Background(), "home", "<EOR>"); err == nil || f.hits != 0 {
		t.Fatalf("err %v hits %d", err, f.hits)
	}
}

// requests counts the requests that reached the fake.
func (e *env) requests() int {
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	return e.f.hits
}

func TestProfileRefusalsAreSkippedNotTokenErrors(t *testing.T) {
	for _, code := range []string{"forbidden", "insufficient_club_permission", "club_access_revoked"} {
		e := newEnv(t, true, true)
		e.s.Cfg.Profiles = []Profile{{"DL9ET", "1"}, {"DL1XX", "2"}}
		e.f.forbidStation, e.f.forbidCode = "1", code
		e.f.addRow("2", rec("DK2AA", t0))
		e.logQSO("DK1AA", t0, "DL9ET")
		e.logQSO("DK2BB", t0, "DL1XX")
		err := e.cycle()
		if err != nil {
			t.Fatalf("%s: err %v", code, err)
		}
		if tokenDead(err) || isAuth(err) {
			t.Fatalf("%s: a profile refusal is no token problem", code)
		}
		st := e.s.Status()
		if len(st.Problems) != 1 || !strings.Contains(st.Problems[0], "DL9ET") || !strings.Contains(st.Problems[0], "station 1") ||
			!strings.Contains(st.Problems[0], code) {
			t.Fatalf("%s: problems %q", code, st.Problems)
		}
		if st.Error != "" || st.BackoffSeconds != 0 || e.count() != 3 || st.Pushed != 1 {
			t.Fatalf("%s: status %+v count %d", code, st, e.count())
		}
		if a := e.attempts("1"); a > 0 {
			t.Fatalf("%s: %d attempts counted for a refused profile", code, a)
		}
		// the first profile is refused once per cycle: its pull only, no push
		if n := e.requests(); n > 5 { // 403 pull(1), pull(2), push(2), nothing else
			t.Fatalf("%s: %d requests", code, n)
		}
		// access restored: the problem clears and the backlog goes out
		e.f.forbidStation = ""
		if err := e.cycle(); err != nil {
			t.Fatal(err)
		}
		if st := e.s.Status(); len(st.Problems) != 0 || st.Pushed != 2 {
			t.Fatalf("%s: after recovery %+v", code, st)
		}
	}
}

func TestDeadTokenIsNotAProfileProblem(t *testing.T) {
	e := newEnv(t, true, true)
	e.s.Cfg.Profiles = []Profile{{"DL9ET", "1"}, {"DL1XX", "2"}}
	e.s.Client.Key = "wrong"
	err := e.cycle()
	if !tokenDead(err) || len(e.s.Status().Problems) != 0 {
		t.Fatalf("err %v problems %q", err, e.s.Status().Problems)
	}
	if n := e.requests(); n != 1 {
		t.Fatalf("%d requests after a dead token, want 1", n)
	}
	// a missing scope is not a dead token either
	if tokenDead(&APIError{Auth: true, Scope: "qso:write"}) {
		t.Fatal("insufficient_scope is not a dead token")
	}
}

func TestEndpointErrorAbortsCycleAndKeepsLargestRetryAfter(t *testing.T) {
	e := newEnv(t, true, true)
	e.s.Cfg.Profiles = []Profile{{"DL9ET", "1"}, {"DL1XX", "2"}}
	e.f.status, e.f.retry = 429, "600"
	e.f.body = `{"error":{"code":"rate_limited","message":"slow down"}}`
	e.logQSO("DK1AA", t0, "DL9ET")
	err := e.cycle()
	if err == nil {
		t.Fatal("want an error")
	}
	if n := e.requests(); n != 1 {
		t.Fatalf("%d requests after a 429 on the first pull, want 1", n)
	}
	e.s.finish(err)
	if got := e.s.Status().BackoffSeconds; got < 600 {
		t.Fatalf("backoff %d s < retry_after 600", got)
	}
	if !e.s.Status().LastPullAt.IsZero() {
		t.Fatal("last_pull_at must not advance")
	}
	// the largest of several RetryAfters wins, whichever comes first
	e2 := newEnv(t, false, false)
	e2.s.finish(errors.Join(&APIError{RetryAfter: 2 * time.Minute}, &APIError{RetryAfter: 10 * time.Minute}, &APIError{RetryAfter: time.Minute}))
	if got := e2.s.Status().BackoffSeconds; got < 600 {
		t.Fatalf("joined backoff %d s, want >= 600", got)
	}
	for _, tc := range []struct {
		status int
		body   string
	}{{500, "boom"}, {502, "<html>Bad Gateway</html>"}, {408, "timeout"}} {
		e := newEnv(t, true, true)
		e.s.Cfg.Profiles = []Profile{{"DL9ET", "1"}, {"DL1XX", "2"}}
		e.f.status, e.f.body = tc.status, tc.body
		if e.cycle() == nil || e.requests() != 1 {
			t.Fatalf("%d: requests %d, want exactly 1", tc.status, e.requests())
		}
	}
}

func TestTransportErrorAbortsCycle(t *testing.T) {
	e := newEnv(t, true, true)
	e.s.Cfg.Profiles = []Profile{{"DL9ET", "1"}, {"DL1XX", "2"}}
	e.f.srv.Close() // connection refused
	if e.cycle() == nil {
		t.Fatal("want an error")
	}
	if n := e.requests(); n != 0 {
		t.Fatalf("%d requests reached the closed server", n)
	}
	if a := e.attempts("1"); a > 0 {
		t.Fatalf("%d attempts counted for a transport error", a)
	}
}

func TestSystemicRejectionBurnsNoAttemptsAndIsBounded(t *testing.T) {
	e := newEnv(t, false, true)
	e.s.Cfg.Batch = 20
	e.f.rejectAll = true // every POST: 400 validation_error "Invalid band"
	for i := 0; i < 20; i++ {
		e.logQSO("DK"+itoa(i)+"AA", t0.Add(time.Duration(i)*time.Hour), "DL9ET")
	}
	err := e.cycle()
	if err == nil || !strings.Contains(err.Error(), "Invalid band") {
		t.Fatalf("err %v: want an endpoint/config error carrying the message", err)
	}
	if isAuth(err) {
		t.Fatal("not an auth error")
	}
	if budget := 3*20 + 10; len(e.f.posts) > budget || len(e.f.posts) > 12 {
		t.Fatalf("%d POSTs for an all-refusing endpoint", len(e.f.posts))
	}
	for i := 1; i <= 20; i++ {
		if a := e.attempts(itoa(i)); a > 0 {
			t.Fatalf("QSO %d burned %d attempts", i, a)
		}
	}
	e.s.finish(err)
	st := e.s.Status()
	if st.PushFailed != 0 || st.PushPending != 20 || !strings.Contains(st.Error, "Invalid band") || st.BackoffSeconds == 0 {
		t.Fatalf("status %+v", st)
	}
	// repeated cycles never condemn the backlog
	for i := 0; i < 8; i++ {
		e.cycle()
	}
	for i := 1; i <= 20; i++ {
		if a := e.attempts(itoa(i)); a > 0 {
			t.Fatalf("after retries QSO %d burned %d attempts", i, a)
		}
	}
	// a lone record on a profile that never pushed anything is systemic too
	e = newEnv(t, false, true)
	e.f.rejectAll = true
	e.logQSO("DK1AA", t0, "DL9ET")
	if e.cycle() == nil || e.attempts("1") > 0 {
		t.Fatalf("lone refusal without any success: attempts %d", e.attempts("1"))
	}
}

func TestOnePoisonRecordAmongTwentyIsBoundedAndIsolated(t *testing.T) {
	for _, pos := range []int{0, 7, 19} {
		e := newEnv(t, false, true)
		e.s.Cfg.Batch = 20
		e.f.badCalls = map[string]bool{"BAD": true}
		for i := 0; i < 20; i++ {
			c := "DK" + itoa(i) + "AA"
			if i == pos {
				c = "BAD"
			}
			e.logQSO(c, t0.Add(time.Duration(i)*time.Hour), "DL9ET")
		}
		if err := e.cycle(); err != nil {
			t.Fatalf("pos %d: %v", pos, err)
		}
		st := e.s.Status()
		if st.Pushed != 19 || st.PushPending != 1 || len(e.f.rows) != 19 {
			t.Fatalf("pos %d: status %+v rows %d", pos, st, len(e.f.rows))
		}
		failedAttempts := 0
		for i := 1; i <= 20; i++ {
			failedAttempts += max(0, e.attempts(itoa(i)))
		}
		if failedAttempts != 1 {
			t.Fatalf("pos %d: %d attempts burned, want 1 (the poison record)", pos, failedAttempts)
		}
		// about 2*log2(20) + 2 requests, never more than the budget
		if len(e.f.posts) > 14 {
			t.Fatalf("pos %d: %d POSTs", pos, len(e.f.posts))
		}
	}
}

func TestPushBudgetStopsBisecting(t *testing.T) {
	// every other record is poison: bisecting would need ~2n requests; the
	// per-batch budget 3n+10 is never exceeded and no endpoint error is lost
	e := newEnv(t, false, true)
	e.s.Cfg.Batch = 8
	e.f.badCalls = map[string]bool{}
	for i := 0; i < 8; i++ {
		c := "DK" + itoa(i) + "AA"
		if i%2 == 1 {
			c = "BAD" + itoa(i)
			e.f.badCalls[c] = true
		}
		e.logQSO(c, t0.Add(time.Duration(i)*time.Hour), "DL9ET")
	}
	if err := e.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(e.f.posts) > 3*8+10 {
		t.Fatalf("%d POSTs exceed the budget", len(e.f.posts))
	}
	if st := e.s.Status(); st.Pushed != 4 || st.PushPending != 4 {
		t.Fatalf("status %+v", st)
	}
}

func TestForbiddenAndNotFoundEnvelopes(t *testing.T) {
	// 403 forbidden over the wire: classified per profile, message kept
	e := newEnv(t, true, false)
	e.f.status = 403
	e.f.body = `{"error":{"code":"forbidden","message":"station 1 is not yours"}}`
	_, err := e.s.Client.GetContacts(context.Background(), "1", 0, 10)
	var ae *APIError
	if !errors.As(err, &ae) || !ae.Profile || ae.Auth || tokenDead(err) || ae.Code != "forbidden" ||
		!strings.Contains(ae.Msg, "station 1 is not yours") {
		t.Fatalf("%#v", err)
	}
	// 404 not_found with a body: an endpoint error with the body text, no
	// version hint, no attempt against any QSO, cycle aborted
	e = newEnv(t, true, true)
	e.s.Cfg.Profiles = []Profile{{"DL9ET", "1"}, {"DL1XX", "2"}}
	e.f.status = 404
	e.f.body = `{"error":{"code":"not_found","message":"Station location 1 does not exist"}}`
	e.logQSO("DK1AA", t0, "DL9ET")
	err = e.cycle()
	if !errors.As(err, &ae) || ae.Status != 404 || ae.Auth || ae.Profile || ae.Record ||
		!strings.Contains(ae.Msg, "Station location 1 does not exist") || strings.Contains(ae.Msg, "3.1.0") {
		t.Fatalf("%#v", err)
	}
	if e.requests() != 1 || e.attempts("1") != 0 || len(e.s.Status().Problems) != 0 {
		t.Fatalf("requests %d attempts %d", e.requests(), e.attempts("1"))
	}
}

func TestHasMoreWithoutProgressTerminatesWithError(t *testing.T) {
	for name, body := range map[string]string{
		"lastfetchedid missing": `{"data":{"adif":"<EOH>\n","exported":1},"meta":{"has_more":true}}`,
		"lastfetchedid zero":    `{"data":{"adif":null,"exported":0,"lastfetchedid":0},"meta":{"has_more":true}}`,
	} {
		e := newEnv(t, true, false)
		e.f.okBody = body
		err := e.cycle()
		if err == nil || !strings.Contains(err.Error(), "did not advance") {
			t.Fatalf("%s: err %v", name, err)
		}
		if n := e.requests(); n != 1 {
			t.Fatalf("%s: %d requests, want 1", name, n)
		}
		if !e.s.Status().LastPullAt.IsZero() {
			t.Fatalf("%s: a broken pull must not count as pulled", name)
		}
	}
	// advances once, then repeats the same id for ever
	e := newEnv(t, true, false)
	e.f.okBody = `{"data":{"adif":null,"exported":0,"lastfetchedid":5},"meta":{"has_more":true}}`
	err := e.cycle()
	if err == nil || !strings.Contains(err.Error(), "did not advance past 5") || e.requests() != 2 {
		t.Fatalf("err %v requests %d", err, e.requests())
	}
}

// The budget is a guard against a bisect that would spend more requests than the
// batch size justifies: with the budget cut to 3 requests, a batch with poison
// records stops with an endpoint error after at most 3 POSTs and burns no attempt.
func TestBatchBudgetGuardIsEnforced(t *testing.T) {
	old := batchBudget
	batchBudget = func(n int) int { return 3 }
	defer func() { batchBudget = old }()
	e := newEnv(t, false, true)
	e.s.Cfg.Batch = 8
	e.f.badCalls = map[string]bool{}
	for i := 0; i < 8; i++ {
		c := "DK" + itoa(i) + "AA"
		if i%2 == 1 {
			c = "BAD" + itoa(i)
			e.f.badCalls[c] = true
		}
		e.logQSO(c, t0.Add(time.Duration(i)*time.Hour), "DL9ET")
	}
	_ = e.cycle()
	if len(e.f.posts) > 3 {
		t.Fatalf("%d POSTs exceed the budget of 3", len(e.f.posts))
	}
	if st := e.s.Status(); st.PushFailed != 0 {
		t.Fatalf("an exhausted budget must burn no attempt: %+v", st)
	}
}

// A server that echoes the bearer token in error.code must not leak it.
func TestErrorCodeIsRedacted(t *testing.T) {
	e := newEnv(t, true, false)
	e.f.status = 400
	e.f.body = `{"error":{"code":"` + e.s.Client.Key + `","message":"x"}}`
	_, err := e.s.Client.GetContacts(context.Background(), "1", 0, 10)
	if err == nil || strings.Contains(err.Error(), e.s.Client.Key) {
		t.Fatalf("token reachable in error: %v", err)
	}
}
