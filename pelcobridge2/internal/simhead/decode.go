package simhead

import "pelcobridge2/internal/pelco"

// DecodeAny parses one complete 7-byte Pelco-D frame off the wire, validating
// the checksum. Returns ok=false for anything else (the head stays silent on
// undecodable bytes, so the simulator does too).
func DecodeAny(b []byte) (pelco.RxFrame, bool) {
	if len(b) >= pelco.FrameLen && b[0] == 0xFF {
		var f pelco.Frame
		copy(f[:], b[:pelco.FrameLen])
		if !f.ChkOK() {
			return pelco.RxFrame{}, false
		}
		return pelco.RxFrame{Frame: f, Wire: append([]byte(nil), b[:pelco.FrameLen]...)}, true
	}
	return pelco.RxFrame{}, false
}
