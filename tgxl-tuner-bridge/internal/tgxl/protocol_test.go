package tgxl

import (
	"math"
	"testing"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		in   string
		want Line
		ok   bool
	}{
		{"V1.1.8 AUTH", Line{Kind: KindVersion, Msg: "V1.1.8 AUTH"}, true},
		{"R3|0|info serial=210387-1 version=1.1.8 nickname=Tuner_Genius_XL 3way=1",
			Line{Kind: KindReply, Seq: 3, Code: 0, Msg: "info serial=210387-1 version=1.1.8 nickname=Tuner_Genius_XL 3way=1"}, true},
		{"R10|0|", Line{Kind: KindReply, Seq: 10}, true},
		{"R5|7|a|b", Line{Kind: KindReply, Seq: 5, Code: 7, Msg: "a|b"}, true},
		{"S7|status fwd=1", Line{Kind: KindStatus, Seq: 7, Msg: "status fwd=1"}, true},
		{"M|Tuning Saved", Line{Kind: KindMessage, Msg: "Tuning Saved"}, true},
		{"M|", Line{Kind: KindMessage}, true},
		{"", Line{}, false},
		{"Rx|0|", Line{}, false},
		{"Q1", Line{}, false},
	}
	for _, c := range cases {
		got, err := ParseLine(c.in + "\n")
		if (err == nil) != c.ok {
			t.Fatalf("%q: err=%v", c.in, err)
		}
		if c.ok && got != c.want {
			t.Errorf("%q: got %+v want %+v", c.in, got, c.want)
		}
	}
}

func TestFormatCmd(t *testing.T) {
	if got := FormatCmd(12, "bypass set=1"); got != "C12|bypass set=1\n" {
		t.Fatalf("got %q", got)
	}
}

const sampleStatus = "status fwd=23.63 peak=23.84 max=0.00 swr=-60.0000 pttA=0 bandA=0 modeA=0 flexA=RATISEVINA " +
	"freqA=0.000 bypassA=0 bypassRxA=0 antA=2 pttB=0 bandB=0 modeB=0 flexB= freqB=0.000 bypassB=0 " +
	"bypassRxB=0 antB=0 state=1 active=1 tuning=1 bypass=0 ag=0 relayC1=10 relayL=20 relayC2=30"

func TestParseStatusPDFSample(t *testing.T) {
	st, err := ParseStatus(sampleStatus)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Operate || !st.Tuning || st.Bypass || st.PTT || st.Antenna != 2 ||
		st.RelayC1 != 10 || st.RelayL != 20 || st.RelayC2 != 30 || st.FwdDBm != 23.63 || st.SWRdB != -60 {
		t.Fatalf("unexpected %+v", st)
	}
}

func TestParseStatusMissingKey(t *testing.T) {
	if _, err := ParseStatus("status fwd=1.0"); err == nil {
		t.Fatal("want error for incomplete status")
	}
	if _, err := ParseStatus("info serial=1"); err == nil {
		t.Fatal("want error for non-status")
	}
}

func TestParseKVEmptyValue(t *testing.T) {
	name, kv := ParseKV("status flexB= a=1")
	if name != "status" || kv["a"] != "1" {
		t.Fatalf("%q %v", name, kv)
	}
	if v, ok := kv["flexB"]; !ok || v != "" {
		t.Fatalf("flexB: %q %v", v, ok)
	}
}

func TestDBmToWatts(t *testing.T) {
	cases := map[float64]uint16{23.63: 0, 30: 1, 40: 10, 50: 100, 60: 1000, -60: 0, 99: 65535}
	// 23.63 dBm = 0.23 W -> 0 W (below 1 W floor)
	for in, want := range cases {
		if got := DBmToWatts(in); got != want {
			t.Errorf("DBmToWatts(%v)=%d want %d", in, got, want)
		}
	}
}

func TestReturnLossToSWR(t *testing.T) {
	cases := []struct{ rl, want float64 }{
		{-60, 1.00}, {-20, 1.22}, {-10, 1.92}, {-9.54, 2.00}, {0, 99}, {-1, 17.39},
	}
	for _, c := range cases {
		if got := ReturnLossToSWR(c.rl); math.Abs(got-c.want) > 0.02 {
			t.Errorf("RL %v: got %v want ~%v", c.rl, got, c.want)
		}
	}
}

func TestParseDiscovery(t *testing.T) {
	d, ok := ParseDiscovery("TunerGenius ip=192.168.1.193 v=1.1.8 serial=210387-1 nickname=Tuner_Genius_XL")
	if !ok || d.IP != "192.168.1.193" || d.Serial != "210387-1" || d.Nickname != "Tuner Genius XL" {
		t.Fatalf("%+v %v", d, ok)
	}
	if _, ok := ParseDiscovery("Other ip=1.2.3.4"); ok {
		t.Fatal("non-TunerGenius accepted")
	}
}
