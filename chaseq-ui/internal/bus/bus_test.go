package bus

import (
	"encoding/json"
	"testing"
)

func TestDecodeChase(t *testing.T) {
	p := []byte(`{"ts":"2026-11-02T10:00:00Z","seq":42,"activity":"contest","contest":{"id":1,"name":"UKEIDXSSB"},
	 "ranked":[{"row_id":7,"call":"EA8XYZ","band":"20m","freq_hz":14231500,"mode":"usb","needed":true,"tag":"mult","source":"bmrec"},
	           {"row_id":-3,"call":"DL1ABC","band":"20m","needed":true,"tag":"need","source":"cluster","spot":{"freq_hz":14025000}}],
	 "undo":"EA8XYZ","problems":["Club Log confirmations not loaded yet"]}`)
	s, err := DecodeChase(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Activity != ActivityContest || s.Contest == nil || s.Contest.Name != "UKEIDXSSB" {
		t.Errorf("activity/contest = %q %+v", s.Activity, s.Contest)
	}
	if len(s.Ranked) != 2 || s.Ranked[1].Source != SourceCluster || s.Ranked[1].Spot.FreqHz != 14025000 {
		t.Errorf("ranked = %+v", s.Ranked)
	}
	r0 := s.Ranked[0]
	if r0.FreqHz != 14231500 || r0.Mode != "usb" || r0.Source != SourceBmrec || r0.WorkedBefore != nil {
		t.Errorf("row 0 = %+v", r0)
	}
	if s.Undo != "EA8XYZ" || len(s.Problems) != 1 {
		t.Errorf("undo/problems = %q %v", s.Undo, s.Problems)
	}
}

func TestDecodeWorkedBefore(t *testing.T) {
	s, err := DecodeChase([]byte(`{"ranked":[{"row_id":1,"band":"20m","worked_before":{"qsos":3,"bands":["20m","40m"],"modes":["cw"],"this_band":true,"last":"2026-05-01"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	w := s.Ranked[0].WorkedBefore
	if w == nil || w.QSOs != 3 || !w.ThisBand || len(w.Bands) != 2 || w.Last != "2026-05-01" {
		t.Errorf("worked_before = %+v", w)
	}
}

func TestDecodeMalformed(t *testing.T) {
	if _, err := DecodeChase([]byte(`{"ranked":`)); err == nil {
		t.Error("want error for truncated JSON")
	}
	if _, err := DecodeRadio([]byte(`nope`)); err == nil {
		t.Error("want error for non-JSON")
	}
}

func TestRadioTuned(t *testing.T) {
	if got := (RadioState{FreqHz: 14100000}).Tuned(); got != 14100000 {
		t.Errorf("not split Tuned = %d", got)
	}
	if got := (RadioState{FreqHz: 14105000, Split: true, RxFreqHz: 14100000}).Tuned(); got != 14100000 {
		t.Errorf("split Tuned = %d", got)
	}
}

func TestCmdPayloadAndTarget(t *testing.T) {
	var c map[string]string
	if err := json.Unmarshal(CmdPayload("skip", TargetValue(Target{RowID: 7, Band: "20m", Call: "EA8XYZ"})), &c); err != nil {
		t.Fatal(err)
	}
	if c["action"] != "skip" {
		t.Errorf("action = %q", c["action"])
	}
	var tg Target
	if err := json.Unmarshal([]byte(c["value"]), &tg); err != nil || tg.RowID != 7 || tg.Band != "20m" || tg.Call != "EA8XYZ" {
		t.Errorf("value = %q (%v)", c["value"], err)
	}
	if string(CmdPayload("undo", "")) != `{"action":"undo","value":""}` {
		t.Errorf("undo payload = %s", CmdPayload("undo", ""))
	}
}
