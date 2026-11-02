// Package bus is chase-queue's MQTT contract (docs/chase-queue-mqtt-api.md):
// what it reads from bmrec's CQ finder slot, what it publishes on its own
// slot, and the /cmd actions it takes. The JSON names here are the wire;
// change them only together with bmrec.
package bus

import "time"

// CQFinderState is bmrec's retained <site>/hf/cqfinder/state: the open CQ
// rows it can rank and the human spots of the last half hour, so a restarted
// chase-queue has its spot horizon back from the retained message.
type CQFinderState struct {
	TS      time.Time   `json:"ts"`
	Seq     int64       `json:"seq"`
	Rows    []Candidate `json:"rows"`
	Spotted []Spot      `json:"spotted"`
}

// Candidate is one open CQ row of bmrec's board.
type Candidate struct {
	RowID      int64     `json:"row_id"`
	Call       string    `json:"call,omitempty"` // "" = no callsign yet
	CallSource string    `json:"call_source,omitempty"`
	Trusted    bool      `json:"call_trusted"` // read back by the answering station or typed by the operator
	Band       string    `json:"band"`
	FreqHz     float64   `json:"freq_hz"`
	FirstUTC   time.Time `json:"first_heard"`
	LastUTC    time.Time `json:"last_heard"`
}

// Spot is a human DX-cluster spot or a spot the operator posted.
type Spot struct {
	Call   string    `json:"call"`
	Band   string    `json:"band"`
	At     time.Time `json:"at"`
	Source string    `json:"source"` // "cluster" | "own"
}

// Activities.
const (
	ActivityDX      = "dx"
	ActivityContest = "contest"
)

// Tags shown on a needed row.
const (
	TagMult = "mult"
	TagNeed = "need"
)

// ChaseState is chase-queue's retained <site>/hf/chase/state.
type ChaseState struct {
	TS time.Time `json:"ts"`
	// Seq echoes the CQFinderState.Seq the ranking was computed from.
	Seq      int64        `json:"seq"`
	Activity string       `json:"activity"`
	Contest  *ContestInfo `json:"contest,omitempty"`
	// Ranked lists the stations on the list, in work order. A row bmrec has
	// that is absent here is not on the list (spotted, worked, skipped, ...).
	Ranked []Ranked `json:"ranked"`
	// Undo names the last action Undo would reverse ("" = nothing).
	Undo string `json:"undo,omitempty"`
	// Problems are what keeps the ranking from being right, worst first.
	Problems []string `json:"problems,omitempty"`
}

// Ranked is one station on the list.
type Ranked struct {
	RowID  int64  `json:"row_id"`
	Call   string `json:"call,omitempty"`
	Band   string `json:"band"`
	Needed bool   `json:"needed"`
	Tag    string `json:"tag,omitempty"`
}

// ContestInfo describes the running contest instance.
type ContestInfo struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	StartDate   string   `json:"start_date"`
	QSOs        int      `json:"qsos"`
	Unsupported []string `json:"unsupported,omitempty"`
}

// /cmd actions: shared/schema.CmdPayload {action, value}; value is a string.
const (
	CmdSkip           = "skip"            // value: Target JSON
	CmdHandled        = "handled"         // value: Target JSON; a contest QSY
	CmdUndo           = "undo"            // value: ""
	CmdSetMode        = "set_mode"        // value: "dx"
	CmdSwitchInstance = "switch_instance" // value: SwitchReq JSON
	CmdDeleteInstance = "delete_instance" // value: instance id
)

// Target names the row an action is for, as the window saw it.
type Target struct {
	RowID int64  `json:"row_id"`
	Band  string `json:"band"`
	Call  string `json:"call,omitempty"` // the call the operator settled on
	How   string `json:"how,omitempty"`
}

// SwitchReq starts contest mode on an existing instance, or on a new one from
// an uploaded definition (and prefill) and a start date.
type SwitchReq struct {
	Instance     int64  `json:"instance,omitempty"`
	New          bool   `json:"new,omitempty"`
	StartDate    string `json:"start_date,omitempty"`
	DefinitionID int64  `json:"definition_id,omitempty"`
	PrefillID    int64  `json:"prefill_id,omitempty"`
}
