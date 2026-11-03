package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chase-queue/internal/bus"
	"chase-queue/internal/contest"
	"chase-queue/internal/store"
)

const (
	contestKeep        = 30 * 24 * time.Hour
	contestPrune       = 24 * time.Hour
	contestUploadMax   = 8 << 20
	contestDirName     = "contest"
	contestStartLayout = "2006-01-02"
	activeSetting      = "active_instance"
)

var errUnknownKind = errors.New("kind must be definition or prefill")

// loadModel reads an instance's files into a model.
func (s *Service) loadModel(c store.Contest) (*contest.Model, int, error) {
	df, ok, err := s.st.ContestFileByID(c.DefinitionFileID)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, errors.New("upload the contest definition first")
	}
	f, err := os.Open(filepath.Join(s.cfg.DataDir, df.Path))
	if err != nil {
		return nil, 0, fmt.Errorf("the stored definition cannot be read; upload it again: %w", err)
	}
	def, err := contest.ParseDefinition(f)
	f.Close()
	if err != nil {
		return nil, 0, err
	}
	model := &contest.Model{Def: def, CTY: s.cty.Load()}
	prefillN := 0
	if c.PrefillFileID != 0 {
		if pf, ok, err := s.st.ContestFileByID(c.PrefillFileID); err == nil && ok {
			col := 0
			for _, d := range def.DBFiles {
				if strings.EqualFold(d.Name, pf.DBFile) {
					col = d.RCVDCol
				}
			}
			if r, err := os.Open(filepath.Join(s.cfg.DataDir, pf.Path)); err == nil {
				p, perr := contest.ParsePrefill(r, col)
				r.Close()
				if perr == nil {
					model.Prefill, prefillN = p, p.Len()
				}
			}
		}
	}
	return model, prefillN, nil
}

// SwitchDX turns contest mode off.
func (s *Service) SwitchDX() error {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	s.actID.Store(0)
	s.eng.ActivateDX()
	// the contest's QSOs count as worked in DX from now on
	if s.view != nil && s.view.Loaded() {
		// station-log is the history source: the local store gets no live QSOs
		s.eng.SetDXWorked(s.view.Worked())
	} else if w, err := s.st.WorkedSet(); err == nil {
		s.eng.SetDXWorked(w)
	} else {
		s.log.Warn("reloading the worked set failed", "err", err)
	}
	s.mu.Lock()
	s.defName, s.prefillN = "", 0
	s.mu.Unlock()
	if err := s.st.SetSetting(activeSetting, "0"); err != nil {
		s.log.Warn("storing the active contest failed", "err", err)
	}
	s.poke()
	return nil
}

// SwitchContest turns contest mode on an existing or a new instance.
func (s *Service) SwitchContest(req bus.SwitchReq) error {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	var c store.Contest
	if req.Instance != 0 && !req.New {
		got, ok, err := s.st.GetContest(req.Instance)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("no such contest instance")
		}
		c = got
	} else {
		df, ok, err := s.st.ContestFileByID(req.DefinitionID)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("upload the contest definition first")
		}
		start := req.StartDate
		if start == "" {
			start = s.now().UTC().Format(contestStartLayout)
		}
		if _, err := time.Parse(contestStartLayout, start); err != nil {
			return errors.New("start date must be YYYY-MM-DD")
		}
		name := df.ContestName
		if name == "" {
			name = df.Name
		}
		// check the files load before an instance is created for them
		if _, _, err := s.loadModel(store.Contest{DefinitionFileID: df.ID, PrefillFileID: req.PrefillID}); err != nil {
			return err
		}
		if c, err = s.st.CreateContest(name, start, df.ID, req.PrefillID, s.now()); err != nil {
			return err
		}
	}
	model, prefillN, err := s.loadModel(c)
	if err != nil {
		return err
	}
	qsos, err := s.st.ListContestQSOs(c.ID)
	if err != nil {
		return err
	}
	handled, err := s.st.ListContestHandled(c.ID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.defName, s.prefillN = model.Def.Name, prefillN
	s.mu.Unlock()
	name := model.Def.Name
	s.actName.Store(&name)
	s.eng.ActivateContest(c, model, handled, qsos)
	s.actID.Store(c.ID) // after the engine: the DXLog writer files QSOs under this id
	if s.dxl != nil {
		s.dxl.ResetCompleteLog()
	}
	if err := s.st.SetSetting(activeSetting, fmt.Sprint(c.ID)); err != nil {
		s.log.Warn("storing the active contest failed; a restart will start in DX mode", "err", err)
	}
	s.reloadQSOs() // QSOs written between the read above and the activation
	s.poke()
	return nil
}

// Upload stores a definition or prefill file under a server-chosen name.
func (s *Service) Upload(kind, name, text string) (store.ContestFile, error) {
	if kind != store.ContestFileDefinition && kind != store.ContestFilePrefill {
		return store.ContestFile{}, errUnknownKind
	}
	if len(text) > contestUploadMax {
		return store.ContestFile{}, errors.New("the file is larger than 8 MiB")
	}
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	if base == "." || base == "/" || base == "" {
		return store.ContestFile{}, errors.New("a file name is needed")
	}
	rec := store.ContestFile{Kind: kind, Name: filepath.Base(strings.ReplaceAll(name, "\\", "/")), Uploaded: s.now()}
	if kind == store.ContestFileDefinition {
		def, err := contest.ParseDefinition(strings.NewReader(text))
		if err != nil {
			return store.ContestFile{}, err
		}
		if def.Name == "" {
			return store.ContestFile{}, errors.New("the file has no CONTESTNAME; is it a DXLog contest definition?")
		}
		rec.ContestName = def.Name
	} else {
		rec.DBFile = base
	}
	dir := filepath.Join(s.cfg.DataDir, contestDirName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return store.ContestFile{}, err
	}
	rel := filepath.Join(contestDirName, fmt.Sprintf("%s-%d.txt", kind, s.now().UnixNano()))
	if err := os.WriteFile(filepath.Join(s.cfg.DataDir, rel), []byte(text), 0o640); err != nil {
		return store.ContestFile{}, err
	}
	rec.Path = rel
	got, replaced, err := s.st.AddContestFile(rec)
	if err != nil {
		os.Remove(filepath.Join(s.cfg.DataDir, rel))
		return store.ContestFile{}, err
	}
	s.removeFiles(replaced)
	s.poke()
	return got, nil
}

func (s *Service) removeFiles(paths []string) {
	for _, p := range paths {
		if p != "" && !filepath.IsAbs(p) && !strings.Contains(p, "..") {
			os.Remove(filepath.Join(s.cfg.DataDir, p))
		}
	}
}

// DeleteInstance removes an instance; the running one cannot be deleted.
func (s *Service) DeleteInstance(id int64) error {
	if id == s.actID.Load() {
		return errors.New("switch contest mode off before deleting its instance")
	}
	paths, err := s.st.DeleteContest(id)
	s.removeFiles(paths)
	s.poke()
	return err
}

// Overview is the setup panel's data (GET /contest).
func (s *Service) Overview() (any, error) {
	inst, err := s.st.ListContests()
	if err != nil {
		return nil, err
	}
	defs, err := s.st.ListContestFiles(store.ContestFileDefinition)
	if err != nil {
		return nil, err
	}
	pre, err := s.st.ListContestFiles(store.ContestFilePrefill)
	if err != nil {
		return nil, err
	}
	return map[string]any{"active": s.actID.Load(), "instances": inst, "definitions": defs, "prefills": pre,
		"listen_configured": s.dxl != nil, "status": s.contestStatus()}, nil
}

// resumeActive restarts the instance that was running before a restart.
func (s *Service) resumeActive() {
	v, ok, err := s.st.Setting(activeSetting)
	if err != nil || !ok || v == "" || v == "0" {
		return
	}
	var id int64
	if _, err := fmt.Sscan(v, &id); err != nil || id <= 0 {
		return
	}
	if err := s.SwitchContest(bus.SwitchReq{Instance: id}); err != nil {
		s.log.Warn("resuming the contest instance failed; staying in DX mode", "instance", id, "err", err)
	}
}

// reloadQSOs refreshes the engine's view of the contest log, at most once a
// second: a complete-log resend is many batches.
func (s *Service) reloadQSOs() {
	if s.reloadPending.Swap(true) {
		return
	}
	time.AfterFunc(time.Second, func() {
		s.reloadPending.Store(false)
		id := s.actID.Load()
		if id == 0 {
			return
		}
		qs, err := s.st.ListContestQSOs(id)
		if err != nil {
			s.log.Warn("reading the contest log failed", "err", err)
			return
		}
		s.eng.SetContestQSOs(id, qs)
		s.poke()
	})
}

// keep prunes old instances, files and callbook entries daily.
func (s *Service) keep(done <-chan struct{}) {
	prune := func() {
		now := s.now()
		// an entry past its refresh is still shown while it is looked up again; twice that is unused
		if err := s.st.PruneCallbook(now.Add(-2 * time.Duration(s.cfg.QRZ.RefreshDays) * 24 * time.Hour)); err != nil {
			s.log.Warn("pruning the callbook cache failed", "err", err)
		}
		paths, n, err := s.st.PruneContests(now.Add(-contestKeep), s.actID.Load())
		if err != nil {
			s.log.Warn("pruning contest instances failed", "err", err)
			return
		}
		s.removeFiles(paths)
		if more, err := s.st.PruneContestFiles(now.Add(-contestKeep)); err == nil {
			s.removeFiles(more)
		}
		if n > 0 {
			s.log.Info("old contest instances pruned", "instances", n)
		}
	}
	prune()
	tick := time.NewTicker(contestPrune)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			prune()
		}
	}
}
