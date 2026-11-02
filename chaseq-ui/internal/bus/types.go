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

// Ranked is one station on the list.
type Ranked struct {
	RowID  int64        `json:"row_id"`
	Call   string       `json:"call,omitempty"`
	Band   string       `json:"band"`
	Needed bool         `json:"needed"`
	Tag    string       `json:"tag,omitempty"`
	Source string       `json:"source,omitempty"` // "cluster" for a DX-cluster row
	Spot   *ClusterSpot `json:"spot,omitempty"`
}

// SourceCluster marks a ranked row from the DX cluster.
const SourceCluster = "cluster"

// ClusterSpot is the latest cluster spot of a cluster row.
type ClusterSpot struct {
	FreqHz int64 `json:"freq_hz"`
}

// CQFinderState is bmrec's retained <site>/hf/cqfinder/state.
type CQFinderState struct {
	TS   time.Time   `json:"ts"`
	Seq  int64       `json:"seq"`
	Rows []Candidate `json:"rows"`
}

// Candidate is one open CQ row heard by bmrec.
type Candidate struct {
	RowID  int64   `json:"row_id"`
	Call   string  `json:"call,omitempty"`
	Band   string  `json:"band"`
	FreqHz float64 `json:"freq_hz"`
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

// DecodeCQFinder decodes a cqfinder state payload.
func DecodeCQFinder(p []byte) (CQFinderState, error) {
	var s CQFinderState
	if err := json.Unmarshal(p, &s); err != nil {
		return CQFinderState{}, fmt.Errorf("cqfinder state: %w", err)
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
