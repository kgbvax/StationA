package web

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"sync"

	"ubctrl/internal/ub/service"
)

type Server struct {
	ctrl *service.Controller
	tmpl *template.Template
	mu   sync.RWMutex
	seq  int
	chs  map[chan []byte]struct{}
}

func New(ctrl *service.Controller) *Server {
	return &Server{
		ctrl: ctrl,
		tmpl: template.Must(template.New("index").Parse(indexHTML)),
		chs:  make(map[chan []byte]struct{}),
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/refresh", s.handleRefresh)
	mux.HandleFunc("/api/retract", s.handleRetract)
	mux.HandleFunc("/api/frequency", s.handleFrequency)
	mux.HandleFunc("/api/mode", s.handleMode)
	return mux
}

func (s *Server) PublishStatus(state service.State) {
	payload, err := json.Marshal(state)
	if err != nil {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for ch := range s.chs {
		select {
		case ch <- payload:
		default:
		}
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	_ = s.tmpl.Execute(w, s.ctrl.State())
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.ctrl.State())
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan []byte, 8)
	s.mu.Lock()
	s.chs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.chs, ch)
		s.mu.Unlock()
		close(ch)
	}()

	writeSSE := func(event string, data []byte) error {
		if _, err := w.Write([]byte("event: " + event + "\n")); err != nil {
			return err
		}
		if _, err := w.Write([]byte("data: ")); err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	if payload, err := json.Marshal(s.ctrl.State()); err == nil {
		_ = writeSSE("status", payload)
	}
	keepAlive := r.Context().Done()
	for {
		select {
		case <-keepAlive:
			return
		case payload := <-ch:
			if err := writeSSE("status", payload); err != nil {
				return
			}
		}
	}
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.ctrl.Refresh(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.PublishStatus(s.ctrl.State())
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleRetract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.ctrl.Retract(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.PublishStatus(s.ctrl.State())
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleFrequency(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	freqStr := r.FormValue("frequency")
	mode := r.FormValue("mode")
	freq, err := strconv.Atoi(freqStr)
	if err != nil || freq <= 0 || freq > 65535 {
		http.Error(w, "invalid frequency", http.StatusBadRequest)
		return
	}
	if mode == "" {
		mode = s.ctrl.State().ModeName
	}
	if err := s.ctrl.SetFrequency(r.Context(), uint16(freq), mode); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.PublishStatus(s.ctrl.State())
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mode := r.FormValue("mode")
	if mode == "" {
		http.Error(w, "mode is required", http.StatusBadRequest)
		return
	}
	if err := s.ctrl.SetMode(r.Context(), mode); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.PublishStatus(s.ctrl.State())
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

const indexHTML = `<!doctype html>
<html>
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width, initial-scale=1">
	<title>ubctrl</title>
	<style>
		:root{color-scheme:dark}
		body{font-family:system-ui,-apple-system,sans-serif;max-width:980px;margin:2rem auto;padding:0 1rem;background:linear-gradient(180deg,#08111f,#0b1220 45%,#10192c);color:#e5eefc}
		.card{background:rgba(18,26,43,.92);border:1px solid #24324d;border-radius:18px;padding:1rem 1.2rem;margin-bottom:1rem;box-shadow:0 12px 30px rgba(0,0,0,.25);backdrop-filter:blur(8px)}
		h1{margin:.2rem 0 1rem;display:flex;gap:.75rem;align-items:center;flex-wrap:wrap}
		label{display:block;margin:.5rem 0 .2rem;color:#9fb3d1}
		input,select,button{font:inherit;border-radius:12px;border:1px solid #2d3d5b;background:#0f1726;color:#e5eefc;padding:.75rem .95rem}
		input,select{width:100%;box-sizing:border-box}
		button{cursor:pointer;background:linear-gradient(180deg,#2d6bff,#2456cc);border-color:#2456cc;font-weight:650}
		button.secondary{background:#334155;border-color:#475569}
		button.mode{background:#172033;border-color:#2d3d5b}
		button.mode.active{background:linear-gradient(180deg,#2d6bff,#2456cc);border-color:#2456cc}
		.row{display:grid;grid-template-columns:repeat(auto-fit,minmax(190px,1fr));gap:1rem}
		.row-inline{display:flex;flex-wrap:wrap;gap:.75rem;align-items:center}
		.kv{padding:.25rem 0}
		.value{font-size:1.35rem;font-weight:750;letter-spacing:.2px}
		.muted{color:#9fb3d1}
		.pill{display:inline-flex;align-items:center;gap:.45rem;padding:.32rem .72rem;border-radius:999px;background:#0f1726;border:1px solid #2d3d5b;color:#9fb3d1;font-size:.88rem}
		.dot{width:.62rem;height:.62rem;border-radius:999px;background:#6b7280;display:inline-block}
		.dot.ok{background:#22c55e}.dot.warn{background:#f59e0b}.dot.bad{background:#ef4444}
		.toolbar{display:flex;flex-wrap:wrap;gap:.75rem;align-items:center;justify-content:space-between}
		.status-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:1rem}
		.actions{display:grid;gap:1rem}
		.foot{display:flex;justify-content:space-between;gap:1rem;flex-wrap:wrap}
		.small{font-size:.9rem}
		.hidden{display:none}
		input:disabled,button:disabled{opacity:.5;cursor:not-allowed}
	</style>
</head>
<body>
	<div class="toolbar">
		<h1>ubctrl <span id="live-pill" class="pill"><span class="dot {{if .Offline}}bad{{else}}ok{{end}}"></span><span id="live-state">{{if .Offline}}offline{{else}}live{{end}}</span></span></h1>
		<div class="pill">Live updates <span id="refresh-count">SSE</span></div>
	</div>

	<div class="card status-grid">
		<div class="kv"><div class="muted">Frequency</div><div class="value" id="frequency">{{.FrequencyKHz}} kHz</div></div>
		<div class="kv"><div class="muted">Band</div><div class="value" id="band">{{.BandName}}</div></div>
		<div class="kv {{if not .MotorsMoving}}hidden{{end}}" id="moving-card"><div class="muted">Motors moving</div><div class="value" id="moving">{{.MotorsMoving}}</div></div>
		<div class="kv"><div class="muted">Mode</div><div class="value" id="mode">{{.ModeName}}</div></div>
		<div class="kv {{if not .Offline}}hidden{{end}}" id="offline-card"><div class="muted">Offline</div><div class="value" id="offline">{{.Offline}}</div></div>
	</div>

	<div class="card actions">
		<div class="row">
			<div><label>Frequency (kHz)</label><input id="frequency-input" type="number" min="1" max="65535" value="{{.FrequencyKHz}}"></div>
			<div style="align-self:end"><button id="freq-btn" type="button" onclick="setFrequency()">Set frequency</button></div>
		</div>

		<div>
			<label>Mode</label>
			<div class="row-inline"><button id="mode-forward" class="mode {{if eq .ModeName "forward"}}active{{end}}" type="button" data-mode="forward" onclick="setMode('forward')">forward</button><button id="mode-reverse" class="mode {{if eq .ModeName "reverse"}}active{{end}}" type="button" data-mode="reverse" onclick="setMode('reverse')">reverse</button><button id="mode-bidirectional" class="mode {{if eq .ModeName "bidirectional"}}active{{end}}" type="button" data-mode="bidirectional" onclick="setMode('bidirectional')">bidirectional</button></div>
		</div>

		<div class="row">
			<button class="secondary" type="button" onclick="retract()">Retract</button>
			<a class="pill small" href="/api/status" style="text-decoration:none;align-self:center">JSON status</a>
		</div>
	</div>

	<div class="foot muted small">
		<div>Updated <span id="updated">{{.UpdatedAt.Format "2006-01-02 15:04:05"}}</span></div>
	</div>

	<script>
		const refreshFields = (data) => {
			const set = (id, value) => { const el = document.getElementById(id); if (el) el.textContent = value; };
			set('frequency', data.frequency_khz + ' kHz');
			set('band', data.band_name || 'unknown');
			set('moving', data.motors_moving ? 'true' : 'false');
			set('mode', data.mode_name || 'unknown');
			set('offline', data.offline ? 'true' : 'false');
			set('updated', data.updated_at ? new Date(data.updated_at).toLocaleString() : '');

			const movingCard = document.getElementById('moving-card');
			if (movingCard) movingCard.classList.toggle('hidden', !data.motors_moving);
			const offlineCard = document.getElementById('offline-card');
			if (offlineCard) offlineCard.classList.toggle('hidden', !data.offline);

			const freqInput = document.getElementById('frequency-input');
			if (freqInput) freqInput.disabled = data.motors_moving;
			const freqBtn = document.getElementById('freq-btn');
			if (freqBtn) freqBtn.disabled = data.motors_moving;
			const modeButtons = document.querySelectorAll('button.mode');
			modeButtons.forEach(btn => {
				btn.disabled = data.motors_moving;
				btn.classList.toggle('active', btn.getAttribute('data-mode') === data.mode_name);
			});

			const live = document.getElementById('live-state');
			const pillDot = document.querySelector('#live-pill .dot');
			if (live && pillDot) {
				live.textContent = data.offline ? 'offline' : 'live';
				pillDot.className = 'dot ' + (data.offline ? 'bad' : (data.motors_moving ? 'warn' : 'ok'));
			}
		};

		const source = new EventSource('/api/events');
		source.addEventListener('status', (event) => {
			try {
				refreshFields(JSON.parse(event.data));
			} catch (e) {
				console.error('bad SSE payload', e);
			}
		});
		source.onerror = () => {
			const live = document.getElementById('live-state');
			const pillDot = document.querySelector('#live-pill .dot');
			if (live && pillDot) {
				live.textContent = 'reconnecting';
				pillDot.className = 'dot warn';
			}
		};

		const postFrequency = async (frequency, mode) => {
			const body = new URLSearchParams();
			body.set('frequency', String(frequency));
			body.set('mode', mode);
			const res = await fetch('/api/frequency', {
				method: 'POST',
				headers: {'Content-Type': 'application/x-www-form-urlencoded; charset=UTF-8'},
				body: body.toString(),
			});
			if (!res.ok) {
				throw new Error(await res.text());
			}
		};

		const postMode = async (mode) => {
			const body = new URLSearchParams();
			body.set('mode', mode);
			const res = await fetch('/api/mode', {
				method: 'POST',
				headers: {'Content-Type': 'application/x-www-form-urlencoded; charset=UTF-8'},
				body: body.toString(),
			});
			if (!res.ok) {
				throw new Error(await res.text());
			}
		};

		const currentFrequency = () => {
			const input = document.getElementById('frequency-input');
			return input ? input.value : '';
		};

		const setFrequency = async () => {
			await postFrequency(currentFrequency(), document.querySelector('#live-state')?.textContent === 'offline' ? 'forward' : getCurrentMode());
		};

		const getCurrentMode = () => {
			const active = document.querySelector('button.mode.active');
			return active ? active.getAttribute('data-mode') || 'forward' : 'forward';
		};

		const setMode = async (mode) => {
			await postMode(mode);
		};

		const retract = async () => {
			const res = await fetch('/api/retract', {method: 'POST'});
			if (!res.ok) throw new Error(await res.text());
		};

		window.setFrequency = setFrequency;
		window.setMode = setMode;
		window.retract = retract;
	</script>
</body>
</html>`
