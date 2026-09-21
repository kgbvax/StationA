// SPDX-License-Identifier: AGPL-3.0-or-later

// Package preview serves the local HLS preview sink to LAN browsers: the
// preview ffmpeg writes live.m3u8 + segments to a tmpfs dir, this server
// hands them out plus a minimal player page. hls.js is vendored into the
// binary so the page works with the internet down — the whole point of a
// local preview.
package preview

import (
	"context"
	"embed"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"vhfcam-restream/internal/config"
)

//go:embed assets/hls.min.js
var hlsJS embed.FS

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>VHF cam — preview</title>
<style>
  html,body{margin:0;background:#111;color:#ddd;font:14px/1.4 system-ui,sans-serif;height:100%}
  header{padding:8px 14px;display:flex;justify-content:space-between;align-items:baseline;gap:12px}
  h1{font-size:15px;margin:0;font-weight:600;color:#fff}
  #st{color:#999;font-size:12px;margin:0}
  video{display:block;width:100%;max-height:85vh;background:#000}
  #bar{padding:8px 14px;display:flex;gap:8px;align-items:center}
  button{background:#2a2a2a;color:#ddd;border:1px solid #444;border-radius:6px;
         padding:7px 14px;font:inherit;cursor:pointer}
  button:hover{background:#3a3a3a}
  button:disabled{opacity:.5;cursor:default}
  #radio-st{font-size:12px;color:#999;margin-left:6px}
</style>
</head>
<body>
<header><h1>VHF cam — preview</h1><p id="st">connecting…</p></header>
<video id="v" autoplay muted playsinline></video>
<div id="bar">
  <button id="btn-connect">Radio: connect</button>
  <button id="btn-disconnect">Radio: disconnect</button>
  <button id="btn-power">Radio: power on</button>
  <span id="radio-st"></span>
</div>
<script src="/hls.js"></script>
<script>
const v = document.getElementById('v'), st = document.getElementById('st');
function retry() { st.textContent = 'stream offline — is the service running? retrying…'; setTimeout(start, 5000); }
function start() {
  if (v.canPlayType('application/vnd.apple.mpegurl')) { v.src = '/hls/live.m3u8'; return; }
  if (window.Hls && Hls.isSupported()) {
    const h = new Hls({ liveDurationInfinity: true });
    h.loadSource('/hls/live.m3u8');
    h.attachMedia(v);
    h.on(Hls.Events.ERROR, (e, d) => { if (d.fatal) { h.destroy(); retry(); } });
  } else { st.textContent = 'browser has no MSE/HLS support'; }
}
v.addEventListener('playing', () => { st.textContent = 'live (HLS, ~10 s behind)'; });
start();

const rst = document.getElementById('radio-st');
async function radioCmd(action, label) {
  const b = document.getElementById('btn-' + (action === 'audio_on' ? 'connect' : action === 'audio_off' ? 'disconnect' : 'power'));
  b.disabled = true;
  rst.textContent = label + '…';
  try {
    const r = await fetch('/api/cmd/' + action, { method: 'POST' });
    if (!r.ok) throw new Error(await r.text());
    rst.textContent = label + ' — sent';
  } catch (e) {
    rst.textContent = label + ' — FAILED: ' + e.message;
  } finally {
    b.disabled = false;
  }
}
document.getElementById('btn-connect').onclick = async () => {
  await radioCmd('audio_on', 'connecting (audio demand on)');
};
document.getElementById('btn-disconnect').onclick = async () => {
  await radioCmd('audio_off', 'disconnecting (radio session frees within 60 s)');
};
document.getElementById('btn-power').onclick = async () => {
  await radioCmd('power_on', 'power on frame sent');
  await radioCmd('audio_on', 'connecting (audio demand on)');
};
</script>
</body>
</html>
`

// Server serves the player page, the vendored hls.js, the HLS files the
// preview ffmpeg writes, and the radio control endpoints.
type Server struct {
	cfgFn func() config.PreviewConfig
	log   *slog.Logger
	cmdFn func(action string) error // publishes a radio /cmd; nil = controls disabled
}

// NewServer builds the preview server. cfgFn is consulted per request, so a
// SIGHUP address/dir change applies without rebuilding (listener changes need
// a restart). cmdFn publishes a radio command by action name; nil disables
// the control buttons (they render but return 503).
func NewServer(cfgFn func() config.PreviewConfig, cmdFn func(string) error, log *slog.Logger) *Server {
	return &Server{cfgFn: cfgFn, log: log, cmdFn: cmdFn}
}

// WithCmd sets the radio-command publisher after construction (main wires it
// once the MQTT connection owner exists).
func (s *Server) WithCmd(fn func(string) error) *Server {
	s.cmdFn = fn
	return s
}

// ListenAndServe runs until ctx is done. Always-on: with the preview sink
// disabled it serves the player page with a playlist that is simply absent —
// the page says "offline".
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfgFn().HTTPAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	s.log.Info("preview: http server listening", "addr", s.cfgFn().HTTPAddr)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// Handler builds the routes: / (player + radio controls), /hls.js (vendored),
// /hls/ (playlist + segments from the preview dir), /api/cmd/{action}
// (allowlisted radio commands, POST).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(pageHTML))
	})
	allowed := map[string]bool{"audio_on": true, "audio_off": true, "power_on": true}
	mux.HandleFunc("POST /api/cmd/{action}", func(w http.ResponseWriter, r *http.Request) {
		action := r.PathValue("action")
		if !allowed[action] {
			http.Error(w, "unknown action", http.StatusNotFound)
			return
		}
		if s.cmdFn == nil {
			http.Error(w, "radio controls not wired", http.StatusServiceUnavailable)
			return
		}
		if err := s.cmdFn(action); err != nil {
			http.Error(w, "publish failed: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"action":"` + action + `"}`))
	})
	mux.HandleFunc("GET /hls.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		b, err := hlsJS.ReadFile("assets/hls.min.js")
		if err != nil {
			http.Error(w, "hls.js missing", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /hls/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/hls/")
		if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		// Playlists must never be cached; segments are delete-on-rotation.
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasSuffix(name, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		} else if strings.HasSuffix(name, ".ts") {
			w.Header().Set("Content-Type", "video/mp2t")
		}
		dir := s.cfgFn().Dir
		http.ServeFile(w, r, filepath.Join(dir, filepath.Base(name)))
	})
	return mux
}
