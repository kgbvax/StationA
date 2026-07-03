package flexradio

import "testing"

// Tests for the real firmware formats observed on a FLEX-8400 (SmartSDR
// v4.2.20). These guard against the parser mismatches that initially broke
// the live deployment.

func TestParseSlice_RF_frequencyMHz(t *testing.T) {
	// Real firmware uses RF_frequency=<MHz float> and agc_mode=<mode>.
	body := "RF_frequency=3.800000 mode=LSB filter_lo=-2800 filter_hi=-100 agc_mode=med active=1 tx=1"
	s, err := ParseSlice("0", body)
	if err != nil {
		t.Fatal(err)
	}
	if s.FreqHz != 3_800_000 {
		t.Errorf("FreqHz = %d, want 3800000", s.FreqHz)
	}
	if s.Mode != "LSB" {
		t.Errorf("Mode = %q", s.Mode)
	}
	if s.AGCMode != "med" {
		t.Errorf("AGCMode = %q, want med", s.AGCMode)
	}
	if !s.Active || !s.TX {
		t.Errorf("Active=%v TX=%v, want true/true", s.Active, s.TX)
	}
	if s.FilterLow != -2800 || s.FilterHigh != -100 {
		t.Errorf("filter = [%d,%d], want [-2800,-100]", s.FilterLow, s.FilterHigh)
	}
}

func TestParseSlice_FrequencyPrecision(t *testing.T) {
	// 14.070.000 Hz expressed as MHz float.
	s, err := ParseSlice("0", "RF_frequency=14.070000 mode=USB")
	if err != nil {
		t.Fatal(err)
	}
	if s.FreqHz != 14_070_000 {
		t.Errorf("FreqHz = %d, want 14070000", s.FreqHz)
	}
}

func TestParseSlice_LegacyFreqStillWorks(t *testing.T) {
	// Older firmware used freq=<dotted Hz>. Still accepted.
	s, err := ParseSlice("0", "freq=14.100.000 mode=USB agc=FAST")
	if err != nil {
		t.Fatal(err)
	}
	if s.FreqHz != 14_100_000 {
		t.Errorf("FreqHz = %d, want 14100000", s.FreqHz)
	}
	if s.AGCMode != "FAST" {
		t.Errorf("AGCMode = %q, want FAST (legacy agc field)", s.AGCMode)
	}
}

func TestParseSlice_HFFrequency(t *testing.T) {
	cases := map[string]int64{
		"3.500000":   3_500_000,
		"7.074000":   7_074_000,
		"14.070000":  14_070_000,
		"50.150000":  50_150_000,
		"144.500000": 144_500_000,
	}
	for mhz, want := range cases {
		s, err := ParseSlice("0", "RF_frequency="+mhz+" mode=USB")
		if err != nil {
			t.Errorf("RF_frequency=%s: %v", mhz, err)
			continue
		}
		if s.FreqHz != want {
			t.Errorf("RF_frequency=%s -> %d, want %d", mhz, s.FreqHz, want)
		}
	}
}

func TestParseMeterListReply_RealFirmware(t *testing.T) {
	// Excerpt of the real reply (heavily truncated; format preserved).
	body := "0 meter 1.src=COD-#1.num=1#1.nam=MICPEAK#1.low=-150.0#1.unit=dBFS#1.fps=40#" +
		"2.src=COD-#2.num=2#2.nam=MIC#2.unit=dBFS#" +
		"3.src=TX-#3.num=5#3.nam=HWALC#3.unit=dBFS#" +
		"4.src=RAD#4.num=334#4.nam=+13.8A#4.unit=Volts#" +
		"5.src=RAD#5.num=300#5.nam=PACURRENT#5.unit=Amps#" +
		"8.src=TX-#8.num=1#8.nam=FWDPWR#8.unit=dBm#" +
		"12.src=SLC#12.num=0#12.nam=24kHz#12.unit=dBm#" +
		"16.src=SLC#16.num=0#16.nam=LEVEL#16.unit=dBm#"
	entries := ParseMeterListReply(body)
	if len(entries) != 8 {
		t.Fatalf("got %d entries, want 8: %+v", len(entries), entries)
	}
	want := map[uint16]struct{ src, nam string }{
		1:  {"COD-", "MICPEAK"},
		2:  {"COD-", "MIC"},
		3:  {"TX-", "HWALC"},
		4:  {"RAD", "+13.8A"},
		5:  {"RAD", "PACURRENT"},
		8:  {"TX-", "FWDPWR"},
		12: {"SLC", "24kHz"},
		16: {"SLC", "LEVEL"},
	}
	got := make(map[uint16]MeterListEntry)
	for _, e := range entries {
		got[e.Index] = e
	}
	for idx, w := range want {
		e, ok := got[idx]
		if !ok {
			t.Errorf("missing index %d", idx)
			continue
		}
		if e.Source != w.src || e.Name != w.nam {
			t.Errorf("index %d = %s/%s, want %s/%s", idx, e.Source, e.Name, w.src, w.nam)
		}
	}
	// Spot-check sourceNum: SLC entries share num=0 (slice index 0).
	if got[16].SourceNum != 0 {
		t.Errorf("LEVEL sourceNum = %d, want 0", got[16].SourceNum)
	}
	// FWDPWR has num=1.
	if got[8].SourceNum != 1 {
		t.Errorf("FWDPWR sourceNum = %d, want 1", got[8].SourceNum)
	}
}

func TestParseMeterListReply_EmptyOrMalformed(t *testing.T) {
	if entries := ParseMeterListReply(""); len(entries) != 0 {
		t.Errorf("empty body -> %d entries, want 0", len(entries))
	}
	if entries := ParseMeterListReply("not a meter list"); len(entries) != 0 {
		t.Errorf("garbage -> %d entries, want 0", len(entries))
	}
}

func TestParseSlice_RealInterlock(t *testing.T) {
	// Confirm the interlock parser copes with the real READY state.
	is := ParseInterlock("state=READY reason= source= tx_allowed=1 amplifier=")
	if is.State != InterlockReady {
		t.Errorf("State = %q, want READY", is.State)
	}
	if is.Transmitting {
		t.Error("READY should not be Transmitting")
	}
}
