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

func TestParseDVK(t *testing.T) {
	// A status= frame carries DVK state for the /state plane.
	d := ParseDVK("status=playback id=3 enabled=1")
	if !d.HasStatus {
		t.Fatal("HasStatus=false for status= frame, want true")
	}
	if d.Status != "playback" {
		t.Errorf("Status = %q, want playback", d.Status)
	}
	if d.ID != 3 {
		t.Errorf("ID = %d, want 3", d.ID)
	}

	// idle is still a status= frame (HasStatus true); no active memory id.
	d = ParseDVK("status=idle")
	if !d.HasStatus {
		t.Error("idle: HasStatus=false, want true")
	}
	if d.Status != "idle" {
		t.Errorf("Status = %q, want idle", d.Status)
	}
	if d.ID != 0 {
		t.Errorf("ID = %d, want 0 (idle has no active memory)", d.ID)
	}

	// Memory-library frames (added/deleted) carry no status= key → not state.
	if d := ParseDVK(`added id=1 name="CQ" duration=5000`); d.HasStatus {
		t.Error(`added id=1 name="CQ" ...: HasStatus=true, want false`)
	}
	if d := ParseDVK("deleted id=1"); d.HasStatus {
		t.Error("deleted id=1: HasStatus=true, want false")
	}
	// "id=1 deleted" word-ordering variant (still no status= key) → not state.
	if d := ParseDVK("id=1 deleted"); d.HasStatus {
		t.Error("id=1 deleted: HasStatus=true, want false")
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

func TestParseDrive(t *testing.T) {
	n, ok := ParseDrive("drive=75 status=Available")
	if !ok || n != 75 {
		t.Errorf("got (%d,%v), want (75,true)", n, ok)
	}
	if _, ok := ParseDrive("tx_power=100"); ok {
		t.Error("absent drive should return ok=false")
	}
}

func TestParseRadioTuning(t *testing.T) {
	if v, ok := ParseRadioTuning("tuning=1 status=Available"); !ok || !v {
		t.Errorf("tuning=1: got (%v,%v), want (true,true)", v, ok)
	}
	if v, ok := ParseRadioTuning("tuning=0"); !ok || v {
		t.Errorf("tuning=0: got (%v,%v), want (false,true)", v, ok)
	}
	if _, ok := ParseRadioTuning("status=Available"); ok {
		t.Error("absent tuning should return ok=false")
	}
}

func TestNormalizeMode(t *testing.T) {
	cases := map[string]string{
		"USB": "usb", "LSB": "lsb",
		"CW-U": "cw", "CW-L": "cw", "CW": "cw",
		"AM": "am", "SAM": "am",
		"FM": "fm", "NFM": "fm",
		"DIGU": "data", "DIGL": "data",
		"RTTY-U": "data", "PKTUSB": "data",
		"FDV": "data",
		"":    "",
	}
	for in, want := range cases {
		if got := NormalizeMode(in); got != want {
			t.Errorf("NormalizeMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePan(t *testing.T) {
	// A "display pan" status line: the frame's TopicArgs is "pan <stream_id>";
	// the handle is the topic arg after the literal "pan" word. center= is MHz;
	// pan status carries no band= field in practice (best-effort here).
	p := ParsePan("pan 0x40000000", "center=14.175 x_pixels=1000")
	if p.Handle != "0x40000000" {
		t.Errorf("Handle = %q, want 0x40000000", p.Handle)
	}
	if p.CenterHz != 14_175_000 {
		t.Errorf("CenterHz = %d, want 14175000", p.CenterHz)
	}

	// band= is parsed when present (best-effort; real pan status uses center).
	p = ParsePan("pan 0x40000000", "band=20 center=14.175")
	if p.Band != 20 {
		t.Errorf("Band = %d, want 20", p.Band)
	}

	// Missing center leaves CenterHz at zero.
	p = ParsePan("pan 0x40000001", "x_pixels=1000")
	if p.Handle != "0x40000001" {
		t.Errorf("Handle = %q, want 0x40000001", p.Handle)
	}
	if p.CenterHz != 0 {
		t.Errorf("CenterHz = %d, want 0 when absent", p.CenterHz)
	}

	// Non-numeric band is ignored.
	p = ParsePan("pan 0x40000002", "band=x0 center=50.0")
	if p.Band != 0 {
		t.Errorf("Band = %d, want 0 for non-integer band", p.Band)
	}
	if p.CenterHz != 50_000_000 {
		t.Errorf("CenterHz = %d, want 50000000", p.CenterHz)
	}

	// No handle after "pan" → empty (caller drops it).
	if p := ParsePan("pan", "center=14.0"); p.Handle != "" {
		t.Errorf("Handle = %q, want empty", p.Handle)
	}
	// Defensive: a caller passing just the handle (no "pan" prefix) still works.
	if p := ParsePan("0x40000003", "center=14.0"); p.Handle != "0x40000003" {
		t.Errorf("Handle = %q, want 0x40000003 (no-prefix fallback)", p.Handle)
	}
}

func TestParseSlice_PanHandle(t *testing.T) {
	// When the slice status carries a pan=<handle> field, it is captured so the
	// bridge can correlate a band change to the slice's panadapter.
	s, err := ParseSlice("0 0", "RF_frequency=14.100000 mode=USB active=1 pan=0x40000000")
	if err != nil {
		t.Fatal(err)
	}
	if s.PanHandle != "0x40000000" {
		t.Errorf("PanHandle = %q, want 0x40000000", s.PanHandle)
	}
	// Absent pan= leaves PanHandle empty (bridge falls back to single/lowest pan).
	s, _ = ParseSlice("0 0", "RF_frequency=14.100000 mode=USB active=1")
	if s.PanHandle != "" {
		t.Errorf("PanHandle = %q, want empty when absent", s.PanHandle)
	}
}

func TestParseStatusFields_NoEquals(t *testing.T) {
	f := ParseStatusFields("foo bar baz")
	if len(f) != 0 {
		t.Errorf("got %d fields, want 0", len(f))
	}
}

func TestParseStatusFields_QuotedValueWithSpaces(t *testing.T) {
	// A quoted value with internal spaces must stay a single token, and the
	// surrounding quotes are retained so the bridge's fieldsString round-trips
	// it. This is the mic-profile-name shape (name="Default ProSet HC6").
	f := ParseStatusFields(`type=mic name="Default ProSet HC6" active=1`)
	if f["type"] != "mic" {
		t.Errorf("type = %q, want mic", f["type"])
	}
	if f["name"] != `"Default ProSet HC6"` {
		t.Errorf("name = %q, want %q (quotes retained)", f["name"], `"Default ProSet HC6"`)
	}
	if f["active"] != "1" {
		t.Errorf("active = %q, want 1", f["active"])
	}
	// A quoted value without spaces behaves the same as before (quotes retained).
	if f := ParseStatusFields(`name="CQ"`); f["name"] != `"CQ"` {
		t.Errorf(`name = %q, want "CQ"`, f["name"])
	}
}

func TestParseProfile(t *testing.T) {
	// Real mic profile LIST frame (captured live on a FLEX-8400, SmartSDR
	// v4.2.20): caret-delimited names that may contain spaces, with a trailing
	// caret. ParseProfile takes the raw body (text after the "profile" topic
	// word) and must NOT space-split the list value.
	p := ParseProfile("mic list=Default^Default FHM-1^Default PR781^RTTYDefault^")
	if p.Type != "mic" {
		t.Errorf("Type = %q, want mic", p.Type)
	}
	if !p.IsList {
		t.Fatal("IsList = false, want true")
	}
	wantNames := []string{"Default", "Default FHM-1", "Default PR781", "RTTYDefault"}
	if len(p.Names) != len(wantNames) {
		t.Fatalf("Names = %v, want %v", p.Names, wantNames)
	}
	for i, w := range wantNames {
		if p.Names[i] != w {
			t.Errorf("Names[%d] = %q, want %q", i, p.Names[i], w)
		}
	}
	if p.IsCurrent {
		t.Error("IsCurrent = true, want false for a list frame")
	}

	// A current= frame carries the active name (a value, not a flag); may be
	// empty. Mic does not emit this on current firmware, but it is honored.
	p = ParseProfile("global current=My Global")
	if p.Type != "global" {
		t.Errorf("Type = %q, want global", p.Type)
	}
	if !p.IsCurrent {
		t.Fatal("IsCurrent = false, want true")
	}
	if p.Current != "My Global" {
		t.Errorf("Current = %q, want %q", p.Current, "My Global")
	}
	if p.IsList {
		t.Error("IsList = true, want false for a current frame")
	}

	// An empty current= (no global profile loaded) parses to IsCurrent + "".
	p = ParseProfile("global current=")
	if !p.IsCurrent || p.Current != "" {
		t.Errorf("empty current= → %+v, want IsCurrent=true Current=\"\"", p)
	}

	// The importing=/exporting= transfer flags ride the same line; flexbridge
	// ignores them (neither IsList nor IsCurrent set), but the type still parses.
	p = ParseProfile("mic importing=1")
	if p.Type != "mic" {
		t.Errorf("Type = %q, want mic", p.Type)
	}
	if p.IsList || p.IsCurrent {
		t.Errorf("importing= should not set IsList/IsCurrent; got %+v", p)
	}

	// A non-profile-type first word yields a zero ProfileStatus (Type="").
	p = ParseProfile("foo bar=1")
	if p.Type != "" || p.IsList || p.IsCurrent {
		t.Errorf("non-profile-type → %+v, want zero", p)
	}

	// No '=' after the type word → nothing to apply, type still parsed.
	p = ParseProfile("mic")
	if p.Type != "mic" || p.IsList || p.IsCurrent {
		t.Errorf("bare type → %+v, want Type=mic only", p)
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
