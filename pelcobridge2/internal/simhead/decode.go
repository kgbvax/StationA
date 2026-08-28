package simhead

import "pelcobridge2/internal/pelco"

// DecodeAny parses one complete frame off the wire — 7-byte Pelco-D or 8-byte
// Pelco-P — validating the matching checksum. Returns ok=false for anything
// else (the head stays silent on undecodable bytes, so the simulator does too).
func DecodeAny(b []byte) (pelco.RxFrame, bool) {
	switch {
	case len(b) >= pelco.FrameLen && b[0] == 0xFF:
		var f pelco.Frame
		copy(f[:], b[:pelco.FrameLen])
		if !f.ChkOK() {
			return pelco.RxFrame{}, false
		}
		return pelco.RxFrame{Frame: f, Wire: append([]byte(nil), b[:pelco.FrameLen]...)}, true
	case len(b) >= pelco.FrameLenP && b[0] == pelco.STX:
		wire := make([]byte, pelco.FrameLenP)
		copy(wire, b[:pelco.FrameLenP])
		if !pelco.PChkOK(wire) {
			return pelco.RxFrame{}, false
		}
		f := pelco.Frame{wire[0], wire[1], wire[2], wire[3], wire[4], wire[5], wire[7]}
		return pelco.RxFrame{Frame: f, P: true, Wire: wire}, true
	}
	return pelco.RxFrame{}, false
}
