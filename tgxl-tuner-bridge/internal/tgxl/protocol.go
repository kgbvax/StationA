// Package tgxl talks to a FlexRadio Tuner Genius XL over its line-based TCP API
// (port 9010). protocol.go is the pure wire layer: command framing, reply
// parsing, key=value parsing and the unit conversions. Units come from the
// vendor API PDF only (docs/TG.XL.API.Commands.pdf) and are confirmed live
// before being trusted (see ../../CLAUDE.md).
package tgxl

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Line kinds on the wire.
const (
	KindVersion = 'V' // banner: "V1.1.8 [AUTH]"
	KindReply   = 'R' // R<seq>|<code>|<msg>
	KindStatus  = 'S' // S<seq>|status k=v ...
	KindMessage = 'M' // M|<text>
)

// Line is one parsed inbound line.
type Line struct {
	Kind byte
	Seq  int    // R/S only
	Code int    // R only
	Msg  string // R: message; S: payload after the seq; M: text; V: banner rest
}

// FormatCmd frames a command: C<seq>|<cmd>\n.
func FormatCmd(seq int, cmd string) string {
	return fmt.Sprintf("C%d|%s\n", seq, cmd)
}

// ParseLine parses one inbound line (without the trailing LF).
func ParseLine(s string) (Line, error) {
	s = strings.TrimRight(s, "\r\n")
	if s == "" {
		return Line{}, fmt.Errorf("empty line")
	}
	switch s[0] {
	case 'V':
		return Line{Kind: KindVersion, Msg: s}, nil
	case 'M':
		if len(s) < 2 || s[1] != '|' {
			return Line{}, fmt.Errorf("bad message line %q", s)
		}
		return Line{Kind: KindMessage, Msg: s[2:]}, nil
	case 'R':
		parts := strings.SplitN(s[1:], "|", 3)
		if len(parts) < 2 {
			return Line{}, fmt.Errorf("bad reply line %q", s)
		}
		seq, err := strconv.Atoi(parts[0])
		if err != nil {
			return Line{}, fmt.Errorf("bad reply seq in %q", s)
		}
		code, err := strconv.Atoi(parts[1])
		if err != nil {
			return Line{}, fmt.Errorf("bad reply code in %q", s)
		}
		l := Line{Kind: KindReply, Seq: seq, Code: code}
		if len(parts) == 3 {
			l.Msg = parts[2]
		}
		return l, nil
	case 'S':
		parts := strings.SplitN(s[1:], "|", 2)
		if len(parts) != 2 {
			return Line{}, fmt.Errorf("bad status line %q", s)
		}
		seq, err := strconv.Atoi(parts[0])
		if err != nil {
			return Line{}, fmt.Errorf("bad status seq in %q", s)
		}
		return Line{Kind: KindStatus, Seq: seq, Msg: parts[1]}, nil
	}
	return Line{}, fmt.Errorf("unknown line %q", s)
}

// ParseKV parses "name k=v k2= k3=v3" into a map. The leading bare word (the
// command name, e.g. "status") is returned separately. Empty values are kept.
func ParseKV(s string) (name string, kv map[string]string) {
	kv = map[string]string{}
	for i, f := range strings.Fields(s) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			if i == 0 {
				name = f
			}
			continue
		}
		kv[k] = v
	}
	return name, kv
}

// RawStatus is the decoded `status` reply for channel A (the 1x3 has one
// channel) plus the device-level fields. Power and SWR are still in wire units.
type RawStatus struct {
	FwdDBm   float64 // fwd [dBm]
	SWRdB    float64 // swr [dB] (return loss; negative when no signal, e.g. -60)
	PTT      bool    // pttA or pttB (either channel keyed)
	Bypass   bool    // bypass (device bypass)
	Operate  bool    // state: 1 = OPERATE, 0 = STANDBY
	Tuning   bool    // tuning
	Antenna  int     // antA (1-3)
	RelayC1  int
	RelayL   int
	RelayC2  int
	Complete bool // every required key was present
}

// ParseStatus decodes the payload of an S line ("status fwd=… …").
func ParseStatus(payload string) (RawStatus, error) {
	name, kv := ParseKV(payload)
	if name != "status" {
		return RawStatus{}, fmt.Errorf("not a status payload: %q", payload)
	}
	var st RawStatus
	var missing []string
	f := func(k string) float64 {
		v, err := strconv.ParseFloat(kv[k], 64)
		if err != nil {
			missing = append(missing, k)
		}
		return v
	}
	n := func(k string) int { return int(f(k)) }
	st.FwdDBm = f("fwd")
	st.SWRdB = f("swr")
	st.PTT = n("pttA") == 1
	if v, ok := kv["pttB"]; ok && v == "1" { // optional: absent on some firmware
		st.PTT = true
	}
	st.Bypass = n("bypass") == 1
	st.Operate = n("state") == 1
	st.Tuning = n("tuning") == 1
	st.Antenna = n("antA")
	st.RelayC1 = n("relayC1")
	st.RelayL = n("relayL")
	st.RelayC2 = n("relayC2")
	if len(missing) > 0 {
		return st, fmt.Errorf("status missing/invalid keys: %s", strings.Join(missing, ","))
	}
	st.Complete = true
	return st, nil
}

// DBmToWatts converts dBm to whole watts. Below 1 W reads as 0 (the meter floor
// reports values such as 0.00 / -60 when idle).
func DBmToWatts(dbm float64) uint16 {
	w := math.Pow(10, (dbm-30)/10)
	if w < 1 || math.IsNaN(w) {
		return 0
	}
	if w > math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(math.Round(w))
}

// ReturnLossToSWR converts the API's "swr [dB]" (return loss) into an SWR
// ratio, clamped to [1, 99] and rounded to 0.01. A non-negative or absent value
// (idle, e.g. -60 with no carrier is a huge RL → 1.00) maps to 1.
func ReturnLossToSWR(db float64) float64 {
	rl := math.Abs(db)
	gamma := math.Pow(10, -rl/20)
	if gamma >= 1 {
		return 99
	}
	swr := (1 + gamma) / (1 - gamma)
	if swr < 1 {
		swr = 1
	}
	if swr > 99 {
		swr = 99
	}
	return math.Round(swr*100) / 100
}

// Discovery is a parsed UDP 9010 broadcast.
type Discovery struct {
	IP, Version, Serial, Nickname string
}

// ParseDiscovery parses "TunerGenius ip=… v=… serial=… nickname=…".
func ParseDiscovery(s string) (Discovery, bool) {
	name, kv := ParseKV(strings.TrimSpace(s))
	if name != "TunerGenius" || kv["ip"] == "" {
		return Discovery{}, false
	}
	return Discovery{IP: kv["ip"], Version: kv["v"], Serial: kv["serial"],
		Nickname: strings.ReplaceAll(kv["nickname"], "_", " ")}, true
}
