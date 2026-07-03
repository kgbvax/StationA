package flexradio

import (
	"reflect"
	"testing"
)

func TestParseFrame_Status(t *testing.T) {
	line := "S0|slice 0 0 freq=14.100.000 mode=USB active=1 tx=0 agc=FAST filter_lo=200 filter_hi=2900"
	f, err := ParseFrame(line)
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != FrameStatus {
		t.Errorf("Kind = %v, want FrameStatus", f.Kind)
	}
	if f.Topic != "slice" {
		t.Errorf("Topic = %q, want slice", f.Topic)
	}
	if f.TopicArgs != "0 0" {
		t.Errorf("TopicArgs = %q, want \"0 0\"", f.TopicArgs)
	}
	wantFields := map[string]string{
		"freq": "14.100.000", "mode": "USB", "active": "1",
		"tx": "0", "agc": "FAST", "filter_lo": "200", "filter_hi": "2900",
	}
	if !reflect.DeepEqual(f.Fields, wantFields) {
		t.Errorf("Fields = %v, want %v", f.Fields, wantFields)
	}
}

func TestParseFrame_Reply(t *testing.T) {
	f, err := ParseFrame("R1|0|OK")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != FrameReply {
		t.Errorf("Kind = %v, want FrameReply", f.Kind)
	}
	if f.Body != "0|OK" {
		t.Errorf("Body = %q, want \"0|OK\"", f.Body)
	}
}

func TestParseFrame_Command(t *testing.T) {
	f, err := ParseFrame("C1|sub slice all")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != FrameCommand {
		t.Errorf("Kind = %v, want FrameCommand", f.Kind)
	}
}

func TestParseFrame_Empty(t *testing.T) {
	if _, err := ParseFrame(""); err == nil {
		t.Error("ParseFrame(\"\") err = nil, want error")
	}
}

func TestParseInterlock_Transmitting(t *testing.T) {
	is := ParseInterlock("state=TRANSMITTING cause=PTT tx=1")
	if is.State != InterlockTransmitting {
		t.Errorf("State = %q, want TRANSMITTING", is.State)
	}
	if !is.Transmitting {
		t.Error("Transmitting = false, want true")
	}
}

func TestParseInterlock_Receiving(t *testing.T) {
	is := ParseInterlock("state=RECEIVING")
	if is.State != InterlockReceiving {
		t.Errorf("State = %q, want RECEIVING", is.State)
	}
	if is.Transmitting {
		t.Error("Transmitting = true, want false")
	}
}

func TestParseInterlock_UnknownEmpty(t *testing.T) {
	is := ParseInterlock("active=0") // no state field
	if is.State != InterlockUnknown {
		t.Errorf("State = %q, want UNKNOWN", is.State)
	}
}

func TestParseATU(t *testing.T) {
	a := ParseATU("status=Tuned active=1")
	if a.Status != "tuned" {
		t.Errorf("Status = %q, want tuned", a.Status)
	}
	if !a.Active {
		t.Error("Active = false, want true")
	}
}

func TestParseSlice(t *testing.T) {
	s, err := ParseSlice("0 0", "freq=14.100.000 mode=USB active=1 tx=0 agc=FAST filter_lo=200 filter_hi=2900")
	if err != nil {
		t.Fatal(err)
	}
	if s.Index != 0 {
		t.Errorf("Index = %d, want 0", s.Index)
	}
	if s.FreqHz != 14100000 {
		t.Errorf("FreqHz = %d, want 14100000", s.FreqHz)
	}
	if s.Mode != "USB" {
		t.Errorf("Mode = %q, want USB", s.Mode)
	}
	if !s.Active {
		t.Error("Active = false, want true")
	}
	if s.TX {
		t.Error("TX = true, want false")
	}
	if s.AGCMode != "FAST" {
		t.Errorf("AGCMode = %q, want FAST", s.AGCMode)
	}
	if s.FilterLow != 200 || s.FilterHigh != 2900 {
		t.Errorf("filter = [%d,%d], want [200,2900]", s.FilterLow, s.FilterHigh)
	}
}

func TestParseSlice_FreqFormats(t *testing.T) {
	cases := map[string]int64{
		"14.100.000": 14100000,
		"14100000":   14100000,
		"0.00720000": 720000, // 720 kHz, dot-grouped
		"7.200.000":  7200000,
	}
	for in, want := range cases {
		s, err := ParseSlice("0 0", "freq="+in+" mode=USB")
		if err != nil {
			t.Errorf("freq %q: %v", in, err)
			continue
		}
		if s.FreqHz != want {
			t.Errorf("freq %q -> %d, want %d", in, s.FreqHz, want)
		}
	}
}

func TestParseTxPower(t *testing.T) {
	n, ok := ParseTxPower("tx_power=100")
	if !ok || n != 100 {
		t.Errorf("got (%d,%v), want (100,true)", n, ok)
	}
	if _, ok := ParseTxPower("status=Available"); ok {
		t.Error("absent tx_power should return ok=false")
	}
}

func TestParseStatusFields_NoEquals(t *testing.T) {
	f := ParseStatusFields("foo bar baz")
	if len(f) != 0 {
		t.Errorf("got %d fields, want 0", len(f))
	}
}

func TestParseDiscoveryReply(t *testing.T) {
	raw := "version=3.4.1 serial=1234-5678-8400.12345 model=FLEX-8400 ip=192.168.1.50 port=4992 status=Available nickname=Shack"
	r := parseDiscoveryReply(raw)
	if r.Serial != "1234-5678-8400.12345" {
		t.Errorf("Serial = %q", r.Serial)
	}
	if r.Model != "FLEX-8400" {
		t.Errorf("Model = %q", r.Model)
	}
	if r.Status != "Available" {
		t.Errorf("Status = %q", r.Status)
	}
	if r.Version != "3.4.1" {
		t.Errorf("Version = %q", r.Version)
	}
	if r.Port != 4992 {
		t.Errorf("Port = %d", r.Port)
	}
	if r.IP == nil || r.IP.String() != "192.168.1.50" {
		t.Errorf("IP = %v", r.IP)
	}
}
