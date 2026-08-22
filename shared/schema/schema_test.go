package schema

import "testing"

func TestTopics(t *testing.T) {
	got := SlotBase("muehle", "hf", "radio")
	want := "muehle/hf/radio"
	if got != want {
		t.Fatalf("SlotBase = %q, want %q", got, want)
	}
	cases := map[string]string{
		MetaTopic("muehle", "power", "master"):         "muehle/power/master/meta",
		StateTopic("muehle", "power", "master"):        "muehle/power/master/state",
		StatusTopic("muehle", "power", "master"):       "muehle/power/master/status",
		CmdTopic("muehle", "power", "master"):          "muehle/power/master/cmd",
		SiblingTopic("muehle", "hf", "radio", "state"): "muehle/hf/radio/state",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
