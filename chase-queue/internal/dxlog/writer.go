package dxlog

import (
	"context"
	"log/slog"
	"time"

	"chase-queue/internal/store"
)

// QSOStore is what the writer needs of the store.
type QSOStore interface {
	UpsertContestQSOs(contestID int64, qsos []store.ContestQSO, now time.Time) error
	DeleteContestQSO(contestID int64, stationID, qsoID string) (int64, error)
}

// WriterConfig configures Write.
type WriterConfig struct {
	Store QSOStore
	// Contest returns the instance QSOs belong to; 0 means none (the events
	// are dropped).
	Contest func() int64
	// Changed is called after each batch reached the store.
	Changed func()
	// Every is the longest an event waits for its batch (default 250 ms); a
	// batch also ends at 200 events.
	Every time.Duration
	Now   func() time.Time
}

// Write stores the events in batches, one transaction per batch, until the
// channel closes, then stores what is left and returns. The store has one
// connection shared with the recorder, so a full-log resend of thousands of
// datagrams must not become thousands of writes.
func Write(ctx context.Context, events <-chan Event, cfg WriterConfig, log *slog.Logger) {
	if cfg.Every <= 0 {
		cfg.Every = 250 * time.Millisecond
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	var batch []Event
	flush := func() {
		if len(batch) == 0 {
			return
		}
		defer func() { batch = batch[:0] }()
		id := cfg.Contest()
		if id == 0 {
			return
		}
		// consecutive upserts go in one transaction; a delete ends the run so
		// the order of edits and deletes is kept
		var run []store.ContestQSO
		flushRun := func() {
			if len(run) == 0 {
				return
			}
			if err := cfg.Store.UpsertContestQSOs(id, run, cfg.Now()); err != nil {
				log.Error("dxlog: storing QSOs failed", "qsos", len(run), "err", err)
			}
			run = run[:0]
		}
		for _, e := range batch {
			if e.Instance != 0 && e.Instance != id {
				continue // arrived under another instance
			}
			if e.Kind == KindDelete {
				flushRun()
				if _, err := cfg.Store.DeleteContestQSO(id, e.QSO.StationID, e.QSO.QSOID); err != nil {
					log.Error("dxlog: deleting a QSO failed", "err", err)
				}
				continue
			}
			run = append(run, toStore(e.QSO))
		}
		flushRun()
		if cfg.Changed != nil {
			cfg.Changed()
		}
	}
	tick := time.NewTicker(cfg.Every)
	defer tick.Stop()
	for {
		select {
		case e, ok := <-events:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= 200 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func toStore(q QSO) store.ContestQSO {
	return store.ContestQSO{StationID: q.StationID, QSOID: q.QSOID, Call: q.Call, Band: q.Band, Mode: q.Mode,
		At: q.At, Exch: q.Exch, Mults: q.Mults, Dupe: q.Dupe, Invalid: q.Invalid, XQSO: q.XQSO}
}
