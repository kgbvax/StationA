package expose

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SlotMeta is a parsed /meta birth certificate. Addr is the slot base topic
// "<site>/<station>/<slot>" derived from the meta topic by stripping "/meta". Expose is
// nil when the slot does not publish an `expose` block.
type SlotMeta struct {
	Addr         string
	Site         string
	Station      string
	Slot         string
	Schema       string
	Role         string
	Link         string
	Location     string
	Host         string
	Device       MetaDevice
	Capabilities map[string]any
	Expose       *Expose
}

// MetaDevice is the identity block from /meta (swappable device info).
type MetaDevice struct {
	Model    string `json:"model"`
	Serial   string `json:"serial"`
	Firmware string `json:"firmware"`
}

// rawMeta is the on-the-wire shape of /meta. Only the keys we consume are listed; unknown
// keys are ignored (forward-compatible).
type rawMeta struct {
	Schema       string          `json:"schema"`
	Role         string          `json:"role"`
	Link         string          `json:"link"`
	Location     string          `json:"location"`
	Host         string          `json:"host"`
	Device       MetaDevice      `json:"device"`
	Capabilities map[string]any  `json:"capabilities"`
	Expose       json.RawMessage `json:"expose"`
}

// Parse parses a retained /meta payload. metaTopic is the full topic the message arrived on
// (e.g. "muehle/hf/radio/meta"); the slot address is derived by stripping the trailing
// "/meta". It rejects payloads whose schema is not "1.0" or whose role is empty, since a
// consumer cannot usefully bind to a malformed birth certificate.
func Parse(metaTopic string, payload []byte) (SlotMeta, error) {
	addr, err := addrFromMetaTopic(metaTopic)
	if err != nil {
		return SlotMeta{}, err
	}
	var raw rawMeta
	if err := json.Unmarshal(payload, &raw); err != nil {
		return SlotMeta{}, fmt.Errorf("parse meta %s: %w", metaTopic, err)
	}
	if raw.Schema != "1.0" {
		return SlotMeta{}, fmt.Errorf("parse meta %s: unsupported schema %q (want \"1.0\")", metaTopic, raw.Schema)
	}
	if raw.Role == "" {
		return SlotMeta{}, fmt.Errorf("parse meta %s: missing role", metaTopic)
	}

	m := SlotMeta{
		Addr:         addr,
		Schema:       raw.Schema,
		Role:         raw.Role,
		Link:         raw.Link,
		Location:     raw.Location,
		Host:         raw.Host,
		Device:       raw.Device,
		Capabilities: raw.Capabilities,
	}
	parts := strings.Split(addr, "/")
	if len(parts) == 3 {
		m.Site, m.Station, m.Slot = parts[0], parts[1], parts[2]
	}
	if len(raw.Expose) > 0 && string(raw.Expose) != "null" {
		var ex Expose
		if err := json.Unmarshal(raw.Expose, &ex); err != nil {
			return SlotMeta{}, fmt.Errorf("parse meta %s expose: %w", metaTopic, err)
		}
		m.Expose = &ex
	}
	return m, nil
}

// AddrFromMetaTopic strips the trailing "/meta" segment from a meta topic and returns the
// slot address "<site>/<station>/<slot>". It rejects topics that are not exactly 3
// address segments plus "meta" (the MetaFilter is "<site>/+/+/meta", so this holds).
func AddrFromMetaTopic(topic string) (string, error) {
	topic = strings.TrimPrefix(topic, "/")
	seg := strings.Split(topic, "/")
	if len(seg) != 4 || seg[3] != "meta" {
		return "", fmt.Errorf("meta topic %q is not <site>/<station>/<slot>/meta", topic)
	}
	return strings.Join(seg[:3], "/"), nil
}

// addrFromMetaTopic is the internal alias used by Parse.
func addrFromMetaTopic(topic string) (string, error) { return AddrFromMetaTopic(topic) }

// CapStringList returns the capabilities entry at key as a []string, or nil. Used by
// consumers to resolve an `options_ref` into an enum option list. Non-string elements are
// stringified with fmt.Sprint so int port lists (e.g. [1,2,3,4,5]) are handled by the
// caller's own shaping if needed; here we return the raw strings as published.
func CapStringList(caps map[string]any, key string) []string {
	if caps == nil {
		return nil
	}
	v, ok := caps[key]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}

// IsAddr reports whether s looks like a slot address (3 slash segments). Useful for
// callers that want to sanity-check a derived addr.
func IsAddr(s string) bool {
	return strings.Count(s, "/") == 2
}
