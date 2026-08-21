package flexradio

import (
	"errors"
	"fmt"
	"math"
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
	// RawBody is the full status body after the topic word (topicArgs + fields,
	// unparsed). Some status lines carry values with embedded spaces that the
	// space-splitting ParseStatusFields cannot preserve (notably the `profile
	// mic list=A^B C^D` line, where profile names contain spaces delimited by
	// carets); callers that need those values parse RawBody by hand.
	RawBody string
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
		f.RawBody = topicRest
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
// into a map. Tokens without '=' are ignored. Quoted substrings that contain
// spaces are kept as a single token (see tokenizeQuoted); the surrounding
// double quotes are NOT stripped here, so a value like name="Default Proset HC6"
// round-trips through bridge.fieldsString and re-parsing intact. Callers that
// want the bare name trim the quotes themselves (e.g. ParseProfile).
func ParseStatusFields(s string) map[string]string {
	out := make(map[string]string)
	for _, tok := range tokenizeQuoted(s) {
		eq := strings.IndexByte(tok, '=')
		if eq <= 0 {
			continue
		}
		out[tok[:eq]] = tok[eq+1:]
	}
	return out
}

// tokenizeQuoted splits s on whitespace but keeps double-quoted substrings
// (which may contain spaces) as a single token, retaining the surrounding
// quotes in the returned token. This is what lets a quoted profile name such
// as name="Default ProSet HC6" survive ParseStatusFields and the bridge's
// fieldsString round-trip. Unbalanced quotes are tolerated (the rest of the
// string becomes one token), which matches the loose parsing the rest of the
// parser already applies.
func tokenizeQuoted(s string) []string {
	var toks []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			cur.WriteByte(c)
		case c == ' ' && !inQuote:
			if cur.Len() > 0 {
				toks = append(toks, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		toks = append(toks, cur.String())
	}
	return toks
}

// SliceStatus holds the per-slice fields flexbridge publishes. Populated
// from "slice" status lines.
type SliceStatus struct {
	Index      int    // first topic arg (slice list index)
	RcvrIndex  int    // second topic arg (receiver/panadapter index, may be absent)
	PanHandle  string // pan=<handle> the slice is on (hex pan stream id, e.g. "0x40000000"); "" if absent
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
// ParseSlice parses a (possibly partial) SmartSDR slice status update and
// merges it onto prev. SmartSDR sends incremental updates — only changed
// fields appear — so callers should pass the currently stored SliceStatus as
// prev; fields absent from the update are carried over unchanged.
// When prev is omitted (e.g. in tests for a full update), a zero value is used.
//
// A malformed RF_frequency/freq token is non-fatal: the freq field is skipped
// (prev FreqHz is carried over) and the rest of the frame is still applied.
// Bailing on one bad field would also drop legitimate mode/tx/filter changes
// in the same incremental update and leave them stale. The returned error is
// therefore currently always nil; it is retained for future frame-level errors.
func ParseSlice(topicArgs, fieldsStr string, prev ...SliceStatus) (SliceStatus, error) {
	f := ParseStatusFields(fieldsStr)
	args := strings.Fields(topicArgs)
	var s SliceStatus
	if len(prev) > 0 {
		s = prev[0]
	}
	if len(args) > 0 {
		s.Index, _ = strconv.Atoi(args[0])
	}
	if len(args) > 1 {
		s.RcvrIndex, _ = strconv.Atoi(args[1])
	}
	// Panadapter the slice is on. SmartSDR carries this as a `pan=<handle>`
	// field (the hex pan stream id, e.g. "0x40000000"). The exact field name is
	// confirmed live; until then this is best-effort and the bridge falls back to
	// the single/lowest tracked pan when PanHandle is empty (single-pan station).
	if v, ok := f["pan"]; ok {
		s.PanHandle = v
	}
	// Frequency: prefer RF_frequency (MHz float); fall back to legacy freq.
	// SmartSDR sends incremental slice updates — only changed fields appear —
	// so a malformed freq value must NOT drop the whole frame: doing so would
	// also discard legitimate mode/tx/filter changes carried in the same update
	// and leave them stale until the next frame that happens to include them.
	// On a bad freq token we skip the freq update (prev FreqHz is carried over)
	// and continue processing the rest of the frame. Only frame-level
	// uninterpretability would warrant bailing; a single bad field does not.
	if v, ok := f["RF_frequency"]; ok {
		mhz, err := strconv.ParseFloat(v, 64)
		if err == nil {
			// math.Round, not truncation: int64(mhz*1e6) truncates toward zero
			// and publishes ~1.2% of 10-Hz-step frequencies 1 Hz low (e.g.
			// RF_frequency=2.000040 -> 2000039 instead of 2000040), because the
			// double product lands just below the integer. Rounding fixes it.
			s.FreqHz = int64(math.Round(mhz * 1e6))
		}
	} else if v, ok := f["freq"]; ok {
		if hz, err := parseFlexFreq(v); err == nil {
			s.FreqHz = hz
		}
	}
	if v, ok := f["mode"]; ok {
		s.Mode = v
	}
	// AGC mode: newer firmware uses agc_mode, older used agc.
	if v, ok := f["agc_mode"]; ok {
		s.AGCMode = v
	} else if v, ok := f["agc"]; ok {
		s.AGCMode = v
	}
	if v, ok := f["active"]; ok {
		s.Active = v == "1"
	}
	if v, ok := f["tx"]; ok {
		s.TX = v == "1"
	}
	if v, err := strconv.Atoi(f["filter_lo"]); err == nil {
		s.FilterLow = v
	}
	if v, err := strconv.Atoi(f["filter_hi"]); err == nil {
		s.FilterHigh = v
	}
	return s, nil
}

// PanStatus holds the per-panadapter fields flexbridge needs to drive band
// changes. Populated from "display pan" status lines (subscribed via
// `sub pan all`); the SmartSDR panadapter status topic is the two-word
// "display pan", so the frame arrives as Topic="display", TopicArgs="pan <stream_id>".
type PanStatus struct {
	Handle   string // pan stream id (hex, e.g. "0x40000000"); the topic arg after "pan"
	Band     int    // band=<wavelength-in-meters>; 0 if absent (pan status carries center, not band)
	CenterHz int64  // center=<MHz> → Hz; 0 if absent
}

// ParsePan parses a "display pan" status line body into a PanStatus. The pan
// stream id (handle) is the topic arg following the literal "pan" word and is
// kept as a raw hex string — `display pan s <handle> band=N` takes it verbatim.
// Panadapter status carries `center` (MHz), not `band`; band is best-effort
// (0 when absent). topicArgs is the frame's TopicArgs ("pan <stream_id> ...").
func ParsePan(topicArgs, fieldsStr string) PanStatus {
	f := ParseStatusFields(fieldsStr)
	args := strings.Fields(topicArgs)
	var p PanStatus
	// TopicArgs = "pan <stream_id> [removed]"; the handle follows "pan".
	// A bare "pan" with no stream id (a malformed/empty pan frame) yields no handle.
	if len(args) >= 2 && args[0] == "pan" {
		p.Handle = args[1]
	} else if len(args) > 0 && args[0] != "pan" {
		p.Handle = args[0] // defensive: caller passed just the handle
	}
	if v, ok := f["band"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			p.Band = n
		}
	}
	if v, ok := f["center"]; ok {
		if mhz, err := strconv.ParseFloat(v, 64); err == nil {
			p.CenterHz = int64(math.Round(mhz * 1e6))
		}
	}
	return p
}

// ProfileStatus holds a parsed "profile" status line. SmartSDR does NOT
// broadcast profiles via `sub radio all`; the list/current are obtained by
// sending the one-shot `profile <type> info` command (undocumented, from
// FlexLib; confirmed live on a FLEX-8400, SmartSDR v4.2.20 — see the
// smartsdr-mic-profile-status-topic memory). The radio then emits status
// frames of the form:
//
//	S<h>|profile mic list=Default^Default FHM-1^…^RTTYDefault^
//	S<h>|profile global current=<name>          (mic emits NO current=)
//
// The `list=` value is caret-delimited and profile names contain spaces, so it
// cannot be parsed by the space-splitting ParseStatusFields — ParseProfile
// parses the raw body by hand. The `current=` value is the active profile name
// (a value, not a flag) and may be empty. `importing=`/`exporting=` transfer
// flags ride the same line; flexbridge ignores them. Only `mic` profiles are
// tracked.
//
// Note: SmartSDR does not report an active *mic* profile (mic profiles are
// load-only presets with no "current" pointer, unlike global profiles), so the
// `current=` line never arrives for mic in practice; the bridge tracks the
// active mic name client-side as "last loaded via set_mic_profile". ParseProfile
// still honors `current=` defensively should a firmware revision emit it.
type ProfileStatus struct {
	Type      string   // mic | global | transmit (lowercased); "" if the first word is not a profile type
	IsList    bool     // true when the frame was a list= frame (authoritative full snapshot)
	Names     []string // caret-delimited names from list= (empty entries skipped); nil if !IsList
	IsCurrent bool     // true when the frame was a current= frame
	Current   string   // active profile name from current= (may be "")
}

// ParseProfile parses a "profile" status line body (the Frame.RawBody, i.e. the
// text after the "profile" topic word) into a ProfileStatus. rawBody looks like
// "mic list=Default^Default FHM-1^…" or "global current=". The first word is the
// profile type; the remainder is a single key=value whose value may contain
// spaces (so it is taken verbatim up to end-of-line, not space-split).
func ParseProfile(rawBody string) ProfileStatus {
	typ, rest := splitFirstWord(rawBody)
	var p ProfileStatus
	if isProfileType(typ) {
		p.Type = strings.ToLower(typ)
	} else {
		return p // not a profile-type line we recognize
	}
	eq := strings.IndexByte(rest, '=')
	if eq < 0 {
		return p
	}
	key := strings.TrimSpace(rest[:eq])
	val := strings.TrimSpace(rest[eq+1:])
	switch key {
	case "list":
		p.IsList = true
		for _, name := range strings.Split(val, "^") {
			if name = strings.TrimSpace(name); name != "" {
				p.Names = append(p.Names, name)
			}
		}
	case "current":
		p.IsCurrent = true
		p.Current = val
	}
	return p
}

func isProfileType(s string) bool {
	switch strings.ToLower(s) {
	case "mic", "global", "transmit":
		return true
	}
	return false
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

// DVKStatus holds the Digital Voice Keyer state parsed from a "dvk" status
// line. SmartSDR v4+ emits (after `sub dvk all`):
//
//	S<h>|dvk status=idle|recording|preview|playback|disabled [id=<N>] [enabled=1|0]
//	S<h>|dvk added   id=<N> name="..." duration=<ms>   (memory library; not state)
//	S<h>|dvk deleted id=<N>                            (memory library; not state)
//
// Only the status= frames carry state for the /state plane; added/deleted are
// reported with HasStatus=false so the bridge can ignore them for state.
type DVKStatus struct {
	Status    string // idle|recording|preview|playback|disabled
	ID        int    // active memory id when present
	HasStatus bool   // true when a status= key was present
}

// ParseDVK parses an "S|dvk ..." status body (the joinArgsFields form, i.e.
// topic-args + key=value fields). Non-status frames (added/deleted) return
// HasStatus=false. The "dvk id=<N> deleted" word-ordering variant is handled
// by the absence of a status= key (still HasStatus=false).
func ParseDVK(fieldsStr string) DVKStatus {
	f := ParseStatusFields(fieldsStr)
	st, ok := f["status"]
	if !ok {
		return DVKStatus{} // added/deleted/other — no state change
	}
	d := DVKStatus{Status: strings.ToLower(st), HasStatus: true}
	if id, err := strconv.Atoi(f["id"]); err == nil {
		d.ID = id
	}
	return d
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

// ParseDrive extracts the transmit drive level from a "radio" status line's
// drive field (0–100). Returns ok=false when absent.
func ParseDrive(fieldsStr string) (int, bool) {
	f := ParseStatusFields(fieldsStr)
	v, ok := f["drive"]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// ParseRadioTuning extracts the tuning flag from a "radio" status line.
// Returns (true, true) when tuning=1 is present, (false, true) when
// tuning=0 is present, and (false, false) when the field is absent.
func ParseRadioTuning(fieldsStr string) (bool, bool) {
	f := ParseStatusFields(fieldsStr)
	v, ok := f["tuning"]
	if !ok {
		return false, false
	}
	return v == "1", true
}

// NormalizeMode maps a SmartSDR firmware mode string to the canonical
// vocabulary used by the station model: cw, usb, lsb, am, fm, data.
// Unknown modes return "" so the caller omits the field — the model forbids
// publishing raw firmware mode strings (band-mode-reference.md).
func NormalizeMode(s string) string {
	switch strings.ToUpper(s) {
	case "USB":
		return "usb"
	case "LSB":
		return "lsb"
	case "CW", "CW-U", "CW-L":
		return "cw"
	case "AM", "SAM":
		return "am"
	case "FM", "NFM", "DFM":
		return "fm"
	case "DIGU", "DIGL", "DATA-U", "DATA-L",
		"FDV", "FDVU", "FDVL",
		"RTTY-U", "RTTY-L",
		"PKTUSB", "PKTLSB",
		"DSTR":
		return "data"
	default:
		return ""
	}
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
