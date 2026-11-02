package clublog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memSettings map[string]string

func (m memSettings) Setting(k string) (string, bool, error) { v, ok := m[k]; return v, ok, nil }
func (m memSettings) SetSetting(k, v string) error           { m[k] = v; return nil }

func serve(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRefreshAndRank(t *testing.T) {
	srv := serve(t, 200, `{"1":"344","2":"123","3":"x"}`)
	m := New(srv.URL, memSettings{}, nil)
	if m.Loaded() {
		t.Fatal("loaded before refresh")
	}
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r, ok := m.Rank(344); !ok || r != 1 {
		t.Errorf("rank 344 = %d %v", r, ok)
	}
	if _, ok := m.Rank(999); ok {
		t.Error("unlisted entity has a rank")
	}
}

// AE6: the offline restart keeps the list and the age of the original fetch.
func TestCachedCopyKeepsItsFetchTime(t *testing.T) {
	store := memSettings{}
	srv := serve(t, 200, `{"1":"344"}`)
	m := New(srv.URL, store, nil)
	fetched := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return fetched }
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.Close()

	m2 := New(srv.URL, store, nil)
	m2.now = func() time.Time { return fetched.Add(10 * 24 * time.Hour) }
	if err := m2.Refresh(context.Background()); err == nil {
		t.Fatal("refresh against a closed server should fail")
	}
	if r, ok := m2.Rank(344); !ok || r != 1 {
		t.Errorf("cached rank = %d %v", r, ok)
	}
	if age, ok := m2.Age(); !ok || age != 10*24*time.Hour {
		t.Errorf("age = %v %v, want 10 days", age, ok)
	}
}

func TestBadAnswersKeepThePreviousCopy(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"404":       {404, "nope"},
		"malformed": {200, "<html>"},
		"empty":     {200, `{}`},
		"too big":   {200, `{"1":"344"` + strings.Repeat(" ", maxBody+10) + `}`},
	} {
		t.Run(name, func(t *testing.T) {
			store := memSettings{}
			good := serve(t, 200, `{"1":"344"}`)
			m := New(good.URL, store, nil)
			if err := m.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			m.url = serve(t, tc.status, tc.body).URL
			if err := m.Refresh(context.Background()); err == nil {
				t.Fatal("expected an error")
			}
			if r, ok := m.Rank(344); !ok || r != 1 {
				t.Errorf("previous copy lost: %d %v", r, ok)
			}
		})
	}
}

func TestNoCacheNoList(t *testing.T) {
	m := New("http://127.0.0.1:1", memSettings{}, nil)
	if m.Loaded() {
		t.Error("loaded without cache")
	}
	if _, ok := m.Age(); ok {
		t.Error("age without a list")
	}
}
