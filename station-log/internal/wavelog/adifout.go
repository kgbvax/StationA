package wavelog

import (
	"fmt"
	"strconv"

	"station-log/internal/adif"
	"station-log/internal/store"
)

// BuildADIF renders a candidate for POST /api/v2/qso (import_type adif; FREQ
// in MHz, as the docs prescribe for ADIF payloads): the raw record of its first
// non-Wavelog source (keeps RST, name, QTH, comments), overlaid with the
// merged QSO values. A QSO without a raw record (imported before qso_raw
// existed) goes out struct-only: lossy, but correct in what it carries.
func BuildADIF(c store.PushCandidate) string {
	q := c.QSO
	r := adif.Record{}
	if c.Raw != "" {
		if recs := adif.Parse(c.Raw); len(recs) > 0 {
			r = recs[0]
		}
	}
	set := func(k, v string) {
		if v != "" {
			r[k] = v
		}
	}
	setInt := func(k string, n int) {
		if n > 0 {
			r[k] = strconv.Itoa(n)
		}
	}
	set("CALL", q.Call)
	set("STATION_CALLSIGN", q.OwnCall)
	set("QSO_DATE", q.At.UTC().Format("20060102"))
	set("TIME_ON", q.At.UTC().Format("150405"))
	set("BAND", q.Band)
	set("MODE", q.Mode)
	set("SUBMODE", q.Submode)
	if q.FreqHz > 0 {
		r["FREQ"] = fmt.Sprintf("%.6f", float64(q.FreqHz)/1e6)
	}
	set("GRIDSQUARE", q.Grid)
	set("STATE", q.State)
	setInt("DXCC", q.DXCC)
	setInt("CQZ", q.CQZone)
	setInt("ITUZ", q.ITUZone)
	set("SOTA_REF", q.SOTARef)
	set("WWFF_REF", q.WWFFRef)
	set("SIG", q.Sig)
	set("SIG_INFO", q.SigInfo)
	if q.QSLRcvd {
		r["QSL_RCVD"] = "Y"
	}
	if q.LoTWRcvd {
		r["LOTW_QSL_RCVD"] = "Y"
	}
	if q.EQSLRcvd {
		r["EQSL_QSL_RCVD"] = "Y"
	}
	return adif.Format(r)
}
