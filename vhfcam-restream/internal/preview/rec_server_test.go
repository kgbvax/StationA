// SPDX-License-Identifier: AGPL-3.0-or-later

package preview

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vhfcam-restream/internal/recorder"
)

// fakeRec is a scripted Recordings for the HTTP layer.
type fakeRec struct {
	startErr, stopErr, deleteErr error
	dir                          string
	deleted                      string
}

func (f *fakeRec) Start() (recorder.Status, error) {
	return recorder.Status{State: recorder.Recording}, f.startErr
}
func (f *fakeRec) Stop() (recorder.Status, error) {
	return recorder.Status{State: recorder.Finalizing}, f.stopErr
}
func (f *fakeRec) Status() recorder.Status { return recorder.Status{Enabled: true, State: recorder.Idle} }
func (f *fakeRec) List() (recorder.Listing, error) {
	return recorder.Listing{Files: []recorder.File{{Name: "vhfcam_2026-09-25T1812Z.mp4", Size: 3}}}, nil
}
func (f *fakeRec) Open(name string) (*os.File, os.FileInfo, error) {
	if !recorder.ValidName(name) {
		return nil, nil, fs.ErrNotExist
	}
	file, err := os.Open(filepath.Join(f.dir, name))
	if err != nil {
		return nil, nil, err
	}
	info, _ := file.Stat()
	return file, info, nil
}
func (f *fakeRec) Delete(name string) error { f.deleted = name; return f.deleteErr }

func do(t *testing.T, h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestRecEndpointsWithoutRecorder(t *testing.T) {
	h := testServer(t).Handler()
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/rec/start"}, {"POST", "/api/rec/stop"}, {"GET", "/api/rec"},
		{"GET", "/api/rec/files/vhfcam_2026-09-25T1812Z.mp4"},
	} {
		if rr := do(t, h, c.method, c.path, nil); rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", c.method, c.path, rr.Code)
		}
	}
	var st RadioStatus
	_ = json.Unmarshal(get(t, h, "/api/radio-status").Body.Bytes(), &st)
	if st.Rec != nil {
		t.Error("rec block present without a recorder")
	}
}

func TestRecStartStopCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"ok", nil, 200},
		{"busy", recorder.ErrBusy, 409},
		{"low disk", recorder.ErrLowDisk, 507},
		{"no preview", recorder.ErrNoPreview, 503},
		{"disabled", recorder.ErrDisabled, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testServer(t).WithRecorder(&fakeRec{startErr: tc.err}).Handler()
			rr := do(t, h, "POST", "/api/rec/start", nil)
			if rr.Code != tc.want {
				t.Fatalf("code = %d, want %d", rr.Code, tc.want)
			}
			var body struct {
				Status recorder.Status `json:"status"`
				Error  string          `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if (tc.err != nil) != (body.Error != "") || body.Status.State != recorder.Recording {
				t.Errorf("body = %+v", body)
			}
		})
	}
	h := testServer(t).WithRecorder(&fakeRec{stopErr: recorder.ErrNotRecording}).Handler()
	if rr := do(t, h, "POST", "/api/rec/stop", nil); rr.Code != 409 {
		t.Errorf("stop not recording = %d", rr.Code)
	}
}

func TestRecStatusInRadioStatus(t *testing.T) {
	h := testServer(t).WithRecorder(&fakeRec{}).Handler()
	var st RadioStatus
	if err := json.Unmarshal(get(t, h, "/api/radio-status").Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Rec == nil || !st.Rec.Enabled || st.Rec.State != recorder.Idle {
		t.Errorf("rec = %+v", st.Rec)
	}
}

func TestRecDownload(t *testing.T) {
	dir := t.TempDir()
	name := "vhfcam_2026-09-25T1812Z.mp4"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("0123456789"), 0644); err != nil {
		t.Fatal(err)
	}
	h := testServer(t).WithRecorder(&fakeRec{dir: dir}).Handler()

	rr := do(t, h, "GET", "/api/rec/files/"+name, nil)
	if rr.Code != 200 || rr.Body.String() != "0123456789" {
		t.Fatalf("download = %d %q", rr.Code, rr.Body.String())
	}
	if cd := rr.Header().Get("Content-Disposition"); cd != `attachment; filename="`+name+`"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type = %q", ct)
	}
	rr = do(t, h, "GET", "/api/rec/files/"+name, map[string]string{"Range": "bytes=2-4"})
	if rr.Code != http.StatusPartialContent || rr.Body.String() != "234" {
		t.Errorf("range = %d %q", rr.Code, rr.Body.String())
	}
	for _, bad := range []string{"..%2F..%2Fetc%2Fpasswd", "notes.txt", ".inprogress"} {
		if rr := do(t, h, "GET", "/api/rec/files/"+bad, nil); rr.Code != 404 {
			t.Errorf("GET %s = %d, want 404", bad, rr.Code)
		}
	}
}

func TestRecDelete(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{nil, 204}, {recorder.ErrActive, 409}, {fs.ErrNotExist, 404},
	} {
		f := &fakeRec{deleteErr: tc.err}
		h := testServer(t).WithRecorder(f).Handler()
		rr := do(t, h, "DELETE", "/api/rec/files/vhfcam_2026-09-25T1812Z.mp4", nil)
		if rr.Code != tc.want || f.deleted != "vhfcam_2026-09-25T1812Z.mp4" {
			t.Errorf("delete err=%v: code %d deleted %q", tc.err, rr.Code, f.deleted)
		}
	}
}

func TestRecListAndPageWiring(t *testing.T) {
	h := testServer(t).WithRecorder(&fakeRec{}).Handler()
	var l recorder.Listing
	if err := json.Unmarshal(get(t, h, "/api/rec").Body.Bytes(), &l); err != nil || len(l.Files) != 1 {
		t.Fatalf("list = %+v err=%v", l, err)
	}
	page := get(t, h, "/").Body.String()
	for _, want := range []string{`id="btn-rec"`, "/api/rec/", "rec-live", "prefers-reduced-motion"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
}
