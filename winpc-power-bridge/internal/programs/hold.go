package programs

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// FreshWindow is how long a 75auto /beat or live /state keeps a session
// counted as active. 75auto beats every 2 s during a session, so a crashed or
// frozen 75auto stops holding the power within this window.
const FreshWindow = 30 * time.Second

// ReadyHold is how long a powerseq chain in state ready keeps the power
// request: powerseq holds a ready chain for the same time while the console
// sends the session start.
const ReadyHold = 30 * time.Minute

// Activity tracks what needs the PC awake: a powerseq start chain that runs or
// is ready and young (muehle/hf/power-seq /state chain), and a live 75auto
// session that is connecting or running with a fresh heartbeat
// (muehle/hf/auto-op /state and /beat). The Observe methods are cheap and safe
// to call from a paho handler.
type Activity struct {
	mu            sync.Mutex
	chainState    string
	chainTS       time.Time
	powerSeqDown  bool
	sessionState  string
	sessionSource string
	lastFresh     time.Time
}

// PowerSeqState takes one powerseq /state payload, received at now. An empty
// payload (a cleared retained message) means no chain; malformed JSON is
// ignored. The chain time is the record's ts; if ts does not parse, it is now.
func (a *Activity) PowerSeqState(payload []byte, now time.Time) {
	var st struct {
		Chain struct {
			State string `json:"state"`
			TS    string `json:"ts"`
		} `json:"chain"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &st); err != nil {
			return
		}
	}
	ts, err := time.Parse(time.RFC3339, st.Chain.TS)
	if err != nil {
		ts = now
	}
	a.mu.Lock()
	a.chainState = st.Chain.State
	a.chainTS = ts
	a.mu.Unlock()
}

// PowerSeqStatus takes one powerseq /status payload. While powerseq is
// offline its retained /state can be stale (a chain that stays running after
// powerseq died), so the chain does not count.
func (a *Activity) PowerSeqStatus(payload []byte) {
	a.mu.Lock()
	a.powerSeqDown = string(payload) == "offline"
	a.mu.Unlock()
}

func (a *Activity) chainWantedLocked(now time.Time) bool {
	if a.powerSeqDown {
		return false
	}
	switch a.chainState {
	case "running":
		return true
	case "ready":
		return now.Sub(a.chainTS) < ReadyHold
	}
	return false
}

// AutoOpState takes one 75auto /state payload. A retained delivery (a broker
// replay at subscribe) can be old: its session state is kept, but it does not
// count as a heartbeat.
func (a *Activity) AutoOpState(payload []byte, retained bool, now time.Time) {
	var st struct {
		Session struct {
			State  string `json:"state"`
			Source string `json:"source"`
		} `json:"session"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &st); err != nil {
			return
		}
	}
	a.mu.Lock()
	a.sessionState = st.Session.State
	a.sessionSource = st.Session.Source
	if !retained && len(payload) > 0 {
		a.lastFresh = now
	}
	a.mu.Unlock()
}

// AutoOpBeat records a 75auto /beat.
func (a *Activity) AutoOpBeat(now time.Time) {
	a.mu.Lock()
	a.lastFresh = now
	a.mu.Unlock()
}

// SessionActive reports a live 75auto session (session.source "live"; a
// replay drives no radio) in connecting or running whose last beat or live
// /state is younger than FreshWindow.
func (a *Activity) SessionActive(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionActiveLocked(now)
}

func (a *Activity) sessionActiveLocked(now time.Time) bool {
	if a.sessionSource != "live" {
		return false
	}
	if a.sessionState != "connecting" && a.sessionState != "running" {
		return false
	}
	return !a.lastFresh.IsZero() && now.Sub(a.lastFresh) < FreshWindow
}

// wanted reports whether the PC must stay awake.
func (a *Activity) wanted(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.chainWantedLocked(now) || a.sessionActiveLocked(now)
}

// PowerRequest is a system power request that keeps the PC out of sleep.
type PowerRequest interface {
	Set() error
	Clear() error
}

// PowerHold sets the power request while Activity wants the PC awake and
// clears it otherwise. Programs that the agent started do not hold it: with no
// chain and no session the PC sleeps as before.
type PowerHold struct {
	act    *Activity
	req    PowerRequest
	log    *slog.Logger
	held   bool
	failed bool
}

// NewPowerHold builds a PowerHold.
func NewPowerHold(act *Activity, req PowerRequest, log *slog.Logger) *PowerHold {
	return &PowerHold{act: act, req: req, log: log}
}

// Update sets or clears the request on a change. A failed call is tried again
// on the next Update and logged once until it works.
func (h *PowerHold) Update(now time.Time) {
	want := h.act.wanted(now)
	if want == h.held {
		return
	}
	var err error
	if want {
		err = h.req.Set()
	} else {
		err = h.req.Clear()
	}
	if err != nil {
		if !h.failed {
			h.log.Warn("power request change failed", "hold", want, "err", err)
		}
		h.failed = true
		return
	}
	h.failed = false
	h.held = want
	if want {
		h.log.Info("power request set: start chain or 75auto session active")
	} else {
		h.log.Info("power request cleared: no start chain and no 75auto session")
	}
}
