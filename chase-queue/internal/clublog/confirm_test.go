package clublog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"chase-queue/internal/store"
)

func TestParseMatrixKeepsConfirmedCells(t *testing.T) {
	b, err := os.ReadFile("testdata/matrix-synthetic.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseMatrix(b, []int{1, 3})
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b store.Confirmation) int { return a.DXCC - b.DXCC })
	want := []store.Confirmation{{DXCC: 29, Band: "20m"}, {DXCC: 230, Band: "20m"}, {DXCC: 291, Band: "15m"}}
	if !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestParseMatrixWrappedAndBad(t *testing.T) {
	if got, err := ParseMatrix([]byte(`{"matrix":{"230":{"20m":1}}}`), []int{1}); err != nil || len(got) != 1 || got[0].Band != "20m" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ParseMatrix([]byte(`[]`), []int{1}); err == nil {
		t.Fatal("array accepted")
	}
	if _, err := ParseMatrix([]byte(`{"x":1}`), []int{1}); err == nil {
		t.Fatal("empty matrix accepted")
	}
	if _, err := ParseMatrix([]byte(`{"230":{"20":2}}`), []int{1}); err == nil {
		t.Fatal("a matrix with no confirmed cell must not replace the stored set")
	}
}

func TestFetchAuthRateLimitAndTokenHygiene(t *testing.T) {
	const token = "cl_secret_token_value"
	var sawAuth string
	mode := "ok"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		switch mode {
		case "401":
			w.WriteHeader(401)
		case "429":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
		default:
			w.Write([]byte(`{"230":{"20":1}}`))
		}
	}))
	defer srv.Close()
	c := &Confirmations{API: srv.URL, Token: token, Call: "dl1abc", Confirmed: []int{1}}
	got, err := c.Fetch(context.Background())
	if err != nil || len(got) != 1 || sawAuth != "Bearer "+token {
		t.Fatalf("%v %v %q", got, err, sawAuth)
	}
	mode = "401"
	if _, err := c.Fetch(context.Background()); !errors.Is(err, ErrAuth) || strings.Contains(err.Error(), token) {
		t.Fatalf("%v", err)
	}
	mode = "429"
	var rl *RateLimited
	if _, err := c.Fetch(context.Background()); !errors.As(err, &rl) || rl.Wait != 7*time.Second {
		t.Fatalf("%v", err)
	}
	if _, err := (&Confirmations{}).Fetch(context.Background()); !errors.Is(err, ErrNoToken) {
		t.Fatal(err)
	}
	// transport errors never carry the token
	srv.Close()
	if _, err := c.Fetch(context.Background()); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("%v", err)
	}
}
