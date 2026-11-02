package cty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFetcherRefresh(t *testing.T) {
	files := map[string][]byte{}
	for _, n := range []string{"cty.dat", "cty.csv"} {
		b, err := os.ReadFile("../app/testdata/" + n)
		if err != nil {
			t.Fatal(err)
		}
		files["/"+n] = b
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer srv.Close()
	dir := t.TempDir()
	f := Fetcher{Source: srv.URL + "/"}
	if changed, err := f.Refresh(context.Background(), dir); err != nil || !changed {
		t.Fatalf("first refresh: %v %v", changed, err)
	}
	if db, err := OpenContest(dir); err != nil || db == nil {
		t.Fatalf("%v %v", db, err)
	}
	if changed, err := f.Refresh(context.Background(), dir); err != nil || changed {
		t.Fatalf("unchanged refresh: %v %v", changed, err)
	}
	// a broken download never replaces good files
	good, _ := os.ReadFile(filepath.Join(dir, "cty.dat"))
	files["/cty.dat"] = []byte("<html>maintenance</html>")
	if changed, err := f.Refresh(context.Background(), dir); err == nil || changed {
		t.Fatalf("broken refresh: %v %v", changed, err)
	}
	if now, _ := os.ReadFile(filepath.Join(dir, "cty.dat")); string(now) != string(good) {
		t.Fatal("good cty.dat replaced")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*.new")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	delete(files, "/cty.csv")
	if _, err := f.Refresh(context.Background(), dir); err == nil {
		t.Fatal("missing cty.csv accepted")
	}
}
