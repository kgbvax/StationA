package wavelog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"codeberg.org/kgbvax/stationa/shared/logging"

	"station-log/internal/importer"
	"station-log/internal/store"
)

const (
	minBackoff  = 30 * time.Second
	maxBackoff  = 15 * time.Minute
	maxAttempts = 5
	// attemptsReset is how often QSOs that used up their attempts get new ones.
	attemptsReset = 24 * time.Hour
)

// Profile maps an own call to a Wavelog station profile id.
type Profile struct{ OwnCall, StationID string }

// Config is the syncer's settings (already validated).
type Config struct {
	Pull, Push bool
	URLHost    string // for /state only
	// URL is the full instance URL; its hash scopes the pull cursors and the
	// push ledger (empty = URLHost).
	URL         string
	Profiles    []Profile
	Interval    time.Duration
	FullResync  time.Duration
	Batch       int
	PullLimit   int
	PushSources []string
	// PushSince: only QSOs at or after it are pushed; zero = the time the
	// sync was first enabled (kept in settings), so history is not bulk-pushed.
	PushSince time.Time
}

// Status is /state.wavelog.
type Status struct {
	Direction        string           `json:"direction"`
	URLHost          string           `json:"url_host"`
	LastPullAt       time.Time        `json:"last_pull_at,omitzero"`
	LastPushAt       time.Time        `json:"last_push_at,omitzero"`
	Cursor           map[string]int64 `json:"cursor"`
	PulledAdded      int              `json:"pulled_added"`
	PulledMerged     int              `json:"pulled_merged"`
	Pushed           int              `json:"pushed"`
	PushPending      int              `json:"push_pending"`
	PushFailed       int              `json:"push_failed"`
	SkippedNoProfile int              `json:"skipped_no_profile"`
	LastFullResync   time.Time        `json:"last_full_resync_at,omitzero"`
	BackoffSeconds   int              `json:"backoff_seconds"`
	Error            string           `json:"error,omitempty"`
	// Problems lists the profiles that are skipped because Wavelog refuses
	// them for this token (403 forbidden, insufficient_club_permission,
	// club_access_revoked); the other profiles keep syncing.
	Problems []string `json:"problems,omitempty"`
	// Note is a one-off remark, e.g. that a changed instance URL reset the
	// cursors and the push ledger.
	Note string `json:"note,omitempty"`
}

// Syncer is the sync loop. It only calls Importer.Import (pull) and reads the
// store (push), so a Wavelog outage cannot block an import.
type Syncer struct {
	Cfg      Config
	Client   *Client
	Store    *store.Store
	Importer *importer.Importer
	Log      *slog.Logger
	Now      func() time.Time
	OnChange func()
	// Ready delays the first cycle until closed (MQTT link up: pulled QSOs
	// publish events).
	Ready <-chan struct{}
	// Debounce is the wait after a Kick before the push-only cycle.
	Debounce time.Duration

	kick      chan struct{}
	mu        sync.Mutex
	status    Status
	streak    logging.Streak
	failing   bool
	backoff   time.Duration
	pushSince time.Time
	checked   bool // instance/profile hashes compared this process
	profProb  map[string]string
}

// Kick asks for a push-only cycle soon (new QSOs from a logger).
func (s *Syncer) Kick() {
	s.init()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Syncer) init() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kick == nil {
		s.kick = make(chan struct{}, 1)
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.status.Cursor == nil {
		dir := "push"
		switch {
		case s.Cfg.Pull && s.Cfg.Push:
			dir = "both"
		case s.Cfg.Pull:
			dir = "pull"
		}
		s.status.Direction, s.status.URLHost, s.status.Cursor = dir, s.Cfg.URLHost, map[string]int64{}
	}
}

// Status returns a copy.
func (s *Syncer) Status() Status {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	st.Problems = append([]string(nil), s.status.Problems...)
	st.Cursor = map[string]int64{}
	for k, v := range s.status.Cursor {
		st.Cursor[k] = v
	}
	return st
}

func (s *Syncer) update(f func(*Status)) {
	s.mu.Lock()
	before := fmt.Sprint(s.status)
	f(&s.status)
	changed := before != fmt.Sprint(s.status)
	s.mu.Unlock()
	if changed && s.OnChange != nil {
		s.OnChange()
	}
}

// Run waits for Ready, then cycles until ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	s.init()
	if s.Ready != nil {
		select {
		case <-s.Ready:
		case <-ctx.Done():
			return
		}
	}
	if s.Debounce == 0 {
		s.Debounce = 10 * time.Second
	}
	next := s.Now()
	for {
		wait := next.Sub(s.Now())
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			next = s.finish(s.Cycle(ctx, true))
		case <-s.kick:
			timer.Stop()
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.Debounce):
			}
			if !s.Cfg.Push {
				continue
			}
			// while failing, a logger kick must not bypass the backoff
			if s.failing && s.Now().Before(next) {
				continue
			}
			if n := s.finish(s.Cycle(ctx, false)); s.failing {
				next = n
			}
		}
	}
}

// finish turns a cycle's result into the status, the log and the next time.
func (s *Syncer) finish(err error) time.Time {
	now := s.Now()
	if err == nil {
		if s.failing && s.Log != nil {
			s.Log.Info("Wavelog sync recovered")
		}
		s.streak.Reset()
		s.failing, s.backoff = false, 0
		s.update(func(st *Status) { st.Error, st.BackoffSeconds = "", 0 })
		return now.Add(s.Cfg.Interval)
	}
	if errors.Is(err, context.Canceled) {
		return now.Add(s.Cfg.Interval)
	}
	s.failing = true
	switch {
	case s.backoff == 0:
		s.backoff = minBackoff
	default:
		s.backoff = min(s.backoff*2, maxBackoff)
	}
	var retryAfter time.Duration
	for _, ae := range apiErrors(err) {
		if ae.Auth {
			s.backoff = maxBackoff
		}
		retryAfter = max(retryAfter, ae.RetryAfter)
	}
	if retryAfter > s.backoff {
		s.backoff = retryAfter
	}
	d := s.backoff + time.Duration(rand.Int63n(int64(s.backoff)/5+1))
	if s.Log != nil {
		s.streak.Fail(s.Log, "Wavelog sync failed", "err", err.Error(), "retry_in", d.Round(time.Second))
	}
	msg := err.Error()
	if len(msg) > 160 {
		msg = msg[:160]
	}
	s.update(func(st *Status) { st.Error, st.BackoffSeconds = msg, int(d.Seconds()) })
	return now.Add(d)
}

// Cycle runs one pass: per profile a pull (if asked and configured) and a
// push. How an error is handled depends on what it is about:
//   - a profile refusal (403 forbidden, insufficient_club_permission,
//     club_access_revoked): that profile is skipped with a /state.wavelog
//     problem naming it, the other profiles carry on, the cycle is not failed;
//   - a missing scope: only that direction stops, the other one carries on;
//   - anything else (dead token, 429, 5xx, transport, timeout, HTML, a
//     response that makes no sense): the endpoint is the problem, so the rest
//     of the cycle is abandoned at once instead of repeating the failure for
//     every profile.
//
// The errors of the parts that ran are joined (errors.As still finds an
// *APIError in the join). A panic is recovered and returned as an error.
func (s *Syncer) Cycle(ctx context.Context, pull bool) (err error) {
	s.init()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("wavelog sync panic: %v", r)
		}
	}()
	if err := s.maintain(); err != nil {
		return err
	}
	var errs []error
	var since time.Time
	if s.Cfg.Push {
		t, e := s.since()
		if e != nil {
			return e
		}
		since = t
	}
	pullFailed, abort := false, false
	pullDenied, pushDenied := false, false // the token lacks one scope only
	for _, p := range s.Cfg.Profiles {
		if abort {
			break
		}
		skip := false // this profile is refused: no further requests for it
		handle := func(e error, denied *bool) {
			var ae *APIError
			switch {
			case errors.As(e, &ae) && ae.Profile:
				skip = true
				s.setProfileProblem(p, fmt.Sprintf("Wavelog profile %s (station %s) skipped: %s", p.OwnCall, p.StationID, ae.Msg))
			case isAuth(e) && !tokenDead(e):
				errs = append(errs, e)
				*denied = true
			default:
				errs = append(errs, e)
				abort = true
			}
		}
		if pull && s.Cfg.Pull {
			if pullDenied {
				pullFailed = true
			} else if e := s.pull(ctx, p); e != nil {
				pullFailed = true
				handle(e, &pullDenied)
			}
		}
		if s.Cfg.Push && !abort && !skip && !pushDenied {
			if e := s.pushProfile(ctx, p, s.baseQuery(since)); e != nil {
				handle(e, &pushDenied)
			}
		}
		if !skip {
			s.setProfileProblem(p, "")
		}
	}
	if abort && pull && s.Cfg.Pull {
		pullFailed = true // later profiles were not pulled
	}
	if pull && s.Cfg.Pull && !pullFailed {
		s.update(func(st *Status) { st.LastPullAt = s.Now().UTC().Truncate(time.Second) })
	}
	if s.Cfg.Push {
		if e := s.pushStatus(since); e != nil {
			errs = append(errs, e)
		}
	}
	if len(errs) == 1 {
		return errs[0]
	}
	return errors.Join(errs...)
}

// setProfileProblem records (or, with "", clears) the refusal of one profile
// and republishes the sorted list in the status.
func (s *Syncer) setProfileProblem(p Profile, msg string) {
	s.mu.Lock()
	if s.profProb == nil {
		s.profProb = map[string]string{}
	}
	if msg == "" {
		delete(s.profProb, p.StationID)
	} else {
		s.profProb[p.StationID] = msg
	}
	var list []string
	for _, m := range s.profProb {
		list = append(list, m)
	}
	s.mu.Unlock()
	sort.Strings(list)
	s.update(func(st *Status) { st.Problems = list })
}

// apiErrors returns every *APIError in err, looking into joined errors.
func apiErrors(err error) []*APIError {
	var out []*APIError
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if ae, ok := e.(*APIError); ok {
			out = append(out, ae)
			return
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, x := range u.Unwrap() {
				walk(x)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	return out
}

func isAuth(err error) bool {
	for _, ae := range apiErrors(err) {
		if ae.Auth {
			return true
		}
	}
	return false
}

// tokenDead: the token is invalid, expired or revoked (401), not just short
// of one scope and not a per-profile refusal.
func tokenDead(err error) bool {
	for _, ae := range apiErrors(err) {
		if ae.Auth && ae.Scope == "" {
			return true
		}
	}
	return false
}

// hashOf is a short stable digest of the given strings.
func hashOf(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}

// maintain resets what no longer fits the configuration: when the instance
// URL changed, the pull cursors, full-resync marks and the push ledger (they
// belong to the old instance); when the profile map changed, or every 24 h,
// the attempt counters of QSOs that exhausted maxAttempts (a wrong URL or
// profile must not condemn the backlog for good).
func (s *Syncer) maintain() error {
	now := s.Now().UTC().Truncate(time.Second)
	if !s.checked {
		u := s.Cfg.URL
		if u == "" {
			u = s.Cfg.URLHost
		}
		urlH := hashOf(strings.ToLower(strings.TrimRight(strings.TrimSpace(u), "/")))
		var profs []string
		for _, p := range s.Cfg.Profiles {
			profs = append(profs, strings.ToUpper(p.OwnCall)+"="+p.StationID)
		}
		sort.Strings(profs)
		profH := hashOf(profs...)
		old, ok, err := s.Store.Setting("wavelog_url_hash")
		if err != nil {
			return err
		}
		if ok && old != urlH {
			if err := s.Store.ResetWavelogState("wavelog_cursor_", "wavelog_full_"); err != nil {
				return err
			}
			if s.Log != nil {
				s.Log.Warn("Wavelog instance URL changed: pull cursors and push ledger reset", "host", s.Cfg.URLHost)
			}
			s.update(func(st *Status) {
				st.Note = "instance URL changed: cursors and push ledger reset"
				st.Cursor = map[string]int64{}
			})
		}
		if !ok || old != urlH {
			if err := s.Store.SetSetting("wavelog_url_hash", urlH); err != nil {
				return err
			}
		}
		oldP, okP, err := s.Store.Setting("wavelog_profiles_hash")
		if err != nil {
			return err
		}
		if okP && oldP != profH {
			n, err := s.Store.ResetPushAttempts("", maxAttempts)
			if err != nil {
				return err
			}
			if s.Log != nil {
				s.Log.Warn("Wavelog profile map changed: failed pushes get new attempts", "qsos", n)
			}
		}
		if !okP || oldP != profH {
			if err := s.Store.SetSetting("wavelog_profiles_hash", profH); err != nil {
				return err
			}
		}
		s.checked = true
	}
	if v, ok, err := s.Store.Setting("wavelog_attempts_reset_at"); err != nil {
		return err
	} else if t, perr := time.Parse(time.RFC3339, v); !ok || perr != nil {
		return s.Store.SetSetting("wavelog_attempts_reset_at", now.Format(time.RFC3339))
	} else if now.Sub(t) >= attemptsReset {
		if _, err := s.Store.ResetPushAttempts("", maxAttempts); err != nil {
			return err
		}
		return s.Store.SetSetting("wavelog_attempts_reset_at", now.Format(time.RFC3339))
	}
	return nil
}

func cursorKey(p Profile) string { return "wavelog_cursor_" + p.StationID }
func fullKey(p Profile) string   { return "wavelog_full_" + p.StationID }
func (s *Syncer) getInt(key string) int64 {
	v, ok, err := s.Store.Setting(key)
	if err != nil || !ok {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

func (s *Syncer) pull(ctx context.Context, p Profile) error {
	cursor := s.getInt(cursorKey(p))
	var lastFull time.Time
	if v, ok, _ := s.Store.Setting(fullKey(p)); ok {
		lastFull, _ = time.Parse(time.RFC3339, v)
	}
	full := lastFull.IsZero() || s.Now().Sub(lastFull) >= s.Cfg.FullResync
	from := cursor
	if full {
		from = 0
	}
	last := from
	for {
		page, err := s.Client.GetContacts(ctx, p.StationID, last, s.Cfg.PullLimit)
		if err != nil {
			return err
		}
		if strings.TrimSpace(page.ADIF) != "" {
			rep, err := s.Importer.Import(ctx, page.ADIF, importer.Options{Source: Source, DefaultOwn: p.OwnCall})
			if err != nil {
				return fmt.Errorf("import of a Wavelog page failed: %w", err)
			}
			s.update(func(st *Status) { st.PulledAdded += rep.Added; st.PulledMerged += rep.Merged })
		}
		progressed := page.LastFetchedID > last
		if progressed {
			last = page.LastFetchedID
		}
		// the cursor moves only after the page was imported
		if !full && progressed {
			if err := s.Store.SetSetting(cursorKey(p), strconv.FormatInt(last, 10)); err != nil {
				return err
			}
		}
		cur := last
		s.update(func(st *Status) { st.Cursor[p.StationID] = max(cur, cursorOf(st, p)) })
		if !page.HasMore {
			break
		}
		if !progressed {
			// has_more=true but no new highest id: asking again would return
			// the same page for ever
			return &APIError{Status: 200, Msg: fmt.Sprintf("unexpected response: has_more=true but lastfetchedid did not advance past %d", last)}
		}
	}
	if full {
		if last > cursor {
			if err := s.Store.SetSetting(cursorKey(p), strconv.FormatInt(last, 10)); err != nil {
				return err
			}
		}
		now := s.Now().UTC().Truncate(time.Second)
		if err := s.Store.SetSetting(fullKey(p), now.Format(time.RFC3339)); err != nil {
			return err
		}
		s.update(func(st *Status) { st.LastFullResync = now })
	}
	return nil
}

func cursorOf(st *Status, p Profile) int64 { return st.Cursor[p.StationID] }

// since is the push lower bound: configured, else the first-enable time.
func (s *Syncer) since() (time.Time, error) {
	if !s.Cfg.PushSince.IsZero() {
		return s.Cfg.PushSince, nil
	}
	if !s.pushSince.IsZero() {
		return s.pushSince, nil
	}
	v, ok, err := s.Store.Setting("wavelog_push_since")
	if err != nil {
		return time.Time{}, err
	}
	if ok {
		if t, perr := time.Parse(time.RFC3339, v); perr == nil && !t.IsZero() {
			s.pushSince = t
			return t, nil
		}
		// never fall back to the zero time: that would bulk-push the history
		if s.Log != nil {
			s.Log.Warn("Wavelog push_since setting unreadable: re-initialised to now", "value", v)
		}
	}
	t := s.Now().UTC().Truncate(time.Second)
	if err := s.Store.SetSetting("wavelog_push_since", t.Format(time.RFC3339)); err != nil {
		return time.Time{}, err
	}
	s.pushSince = t
	return t, nil
}

func (s *Syncer) baseQuery(since time.Time) store.PushQuery {
	return store.PushQuery{Since: since, Prefixes: s.Cfg.PushSources, ExcludeSource: Source,
		MaxAttempts: maxAttempts}
}

// pushProfile pushes one profile's candidates until none are left or a batch
// fails with an error that is not about a single QSO.
func (s *Syncer) pushProfile(ctx context.Context, p Profile, base store.PushQuery) error {
	q := base
	q.OwnCalls, q.ProfileID = []string{strings.ToUpper(p.OwnCall)}, p.StationID
	seen := map[string]bool{} // a failed QSO is tried once per cycle
	st := &pushState{}
	for {
		q.Limit = s.Cfg.Batch + len(seen)
		all, err := s.Store.PushCandidates(q)
		if err != nil {
			return err
		}
		var cands []store.PushCandidate
		for _, c := range all {
			if !seen[c.QSO.ID] && len(cands) < s.Cfg.Batch {
				cands = append(cands, c)
				seen[c.QSO.ID] = true
			}
		}
		if len(cands) == 0 {
			return nil
		}
		n, err := s.pushGroup(ctx, p, cands, st)
		if n > 0 {
			s.update(func(st *Status) { st.LastPushAt = s.Now().UTC().Truncate(time.Second) })
		}
		if err != nil {
			return err
		}
	}
}

// pushStatus refreshes the push bookkeeping of /state.
func (s *Syncer) pushStatus(since time.Time) error {
	base := s.baseQuery(since)
	var mapped []string
	for _, p := range s.Cfg.Profiles {
		mapped = append(mapped, strings.ToUpper(p.OwnCall))
	}
	pending, failed := 0, 0
	for _, p := range s.Cfg.Profiles {
		q := base
		q.OwnCalls, q.ProfileID = []string{strings.ToUpper(p.OwnCall)}, p.StationID
		if c, err := s.Store.PushCandidates(q); err == nil {
			pending += len(c)
		}
		q.MaxAttempts = 0
		if c, err := s.Store.PushCandidates(q); err == nil {
			for _, x := range c {
				if !x.Pushed && x.Attempts >= maxAttempts {
					failed++
				}
			}
		}
	}
	skipped := 0
	q := base
	q.OwnCalls, q.NotOwn = mapped, true
	if c, err := s.Store.PushCandidates(q); err == nil {
		skipped = len(c)
	}
	s.update(func(st *Status) { st.PushPending, st.PushFailed, st.SkippedNoProfile = pending, failed, skipped })
	return nil
}

// pushState is what one pushProfile run knows across its batches.
type pushState struct {
	ok int // POSTs that stored everything they carried
}

// failedLeaf is a record that failed alone, not yet charged an attempt.
type failedLeaf struct {
	c      store.PushCandidate
	reason string
}

// pushGroup posts a batch as one ADIF import (QSO resource, "Import multiple
// QSOs (ADIF)") and returns how many QSOs reached a final state, pushed or
// counted as failed. Wavelog skips duplicates itself, so imported plus
// skipped covering every record is a success; a skipped record is a QSO
// Wavelog already has. Anything else about the records (a 400
// validation_error, or fewer records stored than sent) splits the group in
// two and tries each half, down to single records. Re-sending records that
// were stored is harmless (they skip as duplicates).
//
// A record that fails alone is charged a failed attempt only when a sibling
// was stored (in this batch or earlier in the run), which shows the endpoint,
// token and profile work and this record is the problem. When nothing at all
// is stored the refusal is systemic (a wrong profile, a rule rejecting every
// record): the batch is abandoned with an endpoint error carrying the
// message and no QSO is charged, so the backlog is not condemned. The same
// goes for three identical single-record refusals before any success. A
// single-record batch has no sibling; it counts as the record's own failure
// only when the profile has pushed before. The requests of one batch are
// capped at 3*len+10. Endpoint and auth errors abort without counting an
// attempt.
func (s *Syncer) pushGroup(ctx context.Context, p Profile, cands []store.PushCandidate, st *pushState) (int, error) {
	budget := batchBudget(len(cands))
	var leaves []failedLeaf
	n, err := s.bisect(ctx, p, cands, st, &budget, &leaves)
	if len(leaves) == 0 {
		return n, err
	}
	proven := st.ok > 0
	if !proven && len(cands) == 1 {
		cnt, cerr := s.Store.PushedCount(p.StationID)
		proven = cerr == nil && cnt > 0
	}
	if !proven {
		if err != nil {
			return n, err
		}
		return n, systemicErr(leaves[0].reason, len(leaves))
	}
	for _, l := range leaves {
		reason := l.reason
		if len(reason) > 200 {
			reason = reason[:200]
		}
		if merr := s.Store.MarkFailed(l.c.QSO.ID, p.StationID, l.c.QSO.Fingerprint(), s.Client.redact(reason)); merr != nil {
			return n, merr
		}
		n++
	}
	return n, err
}

func systemicErr(reason string, n int) *APIError {
	return &APIError{Status: 400, Msg: fmt.Sprintf("Wavelog refuses every record sent (%d tried, none stored; not counted against the QSOs): %s", n, reason)}
}

// batchBudget bounds the requests one push batch may cost, bisecting included
// (a variable so a test can pin the guard).
var batchBudget = func(n int) int { return 3*n + 10 }

func (s *Syncer) bisect(ctx context.Context, p Profile, cands []store.PushCandidate, st *pushState, budget *int, leaves *[]failedLeaf) (int, error) {
	if *budget <= 0 {
		return 0, &APIError{Msg: "Wavelog push stopped: request budget for this batch used up"}
	}
	*budget--
	var text strings.Builder
	for _, c := range cands {
		text.WriteString(BuildADIF(c))
	}
	res, err := s.Client.PostADIF(ctx, p.StationID, text.String())
	var reason string
	if err != nil {
		var ae *APIError
		if !errors.As(err, &ae) || !ae.Record {
			return 0, err
		}
		reason = ae.Msg
	} else if res.Imported+res.Skipped >= len(cands) {
		note := ""
		if res.Imported == 0 {
			note = "duplicate"
		}
		for _, c := range cands {
			if err := s.Store.MarkPushed(c.QSO.ID, p.StationID, c.QSO.Fingerprint(), note); err != nil {
				return 0, err
			}
		}
		st.ok++
		s.update(func(x *Status) { x.Pushed += len(cands) })
		return len(cands), nil
	} else {
		reason = strings.TrimSpace(fmt.Sprintf("Wavelog stored %d of %d records: %s", res.Imported+res.Skipped, len(cands), res.Text()))
	}
	if len(cands) == 1 {
		*leaves = append(*leaves, failedLeaf{cands[0], reason})
		if st.ok == 0 && len(*leaves) >= 3 && allSame(*leaves) {
			return 0, systemicErr(reason, len(*leaves))
		}
		return 0, nil
	}
	mid := len(cands) / 2
	a, err := s.bisect(ctx, p, cands[:mid], st, budget, leaves)
	if err != nil {
		return a, err
	}
	b, err := s.bisect(ctx, p, cands[mid:], st, budget, leaves)
	return a + b, err
}

func allSame(l []failedLeaf) bool {
	for _, x := range l {
		if x.reason != l[0].reason {
			return false
		}
	}
	return true
}
