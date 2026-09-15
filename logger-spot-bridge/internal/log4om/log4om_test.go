package log4om

import "testing"

// The outbound CALLSIGN packet format is undocumented (advanced guide names
// the service only). These fixtures cover the shapes the tolerant decoder
// must survive until the first live capture pins the real one.
func TestDecodeCallsignTolerantShapes(t *testing.T) {
	cases := []struct {
		name     string
		datagram string
		wantCall string
		wantFreq int64
		wantLoc  string
	}{
		{
			name:     "pascal case with locator",
			datagram: `<Log4OM><Callsign>DL1ABC</Callsign><Frequency>14250000</Frequency><Band>20m</Band><Mode>USB</Mode><Locator>JO62</Locator></Log4OM>`,
			wantCall: "DL1ABC", wantFreq: 14250000, wantLoc: "JO62",
		},
		{
			name:     "lower case",
			datagram: `<callsign_data><call>OK2XYZ</call><freq>7074000</freq><mode>CW</mode></callsign_data>`,
			wantCall: "OK2XYZ", wantFreq: 7074000, wantLoc: "",
		},
		{
			name:     "n1mm-style radio info spelling scaled to hz",
			datagram: `<RadioInfo><Freq>14025000</Freq><Mode>USB</Mode><OpCall>DJ1XYZ</OpCall></RadioInfo>`,
			// NOTE: RadioInfo is 10 Hz units → 140250000 would be wrong; the
			// decoder scales only values below 100000 (i.e. sub-kHz counts),
			// so 14025000 stays Hz and the call is absent → error. Covered by
			// the no-callsign test below.
			wantCall: "", wantFreq: 0, wantLoc: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := DecodeCallsign([]byte(c.datagram))
			if c.wantCall == "" {
				if err == nil {
					t.Fatalf("expected decode failure, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Call != c.wantCall {
				t.Errorf("Call = %q, want %q", got.Call, c.wantCall)
			}
			if got.FreqHz != c.wantFreq {
				t.Errorf("FreqHz = %d, want %d", got.FreqHz, c.wantFreq)
			}
			if got.Locator != c.wantLoc {
				t.Errorf("Locator = %q, want %q", got.Locator, c.wantLoc)
			}
		})
	}
}

func TestDecodeCallsignPlainText(t *testing.T) {
	// The LIVE format (captured 2026-09-15): the datagram IS the callsign.
	got, err := DecodeCallsign([]byte("VU2ATN"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Call != "VU2ATN" || got.Raw != "VU2ATN" {
		t.Fatalf("got %+v", got)
	}

	// Whitespace padding tolerated; case normalized.
	got, err = DecodeCallsign([]byte(" dl1abc \r\n"))
	if err != nil || got.Call != "DL1ABC" {
		t.Fatalf("padded: %+v err=%v", got, err)
	}

	// Special-event call without digits passes.
	if _, err = DecodeCallsign([]byte("RAEM")); err != nil {
		t.Fatalf("RAEM rejected: %v", err)
	}

	// Rejects: empty, garbage, too short, frequency-like digits-only.
	for _, bad := range []string{"", "  ", "AB", "DL-1ABC-with-a-long-suffix", "14025000"} {
		if _, err := DecodeCallsign([]byte(bad)); err == nil {
			t.Errorf("payload %q accepted as a callsign", bad)
		}
	}
}

func TestDecodeCallsignSubKHzScaling(t *testing.T) {
	// A count that is absurd as Hz but sane as 10 Hz units is scaled
	// (RadioInfo dialect leakage into the callsign service port).
	datagram := `<Log4OM><Callsign>DL1ABC</Callsign><Freq>1402500</Freq></Log4OM>`
	got, err := DecodeCallsign([]byte(datagram))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.FreqHz != 14025000 {
		t.Errorf("FreqHz = %d, want 14025000 (10 Hz units scaled)", got.FreqHz)
	}
}

func TestDecodeCallsignRejects(t *testing.T) {
	if _, err := DecodeCallsign([]byte("plain garbage")); err == nil {
		t.Fatal("non-XML accepted")
	}
	if _, err := DecodeCallsign([]byte(`<RadioInfo><Freq>14025000</Freq></RadioInfo>`)); err == nil {
		t.Fatal("datagram without callsign accepted")
	}
}

func TestRootName(t *testing.T) {
	if got := RootName([]byte(`<?xml version="1.0"?><lookupinfo><call>X</call></lookupinfo>`)); got != "lookupinfo" {
		t.Errorf("RootName = %q", got)
	}
	if got := RootName([]byte(`nope`)); got != "" {
		t.Errorf("RootName(garbage) = %q, want empty", got)
	}
}
