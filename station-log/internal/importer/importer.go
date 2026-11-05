// Package importer turns ADIF text into stored QSOs: it parses, normalises
// each record, merges it into the log (store.Tx.Upsert) in one transaction
// and reports what happened. Every entry point (the CLI flag, POST /import,
// the Log4OM feeder) goes through Import, so the merge rules live once.
package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"station-log/internal/adif"
	"station-log/internal/band"
	"station-log/internal/store"
)

// maxErrors caps the error messages in a Report; skipped_invalid still
// counts every record.
const maxErrors = 20

// ErrNoRecords refuses a replace_source import of a file without one valid
// record: it would delete the whole source.
var ErrNoRecords = errors.New("replace_source refused: the file holds no valid record")

// ErrIncomplete refuses a replace_source import whose text did not parse
// completely or held invalid records: deleting on a partial view of the file
// would drop QSOs the file really has.
var ErrIncomplete = errors.New("replace_source refused: the file is truncated or holds invalid records (fix it, or import without replace_source)")

// ErrMassDelete refuses a replace_source import that would delete more than
// half of the source, unless Options.Force is set.
var ErrMassDelete = errors.New("replace_source refused: it would delete more than half of the source (set force to proceed)")

// Report is the result of one import.
type Report struct {
	Records        int      `json:"records"`         // records parsed from the text
	Added          int      `json:"added"`           // new QSOs
	Merged         int      `json:"merged"`          // matches that changed the stored QSO
	Unchanged      int      `json:"unchanged"`       // matches that changed nothing
	SkippedInvalid int      `json:"skipped_invalid"` // records without call, band, mode, time or own call
	Deleted        int      `json:"deleted"`         // replace_source only
	Errors         []string `json:"errors"`
}

// Options select the source tag and the per-source replace mode.
type Options struct {
	// Source tags new QSOs ("" = "adif").
	Source string
	// ReplaceSource deletes the QSOs the source first stored that this
	// file does not mention (matched or added); a QSO other sources also
	// contributed to stays.
	ReplaceSource bool
	// DefaultOwn overrides the importer's default own call for this import
	// (a Wavelog station profile whose ADIF lacks STATION_CALLSIGN).
	DefaultOwn string
	// Force lets ReplaceSource delete more than half of the source.
	Force bool
}

// LastImport is the summary kept for /state.
type LastImport struct {
	At     time.Time `json:"at"`
	Source string    `json:"source"`
	Added  int       `json:"added"`
	Merged int       `json:"merged"`
}

// Sink receives the events of a committed import.
type Sink func([]store.Event)

// Importer merges ADIF into a store. Imports are serialised.
type Importer struct {
	Store      *store.Store
	Window     time.Duration // duplicate window, +-
	DefaultOwn string        // own call when a record carries neither STATION_CALLSIGN nor OPERATOR
	Sink       Sink          // may be nil
	Log        *slog.Logger
	Now        func() time.Time

	mu sync.Mutex
}

const lastImportKey = "last_import"

// Last returns the last import that held records; ok is false if none.
func (im *Importer) Last() (LastImport, bool) {
	v, ok, err := im.Store.Setting(lastImportKey)
	if err != nil || !ok {
		return LastImport{}, false
	}
	var l LastImport
	if json.Unmarshal([]byte(v), &l) != nil {
		return LastImport{}, false
	}
	return l, true
}

// Import parses text and merges its records. On an error nothing is stored
// and no event is sent.
func (im *Importer) Import(ctx context.Context, text string, opt Options) (Report, error) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if opt.Source == "" {
		opt.Source = "adif"
	}
	if im.Now == nil {
		im.Now = time.Now
	}
	defOwn := im.DefaultOwn
	if opt.DefaultOwn != "" {
		defOwn = opt.DefaultOwn
	}
	recs, truncated := adif.ParseChecked(text)
	rep := Report{Records: len(recs), Errors: []string{}}
	tx, err := im.Store.Begin()
	if err != nil {
		return rep, err
	}
	var events []store.Event
	touched := map[string]bool{}
	for i, r := range recs {
		if i%500 == 0 {
			if err := ctx.Err(); err != nil {
				tx.Rollback()
				return rep, err
			}
		}
		q, err := FromRecord(r, defOwn, opt.Source)
		if err != nil {
			rep.SkippedInvalid++
			if len(rep.Errors) < maxErrors {
				rep.Errors = append(rep.Errors, fmt.Sprintf("record %d: %v", i+1, err))
			}
			continue
		}
		res, err := tx.Upsert(q, im.Window)
		if err != nil {
			tx.Rollback()
			return rep, fmt.Errorf("record %d: %w", i+1, err)
		}
		touched[res.QSO.ID] = true
		if !store.WeakSources[opt.Source] {
			// kept for a later push to Wavelog; the first record of a QSO wins
			if err := tx.SaveRaw(res.QSO.ID, opt.Source, adif.Format(r)); err != nil {
				tx.Rollback()
				return rep, err
			}
		}
		switch res.Action {
		case store.ActionAdded:
			rep.Added++
		case store.ActionMerged:
			rep.Merged++
		default:
			rep.Unchanged++
		}
		if res.Action != "" {
			events = append(events, store.Event{ID: res.QSO.ID, Action: res.Action, QSO: res.QSO})
		}
	}
	if opt.ReplaceSource {
		if rep.Added+rep.Merged+rep.Unchanged == 0 {
			tx.Rollback()
			return rep, ErrNoRecords
		}
		if truncated || rep.SkippedInvalid > 0 {
			tx.Rollback()
			return rep, ErrIncomplete
		}
		ids, err := tx.SourceIDs(opt.Source)
		if err != nil {
			tx.Rollback()
			return rep, err
		}
		var stale []string
		for _, id := range ids {
			if !touched[id] {
				stale = append(stale, id)
			}
		}
		if !opt.Force && len(stale)*2 > len(ids) {
			tx.Rollback()
			return rep, ErrMassDelete
		}
		for _, id := range stale {
			// a QSO other sources also contributed to stays
			q, deleted, err := tx.Release(id, opt.Source)
			if err != nil {
				tx.Rollback()
				return rep, err
			}
			if deleted {
				rep.Deleted++
				events = append(events, store.Event{ID: id, Action: store.ActionDeleted, QSO: q})
			}
		}
	}
	if rep.Records > 0 {
		b, _ := json.Marshal(LastImport{At: im.Now().UTC().Truncate(time.Second), Source: opt.Source, Added: rep.Added, Merged: rep.Merged})
		if err := tx.SetSetting(lastImportKey, string(b)); err != nil {
			tx.Rollback()
			return rep, err
		}
	}
	if err := tx.Commit(); err != nil {
		return rep, err
	}
	if im.Log != nil && rep.Records > 0 {
		im.Log.Info("import", "source", opt.Source, "records", rep.Records, "added", rep.Added, "merged", rep.Merged,
			"unchanged", rep.Unchanged, "skipped_invalid", rep.SkippedInvalid, "deleted", rep.Deleted)
	}
	if im.Sink != nil && len(events) > 0 {
		im.Sink(events)
	}
	return rep, nil
}

// NormalizeCall upper-cases and trims a callsign. Portable forms stay as
// logged: DL1ABC/P and DL1ABC are two calls here.
func NormalizeCall(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// ModeClass is the award family of an ADIF mode: cw, phone or data ("" for an
// empty mode). Everything that is neither CW nor voice is data, as DXCC's
// "digital".
func ModeClass(mode, submode string) string {
	m, sm := strings.ToUpper(strings.TrimSpace(mode)), strings.ToUpper(strings.TrimSpace(submode))
	switch m {
	case "":
		return ""
	case "CW":
		return store.ClassCW
	case "SSB", "USB", "LSB", "AM", "FM", "DIGITALVOICE", "C4FM", "DSTAR", "FREEDV", "DMR", "M17":
		return store.ClassPhone
	}
	switch sm {
	case "USB", "LSB":
		return store.ClassPhone
	}
	return store.ClassData
}

// FromRecord converts an ADIF record into a QSO (no ID yet). The error says
// why the record is unusable.
func FromRecord(r adif.Record, defaultOwn, source string) (store.QSO, error) {
	q := store.QSO{Source: source}
	q.OwnCall = NormalizeCall(r["STATION_CALLSIGN"])
	if q.OwnCall == "" {
		q.OwnCall = NormalizeCall(r["OPERATOR"])
	}
	if q.OwnCall == "" {
		q.OwnCall = NormalizeCall(defaultOwn)
	}
	q.Call = NormalizeCall(r["CALL"])
	q.Mode = strings.ToUpper(strings.TrimSpace(r["MODE"]))
	q.Submode = strings.ToUpper(strings.TrimSpace(r["SUBMODE"]))
	q.ModeClass = ModeClass(q.Mode, q.Submode)
	if f, err := strconv.ParseFloat(strings.TrimSpace(r["FREQ"]), 64); err == nil && f > 0 {
		q.FreqHz = int64(math.Round(f * 1e6))
	}
	q.Band = band.Normalize(r["BAND"])
	if !band.Known(q.Band) {
		q.Band = ""
		if b, ok := band.Of(float64(q.FreqHz)); ok {
			q.Band = b.Name
		}
	}
	date := strings.TrimSpace(r["QSO_DATE"])
	tm := strings.TrimSpace(r["TIME_ON"])
	if len(tm) == 4 {
		tm += "00"
	}
	if t, err := time.Parse("20060102150405", date+tm); err == nil && len(tm) == 6 {
		q.At = t.UTC()
	}
	q.DXCC = atoi(r["DXCC"])
	q.State = strings.ToUpper(strings.TrimSpace(r["STATE"]))
	q.CQZone = atoi(r["CQZ"])
	q.ITUZone = atoi(r["ITUZ"])
	q.Grid = strings.ToUpper(strings.TrimSpace(r["GRIDSQUARE"]))
	q.SOTARef = strings.ToUpper(strings.TrimSpace(r["SOTA_REF"]))
	q.WWFFRef = strings.ToUpper(strings.TrimSpace(r["WWFF_REF"]))
	q.Sig = strings.ToUpper(strings.TrimSpace(r["SIG"]))
	q.SigInfo = strings.ToUpper(strings.TrimSpace(r["SIG_INFO"]))
	q.QSLRcvd = yes(r["QSL_RCVD"])
	q.LoTWRcvd = yes(r["LOTW_QSL_RCVD"])
	q.EQSLRcvd = yes(r["EQSL_QSL_RCVD"])
	q.ClubLog = yes(r["CLUBLOG_QSO_DOWNLOAD_STATUS"])
	q.Confirmed = q.QSLRcvd || q.LoTWRcvd || q.EQSLRcvd || q.ClubLog
	switch {
	case q.Call == "":
		return q, errors.New("no CALL")
	case q.Mode == "":
		return q, errors.New("no MODE")
	case q.Band == "":
		return q, errors.New("no BAND and no usable FREQ")
	case q.At.IsZero():
		return q, errors.New("no usable QSO_DATE/TIME_ON")
	case q.OwnCall == "":
		return q, errors.New("no STATION_CALLSIGN or OPERATOR (set default_own_call)")
	}
	return q, nil
}

// yes is ADIF's received flag: Y (yes) and V (verified) count.
func yes(s string) bool {
	s = strings.ToUpper(strings.TrimSpace(s))
	return s == "Y" || s == "V"
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
