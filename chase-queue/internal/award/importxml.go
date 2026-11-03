package award

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// ErrDeferred is returned for a REFERENCE award (SOTA, WWFF, POTA): those
// come back together later, so they are not imported.
var ErrDeferred = errors.New("REFERENCE awards (SOTA, WWFF, POTA) are deferred and not imported")

// maxKids bounds the children kept of one XML element: an award's item list
// (a REFERENCE award has 60 000 to 180 000 items) is never read into memory.
const maxKids = 500

// node is a generic XML element; the Log4OM award export has no published
// schema, so the importer looks fields up by name instead of binding a struct.
type node struct {
	name      string
	text      string
	kids      []*node
	truncated bool
}

func parseTree(r io.Reader) (*node, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	var stack []*node
	var root *node
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("award XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &node{name: strings.ToLower(t.Name.Local)}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				if len(p.kids) >= maxKids {
					p.truncated = true
					if err := dec.Skip(); err != nil {
						return nil, fmt.Errorf("award XML: %w", err)
					}
					continue
				}
				p.kids = append(p.kids, n)
			} else if root == nil {
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 0 {
				if n := stack[len(stack)-1]; len(n.text) < 4096 {
					n.text += string(t)
				}
			}
		}
	}
	if root == nil {
		return nil, errors.New("award XML: empty document")
	}
	return root, nil
}

func (n *node) child(names ...string) *node {
	for _, want := range names {
		for _, k := range n.kids {
			if k.name == want {
				return k
			}
		}
	}
	return nil
}

func (n *node) str(names ...string) string {
	if c := n.child(names...); c != nil {
		return strings.TrimSpace(c.text)
	}
	return ""
}

// find returns the first element at or below n with a child of the name.
func (n *node) findWithChild(name string) *node {
	if n.child(name) != nil {
		return n
	}
	for _, k := range n.kids {
		if f := k.findWithChild(name); f != nil {
			return f
		}
	}
	return nil
}

// list reads a value list: comma/semicolon/space separated text, or child
// elements whose own text (or RC/Key/Value/Code child: Log4OM keeps the item
// key in RC, its Code is the award code) is the value.
func (n *node) list() []string {
	var out []string
	add := func(s string) {
		for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t' || r == '\r' }) {
			out = append(out, f)
		}
	}
	add(n.text)
	for _, k := range n.kids {
		if v := k.str("rc", "key", "value", "item", "code"); v != "" {
			out = append(out, v)
			continue
		}
		add(k.text)
	}
	return out
}

// ImportXML converts a Log4OM award export into definitions: the award itself
// and one named config per AwardConfiguration. Only the rule header and the
// configurations are read; a QSOFIELDS award keeps its (small) item list, a
// REFERENCE award gives ErrDeferred without its items being read. notes lists
// what could not be carried over.
func ImportXML(r io.Reader) (def Definition, notes []string, err error) {
	root, err := parseTree(r)
	if err != nil {
		return Definition{}, nil, err
	}
	a := root.findWithChild("awardtype")
	if a == nil {
		return Definition{}, nil, errors.New("award XML: no AwardType element: not a Log4OM award export")
	}
	typ := strings.ToUpper(a.str("awardtype"))
	code := a.str("awardcode", "code", "awardname", "name")
	if code == "" {
		return Definition{}, nil, errors.New("award XML: the award has no code or name")
	}
	switch typ {
	case "REFERENCE":
		return Definition{}, nil, fmt.Errorf("award %s: %w", code, ErrDeferred)
	case "QSOFIELDS":
	default:
		return Definition{}, nil, fmt.Errorf("award %s: unknown AwardType %q (QSOFIELDS or REFERENCE)", code, typ)
	}
	def = Definition{Code: sanitizeCode(code), Name: a.str("awardname", "name", "description"), Kind: KindField, Rule: RuleConfirmed}
	if def.Name == "" || def.Name == code {
		def.Name = code
	}
	def.Field = strings.ToUpper(a.str("qsofield", "matchfield", "field"))
	if def.Field != "STATE" {
		return Definition{}, nil, fmt.Errorf("award %s: QSO field %q is not supported (only STATE)", code, def.Field)
	}
	if c := a.child("alloweddxcc", "alloweddxccs"); c != nil {
		for _, v := range c.list() {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return Definition{}, nil, fmt.Errorf("award %s: AllowedDxcc value %q is not an ADIF entity number", code, v)
			}
			def.DXCC = append(def.DXCC, n)
		}
	}
	if c := a.child("items", "awarditems", "awarditemlist"); c != nil {
		if c.truncated {
			return Definition{}, nil, fmt.Errorf("award %s: the item list is too long for a QSOFIELDS award (over %d items)", code, maxKids)
		}
		for _, v := range c.list() {
			def.Items = append(def.Items, strings.ToUpper(v))
		}
	}
	switch m := strings.ToUpper(a.str("confirmationmethod", "confirmation", "confirmationmethods")); {
	case m == "NONE" || m == "WORKED":
		def.Rule = RuleWorked
	case m == "CUSTOM":
		notes = append(notes, "ConfirmationMethod CUSTOM: read as confirmed (any QSL, LoTW, eQSL or Club Log)")
	}
	if cs := a.child("awardconfigurations", "configurations"); cs != nil {
		for _, c := range cs.kids {
			cfg := Config{Name: sanitizeCode(c.str("name", "code", "configname"))}
			if cfg.Name == "" {
				notes = append(notes, "an AwardConfiguration without a name was skipped")
				continue
			}
			if b := c.child("bands", "band"); b != nil {
				for _, v := range b.list() {
					cfg.Bands = append(cfg.Bands, strings.ToLower(v))
				}
			}
			var modes []string
			for _, nm := range []string{"modes", "mode", "emissions", "emission", "emissiontype"} {
				if m := c.child(nm); m != nil {
					modes = append(modes, m.list()...)
				}
			}
			cfg.Modes = modeClasses(modes)
			for _, nm := range []string{"continents", "continent", "groups", "group"} {
				if f := c.child(nm); f != nil && len(f.list()) > 0 {
					notes = append(notes, fmt.Sprintf("config %s: %s filter dropped (not supported)", cfg.Name, nm))
				}
			}
			def.Configs = append(def.Configs, cfg)
		}
	}
	return def, notes, nil
}

func sanitizeCode(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '+', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

// modeClasses maps Log4OM mode or emission names to mode classes.
func modeClasses(in []string) []string {
	var out []string
	for _, m := range in {
		c := ClassData
		switch strings.ToUpper(strings.TrimSpace(m)) {
		case "CW":
			c = ClassCW
		case "SSB", "USB", "LSB", "AM", "FM", "PHONE", "VOICE":
			c = ClassPhone
		}
		dup := false
		for _, o := range out {
			dup = dup || o == c
		}
		if !dup {
			out = append(out, c)
		}
	}
	return out
}

// EncodeTOML renders definitions as a definition file.
func EncodeTOML(defs ...Definition) ([]byte, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(File{Awards: defs}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
