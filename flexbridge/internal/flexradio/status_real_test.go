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

func TestParseSlice_PartialUpdatePreservesActive(t *testing.T) {
	// SmartSDR sends partial updates: only changed fields are included.
	// A frequency-only update must not clobber Active=true from the previous state.
	prev, _ := ParseSlice("0", "RF_frequency=14.100000 mode=USB active=1 tx=0")
	if !prev.Active {
		t.Fatal("initial parse: Active should be true")
	}

	// Frequency-only update — no active= key in the payload.
	cur, err := ParseSlice("0", "RF_frequency=14.200000", prev)
	if err != nil {
		t.Fatal(err)
	}
	if cur.FreqHz != 14_200_000 {
		t.Errorf("FreqHz = %d, want 14200000", cur.FreqHz)
	}
	if !cur.Active {
		t.Error("Active dropped to false on partial frequency update; want true carried over from prev")
	}
	if cur.Mode != "USB" {
		t.Errorf("Mode = %q, want USB carried over from prev", cur.Mode)
	}
}

func TestParseSlice_RFFrequencyRounding(t *testing.T) {
	// int64(mhz*1e6) truncates toward zero and gives 1 Hz low for ~1.2% of
	// 10-Hz-step values, because the double product lands just below the
	// integer (e.g. RF_frequency=2.000040 -> 2.0000399999999998 * 1e6). The
	// conversion must use math.Round. These cases all truncate wrong without it.
	cases := map[string]int64{
		"2.000040":  2_000_040,
		"2.000050":  2_000_050,
		"2.000110":  2_000_110,
		"14.100000": 14_100_000, // baseline, already exact
		"3.800000":  3_800_000,
		"7.074000":  7_074_000,
	}
	for mhz, want := range cases {
		s, err := ParseSlice("0", "RF_frequency="+mhz+" mode=USB")
		if err != nil {
			t.Fatalf("RF_frequency=%s: %v", mhz, err)
		}
		if s.FreqHz != want {
			t.Errorf("RF_frequency=%s -> %d, want %d", mhz, s.FreqHz, want)
		}
	}
}

func TestParseSlice_MalformedFreqKeepsOtherFields(t *testing.T) {
	// A malformed RF_frequency must be skipped (prev freq retained) and the
	// rest of the incremental frame still applied. The parser must NOT return an
	// error that would make the bridge drop the whole frame.
	prev, err := ParseSlice("0", "RF_frequency=14.100000 mode=CW active=1 tx=0")
	if err != nil {
		t.Fatal(err)
	}

	// Empty RF_frequency value, plus mode/tx/active changes.
	cur, err := ParseSlice("0", "RF_frequency= mode=USB tx=1 active=1", prev)
	if err != nil {
		t.Fatalf("malformed RF_frequency must not be fatal: %v", err)
	}
	if cur.FreqHz != 14_100_000 {
		t.Errorf("FreqHz = %d, want retained 14100000", cur.FreqHz)
	}
	if cur.Mode != "USB" {
		t.Errorf("Mode = %q, want USB (applied despite bad freq)", cur.Mode)
	}
	if !cur.TX {
		t.Error("TX = false, want true (applied despite bad freq)")
	}
	if !cur.Active {
		t.Error("Active = false, want true (applied despite bad freq)")
	}

	// A non-numeric RF_frequency is handled the same way.
	cur2, err := ParseSlice("0", "RF_frequency=garbage mode=FM", prev)
	if err != nil {
		t.Fatalf("garbage RF_frequency must not be fatal: %v", err)
	}
	if cur2.FreqHz != 14_100_000 {
		t.Errorf("FreqHz = %d, want retained 14100000", cur2.FreqHz)
	}
	if cur2.Mode != "FM" {
		t.Errorf("Mode = %q, want FM (applied despite bad freq)", cur2.Mode)
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

func TestParseProfile_RealMicList(t *testing.T) {
	// Captured live on a FLEX-8400 (SmartSDR v4.2.20) as the reply to the
	// one-shot `profile mic info` command. The radio emits an authoritative
	// full-snapshot list frame: caret-delimited names (which contain spaces),
	// terminated by a trailing caret. This guards the raw-body parser against
	// any future drift back to space-splitting (which would mangle names like
	// "Default FHM-1" and "Default PR781 ESSB 3_2k").
	const raw = "mic list=Default^Default FHM-1^Default FHM-1 DX^Default FHM-2^" +
		"Default FHM-2 DX^Default FHM-2 ESSB^Default FHM-3^Default FHM-3 DX^" +
		"Default FHM-3 ESSB^Default HDST Condenser^Default HDST Dynamic^" +
		"Default HM-Pro^Default PR781^Default PR781 ESSB 3_2k^Default ProSet HC6^" +
		"Inrad M629^Inrad M650^iOS_default_Profile^macOS_default_Profile^" +
		"radiosport DX M20^radiosport DX M207^radiosport DX M208^radiosport DX M350-ADJ^" +
		"radiosport WIDE M20^radiosport WIDE M207^radiosport WIDE M208^radiosport WIDE M350-ADJ^" +
		"RTTYDefault^"
	p := ParseProfile(raw)
	if p.Type != "mic" || !p.IsList {
		t.Fatalf("Type=%q IsList=%v, want mic/true", p.Type, p.IsList)
	}
	// Spot-check names that contain spaces and a trailing-caret survivor.
	wantContains := []string{
		"Default", "Default FHM-1", "Default PR781 ESSB 3_2k",
		"Default ProSet HC6", "RTTYDefault",
	}
	have := map[string]bool{}
	for _, n := range p.Names {
		have[n] = true
	}
	for _, w := range wantContains {
		if !have[w] {
			t.Errorf("missing name %q in parsed list (%d names)", w, len(p.Names))
		}
	}
	// The trailing caret must NOT produce an empty trailing entry.
	for _, n := range p.Names {
		if n == "" {
			t.Error("parsed an empty name from the trailing caret")
		}
	}
	if p.IsCurrent {
		t.Error("IsCurrent = true for a list frame, want false")
	}
}
