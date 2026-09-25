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
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vhfcam-restream/internal/config"
	"vhfcam-restream/internal/recorder"
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
  #bar{padding:8px 14px;display:flex;gap:8px;align-items:center;flex-wrap:wrap}
  button{background:#2a2a2a;color:#ddd;border:1px solid #444;border-radius:6px;
         padding:7px 14px;font:inherit;cursor:pointer}
  button:hover{background:#3a3a3a}
  button:disabled{opacity:.5;cursor:default}
  #radio-st{font-size:12px;color:#999;margin-left:6px}
  .led{display:inline-flex;align-items:center;gap:5px;font-size:12px;color:#999;
       margin-right:10px}
  .dot{width:9px;height:9px;border-radius:50%;background:#555;display:inline-block}
  .dot.on{background:#3fbf5f}
  .dot.off{background:#c04545}
  .dot.warn{background:#d9a13b}
  .dot.unk{background:#777}
  .dot.rec-live{background:#e03a3a;animation:blink 1s steps(2,start) infinite}
  @keyframes blink{to{visibility:hidden}}
  @media (prefers-reduced-motion:reduce){.dot.rec-live{animation:none}}
  #recbar{padding:0 14px 8px;display:flex;gap:8px;align-items:center;flex-wrap:wrap}
  #rec-label{font-weight:600;color:#fff;font-size:12px}
  #rec-st{font-size:12px;color:#bbb;font-variant-numeric:tabular-nums}
  #rec{padding:4px 14px 20px;max-width:900px}
  #rec h2{font-size:14px;margin:8px 0;color:#fff;font-weight:600}
  #rec table{border-collapse:collapse;width:100%;font-size:13px}
  #rec td{padding:5px 8px 5px 0;border-top:1px solid #2a2a2a;font-variant-numeric:tabular-nums}
  #rec a{color:#8cc8e8}
  #rec-total,#rec-empty{font-size:12px;color:#999}
</style>
</head>
<body>
<header><h1>VHF cam — preview</h1><p id="st">connecting…</p></header>
<video id="v" autoplay muted playsinline></video>
<div id="bar">
  <span class="led"><span class="dot" id="led-bridge"></span>bridge</span>
  <span class="led"><span class="dot" id="led-session"></span>session</span>
  <span class="led"><span class="dot" id="led-radio"></span>radio</span>
  <span class="led"><span class="dot" id="led-audio"></span>audio</span>
  <span class="led"><span class="dot" id="led-sound"></span>sound</span>
  <button id="btn-connect">Radio: connect</button>
  <button id="btn-disconnect">Radio: disconnect</button>
  <button id="btn-power">Radio: power on</button>
  <button id="btn-mute">Unmute</button>
  <button id="btn-yt-start">YouTube: start</button>
  <button id="btn-yt-stop">YouTube: stop</button>
  <span id="radio-st"></span>
</div>
<div id="recbar">
  <span class="led"><span class="dot" id="rec-dot"></span><span id="rec-label">REC</span></span>
  <button id="btn-rec" disabled>Start recording</button>
  <span id="rec-st"></span>
</div>
<section id="rec">
  <h2>Recordings</h2>
  <p id="rec-empty">No recordings yet.</p>
  <table><tbody id="rec-list"></tbody></table>
  <p id="rec-total"></p>
</section>
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
document.getElementById('btn-yt-start').onclick = async () => {
  await radioCmd('yt_start', 'YouTube sink starting');
};
document.getElementById('btn-yt-stop').onclick = async () => {
  await radioCmd('yt_stop', 'YouTube sink stopping');
};
const muteBtn = document.getElementById('btn-mute');
const soundLed = document.getElementById('led-sound');
function reflectMute() {
  muteBtn.textContent = v.muted ? 'Unmute' : 'Mute';
  setLed('led-sound', v.muted ? 'off' : 'on');
}
muteBtn.onclick = () => {
  v.muted = !v.muted;
  reflectMute();
};
reflectMute(); // autoplay starts muted — show it

function setLed(id, state) {
  const d = document.getElementById(id);
  d.classList.remove('on', 'off');
  d.classList.add(state);
}
async function pollStatus() {
  try {
    const r = await fetch('/api/radio-status');
    const s = await r.json();
    setLed('led-bridge', s.leds.bridge);
    setLed('led-session', s.leds.session);
    setLed('led-radio', s.leds.radio);
    setLed('led-audio', s.leds.audio);
    rst.textContent = s.hint || '';
    if (s.audio_holders && s.audio_holders.includes('recording')) {
      rst.textContent = (rst.textContent ? rst.textContent + ' · ' : '') + 'radio audio held by the recording';
    }
    renderRec(s.rec);
  } catch (e) { /* transient — next tick retries */ }
}

const recBtn = document.getElementById('btn-rec'), recSt = document.getElementById('rec-st');
const recDot = document.getElementById('rec-dot');
const two = n => String(n).padStart(2, '0');
const fmtT = s => two(Math.floor(s / 60)) + ':' + two(s % 60);
const fmtGB = b => (b / 1e9).toFixed(1) + ' GB';
const fmtMB = b => b >= 1e9 ? fmtGB(b) : (b / 1e6).toFixed(b < 10e6 ? 1 : 0) + ' MB';
const reasons = {
  max_duration: 'stopped at the time limit', low_disk: 'stopped: disk almost full',
  shutdown: 'stopped by a service shutdown', recovered: 'recovered after an interruption',
};
let recState = 'idle', recMsg = '';
function renderRec(r) {
  if (!r) { document.getElementById('recbar').style.display = 'none'; return; }
  const was = recState;
  recState = r.state;
  recDot.className = 'dot' + (r.state === 'recording' ? ' rec-live' : '');
  if (r.state === 'recording') {
    recBtn.textContent = 'Stop recording';
    recBtn.disabled = false;
    let t = fmtT(r.elapsed_s) + ' · ' + fmtT(r.remaining_s) + ' left · free ' + fmtGB(r.free_bytes);
    if (r.stalled) t += ' · no new video from the preview';
    recSt.textContent = t;
  } else if (r.state === 'finalizing') {
    recBtn.textContent = 'Stop recording';
    recBtn.disabled = true;
    recSt.textContent = 'Saving ' + (r.name || 'recording') + '…';
  } else {
    recBtn.textContent = 'Start recording';
    recBtn.disabled = !r.enabled;
    let t;
    if (!r.enabled) t = 'Recording is off (needs the preview and [record] enabled).';
    else if (r.last_error) t = 'Last recording: ' + r.last_error;
    else if (r.last_file) t = 'Saved ' + r.last_file + (reasons[r.stop_reason] ? ' (' + reasons[r.stop_reason] + ')' : '');
    else t = 'Up to ' + Math.round(r.max_s / 60) + ' min · free ' + fmtGB(r.free_bytes) +
             ' (needs ' + fmtGB(r.min_free_bytes) + ' + room for the recording)';
    recSt.textContent = recMsg || t;
    if (was !== 'idle') loadRecordings();
  }
}
recBtn.onclick = async () => {
  const action = recState === 'recording' ? 'stop' : 'start';
  recBtn.disabled = true;
  recMsg = '';
  try {
    const r = await fetch('/api/rec/' + action, { method: 'POST' });
    const body = await r.json().catch(() => ({}));
    if (!r.ok) recMsg = (action === 'start' ? 'Could not start: ' : 'Could not stop: ') + (body.error || r.status);
    renderRec(body.status);
  } catch (e) {
    recMsg = 'Request failed: ' + e.message;
  }
  setTimeout(() => { recMsg = ''; }, 8000);
  pollStatus();
};

async function loadRecordings() {
  try {
    const r = await fetch('/api/rec');
    if (!r.ok) return;
    const l = await r.json();
    const tb = document.getElementById('rec-list');
    tb.textContent = '';
    for (const f of l.files) {
      const tr = document.createElement('tr');
      const a = document.createElement('a');
      a.href = '/api/rec/files/' + encodeURIComponent(f.name);
      a.download = f.name;
      a.textContent = f.name;
      const del = document.createElement('button');
      del.textContent = 'Delete';
      del.onclick = async () => {
        if (!confirm('Delete ' + f.name + '?')) return;
        const d = await fetch('/api/rec/files/' + encodeURIComponent(f.name), { method: 'DELETE' });
        if (!d.ok) alert('Delete failed: ' + (await d.text()));
        loadRecordings();
      };
      for (const c of [a, document.createTextNode(fmtMB(f.size)),
                       document.createTextNode(new Date(f.mtime).toLocaleString()), del]) {
        const td = document.createElement('td');
        td.appendChild(c);
        tr.appendChild(td);
      }
      tb.appendChild(tr);
    }
    document.getElementById('rec-empty').style.display = l.files.length ? 'none' : '';
    document.getElementById('rec-total').textContent = l.files.length
      ? fmtGB(l.total_bytes) + ' of ' + fmtGB(l.cap_bytes) + ' used — the oldest recordings are deleted beyond that.'
      : '';
  } catch (e) { /* next reload retries */ }
}
loadRecordings();
setInterval(loadRecordings, 30000);
pollStatus();
setInterval(pollStatus, 2000);
</script>
</body>
</html>
`

// RadioStatus is the preview page's indicator payload: one fact per LED plus
// the computed operator hint. Each LED owns exactly one fact — no composite
// booleans — so every state of the world reads true.
type RadioStatus struct {
	BridgeOnline bool   `json:"bridge_online"` // icom9700-radio-bridge reachable (its /status LWT over our MQTT link)
	SessionHeld  bool   `json:"session_held"`  // the bridge holds a live CI-V session (for us, via the audio demand)
	RadioReady   bool   `json:"radio_ready"`   // the radio answers CI-V (false while held = standby)
	AudioStream  bool   `json:"audio_stream"`  // real radio PCM arriving at this host
	Youtube      bool   `json:"youtube"`       // the YouTube sink is running
	Hint         string `json:"hint"`
	Leds         Leds   `json:"leds"`

	// AudioHolders says who keeps the radio audio on ("page", "recording").
	AudioHolders []string `json:"audio_holders"`
	// Rec is the recorder state (absent when no recorder is wired).
	Rec *recorder.Status `json:"rec,omitempty"`
}

// Leds is the per-LED render state: on / off / warn (held-but-deaf) / unk
// (cannot know).
type Leds struct {
	Bridge  string `json:"bridge"`
	Session string `json:"session"`
	Radio   string `json:"radio"`
	Audio   string `json:"audio"`
}

// ComputeStatus is the pure indicator decision (table-tested): map the radio
// facts onto the four LEDs and the operator hint.
func ComputeStatus(bridgeOnline, sessionHeld, radioReady, audioStream bool) RadioStatus {
	st := RadioStatus{
		BridgeOnline: bridgeOnline,
		SessionHeld:  sessionHeld,
		RadioReady:   radioReady,
		AudioStream:  audioStream,
	}
	st.Leds.Bridge = map[bool]string{true: "on", false: "off"}[bridgeOnline]

	switch {
	case !bridgeOnline:
		// Blind: nothing about the radio is knowable — unknown is not failure.
		st.Leds.Session, st.Leds.Radio = "unk", "unk"
		st.Hint = "radio bridge unreachable (broker or bridge process)"
	default:
		st.Leds.Session = map[bool]string{true: "on", false: "off"}[sessionHeld]
		if !sessionHeld {
			st.Leds.Radio = "unk"
			st.Hint = "no radio session (wfview may hold it, or connect failed)"
		} else {
			st.Leds.Session = "on"
			if radioReady {
				st.Leds.Radio = "on"
			} else {
				// Held but deaf: the standby signature. The green session
				// stays — it is the session the power_on wake rides.
				st.Leds.Radio = "warn"
				st.Hint = "radio not answering CI-V — in standby? try Power on"
			}
			if radioReady && !audioStream {
				st.Hint = "radio answers but no audio arriving — check the radio UDP audio path"
			}
		}
	}
	st.Leds.Audio = map[bool]string{true: "on", false: "off"}[audioStream]
	return st
}

// Server serves the player page, the vendored hls.js, the HLS files the
// preview ffmpeg writes, and the radio control endpoints.
type Server struct {
	cfgFn   func() config.PreviewConfig
	log     *slog.Logger
	cmdFn   func(action string) error // publishes a radio /cmd; nil = controls disabled
	statusFn func() RadioStatus       // nil = /api/radio-status reports zeros
	rec      Recordings               // nil = recording endpoints return 503
}

// Recordings is the recorder as the HTTP layer sees it (a fake in tests).
type Recordings interface {
	Start() (recorder.Status, error)
	Stop() (recorder.Status, error)
	Status() recorder.Status
	List() (recorder.Listing, error)
	Open(name string) (*os.File, os.FileInfo, error)
	Delete(name string) error
}

// WithRecorder wires the recording endpoints and the rec status block.
func (s *Server) WithRecorder(r Recordings) *Server {
	s.rec = r
	return s
}

// WithStatus sets the indicator source polled by GET /api/radio-status.
func (s *Server) WithStatus(fn func() RadioStatus) *Server {
	s.statusFn = fn
	return s
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
	allowed := map[string]bool{
		"audio_on": true, "audio_off": true, "power_on": true,
		"yt_start": true, "yt_stop": true,
	}
	mux.HandleFunc("GET /api/radio-status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		var st RadioStatus
		if s.statusFn != nil {
			st = s.statusFn()
		}
		if s.rec != nil {
			rs := s.rec.Status()
			st.Rec = &rs
		}
		_ = json.NewEncoder(w).Encode(st)
	})
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
	s.recRoutes(mux)
	return mux
}

// recRoutes: POST /api/rec/{start,stop}, GET /api/rec (status + files),
// GET /api/rec/files/{name} (download, Range-capable), DELETE /api/rec/files/{name}.
// Start/stop answer JSON {status, error}; the codes carry the reason
// (409 busy / not recording, 507 low disk, 503 no preview / disabled).
func (s *Server) recRoutes(mux *http.ServeMux) {
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	noRecorder := func(w http.ResponseWriter) bool {
		if s.rec == nil {
			http.Error(w, "recording not available", http.StatusServiceUnavailable)
			return true
		}
		return false
	}
	type result struct {
		Status recorder.Status `json:"status"`
		Error  string          `json:"error,omitempty"`
	}
	control := func(fn func() (recorder.Status, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			if noRecorder(w) {
				return
			}
			st, err := fn()
			code := http.StatusOK
			switch {
			case err == nil:
			case errors.Is(err, recorder.ErrBusy), errors.Is(err, recorder.ErrNotRecording):
				code = http.StatusConflict
			case errors.Is(err, recorder.ErrLowDisk):
				code = http.StatusInsufficientStorage
			case errors.Is(err, recorder.ErrNoPreview), errors.Is(err, recorder.ErrDisabled):
				code = http.StatusServiceUnavailable
			default:
				code = http.StatusInternalServerError
			}
			res := result{Status: st}
			if err != nil {
				res.Error = err.Error()
			}
			writeJSON(w, code, res)
		}
	}
	mux.HandleFunc("POST /api/rec/start", control(func() (recorder.Status, error) { return s.rec.Start() }))
	mux.HandleFunc("POST /api/rec/stop", control(func() (recorder.Status, error) { return s.rec.Stop() }))
	mux.HandleFunc("GET /api/rec", func(w http.ResponseWriter, _ *http.Request) {
		if noRecorder(w) {
			return
		}
		l, err := s.rec.List()
		if err != nil {
			s.log.Warn("listing recordings failed", "err", err)
		}
		writeJSON(w, http.StatusOK, l)
	})
	mux.HandleFunc("GET /api/rec/files/{name}", func(w http.ResponseWriter, r *http.Request) {
		if noRecorder(w) {
			return
		}
		name := r.PathValue("name")
		f, info, err := s.rec.Open(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		ctype := "video/mp4"
		if strings.HasSuffix(name, ".ts") {
			ctype = "video/mp2t"
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, name, info.ModTime(), f)
	})
	mux.HandleFunc("DELETE /api/rec/files/{name}", func(w http.ResponseWriter, r *http.Request) {
		if noRecorder(w) {
			return
		}
		switch err := s.rec.Delete(r.PathValue("name")); {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, recorder.ErrActive):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, fs.ErrNotExist):
			http.NotFound(w, r)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}
