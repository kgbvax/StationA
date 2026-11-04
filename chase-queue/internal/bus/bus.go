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
	RowID      int64  `json:"row_id"`
	Call       string `json:"call,omitempty"` // "" = no callsign yet
	CallSource string `json:"call_source,omitempty"`
	Trusted    bool   `json:"call_trusted"` // read back by the answering station or typed by the operator
	// Qualified is bmrec's statement that the operator qualified the row as a
	// CQ (or its confidence is high). A pointer: nil is a bmrec that does not
	// send the field (older), false is a row it sends unqualified.
	Qualified *bool     `json:"qualified,omitempty"`
	Band      string    `json:"band"`
	FreqHz    float64   `json:"freq_hz"`
	FirstUTC  time.Time `json:"first_heard"`
	LastUTC   time.Time `json:"last_heard"`
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
	TagMult = "mult" // the contest award needs the station
	TagNeed = "need" // another award needs it
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
	// BmrecUnqualified counts the bmrec rows the [bmrec] gate kept off the
	// list; absent when none (or the gate is off). Not a problem by itself.
	BmrecUnqualified int `json:"bmrec_unqualified,omitempty"`
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
	// Needs lists the award slots the station would fill, one per enabled
	// goal slot that is not yet satisfied, by goal priority. Needed and Tag
	// are the derived summary: Needed is true exactly when Needs is not
	// empty. Absent on a row that is not needed.
	Needs []Need `json:"needs,omitempty"`
	// FreqHz is the station's frequency in Hz: the heard row's, or the latest
	// spot's for a cluster row. Absent only when unknown.
	FreqHz int64 `json:"freq_hz,omitempty"`
	// Mode is the canonical mode (cw, usb, lsb, am, fm, data) chase-queue
	// derives from the frequency and, for a cluster spot, its comment; absent
	// when unclear.
	Mode string `json:"mode,omitempty"`
	// Source is "bmrec" for a row bmrec hears and "cluster" for a station
	// chase-queue's own DX-cluster login spotted (RowID is then negative and
	// there is no bmrec row). Every row is self-contained: a consumer needs no
	// other topic to show it.
	Source string `json:"source"`
	// ClusterName names the [[cluster]] whose spot the row stands for (the
	// freshest of several that spotted the station); with Source "cluster".
	ClusterName string       `json:"cluster_name,omitempty"`
	Spot        *ClusterSpot `json:"spot,omitempty"` // with Source "cluster"
	// Buddy marks a station on the operator's buddy list. A buddy is listed
	// even when no goal needs it (just below the needed block) and ranks
	// first among equally prioritised needed stations.
	Buddy     bool   `json:"buddy,omitempty"`
	BuddyNote string `json:"buddy_note,omitempty"`
	// Info is the callbook entry (QRZ.com); absent until looked up, when the
	// call is not in the callbook, or with no QRZ login configured.
	Info *CallInfo `json:"info,omitempty"`
	// Worked is the station log's history of this call; absent when never
	// worked. A station worked on the row's band (ThisBand) is still listed,
	// after the unworked ones, with Needed false and no Tag.
	Worked *WorkedBefore `json:"worked_before,omitempty"`
	// OnBand is the engine's verdict that the station counts as worked on the
	// row's band (the tier of the row); it decides Worked.ThisBand and is not
	// published.
	OnBand bool `json:"-"`
}

// Need is one award slot a ranked station would fill.
type Need struct {
	// Award is the award code (a definition's code, "dxcc", "contest").
	Award string `json:"award"`
	// Key is the slot's name: the entity's primary prefix, a state, a
	// multiplier.
	Key string `json:"key"`
	// Band is the slot's band; absent when the award counts over all bands.
	Band string `json:"band,omitempty"`
	// Mode is the slot's mode class (cw, phone, data); absent when the award
	// does not slice or filter by mode.
	Mode string `json:"mode,omitempty"`
	// Status is the slot's state in the log: unworked or worked-unconfirmed.
	Status string `json:"status"`
	// ATNO is set when the slot's key is worked in no slice at all (an
	// all-time new one: a new entity, not only a new band).
	ATNO bool `json:"atno,omitempty"`
}

// Ranked row sources.
const (
	SourceBmrec   = "bmrec"
	SourceCluster = "cluster"
)

// ClusterSpot is the latest cluster spot of a cluster row.
type ClusterSpot struct {
	FreqHz  int64     `json:"freq_hz"`
	Spotter string    `json:"spotter"`
	Comment string    `json:"comment,omitempty"`
	At      time.Time `json:"at"`
	// RBN marks a skimmer spot (spotter ending in "-#").
	RBN bool `json:"rbn,omitempty"`
}

// CallInfo is what the callbook says about a callsign. Street address,
// e-mail and birth date are never published.
type CallInfo struct {
	Source  string   `json:"source"` // "qrz"
	Call    string   `json:"call"`   // the callbook entry's call (the home call of a portable form)
	Name    string   `json:"name,omitempty"`
	QTH     string   `json:"qth,omitempty"` // city
	State   string   `json:"state,omitempty"`
	Country string   `json:"country,omitempty"`
	Grid    string   `json:"grid,omitempty"`
	Lat     *float64 `json:"lat,omitempty"`
	Lon     *float64 `json:"lon,omitempty"`
	Image   string   `json:"image,omitempty"` // URL of the operator's picture
	CQZone  int      `json:"cq_zone,omitempty"`
	ITUZone int      `json:"itu_zone,omitempty"`
}

// WorkedBefore summarises the QSOs with a call in the station log (Log4OM,
// Club Log) and the stored DXLog contest logs, any mode, ever.
type WorkedBefore struct {
	QSOs     int       `json:"qsos"`
	Bands    []string  `json:"bands"`           // band order, low to high
	Modes    []string  `json:"modes,omitempty"` // as logged, upper case
	ThisBand bool      `json:"this_band"`       // worked on the row's band
	Last     time.Time `json:"last"`
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
