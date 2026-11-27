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

// Activity tracks what needs the PC awake: a powerseq start chain that runs
// (muehle/hf/power-seq /state chain.state) and a 75auto session that is
// connecting or running with a fresh heartbeat (muehle/hf/auto-op /state and
// /beat). The Observe methods are cheap and safe to call from a paho handler.
type Activity struct {
	mu           sync.Mutex
	chainRunning bool
	sessionState string
	lastFresh    time.Time
}

// PowerSeqState takes one powerseq /state payload. An empty payload (a
// cleared retained message) means no chain; malformed JSON is ignored.
func (a *Activity) PowerSeqState(payload []byte) {
	var st struct {
		Chain struct {
			State string `json:"state"`
		} `json:"chain"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &st); err != nil {
			return
		}
	}
	a.mu.Lock()
	a.chainRunning = st.Chain.State == "running"
	a.mu.Unlock()
}

// AutoOpState takes one 75auto /state payload. A retained delivery (a broker
// replay at subscribe) can be old: its session state is kept, but it does not
// count as a heartbeat.
func (a *Activity) AutoOpState(payload []byte, retained bool, now time.Time) {
	var st struct {
		Session struct {
			State string `json:"state"`
		} `json:"session"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &st); err != nil {
			return
		}
	}
	a.mu.Lock()
	a.sessionState = st.Session.State
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

// SessionActive reports a 75auto session in connecting or running whose last
// beat or live /state is younger than FreshWindow.
func (a *Activity) SessionActive(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionActiveLocked(now)
}

func (a *Activity) sessionActiveLocked(now time.Time) bool {
	if a.sessionState != "connecting" && a.sessionState != "running" {
		return false
	}
	return !a.lastFresh.IsZero() && now.Sub(a.lastFresh) < FreshWindow
}

// wanted reports whether the PC must stay awake.
func (a *Activity) wanted(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.chainRunning || a.sessionActiveLocked(now)
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
