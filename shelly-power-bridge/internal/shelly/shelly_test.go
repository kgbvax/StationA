package shelly

import (
	"encoding/json"
	"testing"
)

func TestParseStatus(t *testing.T) {
	cases := []struct {
		payload string
		want    string
	}{
		{`{"output":true,"apower":12.3}`, "on"},
		{`{"output":false}`, "off"},
		{`{"output":true}`, "on"},
	}
	for _, c := range cases {
		s, power, err := ParseStatus([]byte(c.payload))
		if err != nil {
			t.Fatalf("ParseStatus(%q): %v", c.payload, err)
		}
		if power != c.want {
			t.Errorf("ParseStatus(%q) power = %q, want %q", c.payload, power, c.want)
		}
		if c.want == "on" && !s.Output {
			t.Errorf("Output not preserved for %q", c.payload)
		}
	}
}

func TestParseStatusRejectsGarbage(t *testing.T) {
	if _, _, err := ParseStatus([]byte(`{not json`)); err == nil {
		t.Error("ParseStatus should reject malformed JSON")
	}
}

func TestSwitchSetPayload(t *testing.T) {
	b := SwitchSet(true)
	var v struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params struct {
			ID int  `json:"id"`
			On bool `json:"on"`
		} `json:"params"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("SwitchSet payload not valid JSON: %v", err)
	}
	if v.Method != "Switch.Set" || v.Params.ID != 0 || !v.Params.On {
		t.Errorf("SwitchSet(true) = %s, want Switch.Set id0 on", b)
	}
	if string(SwitchSet(false)) == string(SwitchSet(true)) {
		t.Error("SwitchSet(false) must differ from SwitchSet(true)")
	}
}

func TestTopics(t *testing.T) {
	if got := StatusTopic("shellyplus1pm-aa"); got != "shellyplus1pm-aa/status/switch:0" {
		t.Errorf("StatusTopic = %q", got)
	}
	if got := RPCTopic("shellyplus1pm-aa"); got != "shellyplus1pm-aa/rpc" {
		t.Errorf("RPCTopic = %q", got)
	}
}
