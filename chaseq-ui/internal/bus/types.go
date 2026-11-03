// Package bus is chaseq-ui's MQTT side: wire types mirrored from
// chase-queue's contract (chase-queue/docs/chase-queue-mqtt-api.md; the types
// are copied, not imported, because chase-queue's bus package is internal),
// payload decoders and the broker session.
package bus

import (
	"encoding/json"
	"fmt"
	"time"
)

// Activities.
const (
	ActivityDX      = "dx"
	ActivityContest = "contest"
)

// ChaseState is chase-queue's retained <site>/hf/chase/state (the fields
// chaseq-ui reads).
type ChaseState struct {
	TS       time.Time    `json:"ts"`
	Seq      int64        `json:"seq"`
	Activity string       `json:"activity"`
	Contest  *ContestInfo `json:"contest,omitempty"`
	Ranked   []Ranked     `json:"ranked"`
	Undo     string       `json:"undo,omitempty"`
	Problems []string     `json:"problems,omitempty"`
}

// ContestInfo describes the running contest instance.
type ContestInfo struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Ranked is one station on the list. Rows are self-contained: freq_hz and
// mode come with the row (an older chase-queue sends neither; a cluster row
// then still has spot.freq_hz).
type Ranked struct {
	RowID        int64         `json:"row_id"`
	Call         string        `json:"call,omitempty"`
	Band         string        `json:"band"`
	FreqHz       int64         `json:"freq_hz,omitempty"`
	Mode         string        `json:"mode,omitempty"` // canonical; "" = unknown
	Needed       bool          `json:"needed"`
	Tag          string        `json:"tag,omitempty"`
	Source       string        `json:"source,omitempty"`       // "bmrec" or "cluster"
	ClusterName  string        `json:"cluster_name,omitempty"` // the [[cluster]] whose spot the row stands for
	Spot         *ClusterSpot  `json:"spot,omitempty"`
	WorkedBefore *WorkedBefore `json:"worked_before,omitempty"` // nil = never worked
	Info         *Info         `json:"info,omitempty"`          // QRZ callbook entry; nil = not looked up
	// Needs lists the award slots the station would fill, by goal priority;
	// Buddy marks a station on the operator's buddy list (listed even when
	// nothing needs it).
	Needs     []Need `json:"needs,omitempty"`
	Buddy     bool   `json:"buddy,omitempty"`
	BuddyNote string `json:"buddy_note,omitempty"`
}

// Need is one award slot this station would fill.
type Need struct {
	Award  string `json:"award"`
	Key    string `json:"key,omitempty"`
	Band   string `json:"band,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Status string `json:"status,omitempty"` // "unworked" | "worked-unconfirmed"
}

// Info is the QRZ callbook entry chase-queue attaches (the fields shown).
type Info struct {
	Name    string `json:"name,omitempty"`
	QTH     string `json:"qth,omitempty"`
	State   string `json:"state,omitempty"`
	Country string `json:"country,omitempty"`
	Grid    string `json:"grid,omitempty"`
	CQZone  int    `json:"cq_zone,omitempty"`
	ITUZone int    `json:"itu_zone,omitempty"`
}

// Row sources.
const (
	SourceBmrec   = "bmrec"
	SourceCluster = "cluster"
)

// ClusterSpot is the latest cluster spot of a cluster row.
type ClusterSpot struct {
	FreqHz  int64     `json:"freq_hz"`
	Spotter string    `json:"spotter,omitempty"`
	Comment string    `json:"comment,omitempty"`
	At      time.Time `json:"at"`
	RBN     bool      `json:"rbn,omitempty"` // skimmer spot (spotter ending in "-#")
}

// WorkedBefore is what the logs know about earlier QSOs with the call.
type WorkedBefore struct {
	QSOs     int       `json:"qsos"`
	Bands    []string  `json:"bands,omitempty"`
	Modes    []string  `json:"modes,omitempty"`
	ThisBand bool      `json:"this_band"`
	Last     time.Time `json:"last"`
}

// RadioState is flexbridge's retained <site>/hf/radio/state (the fields
// chaseq-ui reads). FreqHz follows the TX slice; while Split, RxFreqHz is
// slice A, the one set_freq tunes.
type RadioState struct {
	FreqHz   int64 `json:"freq_hz"`
	Split    bool  `json:"split"`
	RxFreqHz int64 `json:"rx_freq_hz"`
}

// Tuned is the frequency a set_freq lands on in /state: slice A in split,
// else the TX slice.
func (r RadioState) Tuned() int64 {
	if r.Split && r.RxFreqHz > 0 {
		return r.RxFreqHz
	}
	return r.FreqHz
}

// Target names the row a skip/handled is for.
type Target struct {
	RowID int64  `json:"row_id"`
	Band  string `json:"band"`
	Call  string `json:"call,omitempty"`
}

// DecodeChase decodes a chase state payload.
func DecodeChase(p []byte) (ChaseState, error) {
	var s ChaseState
	if err := json.Unmarshal(p, &s); err != nil {
		return ChaseState{}, fmt.Errorf("chase state: %w", err)
	}
	return s, nil
}

// DecodeRadio decodes a radio state payload.
func DecodeRadio(p []byte) (RadioState, error) {
	var s RadioState
	if err := json.Unmarshal(p, &s); err != nil {
		return RadioState{}, fmt.Errorf("radio state: %w", err)
	}
	return s, nil
}

// cmd is the {action, value} /cmd envelope (shared/schema.CmdPayload).
type cmd struct {
	Action string `json:"action"`
	Value  string `json:"value"`
}

// CmdPayload builds a /cmd payload; value rides under "value".
func CmdPayload(action, value string) []byte {
	b, _ := json.Marshal(cmd{Action: action, Value: value})
	return b
}

// TargetValue is the JSON string chase-queue expects as skip/handled value.
func TargetValue(t Target) string {
	b, _ := json.Marshal(t)
	return string(b)
}
