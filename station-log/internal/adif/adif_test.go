package adif

import "testing"

func TestParseTolerant(t *testing.T) {
	rs := Parse("Log4OM export\n<ADIF_VER:5>3.1.4<eoh>\n<call:6>EA8XYZ<BAND:3>20m<mode:3>SSB<qso_date:8>20261101<time_on:6>101500<eor>\n<CALL:5>DL1AB<BAND:3>40m<EOR>")
	if len(rs) != 2 || rs[0]["CALL"] != "EA8XYZ" || rs[0]["BAND"] != "20m" || rs[1]["CALL"] != "DL1AB" {
		t.Fatalf("%v", rs)
	}
}

func TestParseNoHeaderAndTruncated(t *testing.T) {
	rs := Parse("<CALL:6>VP8LP <BAND:3>20m<EOR><CALL:9>BROKEN")
	if len(rs) != 1 || rs[0]["CALL"] != "VP8LP " {
		t.Fatalf("%v", rs)
	}
}

func TestParseInvalidUTF8Header(t *testing.T) {
	recs, trunc := ParseChecked("Gener\xe4ted by x\n<EOH>\n<CALL:5>DL1AB<BAND:3>20m<EOR>")
	if trunc || len(recs) != 1 || recs[0]["CALL"] != "DL1AB" {
		t.Fatalf("%v %v", recs, trunc)
	}
	if _, trunc := ParseChecked("<CALL:5>DL1AB<EOR><CALL:5>DL2"); !trunc {
		t.Fatal("cut-off tail not reported")
	}
}
