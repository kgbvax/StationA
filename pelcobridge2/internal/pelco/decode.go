package pelco

import "fmt"

// maxLogCol bounds every line Decode emits. The log pane truncates with no
// wrap and no ellipsis, so a long line silently loses its tail — which hides
// the position word and the checksum verdict on an 80-column terminal, exactly
// the two fields the operator reads the log for.
const maxLogCol = 50

// Decode renders a frame for the log pane: raw hex first, then a raw field
// breakdown, then the degrees interpretation (textbook hundredths for both
// position responses — bench-confirmed 2026-08-28; there is no untrusted-hint
// attribution anymore because there is no hypothesis anymore).
func Decode(r RxFrame, dir string) []string {
	lines := []string{
		fmt.Sprintf("%s %s", dir, r.Hex()),
		fmt.Sprintf("   raw: addr=%02X cmd1=%02X cmd2=%02X d1=%02X d2=%02X",
			r.Frame[1], r.Frame[2], r.Frame[3], r.Frame[4], r.Frame[5]),
		fmt.Sprintf("   word=%04X (%d)  chk=%02X %s",
			r.Word(), r.Word(), r.chkByte(), chkStr(r)),
	}
	switch r.Op() {
	case OpRspPan:
		lines = append(lines, fmt.Sprintf("   pan %.2f°", WordToDeg(r.Word())))
	case OpRspTilt:
		lines = append(lines, fmt.Sprintf("   tilt %.2f°", WordToDeg(r.Word())))
	case OpSetPan:
		lines = append(lines, fmt.Sprintf("   set pan %.2f°", WordToDeg(r.Word())))
	case OpSetTilt:
		lines = append(lines, fmt.Sprintf("   set tilt %.2f°", WordToDeg(r.Word())))
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = trunc(l, maxLogCol)
	}
	return out
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// chkByte is the checksum byte actually on the wire. For a Pelco-P frame that
// is wire[7] (the XOR), not Frame[6]: Frame is the logical D-fields view whose
// [6] holds the additive D sum.
func (r RxFrame) chkByte() byte {
	if r.P && len(r.Wire) >= FrameLenP {
		return r.Wire[7]
	}
	if len(r.Wire) == FrameLen {
		return r.Wire[6]
	}
	return r.Frame[6]
}

func (r RxFrame) checksum() byte {
	return byte(uint16(r.Frame[1]) + uint16(r.Frame[2]) + uint16(r.Frame[3]) + uint16(r.Frame[4]) + uint16(r.Frame[5]))
}

func chkStr(r RxFrame) string {
	if r.ChkOK() {
		return "ok"
	}
	if r.P {
		if len(r.Wire) >= FrameLenP && r.Wire[6] != ETX {
			return fmt.Sprintf("BAD (byte 7 is %02X, not ETX AF)", r.Wire[6])
		}
		return fmt.Sprintf("BAD (want %02X)", PXor(r.Wire))
	}
	if len(r.Wire) == FrameLenP {
		return "unverifiable (8 bytes, no A0 STX)"
	}
	return fmt.Sprintf("BAD (want %02X)", r.checksum())
}
