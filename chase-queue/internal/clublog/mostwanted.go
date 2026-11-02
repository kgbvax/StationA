// Package clublog keeps Club Log's Most Wanted list: how many Club Log
// users still need each DXCC entity, as a rank per ADIF entity number. The
// contest queue uses it to put the rarest new entity first. The list comes
// from Club Log's public JSON, is refreshed weekly, and is cached in the
// settings table with its fetch time so an offline start still has it.
package clublog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	DefaultURL   = "https://clublog.org/mostwanted.php?api=1"
	settingKey   = "clublog_mostwanted"
	refreshEvery = 7 * 24 * time.Hour
	retryEvery   = time.Hour
	maxBody      = 1 << 20
)

// Settings is the persistence the list needs (implemented by store.Store).
type Settings interface {
	Setting(key string) (string, bool, error)
	SetSetting(key, value string) error
}

// MostWanted holds the rank of each entity; rank 1 is the most wanted.
type MostWanted struct {
	url      string
	client   *http.Client
	settings Settings
	log      *slog.Logger
	now      func() time.Time

	mu      sync.RWMutex
	ranks   map[int]int // ADIF entity number -> rank
	fetched time.Time
}

// New returns a list that has loaded the cached copy, if any.
func New(url string, settings Settings, log *slog.Logger) *MostWanted {
	if url == "" {
		url = DefaultURL
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m := &MostWanted{url: url, client: &http.Client{Timeout: 20 * time.Second}, settings: settings, log: log, now: time.Now}
	m.loadCached()
	return m
}

// Rank returns the rank of an entity by ADIF number; ok is false when the
// list has no such entity or no list is loaded.
func (m *MostWanted) Rank(adif int) (int, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.ranks[adif]
	return r, ok
}

// Loaded reports whether any list, fresh or cached, is available.
func (m *MostWanted) Loaded() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.ranks) > 0
}

// Age is the time since the list was fetched; ok is false without a list.
func (m *MostWanted) Age() (time.Duration, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.ranks) == 0 || m.fetched.IsZero() {
		return 0, false
	}
	return m.now().Sub(m.fetched), true
}

// Run refreshes the list now and then weekly (an hour after a failure)
// until ctx ends.
func (m *MostWanted) Run(ctx context.Context) {
	for {
		wait := refreshEvery
		if err := m.Refresh(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			m.log.Warn("club log most wanted refresh failed", "err", err, "cached", m.Loaded())
			wait = retryEvery
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Refresh fetches the list and caches it. A failed fetch keeps the copy
// already held.
func (m *MostWanted) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "bmrec")
	res, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("club log most wanted: HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxBody {
		return fmt.Errorf("club log most wanted: answer over %d bytes", maxBody)
	}
	ranks, err := parse(body)
	if err != nil {
		return err
	}
	now := m.now()
	m.set(ranks, now)
	if m.settings != nil {
		wrapped, _ := json.Marshal(cached{Fetched: now.UTC().Format(time.RFC3339), Body: string(body)})
		if err := m.settings.SetSetting(settingKey, string(wrapped)); err != nil {
			m.log.Warn("club log most wanted cache write failed", "err", err)
		}
	}
	m.log.Info("club log most wanted refreshed", "entities", len(ranks))
	return nil
}

// cached is the stored form: the answer with the time it was fetched, so
// its age survives a restart.
type cached struct {
	Fetched string `json:"fetched"`
	Body    string `json:"body"`
}

func (m *MostWanted) loadCached() {
	if m.settings == nil {
		return
	}
	raw, ok, err := m.settings.Setting(settingKey)
	if err != nil || !ok {
		return
	}
	var c cached
	if json.Unmarshal([]byte(raw), &c) != nil {
		return
	}
	ranks, err := parse([]byte(c.Body))
	if err != nil {
		return
	}
	t, _ := time.Parse(time.RFC3339, c.Fetched)
	m.set(ranks, t)
}

// parse reads Club Log's {"1":"344","2":"123",...} object: rank to ADIF number.
func parse(body []byte) (map[int]int, error) {
	var raw map[string]string
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("club log most wanted: %w", err)
	}
	ranks := make(map[int]int, len(raw))
	for k, v := range raw {
		rank, err1 := strconv.Atoi(k)
		adif, err2 := strconv.Atoi(v)
		if err1 != nil || err2 != nil || rank < 1 || adif < 1 {
			continue
		}
		if cur, dup := ranks[adif]; !dup || rank < cur {
			ranks[adif] = rank
		}
	}
	if len(ranks) == 0 {
		return nil, fmt.Errorf("club log most wanted: empty")
	}
	return ranks, nil
}

func (m *MostWanted) set(ranks map[int]int, fetched time.Time) {
	m.mu.Lock()
	m.ranks, m.fetched = ranks, fetched
	m.mu.Unlock()
}
