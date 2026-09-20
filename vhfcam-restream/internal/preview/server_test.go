// SPDX-License-Identifier: AGPL-3.0-or-later

package preview

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vhfcam-restream/internal/config"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	return NewServer(func() config.PreviewConfig {
		c := config.Default().Preview
		c.Dir = dir
		return c
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPageAndAsset(t *testing.T) {
	s := testServer(t)
	h := s.Handler()

	rec := get(t, h, "/")
	if rec.Code != 200 {
		t.Fatalf("page status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/hls/live.m3u8") || !strings.Contains(body, "/hls.js") {
		t.Errorf("page missing player wiring")
	}

	rec = get(t, h, "/hls.js")
	if rec.Code != 200 {
		t.Fatalf("hls.js status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("hls.js content-type %q", ct)
	}
	if rec.Body.Len() < 100000 {
		t.Errorf("hls.js suspiciously small: %d bytes", rec.Body.Len())
	}
}

func TestPlaylistAndSegments(t *testing.T) {
	s := testServer(t)
	h := s.Handler()
	dir := s.cfgFn().Dir

	playlist := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n"
	if err := os.WriteFile(filepath.Join(dir, "live.m3u8"), []byte(playlist), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seg_00001.ts"), []byte("fakets"), 0644); err != nil {
		t.Fatal(err)
	}

	rec := get(t, h, "/hls/live.m3u8")
	if rec.Code != 200 {
		t.Fatalf("playlist status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("playlist content-type %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("playlist cache-control %q", cc)
	}

	rec = get(t, h, "/hls/seg_00001.ts")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "video/mp2t" {
		t.Errorf("segment: status=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}

	if rec := get(t, h, "/hls/absent.m3u8"); rec.Code != 404 {
		t.Errorf("missing playlist status %d, want 404", rec.Code)
	}
}

func TestPathTraversalBlocked(t *testing.T) {
	s := testServer(t)
	h := s.Handler()
	secret := filepath.Join(s.cfgFn().Dir, "..", "secret.txt")
	if err := os.WriteFile(secret, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/hls/../secret.txt", "/hls/..%2Fsecret.txt", "/hls/sub/../../secret.txt"} {
		if rec := get(t, h, p); rec.Code == 200 {
			t.Errorf("path %s escaped the preview dir", p)
		}
	}
}
