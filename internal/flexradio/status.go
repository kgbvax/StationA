package flexradio

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SmartSDR status/reply framing.
//
// The TCP API uses newline-delimited ASCII. Outgoing commands look like
//
//	C<handle>|<command> <args...>
//
// Replies are either responses
//
//	R<handle>|<seq>|<OK|error>
//
// or asynchronous status messages
//
//	S<handle>|<topic> <key=value> <key=value> ...
//
// All flexbridge cares about are status (S|) messages and the R| ack to
// the version handshake. This file parses the status payloads into typed
// events; framing (splitting C|R|S lines) lives in client.go.

// FrameKind classifies a parsed TCP line.
type FrameKind int

const (
	FrameUnknown FrameKind = iota
	FrameReply             // R<handle>|...
	FrameStatus            // S<handle>|...
	FrameCommand           // C<handle>|... (echoed; ignored)
)

// Frame is a parsed top-level line.
type Frame struct {
	Kind   FrameKind
	Handle string // the leading C/R/S prefix content (e.g. "0") - mostly unused
	// For FrameReply: the sequence number and body ("0|OK" etc).
	Seq  string
	Body string
	// For FrameStatus: the topic (e.g. "slice", "interlock", "atu") and
	// the remainder (e.g. "0 0 freq=...").
	Topic     string
	TopicArgs string // text between the topic and the first key=value
	Fields    map[string]string
}

// ParseFrame splits one complete newline-delimited line into a Frame.
// It does NOT parse the status key=value payload (that's ParseStatusFields);
// for non-status frames Fields is nil.
func ParseFrame(line string) (Frame, error) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return Frame{}, errors.New("flexradio: empty frame")
	}
	f := Frame{Kind: FrameUnknown}
	switch line[0] {
	case 'R':
		f.Kind = FrameReply
	case 'S':
		f.Kind = FrameStatus
	case 'C':
		f.Kind = FrameCommand
	default:
		return f, fmt.Errorf("flexradio: unrecognized frame prefix %q", line[0])
	}
	rest := line[1:]
	if idx := strings.IndexByte(rest, '|'); idx >= 0 {
		f.Handle = rest[:idx]
		rest = rest[idx+1:]
	} else {
		f.Handle = rest
		rest = ""
	}

	if f.Kind == FrameStatus {
		// rest = "<topic> <topic-args> <key=value> ..."
		topic, topicRest := splitFirstWord(rest)
		f.Topic = topic
		f.Fields = ParseStatusFields(topicRest)
		// topicArgs = leading words before the first key=value token
		f.TopicArgs = extractTopicArgs(topicRest)
	} else {
		f.Body = rest
	}
	return f, nil
}

// splitFirstWord splits s into its first whitespace-delimited word and the
// remainder (trimmed). If there is no whitespace, the whole string is the
// first word and the remainder is empty.
func splitFirstWord(s string) (string, string) {
	s = strings.TrimLeft(s, " ")
	i := strings.IndexByte(s, ' ')
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeft(s[i+1:], " ")
}

// extractTopicArgs returns the leading words of s that are NOT key=value
// tokens. For "0 0 freq=14.100.000" that's "0 0".
func extractTopicArgs(s string) string {
	var args []string
	for _, tok := range strings.Fields(s) {
		if strings.Contains(tok, "=") {
			break
		}
		args = append(args, tok)
	}
	return strings.Join(args, " ")
}

// ParseStatusFields parses the trailing key=value tokens of a status line
// into a map. Tokens without '=' are ignored. Values are not unquoted
// (SmartSDR rarely quotes values; the rare quoted ones are passed through).
func ParseStatusFields(s string) map[string]string {
	out := make(map[string]string)
	for _, tok := range strings.Fields(s) {
		eq := strings.IndexByte(tok, '=')
		if eq <= 0 {
			continue
		}
		out[tok[:eq]] = tok[eq+1:]
	}
	return out
}

// SliceStatus holds the per-slice fields flexbridge publishes. Populated
// from "slice" status lines.
type SliceStatus struct {
	Index      int    // first topic arg (slice list index)
	RcvrIndex  int    // second topic arg (receiver/panadapter index, may be absent)
	FreqHz     int64  // frequency in Hz (parsed from RF_frequency in MHz or freq)
	Mode       string // USB, LSB, CW, ...
	Active     bool   // active=1
	TX         bool   // tx=1
	AGCMode    string // agc_mode / agc
	FilterLow  int    // filter_lo (Hz)
	FilterHigh int    // filter_hi (Hz)
}

// ParseSlice parses an "S|slice ..." status line body into a SliceStatus.
//
// Firmware field names (SmartSDR v3/v4):
//   - RF_frequency=<MHz float>  (e.g. "3.800000") -- the live VFO frequency
//   - freq=<dotted Hz>          (legacy/older firmware, e.g. "14.100.000")
//   - agc_mode=<mode>           (med, fast, slow, off; older fw used "agc")
//   - mode, active, tx, filter_lo, filter_hi
//
// We accept both naming conventions.
func ParseSlice(topicArgs, fieldsStr string) (SliceStatus, error) {
	f := ParseStatusFields(fieldsStr)
	args := strings.Fields(topicArgs)
	var s SliceStatus
	if len(args) > 0 {
		s.Index, _ = strconv.Atoi(args[0])
	}
	if len(args) > 1 {
		s.RcvrIndex, _ = strconv.Atoi(args[1])
	}
	// Frequency: prefer RF_frequency (MHz float); fall back to legacy freq.
	if v, ok := f["RF_frequency"]; ok {
		mhz, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return s, fmt.Errorf("slice RF_frequency: %w", err)
		}
		s.FreqHz = int64(mhz * 1e6)
	} else if v, ok := f["freq"]; ok {
		hz, err := parseFlexFreq(v)
		if err != nil {
			return s, fmt.Errorf("slice freq: %w", err)
		}
		s.FreqHz = hz
	}
	s.Mode = f["mode"]
	// AGC mode: newer firmware uses agc_mode, older used agc.
	if v, ok := f["agc_mode"]; ok {
		s.AGCMode = v
	} else {
		s.AGCMode = f["agc"]
	}
	s.Active = f["active"] == "1"
	s.TX = f["tx"] == "1"
	if v, err := strconv.Atoi(f["filter_lo"]); err == nil {
		s.FilterLow = v
	}
	if v, err := strconv.Atoi(f["filter_hi"]); err == nil {
		s.FilterHigh = v
	}
	return s, nil
}

// parseFlexFreq parses a SmartSDR frequency string like "14.100.000" or
// "0.14100000" or "14100000" into Hz.
func parseFlexFreq(s string) (int64, error) {
	// Strip dot digit grouping, then parse as integer Hz.
	clean := strings.ReplaceAll(s, ".", "")
	if clean == "" {
		return 0, errors.New("empty frequency")
	}
	return strconv.ParseInt(clean, 10, 64)
}

// InterlockState enumerates radio transmit interlock states.
type InterlockState string

const (
	InterlockReceiving    InterlockState = "RECEIVING"
	InterlockTransmitting InterlockState = "TRANSMITTING"
	InterlockReady        InterlockState = "READY"
	InterlockPTT          InterlockState = "PTT_REQUESTED"
	InterlockError        InterlockState = "ERROR"
	InterlockUnknown      InterlockState = "UNKNOWN"
)

// ParseInterlock parses an "S|interlock ..." status body and returns the
// state plus the ready/transmitting booleans we publish.
type InterlockStatus struct {
	State        InterlockState
	Transmitting bool
	Reason       string // cause string, if present
}

// ParseInterlock extracts the interlock state from a status fields map.
func ParseInterlock(fieldsStr string) InterlockStatus {
	f := ParseStatusFields(fieldsStr)
	st := InterlockState(strings.ToUpper(f["state"]))
	if st == "" {
		st = InterlockUnknown
	}
	return InterlockStatus{
		State:        st,
		Transmitting: st == InterlockTransmitting,
		Reason:       f["cause"],
	}
}

// ATUStatus holds the automatic tuner state we publish.
type ATUStatus struct {
	Status string // "Tuned", "Bypass", "Tuning", "Bypass", "Error"
	Active bool
}

// ParseATU parses an "S|atu ..." status body.
func ParseATU(fieldsStr string) ATUStatus {
	f := ParseStatusFields(fieldsStr)
	st := strings.ToLower(f["status"])
	if st == "" {
		st = "unknown"
	}
	return ATUStatus{
		Status: st,
		Active: f["active"] == "1",
	}
}

// ParseTxPower extracts the configured transmit power from a "radio" status
// line's tx_power field (0..100). Returns ok=false when absent.
func ParseTxPower(fieldsStr string) (int, bool) {
	f := ParseStatusFields(fieldsStr)
	v, ok := f["tx_power"]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// ParseTunePower extracts the tune (carrier during ATU/tune) power from a
// "radio" status line's tune_power field (0..100). Returns ok=false when
// absent.
func ParseTunePower(fieldsStr string) (int, bool) {
	f := ParseStatusFields(fieldsStr)
	v, ok := f["tune_power"]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// MeterListEntry is one parsed entry from the packed "meter list" reply.
type MeterListEntry struct {
	Index     uint16 // the runtime meter index (1-based in the reply)
	Source    string // COD-, TX-, RAD, SLC, AMP
	SourceNum int    // source-internal num (slice index for SLC)
	Name      string // meter name (MICPEAK, FWDPWR, LEVEL, ...)
}

// ParseMeterListReply parses the body of the "meter list" command reply.
//
// The radio returns a single R<h>|<seq>|0 line whose body is:
//
//	meter 1.src=COD-#1.num=1#1.nam=MICPEAK#1.low=-150.0#1.hi=20.0#1.desc=...
//	#2.src=COD-#2.num=2#2.nam=MIC#...#3.src=TX-#3.num=5#3.nam=HWALC#...
//
// Tokens are '#'-separated, each of the form "<idx>.<field>=<value>". Fields
// per meter: src, num, nam, low, hi, desc, unit, fps. We extract src, num and
// nam for every index that appears.
//
// body is the reply body (the part after "R<h>|<seq>|"). Pass the full body
// and this function skips the leading "meter " prefix if present.
func ParseMeterListReply(body string) []MeterListEntry {
	s := strings.TrimSpace(body)
	// Strip an optional leading "meter " prefix (the reply body starts with
	// "0 meter 1.src=..." or "meter 1.src=...").
	if i := strings.Index(s, "meter "); i >= 0 {
		s = strings.TrimLeft(s[i+len("meter "):], " ")
	}
	type mf struct {
		src, nam string
		num      int
	}
	byIndex := make(map[int]*mf)
	var order []int
	for _, tok := range strings.Split(s, "#") {
		tok = strings.TrimSpace(tok)
		eq := strings.IndexByte(tok, '=')
		dot := strings.IndexByte(tok, '.')
		if eq <= 0 || dot <= 0 || dot >= eq {
			continue
		}
		idx, err := strconv.Atoi(tok[:dot])
		if err != nil {
			continue
		}
		field := tok[dot+1 : eq]
		value := tok[eq+1:]
		m, ok := byIndex[idx]
		if !ok {
			m = &mf{}
			byIndex[idx] = m
			order = append(order, idx)
		}
		switch field {
		case "src":
			m.src = value
		case "nam":
			m.nam = value
		case "num":
			if n, err := strconv.Atoi(value); err == nil {
				m.num = n
			}
		}
	}
	out := make([]MeterListEntry, 0, len(order))
	for _, idx := range order {
		m := byIndex[idx]
		if m.src == "" || m.nam == "" {
			continue
		}
		out = append(out, MeterListEntry{
			Index: uint16(idx), Source: m.src, SourceNum: m.num, Name: m.nam,
		})
	}
	return out
}
