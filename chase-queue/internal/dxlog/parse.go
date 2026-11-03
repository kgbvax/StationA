// Package dxlog receives DXLog.net's UDP QSO broadcast: one XML datagram per
// logged (or edited, or deleted) QSO. The contest mode of the CQ page learns
// the log from it. Nothing is ever sent back.
package dxlog

import (
	"bytes"
	"encoding/xml"
	"errors"
	"strconv"
	"strings"
	"time"

	"chase-queue/internal/band"
)

// Kind says what a datagram means.
type Kind int

const (
	KindIgnored Kind = iota // another message (radio info, spots, lookups)
	KindQSO                 // contactinfo: a new or edited QSO
	KindReplace             // contactreplace: the same fields; sent when the whole log is broadcast
	KindDelete              // contactdelete: a QSO removed
)

// QSO is what bmrec keeps of a logged QSO.
type QSO struct {
	StationID   string
	QSOID       string
	ContestName string
	Call        string
	Band        string // "20m"
	Mode        string
	At          time.Time
	Exch        [4]string
	Mults       [3]string
	Dupe        bool
	Invalid     bool
	XQSO        bool
}

// Key identifies a QSO across edits: the station that made it and its id
// there. DXLog's stationqso is that id; qsoid is DXLog's own id of the
// instance that broadcast, used when the station id is missing.
func (q QSO) Key() (station, id string) { return q.StationID, q.QSOID }

type contactXML struct {
	XMLName     xml.Name
	QSOID       string `xml:"qsoid"`
	StationID   string `xml:"stationid"`
	StationQSO  string `xml:"stationqso"`
	ContestName string `xml:"contestname"`
	Timestamp   string `xml:"timestamp"`
	Band        string `xml:"band"`
	Mode        string `xml:"mode"`
	Call        string `xml:"call"`
	Exch1       string `xml:"exch1"`
	Exch2       string `xml:"exch2"`
	Exch3       string `xml:"exch3"`
	Exch4       string `xml:"exch4"`
	XQSO        string `xml:"xqso"`
	Invalid     string `xml:"invalid"`
	Duplicate   string `xml:"duplicate"`
	Mult1       string `xml:"mult1"`
	Mult2       string `xml:"mult2"`
	Mult3       string `xml:"mult3"`
}

// ErrIgnored is returned by Parse for a datagram that is not a QSO message.
var ErrIgnored = errors.New("dxlog: not a QSO message")

// Parse reads one datagram.
func Parse(b []byte) (Kind, QSO, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	var c contactXML
	if err := dec.Decode(&c); err != nil {
		return KindIgnored, QSO{}, err
	}
	var kind Kind
	switch strings.ToLower(c.XMLName.Local) {
	case "contactinfo":
		kind = KindQSO
	case "contactreplace":
		kind = KindReplace
	case "contactdelete":
		kind = KindDelete
	default:
		return KindIgnored, QSO{}, ErrIgnored
	}
	q := QSO{
		StationID:   strings.TrimSpace(c.StationID),
		ContestName: strings.TrimSpace(c.ContestName),
		Call:        strings.ToUpper(strings.TrimSpace(c.Call)),
		Band:        bandName(c.Band),
		Mode:        strings.ToUpper(strings.TrimSpace(c.Mode)),
		Exch:        [4]string{strings.TrimSpace(c.Exch1), strings.TrimSpace(c.Exch2), strings.TrimSpace(c.Exch3), strings.TrimSpace(c.Exch4)},
		Mults:       [3]string{strings.TrimSpace(c.Mult1), strings.TrimSpace(c.Mult2), strings.TrimSpace(c.Mult3)},
		Dupe:        isTrue(c.Duplicate),
		Invalid:     isTrue(c.Invalid),
		XQSO:        isTrue(c.XQSO),
	}
	q.QSOID = strings.TrimSpace(c.StationQSO)
	if q.QSOID == "" {
		q.QSOID = strings.TrimSpace(c.QSOID)
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(c.Timestamp), time.UTC); err == nil {
		q.At = t
	}
	if q.QSOID == "" || (kind != KindDelete && q.Call == "") {
		return KindIgnored, QSO{}, errors.New("dxlog: QSO message without id or call")
	}
	return kind, q, nil
}

func isTrue(s string) bool { return strings.EqualFold(strings.TrimSpace(s), "true") }

// bandName turns DXLog's band in MHz ("14", "14.0", "14,0", "3.5") into the
// lab's band name ("20m"). DXLog rounds some bands (18 for 17 m), so a value
// just below a band's lower edge still belongs to it.
func bandName(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", ".")
	mhz, err := strconv.ParseFloat(s, 64)
	if err != nil || mhz <= 0 {
		return ""
	}
	if b, ok := band.Of(mhz * 1e6); ok {
		return b.Name
	}
	for _, b := range band.Bands {
		if mhz >= b.LoHz/1e6-0.5 && mhz <= b.HiHz/1e6 {
			return b.Name
		}
	}
	return ""
}
